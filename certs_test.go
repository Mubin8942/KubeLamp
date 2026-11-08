package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCertState(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		after time.Duration
		days  int
		state string
	}{
		{400 * 24 * time.Hour, 400, "ok"},
		{30*24*time.Hour + time.Hour, 30, "ok"},
		{29*24*time.Hour + time.Hour, 29, "warn"},
		{7*24*time.Hour + time.Hour, 7, "warn"},
		{6*24*time.Hour + time.Hour, 6, "crit"},
		{2 * time.Hour, 0, "crit"},
		{-time.Minute, -1, "expired"},
		{-3*24*time.Hour - time.Hour, -4, "expired"},
	} {
		d, st := certState(now.Add(c.after), now)
		if d != c.days || st != c.state {
			t.Errorf("%v: got %d %s, want %d %s", c.after, d, st, c.days, c.state)
		}
	}
	if len(parsePEMCerts([]byte("junk"))) != 0 {
		t.Error("junk PEM")
	}
}

func TestCertExpiryAgainstMock(t *testing.T) {
	ts, _ := startMockServers(t, nil)
	type sum struct {
		Name   string      `json:"name"`
		Status string      `json:"status"`
		Error  string      `json:"error"`
		Certs  *CertReport `json:"certs"`
	}
	get := func() map[string]sum {
		r, err := http.Get(ts.URL + "/api/clusters")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var list []sum
		_ = json.NewDecoder(r.Body).Decode(&list)
		m := map[string]sum{}
		for _, s := range list {
			m[s.Name] = s
		}
		return m
	}
	ready := func(m map[string]sum) bool {
		for _, n := range []string{"prod-dc1", "team-payments", "legacy-dc3"} {
			s := m[n]
			if s.Certs == nil || len(s.Certs.Certs) == 0 || s.Certs.Certs[0].Role != "server" {
				return false
			}
		}
		return true
	}
	var m map[string]sum
	for i := 0; i < 60; i++ {
		if m = get(); ready(m) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready(m) {
		t.Fatalf("certificates not read: %+v", m)
	}
	byRole := func(r *CertReport, role string) *CertInfo {
		for i := range r.Certs {
			if r.Certs[i].Role == role {
				return &r.Certs[i]
			}
		}
		return nil
	}
	prod := m["prod-dc1"].Certs
	if s := byRole(prod, "server"); s == nil || s.State != "warn" || s.DaysLeft != 25 || s.Subject != "kube-apiserver" || !strings.Contains(strings.Join(s.SANs, ","), "127.0.0.1") {
		t.Errorf("prod server cert: %+v", s)
	}
	if c := byRole(prod, "client"); c == nil || c.State != "warn" || c.DaysLeft != 12 || c.Subject != "kubernetes-admin" {
		t.Errorf("prod client cert: %+v", c)
	}
	if ca := byRole(prod, "ca"); ca == nil || ca.State != "ok" || ca.Subject != "kubernetes" {
		t.Errorf("prod CA: %+v", ca)
	}
	if prod.Worst != "warn" || prod.Soonest == nil || prod.Soonest.Role != "client" {
		t.Errorf("prod worst/soonest: %s %+v", prod.Worst, prod.Soonest)
	}
	if s := byRole(m["team-payments"].Certs, "server"); s.State != "crit" || s.DaysLeft != 5 {
		t.Errorf("team server cert: %+v", s)
	}
	// expired: the cluster is unreachable, and the probe still reads the certificate
	leg := m["legacy-dc3"]
	if s := byRole(leg.Certs, "server"); s.State != "expired" || s.DaysLeft >= 0 || leg.Certs.Worst != "expired" {
		t.Errorf("legacy server cert: %+v", s)
	}
	if leg.Status != "error" || !strings.Contains(strings.ToLower(leg.Error), "certificate") {
		t.Errorf("legacy cluster should fail with a certificate error: %s %q", leg.Status, leg.Error)
	}
	if eks := m["arn:aws:eks:ap-southeast-1:111122223333:cluster/eks-binge"].Certs; eks != nil && byRole(eks, "client") != nil {
		t.Errorf("token/exec-token auth has no client certificate: %+v", eks)
	}
	if st := m["staging-dc2"].Certs; st == nil || !st.Insecure || len(st.Certs) != 0 || st.Err == "" {
		t.Errorf("unreachable insecure cluster: %+v", st)
	}
}
