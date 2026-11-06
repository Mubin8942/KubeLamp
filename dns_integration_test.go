package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// startMockServers builds and starts the mock cluster and returns a normal and a read-only KubeLamp.
func startMockServers(t *testing.T, mut func(*Server)) (rw, ro *httptest.Server) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "mockapi")
	if out, err := exec.Command("go", "build", "-o", bin, "./tools/mockapi").CombinedOutput(); err != nil {
		t.Skipf("cannot build the mock cluster: %v\n%s", err, out)
	}
	mock := exec.Command(bin, "-dir", dir)
	if err := mock.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Process.Kill(); _, _ = mock.Process.Wait() })
	kc := filepath.Join(dir, "kubeconfig")
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(kc); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	reg := &Registry{}
	reg.Load([]string{kc}, nil)
	s := &Server{reg: reg, dataDir: dir, localOnly: true}
	if mut != nil {
		mut(s)
	}
	rw = httptest.NewServer(s.routes())
	ro = httptest.NewServer((&Server{reg: reg, dataDir: dir, localOnly: true, readOnly: true}).routes())
	t.Cleanup(rw.Close)
	t.Cleanup(ro.Close)
	return
}

func postJSON(t *testing.T, base, path string, v any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(v)
	r, err := http.Post(base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return r.StatusCode, m
}

func TestDNSCheckerAgainstMock(t *testing.T) {
	ts, ro := startMockServers(t, nil)
	const q = "?ctx=prod-dc1"
	if code, _ := postJSON(t, ro.URL, "/api/tools/dns"+q, map[string]any{"ns": "payments", "names": "a.example.com"}); code != 403 {
		t.Errorf("read-only: %d", code)
	}
	for name, body := range map[string]map[string]any{
		"no names":   {"ns": "payments", "names": ""},
		"bad name":   {"ns": "payments", "names": "bad host!"},
		"bad expect": {"ns": "payments", "names": "a.example.com", "expect": "nope"},
		"bad server": {"ns": "payments", "names": "a.example.com", "server": "dns.example.com"},
	} {
		if code, _ := postJSON(t, ts.URL, "/api/tools/dns"+q, body); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}

	code, m := postJSON(t, ts.URL, "/api/tools/dns"+q, map[string]any{"ns": "payments", "podMode": "all",
		"names": "kubernetes.default api.example.com slow.example.com x.invalid", "expect": "10.1.1.0/24, 10.96.0.1, 10.50.0.0/16", "timeout": 2})
	if code != 200 {
		t.Fatalf("dns: %d %v", code, m)
	}
	srcs := m["sources"].([]any)
	results := m["results"].([]any)
	if len(srcs) < 3 || len(results) != len(srcs)*4 {
		t.Fatalf("sources=%d results=%d", len(srcs), len(results))
	}
	byName := map[string]map[string]int{} // name → state → count
	sets := map[string]bool{}             // distinct api.example.com answers
	var sawNotool bool
	for _, x := range results {
		r := x.(map[string]any)
		n, st := r["name"].(string), r["state"].(string)
		if byName[n] == nil {
			byName[n] = map[string]int{}
		}
		byName[n][st]++
		if n == "api.example.com" && st == "ok" {
			var ips []string
			for _, ip := range r["ips"].([]any) {
				ips = append(ips, ip.(string))
			}
			sort.Strings(ips)
			sets[strings.Join(ips, ",")] = true
		}
		sawNotool = sawNotool || st == "notool"
	}
	if byName["kubernetes.default"]["ok"] == 0 || byName["x.invalid"]["notfound"] == 0 {
		t.Errorf("states: %v", byName)
	}
	if byName["slow.example.com"]["timeout"] == 0 || byName["slow.example.com"]["ok"] == 0 {
		t.Errorf("slow.example.com should time out on the third node only: %v", byName["slow.example.com"])
	}
	if len(sets) < 2 {
		t.Errorf("api.example.com should differ between nodes: %v", sets)
	}
	if !sawNotool {
		t.Error("ledger-worker pods have no DNS tool")
	}
	if un, _ := m["unexpected"].([]any); len(un) != 1 || un[0] != "10.1.9.9" {
		t.Errorf("unexpected: %v", m["unexpected"])
	}
	s0 := srcs[0].(map[string]any)
	if rv := s0["resolv"].(map[string]any); len(rv["nameservers"].([]any)) == 0 || len(rv["search"].([]any)) == 0 {
		t.Errorf("resolv: %v", rv)
	}

	// nslookup output (binge-web) and an explicit DNS server
	code, m = postJSON(t, ts.URL, "/api/tools/dns"+q, map[string]any{"ns": "ott-streaming", "podMode": "all", "names": "api.example.com"})
	tools := map[string]bool{}
	for _, s := range m["sources"].([]any) {
		tools[s.(map[string]any)["tool"].(string)] = true
	}
	if code != 200 || !tools["nslookup"] || !tools["getent"] {
		t.Errorf("tools in ott-streaming: %v", tools)
	}
	code, m = postJSON(t, ts.URL, "/api/tools/dns"+q, map[string]any{"ns": "ott-streaming", "podMode": "per-node", "names": "kubernetes.default", "server": "10.9.9.9", "timeout": 1})
	for _, x := range m["results"].([]any) {
		if st := x.(map[string]any)["state"]; st != "timeout" {
			t.Errorf("unreachable DNS server should time out, got %v", st)
		}
	}
}
