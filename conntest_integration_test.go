package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs a whole connectivity job against the mock cluster (tools/mockapi): pod
// selection, exec batching, classification, policy verdicts, cancel, limits, read-only.
func TestConnJobAgainstMock(t *testing.T) {
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
	defer func() { _ = mock.Process.Kill(); _, _ = mock.Process.Wait() }()
	kc := filepath.Join(dir, "kubeconfig")
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(kc); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	reg := &Registry{}
	reg.Load([]string{kc}, nil)
	srv := &Server{reg: reg, dataDir: dir, localOnly: true}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	ro := httptest.NewServer((&Server{reg: reg, dataDir: dir, localOnly: true, readOnly: true}).routes())
	defer ro.Close()

	post := func(base, path string, v any) (int, map[string]any) {
		b, _ := json.Marshal(v)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	get := func(path string) map[string]any {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	const q = "?ctx=prod-dc1"

	if code, _ := post(ro.URL, "/api/tools/conntest"+q, map[string]any{"ns": "payments", "targets": "10.0.0.1", "ports": "22"}); code != 403 {
		t.Errorf("read-only: got %d, want 403", code)
	}
	if code, _ := post(ts.URL, "/api/tools/conntest"+q, map[string]any{"ns": "payments", "targets": "bad_host!"}); code != 400 {
		t.Errorf("bad target: got %d, want 400", code)
	}
	if code, _ := post(ts.URL, "/api/tools/conntest"+q, map[string]any{"ns": "payments", "targets": "10.0.0.0/22", "ports": "1-20"}); code != 400 {
		t.Errorf("limit: got %d, want 400", code)
	}

	code, m := post(ts.URL, "/api/tools/conntest"+q, map[string]any{"ns": "payments", "podMode": "per-node", "targets": "172.18.1.33, 172.18.1.40, 172.18.2.5",
		"ports": "22-25", "policyCheck": true, "timeout": 3, "parallel": 8})
	if code != 200 {
		t.Fatalf("start: %d %v", code, m)
	}
	id := m["id"].(string)
	var job map[string]any
	for i := 0; i < 100; i++ {
		job = get("/api/tools/conntest/" + id + "?full=1")
		if job["state"] == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if job["state"] != "done" {
		t.Fatalf("job did not finish: %v", job["state"])
	}
	srcs := job["sources"].([]any)
	checks := job["checks"].([]any)
	results := job["results"].([]any)
	if len(checks) != 12 || len(results) != len(srcs)*len(checks) {
		t.Fatalf("checks=%d results=%d sources=%d", len(checks), len(results), len(srcs))
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.(map[string]any)["state"].(string)]++
	}
	for _, st := range []string{"open", "refused", "timeout", "unreachable"} {
		if counts[st] == 0 {
			t.Errorf("no %q results: %v", st, counts)
		}
	}
	// policy: payments-api pods are selected by the egress policies; 172.18.2.5 and .1.40 are not allowed
	pol, _ := job["policy"].(map[string]any)
	if len(pol) == 0 {
		t.Fatal("no policy verdicts")
	}
	var blocked, allowed int
	for _, cells := range pol {
		for _, v := range cells.(map[string]any) {
			switch v.(map[string]any)["s"] {
			case "blocked":
				blocked++
			case "allowed":
				allowed++
			}
		}
	}
	if blocked == 0 || allowed == 0 {
		t.Errorf("policy verdicts: blocked=%d allowed=%d", blocked, allowed)
	}

	// policy check off: no policy data
	_, m = post(ts.URL, "/api/tools/conntest"+q, map[string]any{"ns": "payments", "targets": "172.18.1.33", "ports": "22", "policyCheck": false})
	id2 := m["id"].(string)
	for i := 0; i < 50; i++ {
		job = get("/api/tools/conntest/" + id2 + "?full=1")
		if job["state"] == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if p, _ := job["policy"].(map[string]any); len(p) != 0 {
		t.Errorf("policy data present with the check off: %v", p)
	}

	// cancel
	_, m = post(ts.URL, "/api/tools/conntest"+q, map[string]any{"ns": "payments", "podMode": "all", "targets": "172.18.1.0/24", "ports": "22-25", "timeout": 10})
	id3 := m["id"].(string)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/tools/conntest/"+id3, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("cancel: %v", err)
	}
	for i := 0; i < 30; i++ {
		job = get("/api/tools/conntest/" + id3)
		if job["state"] == "cancelled" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if job["state"] != "cancelled" {
		t.Errorf("state after cancel: %v", job["state"])
	}
}

func TestConnJobResolvedIPs(t *testing.T) {
	ts, _ := startMockServers(t, nil)
	code, m := postJSON(t, ts.URL, "/api/tools/conntest?ctx=prod-dc1", map[string]any{"ns": "payments", "podMode": "per-node",
		"targets": "www.example.com, www.split.example, nothing.invalid, 172.18.1.33, https://api.example.com/health", "ports": "22", "policyCheck": false, "timeout": 2})
	if code != 200 {
		t.Fatalf("start: %d %v", code, m)
	}
	id := m["id"].(string)
	var job map[string]any
	for i := 0; i < 100; i++ {
		r, err := http.Get(ts.URL + "/api/tools/conntest/" + id + "?full=1")
		if err != nil {
			t.Fatal(err)
		}
		job = nil
		_ = json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job["state"] == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	checks := job["checks"].([]any)
	host := func(ci int) string { return checks[ci].(map[string]any)["host"].(string) }
	answers := map[string]map[string]bool{}
	var via string
	for _, x := range job["results"].([]any) {
		r := x.(map[string]any)
		h := host(int(r["check"].(float64)))
		if r["state"] == "notool" || r["state"] == "noshell" {
			continue
		}
		ips, isList := r["ips"].([]any)
		if answers[h] == nil {
			answers[h] = map[string]bool{}
		}
		switch {
		case !isList:
			answers[h]["<none>"] = true
		default:
			var s []string
			for _, ip := range ips {
				s = append(s, ip.(string))
			}
			answers[h][strings.Join(s, ",")] = true
		}
		if h == "api.example.com" && r["via"] != nil {
			via = r["via"].(string)
		}
	}
	if len(answers["www.example.com"]) != 1 || answers["www.example.com"][""] || answers["www.example.com"]["<none>"] {
		t.Errorf("www.example.com: %v", answers["www.example.com"])
	}
	if len(answers["www.split.example"]) != 2 {
		t.Errorf("www.split.example should differ between nodes: %v", answers["www.split.example"])
	}
	if !answers["nothing.invalid"][""] || len(answers["nothing.invalid"]) != 1 {
		t.Errorf("nothing.invalid should resolve to no address: %v", answers["nothing.invalid"])
	}
	if !answers["172.18.1.33"]["<none>"] || len(answers["172.18.1.33"]) != 1 {
		t.Errorf("IP targets carry no resolved addresses: %v", answers["172.18.1.33"])
	}
	if !strings.HasPrefix(via, "203.0.") {
		t.Errorf("curl connected address for the URL target: %q", via)
	}
}
