package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseQuantity(t *testing.T) {
	cases := map[string]float64{
		"250m": 0.25, "1": 1, "1.5": 1.5, "123456789n": 0.123456789, "500u": 0.0005,
		"128Mi": 128 << 20, "1Gi": 1 << 30, "32876544Ki": 32876544 * 1024, "1e3": 1000,
		"500M": 500e6, "2k": 2000, "": 0, "garbage": 0,
	}
	for in, want := range cases {
		if got := parseQuantity(in); math.Abs(got-want) > 1e-9*math.Max(1, want) {
			t.Errorf("parseQuantity(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSelector(t *testing.T) {
	var s LabelSelector
	s.MatchLabels = map[string]string{"app": "web", "tier": "api"}
	if got := s.String(); got != "app=web,tier=api" {
		t.Fatalf("String() = %q", got)
	}
	if !s.Matches(map[string]string{"app": "web", "tier": "api", "x": "y"}) || s.Matches(map[string]string{"app": "web"}) {
		t.Fatal("Matches wrong")
	}
	p, err := parseSelector("app = web, env!=dev")
	if err != nil || p.MatchLabels["app"] != "web" || !p.Matches(map[string]string{"app": "web", "env": "prod"}) || p.Matches(map[string]string{"app": "web", "env": "dev"}) {
		t.Fatalf("parseSelector: %+v %v", p, err)
	}
	if (LabelSelector{}).Matches(map[string]string{"a": "b"}) {
		t.Fatal("empty selector must not match everything")
	}
}

func TestKubeconfigMerge(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	os.WriteFile(a, []byte(`
current-context: one
clusters:
- name: c1
  cluster: {server: "https://one:6443", insecure-skip-tls-verify: true}
users:
- name: u1
  user: {token: t1}
contexts:
- name: one
  context: {cluster: c1, user: u1, namespace: payments}
`), 0o600)
	os.WriteFile(b, []byte(`
current-context: two
clusters:
- name: c1
  cluster: {server: "https://SHOULD-NOT-WIN"}
- name: c2
  cluster: {server: "https://two:6443"}
users:
- name: u2
  user:
    exec: {command: aws, args: [eks, get-token]}
contexts:
- name: one
  context: {cluster: c2, user: u2}
- name: two
  context: {cluster: c2, user: u2}
- name: broken
  context: {cluster: nope, user: u2}
`), 0o600)
	ctxs, err := loadKubeconfig([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(ctxs) != 3 {
		t.Fatalf("want 3 contexts, got %d", len(ctxs))
	}
	if ctxs[0].Server != "https://one:6443" || !ctxs[0].Current || ctxs[0].Namespace != "payments" {
		t.Fatalf("first file must win: %+v", ctxs[0].ContextInfo)
	}
	if ctxs[1].AuthType != "exec: aws" {
		t.Fatalf("auth type = %q", ctxs[1].AuthType)
	}
	if ctxs[2].Error == "" {
		t.Fatal("missing cluster should be reported")
	}
	if _, err := newClient(ctxs[2]); err == nil {
		t.Fatal("client for broken context should fail")
	}
	if _, err := loadKubeconfig([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing file should error")
	}
}

func TestSplitPaths(t *testing.T) {
	home, _ := os.UserHomeDir()
	got := splitPaths("~/.kube/config\n/tmp/other")
	if len(got) != 2 || got[0] != filepath.Join(home, ".kube/config") {
		t.Fatalf("splitPaths = %v", got)
	}
}

func TestDeploymentHealth(t *testing.T) {
	var d Deployment
	r := int32(3)
	d.Spec.Replicas = &r
	d.Status.Replicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas = 3, 3, 3
	if h := deploymentHealth(&d); h != "healthy" {
		t.Fatal(h)
	}
	d.Status.AvailableReplicas = 1
	if h := deploymentHealth(&d); h != "progressing" {
		t.Fatal(h)
	}
	d.Status.AvailableReplicas = 0
	if h := deploymentHealth(&d); h != "degraded" {
		t.Fatal(h)
	}
}

func TestNormalizeAccess(t *testing.T) {
	a := normalizeAccess(AccessSettings{Mode: "weird", Namespaces: []string{"payments, ecrm", "payments", "Bad_Name", " ott-streaming "}})
	if a.Mode != "auto" || len(a.Namespaces) != 3 || a.Namespaces[0] != "payments" || a.Namespaces[2] != "ott-streaming" {
		t.Fatalf("normalizeAccess = %+v", a)
	}
	if validDNSLabel("-x") || validDNSLabel("UPPER") || !validDNSLabel("team-a1") {
		t.Fatal("validDNSLabel wrong")
	}
	c := &Cluster{Info: ContextInfo{Namespace: "payments"}, access: AccessSettings{Namespaces: []string{"ecrm"}}}
	if got := c.scopeNamespaces(); len(got) != 2 || got[0] != "payments" || got[1] != "ecrm" {
		t.Fatalf("scopeNamespaces = %v", got)
	}
	empty := &Cluster{}
	if got := empty.scopeNamespaces(); len(got) != 1 || got[0] != "default" {
		t.Fatalf("fallback namespace = %v", got)
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	js := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"payments","managedFields":[{"manager":"kubectl"}],"labels":{"app":"web"}},"data":{"enabled":"true","port":"8080","script.sh":"#!/bin/sh\necho hi\n"}}`)
	y, err := toYAML(js)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(y, "managedFields") || !strings.HasPrefix(y, "apiVersion: v1\nkind: ConfigMap\n") {
		t.Fatalf("unexpected yaml:\n%s", y)
	}
	if !strings.Contains(y, `enabled: "true"`) || !strings.Contains(y, `port: "8080"`) || !strings.Contains(y, "script.sh: |") {
		t.Fatalf("strings must stay strings / multi-line literal:\n%s", y)
	}
	back, obj, err := fromYAML(y)
	if err != nil || obj["kind"] != "ConfigMap" {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(back, &m)
	if m["data"].(map[string]any)["enabled"] != "true" || m["data"].(map[string]any)["script.sh"] != "#!/bin/sh\necho hi\n" {
		t.Fatalf("round trip changed values: %s", back)
	}
	if _, _, err := fromYAML("- a\n- b\n"); err == nil {
		t.Fatal("list should be rejected")
	}
}

func TestParseDfScript(t *testing.T) {
	out := "@@/data\n10.1.2.3:6789:/volumes/csi/x 10485760 3355443 7130317 33% /data\n@@/missing\ndf: /missing: No such file or directory\n@@/var/ledger\nceph-fuse 20971520 18245222 2726298 87% /var/ledger\n"
	got := parseDfScript(out)
	if len(got) != 2 {
		t.Fatalf("want 2 readings, got %d: %v", len(got), got)
	}
	if u := got["/data"]; u.Size != 10485760*1024 || u.Used != 3355443*1024 || int(math.Round(u.Pct)) != 32 {
		t.Errorf("/data parsed wrong: %+v", u)
	}
	if u := got["/var/ledger"]; int(math.Round(u.Pct)) != 87 {
		t.Errorf("/var/ledger pct = %v", u.Pct)
	}
	if _, bad := got["/missing"]; bad {
		t.Error("error line must not produce a reading")
	}
}

func TestPVCCandidates(t *testing.T) {
	mk := func(name, phase string, running bool) Pod {
		var p Pod
		p.Metadata.Name, p.Metadata.Namespace = name, "ns"
		p.Status.Phase = phase
		p.Spec.Volumes = []Volume{{Name: "v", PVC: &struct {
			ClaimName string `json:"claimName"`
		}{"claim-a"}}}
		p.Spec.Containers = []Container{{Name: "c", VolumeMounts: []VolumeMount{{Name: "v", MountPath: "/d"}}}}
		cs := ContainerStatus{Name: "c"}
		if running {
			cs.State.Running = &struct {
				StartedAt time.Time `json:"startedAt"`
			}{}
		}
		p.Status.ContainerStatuses = []ContainerStatus{cs}
		return p
	}
	pods := []Pod{mk("up", "Running", true), mk("pending", "Pending", false), mk("crash", "Running", false)}
	c := pvcCandidates(pods, "ns")["claim-a"]
	if len(c) != 1 || c[0].pod != "up" || c[0].path != "/d" {
		t.Fatalf("only the running container should be a candidate, got %+v", c)
	}
}

func TestPodViewRestartInfo(t *testing.T) {
	var p Pod
	p.Metadata.Name = "x"
	fin := time.Now().Add(-5 * time.Minute)
	cs := ContainerStatus{Name: "c", RestartCount: 3}
	cs.LastState.Terminated = &struct {
		Reason     string    `json:"reason"`
		ExitCode   int       `json:"exitCode"`
		FinishedAt time.Time `json:"finishedAt"`
	}{"OOMKilled", 137, fin}
	p.Status.ContainerStatuses = []ContainerStatus{cs}
	v := podView(&Snapshot{owner: map[string]string{}, PodUsage: map[string]Usage{}}, &p, false)
	if v.LastReason != "OOMKilled" || v.LastExit == nil || *v.LastExit != 137 || v.LastRestart == nil {
		t.Fatalf("restart info missing: %+v", v)
	}
}

func TestMigrateLegacyData(t *testing.T) {
	base := t.TempDir()
	oldDir, newDir := filepath.Join(base, "KubeDeck"), filepath.Join(base, "KubeLamp")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "settings.json"), []byte(`{"kubeconfig":["/x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyData(oldDir, newDir)
	if b, err := os.ReadFile(filepath.Join(newDir, "settings.json")); err != nil || string(b) != `{"kubeconfig":["/x"]}` {
		t.Fatalf("settings not migrated: %v %q", err, b)
	}
	// a second run must not overwrite newer settings
	_ = os.WriteFile(filepath.Join(newDir, "settings.json"), []byte("new"), 0o600)
	migrateLegacyData(oldDir, newDir)
	if b, _ := os.ReadFile(filepath.Join(newDir, "settings.json")); string(b) != "new" {
		t.Fatal("migration overwrote existing settings")
	}
}
