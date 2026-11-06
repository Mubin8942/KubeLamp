package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"kubelamp/internal/ws"
)

// Browser <-> KubeLamp terminal protocol:
//
//	browser → server: text JSON {"t":"i","d":"<keys>"} or {"t":"r","cols":N,"rows":N}
//	server  → browser: binary frames = terminal output; text JSON {"t":"exit","msg":"…"} at the end
type termMsg struct {
	T    string `json:"t"`
	D    string `json:"d"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // non-browser client
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (s *Server) shellAllowed() bool { return !s.readOnly && (s.localOnly || s.allowShell) }

func (s *Server) terminalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/exec", s.handleExec)
	mux.HandleFunc("GET /api/shell", s.handleShell)
}

// ---------------------------------------------------------------- pod exec

// handleExec bridges a browser terminal to `kubectl exec -it` semantics using the
// API server's WebSocket exec endpoint (channel.k8s.io protocol).
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin websocket blocked", http.StatusForbidden)
		return
	}
	if s.denyWrite(w) {
		return
	}
	q := r.URL.Query()
	c := s.reg.Get(q.Get("ctx"))
	if c == nil || c.client == nil {
		http.Error(w, "unknown context", http.StatusNotFound)
		return
	}
	c.touch()
	browser, _, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	defer browser.Close()
	fail := func(msg string) {
		_ = browser.WriteMessage(ws.OpBinary, []byte("\r\n\x1b[31m"+msg+"\x1b[0m\r\n"))
		b, _ := json.Marshal(map[string]string{"t": "exit", "msg": msg})
		_ = browser.WriteMessage(ws.OpText, b)
	}
	ns, pod, container := q.Get("ns"), q.Get("pod"), q.Get("container")
	cmd := q["cmd"]
	if len(cmd) == 0 {
		// best shell available in the image
		cmd = []string{"sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash -l || { command -v ash >/dev/null 2>&1 && exec ash -l; } || exec sh"}
	}
	eq := url.Values{"stdin": {"true"}, "stdout": {"true"}, "tty": {"true"}, "command": cmd}
	if container != "" {
		eq.Set("container", container)
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	rwc, proto, err := c.client.upgrade(dctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/exec", eq,
		[]string{"v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io"})
	dcancel()
	if err != nil {
		fail("exec failed: " + friendlyErr(err))
		return
	}
	api := ws.NewClient(rwc)
	defer api.Close()
	log.Printf("exec %s/%s [%s] via %s", ns, pod, container, proto)

	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }

	// API server → browser
	go func() {
		defer finish()
		for {
			op, msg, err := api.ReadMessage()
			if err != nil {
				fail("session closed")
				return
			}
			if op != ws.OpBinary || len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case 1, 2: // stdout, stderr
				if len(msg) > 1 {
					if browser.WriteMessage(ws.OpBinary, msg[1:]) != nil {
						return
					}
				}
			case 3: // error channel: Status JSON when the process exits
				if len(msg) > 1 {
					var st struct {
						Status  string `json:"status"`
						Message string `json:"message"`
					}
					_ = json.Unmarshal(msg[1:], &st)
					m := "process exited"
					if st.Status == "Failure" && st.Message != "" {
						m = st.Message
					}
					b, _ := json.Marshal(map[string]string{"t": "exit", "msg": m})
					_ = browser.WriteMessage(ws.OpText, b)
					return
				}
			}
		}
	}()
	// browser → API server
	go func() {
		defer finish()
		for {
			_, msg, err := browser.ReadMessage()
			if err != nil {
				return
			}
			var m termMsg
			if json.Unmarshal(msg, &m) != nil {
				continue
			}
			switch m.T {
			case "i":
				if api.WriteMessage(ws.OpBinary, append([]byte{0}, m.D...)) != nil {
					return
				}
			case "r":
				if m.Cols > 0 && m.Rows > 0 {
					b, _ := json.Marshal(map[string]int{"Width": m.Cols, "Height": m.Rows})
					_ = api.WriteMessage(ws.OpBinary, append([]byte{4}, b...))
				}
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------- local shell

// handleShell starts the user's own shell in a pseudo-terminal, with KUBECONFIG
// set so plain `kubectl` talks to the cluster currently open in KubeLamp.
func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin websocket blocked", http.StatusForbidden)
		return
	}
	if !s.shellAllowed() {
		http.Error(w, "the local shell is disabled (read-only mode or non-local listen address)", http.StatusForbidden)
		return
	}
	ctxName := r.URL.Query().Get("ctx")
	browser, _, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	defer browser.Close()
	say := func(text string) { _ = browser.WriteMessage(ws.OpBinary, []byte(text)) }

	s.reg.mu.RLock()
	paths := append([]string(nil), s.reg.paths...)
	s.reg.mu.RUnlock()
	kcEnv := strings.Join(paths, string(os.PathListSeparator))
	var tmp string
	if ctxName != "" && s.reg.Get(ctxName) != nil {
		// A tiny kubeconfig listed first selects the context for this shell only;
		// `kubectl config use-context` inside the shell also stays local to it.
		f, err := os.CreateTemp("", "kubelamp-ctx-*.yaml")
		if err == nil {
			fmt.Fprintf(f, "apiVersion: v1\nkind: Config\ncurrent-context: %q\n", ctxName)
			f.Close()
			tmp = f.Name()
			kcEnv = tmp + string(os.PathListSeparator) + kcEnv
			defer os.Remove(tmp)
		}
	}
	shell, args := userShell()
	cmd := exec.Command(shell, args...)
	home, _ := os.UserHomeDir()
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kcEnv, "TERM=xterm-256color", "COLORTERM=truecolor", "KUBELAMP=1")
	pty, err := startPTY(cmd)
	if err != nil {
		say("\r\n\x1b[31mCannot start a shell: " + err.Error() + "\x1b[0m\r\n")
		return
	}
	defer pty.Close()
	if ctxName != "" {
		say(fmt.Sprintf("\x1b[2mKubeLamp shell · kubectl context: \x1b[0m\x1b[1m%s\x1b[0m\r\n", ctxName))
	}
	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()
	defer func() {
		if cmd.Process != nil {
			killTree(cmd)
		}
	}()

	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				if browser.WriteMessage(ws.OpBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("shell read: %v", err)
				}
				b, _ := json.Marshal(map[string]string{"t": "exit", "msg": "shell exited"})
				_ = browser.WriteMessage(ws.OpText, b)
				_ = browser.Close()
				return
			}
		}
	}()
	for {
		_, msg, err := browser.ReadMessage()
		if err != nil {
			return
		}
		var m termMsg
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch m.T {
		case "i":
			if _, err := pty.Write([]byte(m.D)); err != nil {
				return
			}
		case "r":
			pty.Resize(m.Cols, m.Rows)
		}
		select {
		case <-waitDone:
			return
		default:
		}
	}
}

func userShell() (string, []string) {
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("pwsh.exe"); err == nil {
			return p, []string{"-NoLogo"}
		}
		return "powershell.exe", []string{"-NoLogo"}
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		for _, c := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
			if _, err := os.Stat(c); err == nil {
				sh = c
				break
			}
		}
	}
	return sh, []string{"-l"}
}

// Terminal is a pseudo-terminal (or pipe pair on platforms without one).
type Terminal interface {
	io.ReadWriteCloser
	Resize(cols, rows int)
}
