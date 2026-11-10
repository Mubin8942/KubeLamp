package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestQuantityPct(t *testing.T) {
	if p := quantityPct("2", "4"); p != 50 {
		t.Errorf("2/4: %v", p)
	}
	if p := quantityPct("512Mi", "1Gi"); p != 50 {
		t.Errorf("512Mi/1Gi: %v", p)
	}
	if quantityPct("", "4") != -1 || quantityPct("1", "") != -1 {
		t.Error("missing values must give -1")
	}
}

func TestNamespaceDashAgainstMock(t *testing.T) {
	ts, _ := startMockServers(t, nil)
	get := func(path string) (int, NSDash) {
		var d NSDash
		var code int
		for i := 0; i < 40; i++ { // the first snapshot takes a moment
			r, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			code = r.StatusCode
			_ = json.NewDecoder(r.Body).Decode(&d)
			r.Body.Close()
			if code != 503 && code != 404 || i > 30 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		return code, d
	}
	code, d := get("/api/nsdash?ctx=prod-dc1&ns=payments")
	if code != 200 {
		t.Fatalf("nsdash: %d", code)
	}
	if d.Name != "payments" || d.Phase != "Active" || d.Summary.Pods == 0 || d.Phases["Running"] == 0 || d.Containers == 0 {
		t.Errorf("basics: %+v", d.Summary)
	}
	if len(d.Workloads) < 3 || d.Workloads[0].Health == "healthy" {
		t.Errorf("workloads (unhealthy first): %d first=%+v", len(d.Workloads), d.Workloads[0].Name)
	}
	if len(d.Problems) == 0 || d.Health.Sev != "critical" || len(d.Health.Items) == 0 {
		t.Errorf("health: %s %d problems", d.Health.Sev, len(d.Problems))
	}
	if len(d.Nodes) == 0 || len(d.TopCPU) == 0 || len(d.PVCs) == 0 {
		t.Errorf("nodes=%d top=%d pvcs=%d", len(d.Nodes), len(d.TopCPU), len(d.PVCs))
	}
	if d.Summary.CPU.Quota != 4 || d.Summary.Mem.Quota == 0 || len(d.Quotas) != 1 {
		t.Errorf("quota: %+v %+v", d.Summary.CPU, d.Quotas)
	}
	keys := map[string]*int{}
	for _, c := range d.Counts {
		keys[c.Key] = c.Count
	}
	if keys["services"] == nil || keys["pvcs"] == nil || *keys["pvcs"] != len(d.PVCs) {
		t.Errorf("counts: %+v", d.Counts)
	}
	if code, _ := get("/api/nsdash?ctx=prod-dc1&ns=does-not-exist"); code != 404 {
		t.Errorf("missing namespace: %d", code)
	}
	if r, _ := http.Get(ts.URL + "/api/nsdash?ctx=prod-dc1"); r.StatusCode != 400 {
		t.Errorf("no namespace: %d", r.StatusCode)
	}
	// namespace-scoped credentials: forbidden kinds show as "no permission", the rest still counts
	code, d = get("/api/nsdash?ctx=team-payments&ns=payments")
	if code != 200 {
		t.Fatalf("team: %d", code)
	}
	var forbidden, counted int
	for _, c := range d.Counts {
		if c.Err == "no permission" {
			forbidden++
		} else if c.Count != nil {
			counted++
		}
	}
	if forbidden == 0 || counted == 0 {
		t.Errorf("team counts: %+v", d.Counts)
	}
}
