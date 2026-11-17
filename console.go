package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kubelamp/internal/ws"
)

// The kubectl console: a terminal for every signed-in user that runs only kubectl,
// with the user's own kubeconfig and the cluster/namespace picked above the terminal.
//
// It is not a shell. KubeLamp reads the command line itself (no sh, so no ; && $() or
// redirection), checks it, and starts `kubectl <args>` directly under a pseudo-terminal,
// so interactive commands (exec -it, logs -f, get -w, Ctrl+C) work. Anything that would
// let kubectl read or write files on the KubeLamp server, use other credentials or reach
// other servers is refused: other users' kubeconfigs live on that server.
//
//	browser → server: {"t":"i","d":keys} {"t":"r","cols","rows"} {"t":"c","ctx","ns"}
//	server  → browser: binary terminal output; {"t":"ctx","ctx","ns"} after a switch

// subcommands that work on the server's files or network, or need an editor
var consoleBlockedSub = map[string]string{
	"cp":           "copies files on the KubeLamp server",
	"plugin":       "runs programs on the KubeLamp server",
	"proxy":        "opens a port on the KubeLamp server",
	"port-forward": "opens a port on the KubeLamp server",
	"edit":         "needs an editor; use Edit YAML in KubeLamp",
	"kustomize":    "reads a folder on the KubeLamp server",
	"alpha":        "experimental commands are not available here",
}

// in read-only mode only these read
var consoleReadOnlySub = map[string]bool{
	"get": true, "describe": true, "logs": true, "top": true, "explain": true, "api-resources": true, "api-versions": true,
	"version": true, "cluster-info": true, "auth": true, "events": true, "config": true, "help": true, "wait": true,
}

// flags that name files on the server, change the credentials or the server, or write logs
var consoleBlockedFlags = map[string]string{
	"kubeconfig": "the console uses your own kubeconfig", "context": "pick the cluster in the menu above",
	"cluster": "pick the cluster in the menu above", "server": "the console talks only to the selected cluster",
	"certificate-authority": "reads a file on the KubeLamp server", "client-certificate": "reads a file on the KubeLamp server",
	"client-key": "reads a file on the KubeLamp server", "cache-dir": "writes on the KubeLamp server",
	"filename": "local files are not available here (use the YAML editor in KubeLamp)", "kustomize": "reads a folder on the KubeLamp server",
	"from-file": "reads a file on the KubeLamp server (use --from-literal)", "from-env-file": "reads a file on the KubeLamp server",
	"env-file": "reads a file on the KubeLamp server", "patch-file": "reads a file on the KubeLamp server (use --patch)",
	"output-directory": "writes on the KubeLamp server", "log-file": "writes on the KubeLamp server", "log-dir": "writes on the KubeLamp server",
	"log_file": "writes on the KubeLamp server", "log_dir": "writes on the KubeLamp server",
	"profile-output": "writes on the KubeLamp server", "token": "the console uses your own kubeconfig", "username": "the console uses your own kubeconfig",
	"password": "the console uses your own kubeconfig", "tls-server-name": "the console talks only to the selected cluster",
}

// splitLine splits a command line like a shell would, without running one: words,
// 'single' and "double" quotes, backslash escapes. A bare | is its own word.
func splitLine(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote, esc := false, rune(0), false
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc, in = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		case r == '|':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
			out = append(out, "|")
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("missing closing quote")
	}
	if esc {
		return nil, errors.New("line ends with a backslash")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// grepFilter is the one pipe the console understands: | grep [-i] [-v] [-E] pattern
type grepFilter struct {
	re     *regexp.Regexp
	invert bool
}

func (g *grepFilter) keep(line string) bool { return g.re.MatchString(stripANSI(line)) != g.invert }

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(strings.TrimRight(s, "\r"), "") }

// kubectl subcommands, for "get pods" without the kubectl prefix and Tab completion
var kubectlSubs = []string{"annotate", "api-resources", "api-versions", "apply", "attach", "auth", "autoscale", "certificate",
	"cluster-info", "completion", "config", "cordon", "create", "debug", "delete", "describe", "diff", "drain", "events", "exec",
	"explain", "expose", "get", "label", "logs", "patch", "replace", "rollout", "run", "scale", "set", "taint", "top", "uncordon",
	"version", "wait", "cp", "proxy", "port-forward", "edit", "plugin", "kustomize", "alpha"} // the last ones are refused with a reason

// parseConsole turns a command line into kubectl arguments and an optional grep.
// builtin is set for console commands (help, clear).
func parseConsole(line string) (args []string, g *grepFilter, builtin string, err error) {
	words, err := splitLine(line)
	if err != nil || len(words) == 0 {
		return nil, nil, "", err
	}
	switch words[0] {
	case "help", "?":
		return nil, nil, "help", nil
	case "clear", "cls":
		return nil, nil, "clear", nil
	}
	if words[0] == "kubectl" || words[0] == "k" {
		words = words[1:]
	} else if !contains(kubectlSubs, words[0]) {
		return nil, nil, "", fmt.Errorf("only kubectl runs here — try `kubectl get pods` (type help)")
	}
	cmd := words
	if i := indexOf(words, "|"); i >= 0 && !(indexOf(words, "--") >= 0 && indexOf(words, "--") < i) {
		cmd = words[:i]
		pipe := words[i+1:]
		if len(pipe) == 0 || pipe[0] != "grep" || indexOf(pipe, "|") >= 0 {
			return nil, nil, "", errors.New("only one `| grep pattern` can follow a command")
		}
		g = &grepFilter{}
		ci, pat := false, ""
		for _, p := range pipe[1:] {
			switch p {
			case "-i":
				ci = true
			case "-v":
				g.invert = true
			case "-E", "-e":
			case "-iv", "-vi":
				ci, g.invert = true, true
			default:
				if pat != "" {
					return nil, nil, "", errors.New("grep takes one pattern (quote it if it has spaces)")
				}
				pat = p
			}
		}
		if pat == "" {
			return nil, nil, "", errors.New("grep needs a pattern")
		}
		if ci {
			pat = "(?i)" + pat
		}
		if g.re, err = regexp.Compile(pat); err != nil {
			return nil, nil, "", fmt.Errorf("grep pattern: %v", err)
		}
	}
	if len(cmd) == 0 {
		return nil, nil, "", errors.New("type a kubectl command, e.g. `get pods`")
	}
	return cmd, g, "", nil
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// checkKubectlArgs refuses what could reach the KubeLamp server's files, network or
// other credentials. Words after "--" go to the container (exec, debug, run) and are
// not checked.
func checkKubectlArgs(args []string, readOnly bool) error {
	end := indexOf(args, "--")
	if end < 0 {
		end = len(args)
	}
	sub, sub2 := "", ""
	for _, a := range args[:end] {
		if !strings.HasPrefix(a, "-") {
			if sub == "" {
				sub = a
			} else if sub2 == "" {
				sub2 = a
			}
		}
	}
	if why, bad := consoleBlockedSub[sub]; bad {
		return fmt.Errorf("`kubectl %s` isn't available here: it %s", sub, why)
	}
	if sub == "apply" && (sub2 == "edit-last-applied" || sub2 == "set-last-applied") {
		return fmt.Errorf("`kubectl apply %s` isn't available here", sub2)
	}
	if sub == "config" && sub2 != "view" && sub2 != "get-contexts" && sub2 != "current-context" && sub2 != "get-clusters" && sub2 != "get-users" {
		return errors.New("only `kubectl config view / get-contexts / current-context` work here; pick the cluster in the menu above")
	}
	if readOnly && sub != "" && !consoleReadOnlySub[sub] {
		return fmt.Errorf("KubeLamp is read-only: `kubectl %s` is disabled (get, describe, logs, top, events … work)", sub)
	}
	pre := args[:end]
	for i, a := range pre {
		next := ""
		if i+1 < len(pre) {
			next = pre[i+1]
		}
		// output templates read from a file: -o go-template-file=… in any spelling
		if fileOutputRe.MatchString(a) {
			return errors.New("template files are not available here; give the template inline")
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		// "-f -" / "--filename=-": the manifest is pasted into the terminal (stdin), no file
		if (a == "-f" || a == "--filename") && next == "-" || a == "-f=-" || a == "--filename=-" {
			continue
		}
		if strings.HasPrefix(a, "--") {
			name, val, _ := strings.Cut(a[2:], "=")
			if why, bad := consoleBlockedFlags[name]; bad {
				return fmt.Errorf("--%s isn't available here: %s", name, why)
			}
			if (name == "output" || name == "template") && strings.Contains(val, "file=") {
				return errors.New("template files are not available here; give the template inline")
			}
			continue
		}
		// short flags: -f (filename, but --follow for logs), -k (kustomize), -s (server)
		switch {
		case strings.HasPrefix(a, "-f") && sub != "logs":
			return errors.New("-f (local files) isn't available here: use the YAML editor in KubeLamp")
		case strings.HasPrefix(a, "-k"):
			return errors.New("-k (kustomize folders) isn't available here")
		case strings.HasPrefix(a, "-s"):
			return errors.New("-s (another server) isn't available here")
		case strings.HasPrefix(a, "-o") && strings.Contains(a, "file="):
			return errors.New("template files are not available here; give the template inline")
		}
	}
	return nil
}

var fileOutputRe = regexp.MustCompile(`(?i)(template|jsonpath|columns)-file\b`)

// kubectlArgv adds the console's context and namespace. They go before "--" so they
// never reach a container command. A namespace given on the line wins.
func kubectlArgv(args []string, ctxName, ns string, inCluster bool) []string {
	end := indexOf(args, "--")
	if end < 0 {
		end = len(args)
	}
	var extra []string
	if !inCluster {
		extra = append(extra, "--context", ctxName)
	}
	hasNS := false
	for _, a := range args[:end] {
		if a == "-n" || strings.HasPrefix(a, "-n=") || strings.HasPrefix(a, "--namespace") || a == "-A" || a == "--all-namespaces" ||
			(strings.HasPrefix(a, "-n") && len(a) > 2 && !strings.HasPrefix(a, "--")) {
			hasNS = true
		}
	}
	if ns != "" && !hasNS {
		extra = append(extra, "--namespace", ns)
	}
	out := append([]string{}, args[:end]...)
	out = append(out, extra...)
	return append(out, args[end:]...)
}

// completeWord completes the word at the end of the line from kubectl subcommands and
// common resource types.
var completeWords = append(append([]string{}, kubectlSubs[:len(kubectlSubs)-7]...), "pods", "deployments", "statefulsets", "daemonsets", "services",
	"ingresses", "configmaps", "secrets", "jobs", "cronjobs", "nodes", "namespaces", "events", "persistentvolumeclaims",
	"persistentvolumes", "replicasets", "serviceaccounts", "endpoints", "ingressroutes", "httproutes", "middlewares",
	"--all-namespaces", "--namespace", "--selector", "--output", "--watch", "--show-labels", "--tail", "--follow", "--previous", "--container")

func completeLine(line string) (string, []string) {
	i := strings.LastIndexAny(line, " ") + 1
	word := line[i:]
	if word == "" {
		return line, nil
	}
	var hits []string
	for _, w := range completeWords {
		if strings.HasPrefix(w, word) {
			hits = append(hits, w)
		}
	}
	sort.Strings(hits)
	if len(hits) == 1 {
		return line[:i] + hits[0] + " ", nil
	}
	if len(hits) > 1 { // extend to the common prefix
		p := hits[0]
		for _, h := range hits[1:] {
			for !strings.HasPrefix(h, p) {
				p = p[:len(p)-1]
			}
		}
		return line[:i] + p, hits
	}
	return line, nil
}

// ---------------------------------------------------------------- the session

var consoleCount = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

const consolePerUser = 6

type console struct {
	s        *Server
	r        *http.Request
	ws       *ws.Conn
	ctx, ns  string
	line     []rune
	cur      int
	hist     []string
	hi       int
	cols     int
	rows     int
	mu       sync.Mutex
	run      *exec.Cmd
	pty      Terminal
	esc      string // an escape sequence still arriving
	pending  string // keys typed while a command was starting
	userName string
	home     string
	aliases  map[string]string // this user's own; built-in ones in builtinAliases
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin websocket blocked", http.StatusForbidden)
		return
	}
	uid, uname := reqUserID(r), "local"
	if u := reqUser(r); u != nil {
		uname = u.Username
	}
	consoleCount.Lock()
	if consoleCount.n[uid] >= consolePerUser {
		consoleCount.Unlock()
		http.Error(w, "too many open consoles", http.StatusTooManyRequests)
		return
	}
	consoleCount.n[uid]++
	consoleCount.Unlock()
	defer func() { consoleCount.Lock(); consoleCount.n[uid]--; consoleCount.Unlock() }()

	conn, _, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	// a private HOME (kubectl's cache) per user; /tmp may be read-only in a pod, then /data
	home := filepath.Join(os.TempDir(), "kubelamp-console", safeDirName(uid))
	if err := os.MkdirAll(home, 0o700); err != nil {
		home = filepath.Join(s.dataDir, "console", safeDirName(uid))
		_ = os.MkdirAll(home, 0o700)
	}
	c := &console{s: s, r: r, ws: conn, cols: 120, rows: 30, userName: uname, home: home, aliases: loadConsoleSettings(s.userDir(r)).Aliases}
	q := r.URL.Query()
	c.setCtx(q.Get("ctx"), q.Get("ns"), false)
	if _, err := exec.LookPath("kubectl"); err != nil {
		c.out("\x1b[31mkubectl isn't installed on the KubeLamp server.\x1b[0m\r\n")
	}
	c.out("\x1b[2mkubectl console · runs kubectl with your kubeconfig · type help\x1b[0m\r\n")
	c.prompt()
	defer c.stop()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			T    string `json:"t"`
			D    string `json:"d"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
			Ctx  string `json:"ctx"`
			NS   string `json:"ns"`
		}
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch m.T {
		case "r":
			c.mu.Lock()
			if m.Cols > 0 && m.Rows > 0 {
				c.cols, c.rows = m.Cols, m.Rows
			}
			if c.pty != nil {
				c.pty.Resize(c.cols, c.rows)
			}
			c.mu.Unlock()
		case "c":
			if c.busy() {
				c.out("\r\n\x1b[33m(the cluster switches when this command ends — Ctrl+C stops it)\x1b[0m\r\n")
			}
			c.setCtx(m.Ctx, m.NS, true)
		case "i":
			c.input(m.D)
		}
	}
}

func safeDirName(s string) string {
	if s == "" {
		return "local"
	}
	return regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(s, "_")
}

func (c *console) out(s string) { _ = c.ws.WriteMessage(ws.OpBinary, []byte(s)) }

func (c *console) busy() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.run != nil }

// setCtx picks the cluster (one of this user's contexts) and namespace.
func (c *console) setCtx(ctxName, ns string, announce bool) {
	reg := c.s.R(c.r)
	if ctxName == "" || reg.Get(ctxName) == nil {
		if all := reg.All(); len(all) > 0 && (ctxName == "" || reg.Get(ctxName) == nil) {
			if ctxName != "" && announce {
				c.out("\r\n\x1b[31munknown cluster " + ctxName + " — staying on your own clusters\x1b[0m\r\n")
			}
			ctxName = all[0].Info.Name
		} else {
			ctxName = ""
		}
	}
	if ns != "" && !validDNSLabel(ns) {
		ns = ""
	}
	c.mu.Lock()
	c.ctx, c.ns = ctxName, ns
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"t": "ctx", "ctx": ctxName, "ns": ns})
	_ = c.ws.WriteMessage(ws.OpText, b)
	if announce && !c.busy() {
		c.out("\r\x1b[K")
		c.prompt()
	}
}

func (c *console) prompt() {
	c.mu.Lock()
	ctxName, ns := c.ctx, c.ns
	c.mu.Unlock()
	if ctxName == "" {
		c.out("\x1b[33mno cluster yet — add a kubeconfig in KubeLamp\x1b[0m\r\n$ ")
		return
	}
	nsTxt := ns
	if nsTxt == "" {
		nsTxt = "default ns"
	}
	c.out(fmt.Sprintf("\x1b[38;5;75m%s\x1b[0m:\x1b[33m%s\x1b[0m \x1b[1m$\x1b[0m ", shortCtx(ctxName), nsTxt))
	c.redraw()
}

func shortCtx(n string) string {
	if strings.HasPrefix(n, "arn:aws:eks:") {
		return n[strings.LastIndex(n, "/")+1:]
	}
	return n
}

// redraw shows the line being typed after the prompt and puts the cursor in place.
func (c *console) redraw() {
	b := "\x1b[K" + string(c.line)
	if back := len(c.line) - c.cur; back > 0 {
		b += fmt.Sprintf("\x1b[%dD", back)
	}
	c.out(b)
}

// retype clears what was typed (cursor anywhere) and writes the new line.
func (c *console) retype(prevCur int) {
	var b strings.Builder
	if prevCur > 0 {
		fmt.Fprintf(&b, "\x1b[%dD", prevCur)
	}
	b.WriteString("\x1b[K")
	b.WriteString(string(c.line))
	if back := len(c.line) - c.cur; back > 0 {
		fmt.Fprintf(&b, "\x1b[%dD", back)
	}
	c.out(b.String())
}

// input handles keys: they go to the running command, or edit the line.
func (c *console) input(d string) {
	c.mu.Lock()
	p := c.pty
	running := c.run != nil
	c.mu.Unlock()
	if running {
		if p != nil {
			_, _ = p.Write([]byte(d))
		} else {
			c.pending += d
		}
		return
	}
	d = c.esc + d
	c.esc = ""
	for len(d) > 0 {
		if d[0] == 0x1b {
			seq, ok := escSeq(d)
			if !ok {
				c.esc = d // wait for the rest of the sequence
				return
			}
			c.key(seq)
			d = d[len(seq):]
			continue
		}
		r, n := utf8.DecodeRuneInString(d)
		d = d[n:]
		switch r {
		case '\r', '\n':
			line := string(c.line)
			c.line, c.cur = nil, 0
			c.out("\r\n")
			c.exec(line)
			if c.busy() { // the rest of a paste goes to the command
				c.mu.Lock()
				c.pending += d
				c.mu.Unlock()
				return
			}
		case 0x7f, 0x08: // backspace
			if c.cur > 0 {
				prev := c.cur
				c.line = append(c.line[:c.cur-1], c.line[c.cur:]...)
				c.cur--
				c.retype(prev)
			}
		case 0x03: // Ctrl+C: drop the line
			c.out("^C\r\n")
			c.line, c.cur = nil, 0
			c.prompt()
		case 0x15: // Ctrl+U
			prev := c.cur
			c.line, c.cur = nil, 0
			c.retype(prev)
		case 0x01: // Ctrl+A
			if c.cur > 0 {
				c.out(fmt.Sprintf("\x1b[%dD", c.cur))
				c.cur = 0
			}
		case 0x05: // Ctrl+E
			if back := len(c.line) - c.cur; back > 0 {
				c.out(fmt.Sprintf("\x1b[%dC", back))
				c.cur = len(c.line)
			}
		case 0x0c: // Ctrl+L
			c.out("\x1b[H\x1b[2J")
			c.prompt()
		case 0x04: // Ctrl+D
			if len(c.line) == 0 {
				c.out("\x1b[2m(close the tab to end the console)\x1b[0m\r\n")
				c.prompt()
			}
		case '\t':
			prev := c.cur
			line, hits := completeLine(string(c.line[:c.cur]))
			rest := c.line[c.cur:]
			if len(hits) > 1 && line == string(c.line[:c.cur]) {
				c.out("\r\n" + strings.Join(hits, "  ") + "\r\n")
				c.prompt()
				continue
			}
			c.line = append([]rune(line), rest...)
			c.cur = utf8.RuneCountInString(line)
			c.retype(prev)
		default:
			if r >= 0x20 && len(c.line) < 4096 {
				prev := c.cur
				c.line = append(c.line[:c.cur], append([]rune{r}, c.line[c.cur:]...)...)
				c.cur++
				if c.cur == len(c.line) {
					c.out(string(r))
				} else {
					c.retype(prev)
				}
			}
		}
	}
}

// escSeq returns the whole escape sequence at the start of d.
func escSeq(d string) (string, bool) {
	if len(d) < 2 {
		return "", false
	}
	if d[1] != '[' && d[1] != 'O' {
		return d[:2], true
	}
	for i := 2; i < len(d); i++ {
		if b := d[i]; b >= 0x40 && b <= 0x7e {
			return d[:i+1], true
		}
	}
	return "", false
}

func (c *console) key(seq string) {
	prev := c.cur
	switch seq {
	case "\x1b[D", "\x1bOD":
		if c.cur > 0 {
			c.cur--
			c.out("\x1b[D")
		}
	case "\x1b[C", "\x1bOC":
		if c.cur < len(c.line) {
			c.cur++
			c.out("\x1b[C")
		}
	case "\x1b[H", "\x1bOH", "\x1b[1~":
		if c.cur > 0 {
			c.out(fmt.Sprintf("\x1b[%dD", c.cur))
			c.cur = 0
		}
	case "\x1b[F", "\x1bOF", "\x1b[4~":
		if back := len(c.line) - c.cur; back > 0 {
			c.out(fmt.Sprintf("\x1b[%dC", back))
			c.cur = len(c.line)
		}
	case "\x1b[3~": // Delete
		if c.cur < len(c.line) {
			c.line = append(c.line[:c.cur], c.line[c.cur+1:]...)
			c.retype(prev)
		}
	case "\x1b[A", "\x1bOA", "\x1b[B", "\x1bOB": // history
		if len(c.hist) == 0 {
			return
		}
		if seq == "\x1b[A" || seq == "\x1bOA" {
			if c.hi > 0 {
				c.hi--
			}
		} else if c.hi < len(c.hist) {
			c.hi++
		}
		c.line = nil
		if c.hi < len(c.hist) {
			c.line = []rune(c.hist[c.hi])
		}
		c.cur = len(c.line)
		c.retype(prev)
	}
}

const consoleHelp = "\x1b[1mkubectl console\x1b[0m — runs kubectl with your kubeconfig on the cluster and namespace chosen above.\r\n" +
	"  kubectl get pods        \x1b[2m(or k get pods, or just get pods)\x1b[0m\r\n" +
	"  get pods -A | grep api  \x1b[2mone | grep [-i] [-v] pattern may follow\x1b[0m\r\n" +
	"  logs -f deploy/web      exec -it pod/web -- sh      describe node x\r\n" +
	"  ↑ ↓ history · Tab completes · Ctrl+C stops a command · Ctrl+L clears\r\n" +
	"  alias            list aliases (kgp = kubectl get pods, kl = kubectl logs …)\r\n" +
	"  alias kgw='kubectl get pods -w'   your own, saved for next time · unalias kgw\r\n" +
	"\x1b[2mNot available: local files (-f, --from-file), cp, edit, proxy, port-forward, --kubeconfig/--context/--server.\r\n" +
	"This is not a shell: ; && $() and redirection are passed to kubectl as plain words.\x1b[0m\r\n"

// exec runs one command line.
func (c *console) exec(line string) {
	if strings.TrimSpace(line) == "" {
		c.prompt()
		return
	}
	if len(c.hist) == 0 || c.hist[len(c.hist)-1] != line {
		c.hist = append(c.hist, line)
		if len(c.hist) > 200 {
			c.hist = c.hist[1:]
		}
	}
	c.hi = len(c.hist)
	if out, ok := c.aliasCommand(line); ok {
		c.out(out)
		c.prompt()
		return
	}
	c.mu.Lock()
	expanded, _ := expandAlias(line, c.aliases)
	c.mu.Unlock()
	args, grep, builtin, err := parseConsole(expanded)
	if err == nil && builtin == "" {
		err = checkKubectlArgs(args, c.s.readOnly)
	}
	switch {
	case err != nil:
		c.out("\x1b[31m" + err.Error() + "\x1b[0m\r\n")
		c.prompt()
		return
	case builtin == "help":
		c.out(consoleHelp)
		c.prompt()
		return
	case builtin == "clear":
		c.out("\x1b[H\x1b[2J")
		c.prompt()
		return
	}
	c.mu.Lock()
	ctxName, ns := c.ctx, c.ns
	c.mu.Unlock()
	reg := c.s.R(c.r)
	cl := reg.Get(ctxName)
	if cl == nil {
		c.out("\x1b[31mno cluster selected — add a kubeconfig in KubeLamp, then pick it above\x1b[0m\r\n")
		c.prompt()
		return
	}
	inCluster := cl.Info.Source == "in-cluster"
	argv := kubectlArgv(args, ctxName, ns, inCluster)
	cmd := exec.Command("kubectl", argv...)
	cmd.Dir = c.home
	env := []string{"HOME=" + c.home, "TERM=xterm-256color", "COLORTERM=truecolor", "KUBECTL_COMMAND_HEADERS=false"}
	for _, kv := range os.Environ() { // keep PATH, proxy and cloud-login settings; drop KubeLamp's own
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "KUBELAMP") || k == "KUBECONFIG" || k == "HOME" || k == "TERM" || k == "KUBE_EDITOR" || k == "EDITOR" || k == "KUBECTL_EXTERNAL_DIFF" {
			continue
		}
		env = append(env, kv)
	}
	if !inCluster {
		env = append(env, "KUBECONFIG="+strings.Join(c.s.currentPaths(c.r), string(os.PathListSeparator)))
	} else {
		env = append(env, "KUBECONFIG=") // the pod's service account
	}
	cmd.Env = env
	log.Printf("console: %s on %s: kubectl %s", c.userName, ctxName, redactArgs(args))
	pty, err := startPTY(cmd)
	if err != nil {
		c.out("\x1b[31mcannot run kubectl: " + err.Error() + "\x1b[0m\r\n")
		c.prompt()
		return
	}
	c.mu.Lock()
	c.run, c.pty = cmd, pty
	pty.Resize(c.cols, c.rows)
	pend := c.pending
	c.pending = ""
	c.mu.Unlock()
	if pend != "" {
		_, _ = pty.Write([]byte(pend))
	}
	go c.pump(cmd, pty, grep)
}

// pump copies the command's output to the browser, then shows the prompt again.
func (c *console) pump(cmd *exec.Cmd, pty Terminal, grep *grepFilter) {
	buf := make([]byte, 32<<10)
	var part string
	nl := true // did the output end with a newline?
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			if grep == nil {
				c.out(chunk)
				nl = strings.HasSuffix(chunk, "\n")
			} else {
				part += chunk
				var keep strings.Builder
				for {
					i := strings.IndexByte(part, '\n')
					if i < 0 {
						break
					}
					if line := part[:i+1]; grep.keep(line) {
						keep.WriteString(line)
					}
					part = part[i+1:]
				}
				if keep.Len() > 0 {
					c.out(keep.String())
				}
			}
		}
		if err != nil {
			break
		}
	}
	if grep != nil && part != "" && grep.keep(part) {
		c.out(part + "\r\n")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-time.After(3 * time.Second):
		killTree(cmd)
		werr = <-done
	}
	pty.Close()
	c.mu.Lock()
	c.run, c.pty = nil, nil
	c.mu.Unlock()
	if !nl {
		c.out("\r\n") // the prompt starts on its own line
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) && ee.ExitCode() > 0 && grep == nil {
		c.out(fmt.Sprintf("\x1b[2m[exit %d]\x1b[0m\r\n", ee.ExitCode()))
	}
	c.prompt()
}

// stop ends a running command when the console closes.
func (c *console) stop() {
	c.mu.Lock()
	cmd := c.run
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		killTree(cmd)
	}
}

var redactRe = regexp.MustCompile(`(?i)^(--from-literal=[^=]*=|--token=|--password=|--docker-password=).*`)

func redactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactRe.ReplaceAllString(a, "${1}***")
	}
	s := strings.Join(out, " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
