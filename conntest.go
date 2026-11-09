package main

// Pod connectivity tester: runs TCP/HTTP probes from inside pods (via exec) towards
// IPs, ports and domains, to show whether a firewall allows traffic from the workers
// that host those pods. Optionally cross-checks NetworkPolicy / Cilium policies.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	connMaxChecks  = 1024
	connMaxSources = 50
	connMaxProbes  = 20000
	connBatchBytes = 5500
)

var toolExecSem = make(chan struct{}, 4)

// ---------------------------------------------------------------- input parsing

type ConnCheck struct {
	ID    int    `json:"id"`
	Mode  string `json:"mode"` // tcp | http
	Host  string `json:"host"`
	Port  int    `json:"port"`
	URL   string `json:"url,omitempty"`
	Label string `json:"label"`
}

type TokenErr struct {
	Token string `json:"token"`
	Msg   string `json:"msg"`
}

var (
	sepRE      = regexp.MustCompile(`[\s,;]+`)
	hostLabel  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	ipv4Prefix = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+`)
	urlSafe    = regexp.MustCompile(`^[A-Za-z0-9._~:/?&=%+@,-]*$`)
)

func validHostname(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func parsePortSpec(spec string) ([]int, error) {
	var out []int
	for _, t := range sepRE.Split(strings.TrimSpace(spec), -1) {
		if t == "" {
			continue
		}
		lo, hi := t, t
		if i := strings.Index(t, "-"); i >= 0 {
			lo, hi = t[:i], t[i+1:]
		}
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || b < a {
			return nil, fmt.Errorf("bad port %q (use 1-65535, ranges like 22-25)", t)
		}
		if len(out)+(b-a+1) > connMaxChecks {
			return nil, fmt.Errorf("too many ports (limit %d checks per run)", connMaxChecks)
		}
		for p := a; p <= b; p++ {
			out = append(out, p)
		}
	}
	return out, nil
}

func ipFromUint(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", v>>24, (v>>16)&255, (v>>8)&255, v&255)
}

func ipToUint(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// expandIPs turns "1.2.3.4", "1.2.3.4-40", "1.2.3.4-1.2.4.9" or "1.2.3.0/28" into addresses.
func expandIPs(expr string) ([]string, error) {
	if strings.Contains(expr, "/") {
		pfx, err := netip.ParsePrefix(expr)
		if err != nil || !pfx.Addr().Is4() {
			return nil, fmt.Errorf("bad CIDR %q", expr)
		}
		if pfx.Bits() < 22 {
			return nil, errors.New("CIDR too large (smallest allowed prefix is /22)")
		}
		pfx = pfx.Masked()
		start := ipToUint(pfx.Addr())
		n := uint32(1) << (32 - pfx.Bits())
		var out []string
		for i := uint32(0); i < n; i++ {
			if pfx.Bits() < 31 && (i == 0 || i == n-1) {
				continue // network and broadcast
			}
			out = append(out, ipFromUint(start+i))
		}
		return out, nil
	}
	lo, hi := expr, ""
	if i := strings.Index(expr, "-"); i >= 0 {
		lo, hi = expr[:i], expr[i+1:]
	}
	a, err := netip.ParseAddr(lo)
	if err != nil || !a.Is4() {
		return nil, fmt.Errorf("bad IP %q", lo)
	}
	if hi == "" {
		return []string{a.String()}, nil
	}
	var end uint32
	if strings.Contains(hi, ".") {
		b, err := netip.ParseAddr(hi)
		if err != nil || !b.Is4() {
			return nil, fmt.Errorf("bad range end %q", hi)
		}
		end = ipToUint(b)
	} else {
		n, err := strconv.Atoi(hi)
		if err != nil || n < 0 || n > 255 {
			return nil, fmt.Errorf("bad range end %q (last octet 0-255)", hi)
		}
		end = (ipToUint(a) &^ 255) | uint32(n)
	}
	start := ipToUint(a)
	if end < start {
		return nil, errors.New("range end is before its start")
	}
	if end-start+1 > connMaxChecks {
		return nil, fmt.Errorf("range too large (limit %d addresses)", connMaxChecks)
	}
	var out []string
	for v := start; v <= end; v++ {
		out = append(out, ipFromUint(v))
	}
	return out, nil
}

// parseTargets expands the Targets and Ports inputs into a de-duplicated list of checks.
func parseTargets(targets, ports string) ([]ConnCheck, []TokenErr) {
	var errs []TokenErr
	var checks []ConnCheck
	seen := map[string]bool{}
	add := func(c ConnCheck) bool {
		k := c.Mode + "|" + c.Host + "|" + strconv.Itoa(c.Port) + "|" + c.URL
		if seen[k] {
			return true
		}
		if len(checks) >= connMaxChecks {
			return false
		}
		seen[k] = true
		c.ID = len(checks)
		checks = append(checks, c)
		return true
	}
	global, perr := parsePortSpec(ports)
	if perr != nil {
		errs = append(errs, TokenErr{"ports", perr.Error()})
	}
	over := false
	for _, tok := range sepRE.Split(strings.TrimSpace(targets), -1) {
		if tok == "" {
			continue
		}
		bad := func(m string) { errs = append(errs, TokenErr{tok, m}) }
		if strings.Contains(tok, "://") {
			u, err := url.Parse(tok)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
				bad("use http://host[:port][/path] or https://host[:port][/path]")
				continue
			}
			h := u.Hostname()
			if _, e := netip.ParseAddr(h); e != nil && !validHostname(h) {
				bad("bad host in URL")
				continue
			}
			if strings.Contains(h, ":") {
				bad("IPv6 is not supported yet")
				continue
			}
			port := 80
			if u.Scheme == "https" {
				port = 443
			}
			if ps := u.Port(); ps != "" {
				n, e := strconv.Atoi(ps)
				if e != nil || n < 1 || n > 65535 {
					bad("bad port in URL")
					continue
				}
				port = n
			}
			full := u.Scheme + "://" + h + ":" + strconv.Itoa(port) + u.EscapedPath()
			if u.RawQuery != "" {
				full += "?" + u.RawQuery
			}
			if full == "" || !urlSafe.MatchString(full) || len(full) > 400 {
				bad("URL has characters that are not allowed")
				continue
			}
			if !add(ConnCheck{Mode: "http", Host: h, Port: port, URL: full, Label: tok}) {
				over = true
			}
			continue
		}
		hostPart, portPart := tok, ""
		switch strings.Count(tok, ":") {
		case 0:
		case 1:
			i := strings.Index(tok, ":")
			hostPart, portPart = tok[:i], tok[i+1:]
		default:
			bad("IPv6 is not supported yet")
			continue
		}
		var plist []int
		hasPort := false
		if portPart != "" {
			var e error
			if plist, e = parsePortSpec(portPart); e != nil {
				bad(e.Error())
				continue
			}
			hasPort = true
		} else if len(global) > 0 {
			plist, hasPort = global, true
		}
		if ipv4Prefix.MatchString(hostPart) {
			ips, e := expandIPs(hostPart)
			if e != nil {
				bad(e.Error())
				continue
			}
			if !hasPort {
				if perr == nil {
					bad("add a port (token:port, or fill the Ports field)")
				}
				continue
			}
			for _, ip := range ips {
				for _, p := range plist {
					if !add(ConnCheck{Mode: "tcp", Host: ip, Port: p, Label: ip + ":" + strconv.Itoa(p)}) {
						over = true
					}
				}
			}
			continue
		}
		if !validHostname(hostPart) {
			bad("not a valid IP, range, CIDR or domain name")
			continue
		}
		h := strings.TrimSuffix(strings.ToLower(hostPart), ".")
		if !hasPort {
			if !add(ConnCheck{Mode: "http", Host: h, Port: 443, URL: "https://" + h + ":443/", Label: h + " (https)"}) {
				over = true
			}
			continue
		}
		for _, p := range plist {
			if !add(ConnCheck{Mode: "tcp", Host: h, Port: p, Label: h + ":" + strconv.Itoa(p)}) {
				over = true
			}
		}
	}
	if over {
		errs = append(errs, TokenErr{"targets", fmt.Sprintf("too many checks (limit %d per run); narrow the ranges", connMaxChecks)})
	}
	return checks, errs
}

// ---------------------------------------------------------------- probe script

// probeScript runs inside the pod. It only reports raw facts (tool, exit code, elapsed
// ms, one-line message) as "@@r|id|tool|rc|ms|msg"; Go classifies them. Targets arrive
// as positional arguments ("id|mode|host|port|url"), never spliced into the script.
const probeScript = `set -f
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy
T=$1; P=$2; shift 2
have(){ command -v "$1" >/dev/null 2>&1; }
TO=
if have timeout; then
  if timeout 1 true 2>/dev/null; then TO="timeout $((T+3))"; elif timeout -t 1 true 2>/dev/null; then TO="timeout -t $((T+3))"; fi
fi
NCO=
if have nc && nc -h 2>&1 | grep -q -- -z; then NCO="-zv"; fi
now(){ s=$(date +%s%N 2>/dev/null); case $s in *N|'') echo $(( $(date +%s)*1000 ));; *) echo $(( s/1000000 ));; esac; }
probe(){
  t0=$(now); tool=none; rc=0; out=
  if [ "$2" = http ] && have curl; then tool=curl; out=$($TO curl -sS -o /dev/null -w '%{http_code} %{time_connect} %{remote_ip}' --connect-timeout "$T" -m $((T*2)) "$5" 2>&1); rc=$?
  elif [ "$2" = http ] && have wget; then tool=wget; out=$($TO wget -S -q -T "$T" -t 1 -O /dev/null "$5" 2>&1); rc=$?
  elif have nc; then tool=nc; out=$($TO nc $NCO -w "$T" "$3" "$4" </dev/null 2>&1); rc=$?
  elif have bash; then tool=bash; out=$($TO bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$3" "$4" 2>&1); rc=$?
  elif have curl; then tool=curl; out=$($TO curl -sS -o /dev/null -w '%{http_code} %{time_connect} %{remote_ip}' --connect-timeout "$T" -m $((T*2)) "http://$3:$4/" </dev/null 2>&1); rc=$?
  elif have wget; then tool=wget; out=$($TO wget -q -T "$T" -t 1 -O /dev/null "http://$3:$4/" 2>&1); rc=$?
  fi
  out=$(printf '%s' "$out" | tr '\n|\r' '   ' | cut -c1-240)
  printf '@@r|%s|%s|%s|%s|%s\n' "$1" "$tool" "$rc" $(( $(now)-t0 )) "$out"
}
one(){ IFS='|'; set -- $1; IFS=' '; probe "$1" "$2" "$3" "$4" "$5"; }
hostof(){ IFS='|'; set -- $1; IFS=' '; printf '%s' "$3"; }
isip(){ case $1 in *:*) return 0;; *[!0-9.]*) return 1;; *) return 0;; esac; }
# resolve each host name once, inside the pod (so the answer is what the application gets)
res(){
  rt=none; ro=
  if have getent; then rt=getent; ro=$($TO getent ahosts "$1" 2>&1)
  elif have nslookup; then rt=nslookup; ro=$($TO nslookup "$1" 2>&1)
  elif have dig; then rt=dig; ro=$($TO dig +short +time="$T" +tries=1 "$1" A 2>&1)
  elif have host; then rt=host; ro=$($TO host -W "$T" "$1" 2>&1)
  fi
  [ "$rt" = none ] && return 0
  printf '@@ip|%s|%s|%s\n' "$1" "$rt" "$(printf '%s' "$ro" | head -n 40 | tr '\n|' '\t ')"
}
echo "@@tools|$(for t in nc curl wget bash; do have $t && printf '%s ' $t; done)"
seen=' '; n=0
for c in "$@"; do
  h=$(hostof "$c")
  if [ -z "$h" ] || isip "$h"; then continue; fi
  case "$seen" in *" $h "*) continue;; esac
  seen="$seen$h "
  res "$h" &
  n=$((n+1))
  if [ "$n" -ge "$P" ]; then wait; n=0; fi
done
wait
n=0
for c in "$@"; do
  one "$c" &
  n=$((n+1))
  if [ "$n" -ge "$P" ]; then wait; n=0; fi
done
wait
echo '@@end'
`

type probeLine struct {
	ID   int
	Tool string
	RC   int
	MS   int
	Msg  string
}

// parseProbeOutput reads the script's output. It tolerates garbage and a missing @@end.
func parseProbeOutput(out string) (lines []probeLine, tools string, ended bool) {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r ")
		switch {
		case strings.HasPrefix(ln, "@@tools|"):
			tools = strings.TrimSpace(strings.TrimPrefix(ln, "@@tools|"))
		case ln == "@@end":
			ended = true
		case strings.HasPrefix(ln, "@@r|"):
			f := strings.SplitN(ln, "|", 6)
			if len(f) < 5 {
				continue
			}
			id, e1 := strconv.Atoi(f[1])
			rc, e2 := strconv.Atoi(f[3])
			ms, e3 := strconv.Atoi(f[4])
			if e1 != nil || e2 != nil || e3 != nil {
				continue
			}
			msg := ""
			if len(f) == 6 {
				msg = strings.TrimSpace(f[5])
			}
			lines = append(lines, probeLine{id, f[2], rc, ms, msg})
		}
	}
	return
}

// parseProbeIPs reads the "@@ip|host|tool|output" lines: the addresses each host name resolved
// to inside the pod.
func parseProbeIPs(out string) map[string][]string {
	m := map[string][]string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if !strings.HasPrefix(ln, "@@ip|") {
			continue
		}
		f := strings.SplitN(ln, "|", 4)
		if len(f) < 4 || f[1] == "" {
			continue
		}
		var lines []string
		for _, l := range strings.Split(f[3], "\t") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		ips, _ := parseDNSAnswer(f[2], lines)
		sort.Strings(ips)
		if ips == nil {
			ips = []string{}
		}
		m[f[1]] = ips
	}
	return m
}

// ---------------------------------------------------------------- classification

var (
	httpCodeRE  = regexp.MustCompile(`(\d{3}) (\d+\.\d+)(?: ([0-9A-Fa-f:.]*))?\s*$`)
	wgetCodeRE  = regexp.MustCompile(`HTTP/\d(?:\.\d)? (\d{3})`)
	dnsTokens   = []string{"bad address", "could not resolve", "name or service not known", "unknown host", "does not resolve", "nodename nor servname", "temporary failure in name resolution", "no address associated", "getaddrinfo", "name does not resolve", "host not found"}
	unreachToks = []string{"no route to host", "network is unreachable", "host is unreachable", "host is down", "unreachable"}
)

func hasAny(s string, toks []string) bool {
	for _, t := range toks {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// classifyProbe turns raw probe facts into one of the result states.
func classifyProbe(mode, tool string, rc, ms, timeoutMs int, msg string) (state string, httpCode int, detail string) {
	low := strings.ToLower(msg)
	detail = msg
	if tool == "none" {
		return "notool", 0, "no nc, curl, wget or bash in this container"
	}
	if rc == 124 || rc == 143 || rc == 137 {
		return "timeout", 0, fmt.Sprintf("no answer within %.0fs", float64(timeoutMs)/1000)
	}
	if m := httpCodeRE.FindStringSubmatch(msg); m != nil && tool == "curl" {
		httpCode, _ = strconv.Atoi(m[1])
	}
	if m := wgetCodeRE.FindAllStringSubmatch(msg, -1); len(m) > 0 && tool == "wget" {
		httpCode, _ = strconv.Atoi(m[len(m)-1][1])
	}
	note := func() string {
		if mode == "http" && (tool == "nc" || tool == "bash") {
			return "TCP connect only (no curl or wget here)"
		}
		return ""
	}
	if rc == 0 {
		d := note()
		if httpCode > 0 {
			d = fmt.Sprintf("HTTP %d", httpCode)
		}
		return "open", httpCode, d
	}
	if tool == "wget" && (strings.Contains(low, "ssl_client") || strings.Contains(low, "not an http or ftp url") || strings.Contains(low, "unsupported scheme") || strings.Contains(low, "https support not compiled")) {
		return "error", 0, "wget in this container has no HTTPS support; test the port with host:443 instead"
	}
	switch {
	case strings.Contains(low, "refused"):
		return "refused", 0, "the host answered with a reset: the path is open but nothing listens on this port"
	case hasAny(low, dnsTokens), tool == "curl" && rc == 6:
		return "dns", 0, "the name did not resolve from inside the pod"
	case hasAny(low, unreachToks):
		return "unreachable", 0, msg
	}
	if tool == "curl" {
		switch rc {
		case 28:
			if m := httpCodeRE.FindStringSubmatch(msg); m != nil && m[2] != "0.000000" && m[2] != "0.000" {
				return "open", 0, "connected, but no reply within the time limit"
			}
			return "timeout", 0, "no connection within the time limit"
		case 35, 51, 58, 60, 77, 83, 90, 91:
			return "tls", httpCode, "connected, but the TLS handshake or certificate check failed: " + msg
		case 1, 8, 18, 52, 55, 56:
			return "open", 0, "connected, but the server did not give a normal HTTP reply"
		}
	}
	if tool == "wget" && (httpCode > 0 || strings.Contains(low, "server returned error") || strings.Contains(low, "error getting response") || strings.Contains(low, "short read")) {
		return "open", httpCode, "connected; server replied with an error status"
	}
	if strings.Contains(low, "timed out") || strings.Contains(low, "timeout") {
		return "timeout", 0, "no answer within the time limit"
	}
	if (tool == "nc" || tool == "bash") && ms >= timeoutMs-300 {
		return "timeout", 0, "no answer within the time limit"
	}
	if msg == "" {
		return "error", 0, fmt.Sprintf("%s exited with code %d and no message (failed quickly: refused or unreachable)", tool, rc)
	}
	return "error", 0, msg
}

// ---------------------------------------------------------------- jobs

type ConnSource struct {
	Pod         string   `json:"pod"`
	Container   string   `json:"container,omitempty"`
	Node        string   `json:"node"`
	PodIP       string   `json:"podIP"`
	HostIP      string   `json:"hostIP"`
	HostNetwork bool     `json:"hostNetwork,omitempty"`
	State       string   `json:"state"` // pending | running | done | noshell | notool | error | cancelled
	Tool        string   `json:"tool,omitempty"`
	Err         string   `json:"err,omitempty"`
	Policies    []string `json:"policies,omitempty"`
}

type ConnResult struct {
	Src    int    `json:"src"`
	Check  int    `json:"check"`
	State  string `json:"state"`
	Ms     int    `json:"ms"`
	Tool   string `json:"tool,omitempty"`
	HTTP   int    `json:"http,omitempty"`
	Detail string `json:"detail,omitempty"`
	// for host names: the addresses the name resolved to inside this pod (empty: did not
	// resolve; absent: an IP target or no resolver tool), and the address curl connected to
	IPs []string `json:"ips"` // null: IP target or not resolved; []: no address
	Via string   `json:"via,omitempty"`
}

type ConnJob struct {
	ID       string
	Ctx      string
	NS       string
	Started  time.Time
	Finished time.Time
	State    string // preparing | running | done | cancelled
	Total    int
	Done     int
	Sources  []ConnSource
	Checks   []ConnCheck
	Results  []ConnResult
	Warnings []string
	Policy   map[int]map[int]PolicyVerdict // source → check → verdict (policy check only)
	PolicyOn bool
	Timeout  int
	Parallel int

	mu     sync.Mutex
	cancel context.CancelFunc
	pods   []*Pod
}

type connManager struct {
	mu    sync.Mutex
	jobs  map[string]*ConnJob
	order []string
}

var connMgr = &connManager{jobs: map[string]*ConnJob{}}

func (m *connManager) add(j *ConnJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.ID] = j
	m.order = append(m.order, j.ID)
	for len(m.order) > 20 {
		old := m.order[0]
		if oj := m.jobs[old]; oj != nil {
			oj.mu.Lock()
			running := oj.State == "preparing" || oj.State == "running"
			oj.mu.Unlock()
			if running {
				break
			}
		}
		delete(m.jobs, old)
		m.order = m.order[1:]
	}
}

func (m *connManager) get(id string) *ConnJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

type connRequest struct {
	NS          string   `json:"ns"`
	PodMode     string   `json:"podMode"` // per-node | all | pick
	Pods        []string `json:"pods"`
	Targets     string   `json:"targets"`
	Ports       string   `json:"ports"`
	Timeout     int      `json:"timeout"`
	Parallel    int      `json:"parallel"`
	PolicyCheck bool     `json:"policyCheck"`
}

func runningContainers(p *Pod) []string {
	up := map[string]bool{}
	for _, cs := range p.Status.ContainerStatuses {
		up[cs.Name] = cs.State.Running != nil
	}
	var out []string
	for _, c := range p.Spec.Containers {
		if up[c.Name] {
			out = append(out, c.Name)
		}
	}
	return out
}

func podReadyCount(p *Pod) int {
	n := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			n++
		}
	}
	return n
}

// pickSources chooses the pods the probes run from.
func pickSources(pods []Pod, req connRequest) ([]*Pod, error) {
	var run []*Pod
	for i := range pods {
		p := &pods[i]
		if p.Metadata.Namespace != req.NS || p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != nil || len(runningContainers(p)) == 0 {
			continue
		}
		run = append(run, p)
	}
	var out []*Pod
	switch req.PodMode {
	case "all":
		out = run
	case "pick":
		want := map[string]bool{}
		for _, n := range req.Pods {
			want[n] = true
		}
		for _, p := range run {
			if want[p.Metadata.Name] {
				out = append(out, p)
				delete(want, p.Metadata.Name)
			}
		}
		if len(want) > 0 {
			var miss []string
			for n := range want {
				miss = append(miss, n)
			}
			sort.Strings(miss)
			return nil, fmt.Errorf("not running (or not found): %s", strings.Join(miss, ", "))
		}
	default: // per-node
		byNode := map[string]*Pod{}
		better := func(a, b *Pod) bool { // is a a better representative than b?
			if a.Spec.HostNetwork != b.Spec.HostNetwork {
				return !a.Spec.HostNetwork
			}
			if podReadyCount(a) != podReadyCount(b) {
				return podReadyCount(a) > podReadyCount(b)
			}
			return a.Metadata.CreationTimestamp.Before(b.Metadata.CreationTimestamp)
		}
		for _, p := range run {
			n := p.Spec.NodeName
			if cur, ok := byNode[n]; !ok || better(p, cur) {
				byNode[n] = p
			}
		}
		for _, p := range byNode {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spec.NodeName != out[j].Spec.NodeName {
			return out[i].Spec.NodeName < out[j].Spec.NodeName
		}
		return out[i].Metadata.Name < out[j].Metadata.Name
	})
	if len(out) == 0 {
		return nil, errors.New("no running pods in this namespace to test from")
	}
	return out, nil
}

func sourceView(p *Pod) ConnSource {
	return ConnSource{Pod: p.Metadata.Name, Node: p.Spec.NodeName, PodIP: p.Status.PodIP, HostIP: p.Status.HostIP, HostNetwork: p.Spec.HostNetwork, State: "pending"}
}

func newJobID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (j *ConnJob) setSource(si int, f func(s *ConnSource)) {
	j.mu.Lock()
	f(&j.Sources[si])
	j.mu.Unlock()
}

func (j *ConnJob) addResults(si int, rs []ConnResult) {
	j.mu.Lock()
	j.Results = append(j.Results, rs...)
	j.Done += len(rs)
	j.mu.Unlock()
}

var noShellRE = regexp.MustCompile(`(?i)executable file not found|not found in \$path|no such file or directory|oci runtime exec failed|cannot exec|is not running|container not found|unable to start container process`)

// batches splits checks so each exec's command line stays small.
func connBatches(cs []ConnCheck) [][]ConnCheck {
	var out [][]ConnCheck
	var cur []ConnCheck
	size := 0
	for _, c := range cs {
		n := len(connArg(c)) + 8
		if len(cur) > 0 && size+n > connBatchBytes {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, c)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func connArg(c ConnCheck) string {
	return fmt.Sprintf("%d|%s|%s|%d|%s", c.ID, c.Mode, c.Host, c.Port, c.URL)
}

func (j *ConnJob) runSource(ctx context.Context, c *Cluster, si int) {
	p := j.pods[si]
	j.setSource(si, func(s *ConnSource) { s.State = "running" })
	finishAll := func(state, detail string) {
		rs := make([]ConnResult, 0, len(j.Checks))
		for _, ck := range j.Checks {
			rs = append(rs, ConnResult{Src: si, Check: ck.ID, State: state, Detail: detail})
		}
		j.addResults(si, rs)
	}
	timeoutMs := j.Timeout * 1000
	batches := connBatches(j.Checks)
	lastErr := ""
	noTool := false
	for _, ctr := range runningContainers(p) {
		if ctx.Err() != nil {
			break
		}
		var res []ConnResult
		next := false // this container can't be used: try the next one
		for bi, b := range batches {
			args := []string{"sh", "-c", probeScript, "sh", strconv.Itoa(j.Timeout), strconv.Itoa(j.Parallel)}
			for _, ck := range b {
				args = append(args, connArg(ck))
			}
			names := map[string]bool{}
			for _, ck := range b {
				if net.ParseIP(ck.Host) == nil {
					names[ck.Host] = true
				}
			}
			per := (len(b)+j.Parallel-1)/j.Parallel*(j.Timeout*2+3) + (len(names)+j.Parallel-1)/j.Parallel*(j.Timeout+4) + 10
			if per > 300 {
				per = 300
			}
			out, err := c.execWith(ctx, toolExecSem, p.Metadata.Namespace, p.Metadata.Name, ctr, args, time.Duration(per)*time.Second)
			if err != nil && out == "" {
				if ctx.Err() != nil {
					break
				}
				lastErr = err.Error()
				if len(res) == 0 {
					next = true
					break
				}
				for _, rb := range batches[bi:] {
					for _, ck := range rb {
						res = append(res, ConnResult{Src: si, Check: ck.ID, State: "error", Detail: "exec failed: " + lastErr})
					}
				}
				break
			}
			lines, _, _ := parseProbeOutput(out)
			resolved := parseProbeIPs(out)
			if bi == 0 && len(lines) > 0 && lines[0].Tool == "none" {
				noTool = true
				next = true
				break
			}
			got := map[int]bool{}
			for _, l := range lines {
				st, code, d := classifyProbe(checkMode(j.Checks, l.ID), l.Tool, l.RC, l.MS, timeoutMs, l.Msg)
				cr := ConnResult{Src: si, Check: l.ID, State: st, Ms: l.MS, Tool: l.Tool, HTTP: code, Detail: d}
				if l.ID >= 0 && l.ID < len(j.Checks) {
					if ips, ok := resolved[j.Checks[l.ID].Host]; ok {
						cr.IPs = ips
						if ips == nil {
							cr.IPs = []string{}
						}
					}
				}
				if m := httpCodeRE.FindStringSubmatch(l.Msg); m != nil && l.Tool == "curl" && net.ParseIP(m[3]) != nil {
					cr.Via = m[3]
				}
				res = append(res, cr)
				got[l.ID] = true
			}
			for _, ck := range b {
				if !got[ck.ID] {
					res = append(res, ConnResult{Src: si, Check: ck.ID, State: "error", Detail: "probe did not finish (exec timed out or was cut off, or this container has no usable sh)"})
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		if next {
			if lastErr != "" && !noShellRE.MatchString(lastErr) {
				break // not a "no shell here" problem (RBAC, timeout...): other containers won't differ
			}
			continue
		}
		j.addResults(si, res)
		j.setSource(si, func(s *ConnSource) {
			s.State, s.Container = "done", ctr
			if len(res) > 0 {
				s.Tool = res[0].Tool
			}
		})
		return
	}
	switch {
	case ctx.Err() != nil:
		j.setSource(si, func(s *ConnSource) { s.State = "cancelled" })
	case lastErr != "" && !noShellRE.MatchString(lastErr):
		j.setSource(si, func(s *ConnSource) { s.State = "error"; s.Err = lastErr })
		finishAll("error", lastErr)
	case noTool:
		j.setSource(si, func(s *ConnSource) { s.State = "notool"; s.Err = "no nc, curl, wget or bash in any container" })
		finishAll("notool", "no nc, curl, wget or bash in any container of this pod")
	default:
		j.setSource(si, func(s *ConnSource) { s.State = "noshell"; s.Err = "no shell in any container (distroless?)" })
		finishAll("noshell", "no sh in any container of this pod (distroless image?)")
	}
}

func checkMode(cs []ConnCheck, id int) string {
	if id >= 0 && id < len(cs) {
		return cs[id].Mode
	}
	return "tcp"
}

func (j *ConnJob) run(ctx context.Context, c *Cluster, snap *Snapshot) {
	if j.PolicyOn {
		env := c.loadPolicyEnv(ctx, snap, j.NS)
		j.mu.Lock()
		j.Warnings = append(j.Warnings, env.Warnings...)
		j.Policy = map[int]map[int]PolicyVerdict{}
		for si, p := range j.pods {
			pp := env.selectFor(p)
			j.Sources[si].Policies = pp.names
			cells := map[int]PolicyVerdict{}
			for _, ck := range j.Checks {
				v := env.evaluate(pp, policyTarget(ck.Host, ck.Port, ck.Label))
				if v.State != "none" || v.Why != "" {
					cells[ck.ID] = v
				}
			}
			if len(cells) > 0 {
				j.Policy[si] = cells
			}
		}
		if !env.Found && !env.NPDenied {
			j.Warnings = append(j.Warnings, "No NetworkPolicy or Cilium policy objects were found for this namespace, so policies are not what blocks traffic here.")
		}
		hn := 0
		for _, p := range j.pods {
			if p.Spec.HostNetwork {
				hn++
			}
		}
		if hn > 0 {
			j.Warnings = append(j.Warnings, fmt.Sprintf("%d source pod(s) use the host network: their traffic leaves with the node IP and pod network policies do not apply to them.", hn))
		}
		j.mu.Unlock()
	}
	j.mu.Lock()
	j.State = "running"
	j.mu.Unlock()
	var wg sync.WaitGroup
	for si := range j.pods {
		wg.Add(1)
		go func(si int) {
			defer wg.Done()
			j.runSource(ctx, c, si)
		}(si)
	}
	wg.Wait()
	j.mu.Lock()
	if ctx.Err() != nil {
		j.State = "cancelled"
	} else {
		j.State = "done"
	}
	j.Finished = time.Now()
	j.mu.Unlock()
}

// snapshot copies the job for JSON output. Results are returned from offset `from`;
// checks, policy cells and warnings only when full is set.
func (j *ConnJob) snapshot(from int, full bool) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	if from < 0 || from > len(j.Results) {
		from = 0
	}
	out := map[string]any{
		"id": j.ID, "ns": j.NS, "state": j.State, "total": j.Total, "done": j.Done,
		"sources": append(make([]ConnSource, 0, len(j.Sources)), j.Sources...), "results": append(make([]ConnResult, 0, len(j.Results)-from), j.Results[from:]...),
		"next": len(j.Results), "started": j.Started, "policyCheck": j.PolicyOn, "timeout": j.Timeout,
	}
	if !j.Finished.IsZero() {
		out["finished"] = j.Finished
	}
	if full {
		out["checks"] = j.Checks
		out["policy"] = j.Policy
	}
	out["warnings"] = append(make([]string, 0, len(j.Warnings)), j.Warnings...)
	return out
}

// ---------------------------------------------------------------- routes

func (s *Server) connRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tools/conntest/preview", func(w http.ResponseWriter, r *http.Request) {
		var req connRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		checks, errs := parseTargets(req.Targets, req.Ports)
		if errs == nil {
			errs = []TokenErr{}
		}
		sample := make([]string, 0, 12)
		for i, c := range checks {
			if i >= 12 {
				break
			}
			sample = append(sample, c.Label)
		}
		resp := map[string]any{"count": len(checks), "sample": sample, "errors": errs, "limit": connMaxChecks}
		if req.NS != "" {
			if c, _, ok := s.cluster(w, r); ok {
				if pods, err := c.FreshPods(r.Context(), req.NS); err == nil {
					if src, err := pickSources(pods, req); err == nil {
						views := make([]ConnSource, len(src))
						for i, p := range src {
							views[i] = sourceView(p)
						}
						resp["sources"] = views
					} else {
						resp["sourceErr"] = err.Error()
					}
				}
			} else {
				return
			}
		}
		writeJSON(w, 200, resp)
	})

	mux.HandleFunc("POST /api/tools/conntest", func(w http.ResponseWriter, r *http.Request) {
		if s.readOnly {
			writeErr(w, 403, errors.New("KubeLamp is running in read-only mode (-read-only): the connectivity tester runs commands in pods, so it is disabled"))
			return
		}
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		var req connRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.NS == "" {
			writeErr(w, 400, errors.New("namespace required"))
			return
		}
		if req.Timeout < 1 || req.Timeout > 10 {
			req.Timeout = 3
		}
		if req.Parallel < 1 || req.Parallel > 16 {
			req.Parallel = 8
		}
		checks, errs := parseTargets(req.Targets, req.Ports)
		if len(errs) > 0 {
			writeErr(w, 400, fmt.Errorf("%s: %s", errs[0].Token, errs[0].Msg))
			return
		}
		if len(checks) == 0 {
			writeErr(w, 400, errors.New("enter at least one target"))
			return
		}
		pods, err := c.FreshPods(r.Context(), req.NS)
		if err != nil {
			writeErr(w, 502, errors.New(friendlyErr(err)))
			return
		}
		src, err := pickSources(pods, req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if len(src) > connMaxSources {
			writeErr(w, 400, fmt.Errorf("%d pods selected (limit %d): use \"One pod per node\" or pick pods", len(src), connMaxSources))
			return
		}
		if len(src)*len(checks) > connMaxProbes {
			writeErr(w, 400, fmt.Errorf("%d pods × %d checks is over the limit of %d probes: narrow the targets or pods", len(src), len(checks), connMaxProbes))
			return
		}
		j := &ConnJob{ID: newJobID(), Ctx: r.URL.Query().Get("ctx"), NS: req.NS, Started: time.Now(), State: "preparing",
			Checks: checks, Total: len(src) * len(checks), PolicyOn: req.PolicyCheck, Timeout: req.Timeout, Parallel: req.Parallel, pods: src}
		for _, p := range src {
			j.Sources = append(j.Sources, sourceView(p))
		}
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel = cancel
		connMgr.add(j)
		go j.run(ctx, c, snap)
		writeJSON(w, 200, map[string]any{"id": j.ID})
	})

	mux.HandleFunc("GET /api/tools/conntest/{id}", func(w http.ResponseWriter, r *http.Request) {
		j := connMgr.get(r.PathValue("id"))
		if j == nil {
			writeErr(w, 404, errors.New("test not found (it may have expired)"))
			return
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		writeJSON(w, 200, j.snapshot(from, r.URL.Query().Get("full") == "1"))
	})

	mux.HandleFunc("DELETE /api/tools/conntest/{id}", func(w http.ResponseWriter, r *http.Request) {
		j := connMgr.get(r.PathValue("id"))
		if j == nil {
			writeErr(w, 404, errors.New("test not found"))
			return
		}
		j.cancel()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
}
