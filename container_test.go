package main

import (
	"context"
	"encoding/base64"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestApplyEnv(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7080", "")
	ro := fs.Bool("read-only", false, "")
	maxDown := fs.Int("max-download", 256, "")
	interval := fs.Duration("interval", 15*time.Second, "")
	env := map[string]string{"KUBELAMP_ADDR": "0.0.0.0:9000", "KUBELAMP_READ_ONLY": "true", "KUBELAMP_MAX_DOWNLOAD": "64", "KUBELAMP_INTERVAL": "30s"}
	if err := applyEnv(fs, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if *addr != "0.0.0.0:9000" || !*ro || *maxDown != 64 || *interval != 30*time.Second {
		t.Errorf("got %s %v %d %s", *addr, *ro, *maxDown, *interval)
	}
	// command-line flags still win
	if err := fs.Parse([]string{"-addr", "127.0.0.1:1"}); err != nil || *addr != "127.0.0.1:1" {
		t.Errorf("flag override: %v %s", err, *addr)
	}
	if err := applyEnv(fs, func(k string) string { return map[string]string{"KUBELAMP_MAX_DOWNLOAD": "lots"}[k] }); err == nil || !strings.Contains(err.Error(), "KUBELAMP_MAX_DOWNLOAD") {
		t.Errorf("bad value should name the variable: %v", err)
	}
	if envName("max-download") != "KUBELAMP_MAX_DOWNLOAD" {
		t.Error(envName("max-download"))
	}
}

func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	if tok, src, _ := resolveToken("given", "", dir); tok != "given" || src != "flag" {
		t.Errorf("flag: %s %s", tok, src)
	}
	f := filepath.Join(dir, "secret")
	_ = os.WriteFile(f, []byte("  from-file\n"), 0o600)
	if tok, src, _ := resolveToken("", f, dir); tok != "from-file" || src != "file" {
		t.Errorf("file: %s %s", tok, src)
	}
	if _, _, err := resolveToken("", filepath.Join(dir, "missing"), dir); err == nil {
		t.Error("missing token file must fail")
	}
	data := filepath.Join(dir, "data")
	t1, src, _ := resolveToken("", "", data)
	if len(t1) != 32 || src != "new-saved" {
		t.Fatalf("generated: %q %s", t1, src)
	}
	if st, _ := os.Stat(filepath.Join(data, "token")); st == nil || st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode: %v", st)
	}
	t2, src, _ := resolveToken("", "", data)
	if t2 != t1 || src != "saved" {
		t.Errorf("restart must reuse the saved token: %s %s", t2, src)
	}
}

func TestInClusterContextFiles(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := inClusterContext(""); err == nil {
		t.Error("outside Kubernetes it must fail")
	}
	dir := t.TempDir()
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBELAMP_SA_DIR", dir)
	if _, err := inClusterContext(""); err == nil {
		t.Error("missing service account files must fail")
	}
	_ = os.WriteFile(filepath.Join(dir, "token"), []byte("t"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("x"), 0o600)
	rc, err := inClusterContext("")
	if err != nil || rc.Server != "https://[fd00::1]:443" || rc.Name != "in-cluster" || rc.AuthType != "service account" {
		t.Errorf("%+v %v", rc, err)
	}
}

// In-cluster against the mock API server: the pod's token and CA files, token rotation, and the
// registry showing the in-cluster context next to (or instead of) kubeconfig contexts.
func TestInClusterAgainstMock(t *testing.T) {
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
	kcPath := filepath.Join(dir, "kubeconfig")
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(kcPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(kcPath)
	var kc kcFile
	if err := yaml.Unmarshal(b, &kc); err != nil {
		t.Fatal(err)
	}
	var server, caB64 string
	for _, c := range kc.Clusters { // the EKS-like mock cluster accepts a bearer token
		if strings.Contains(c.Name, "eks-binge") {
			server, caB64 = c.Cluster.Server, c.Cluster.CAData
		}
	}
	u, _ := url.Parse(server)
	host, port, _ := net.SplitHostPort(u.Host)
	ca, _ := base64.StdEncoding.DecodeString(caB64)
	sa := filepath.Join(dir, "sa")
	_ = os.MkdirAll(sa, 0o755)
	_ = os.WriteFile(filepath.Join(sa, "ca.crt"), ca, 0o644)
	_ = os.WriteFile(filepath.Join(sa, "token"), []byte("mock-eks-token-123\n"), 0o600)
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	t.Setenv("KUBELAMP_SA_DIR", sa)

	ic, err := inClusterContext("binge-prod")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(*ic)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var ns struct{ Items []any }
	if err := cl.get(ctx, "/api/v1/namespaces", nil, &ns); err != nil || len(ns.Items) == 0 {
		t.Fatalf("in-cluster request: %v (%d namespaces)", err, len(ns.Items))
	}
	// Kubernetes rotates the projected token: the next request must use the new file content
	_ = os.WriteFile(filepath.Join(sa, "token"), []byte("rotated-but-wrong"), 0o600)
	if err := cl.get(ctx, "/api/v1/namespaces", nil, &ns); err == nil {
		t.Error("a wrong token in the file must be used (and fail)")
	}
	_ = os.WriteFile(filepath.Join(sa, "token"), []byte("mock-eks-token-123"), 0o600)
	if err := cl.get(ctx, "/api/v1/namespaces", nil, &ns); err != nil {
		t.Errorf("after rotation back: %v", err)
	}

	// registry: in-cluster only (no kubeconfig) → no load error; with a kubeconfig → both
	reg := &Registry{inCluster: ic}
	reg.Load(nil, nil)
	if reg.loadErr != "" || len(reg.All()) != 1 || reg.All()[0].Info.Name != "binge-prod" {
		t.Errorf("in-cluster only: err=%q n=%d", reg.loadErr, len(reg.All()))
	}
	reg2 := &Registry{inCluster: ic}
	reg2.Load([]string{kcPath}, nil)
	if len(reg2.All()) != len(kc.Contexts)+1 || reg2.All()[0].Info.Source != "in-cluster" || reg2.All()[0].Info.Current {
		t.Errorf("with kubeconfig: n=%d first=%+v", len(reg2.All()), reg2.All()[0].Info)
	}
	srv := httptest.NewServer((&Server{reg: reg, dataDir: dir, token: "tok"}).routes())
	defer srv.Close()
	// the token guard applies to the API but not to /healthz
	r, _ := http.Get(srv.URL + "/healthz")
	if r.StatusCode != 200 {
		t.Errorf("healthz: %d", r.StatusCode)
	}
	r.Body.Close()
	r, _ = http.Get(srv.URL + "/api/clusters")
	if r.StatusCode != 401 {
		t.Errorf("api without token: %d", r.StatusCode)
	}
	r.Body.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/overview?ctx=binge-prod", nil)
	req.Header.Set("Authorization", "Bearer tok")
	var ok bool
	for i := 0; i < 40 && !ok; i++ {
		r, err = http.DefaultClient.Do(req)
		if err == nil {
			ok = r.StatusCode == 200
			r.Body.Close()
		}
		if !ok {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if !ok {
		t.Errorf("overview of the in-cluster context did not load")
	}
}

func TestHealthcheckCommand(t *testing.T) {
	srv := httptest.NewServer((&Server{reg: &Registry{}, token: "x"}).routes())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if runHealthcheck(u.Host) != 0 {
		t.Error("healthy server reported unhealthy")
	}
	if runHealthcheck("127.0.0.1:1") == 0 {
		t.Error("closed port reported healthy")
	}
}
