package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() { pbkdfIter = 1000 } // fast hashes in tests; production uses 600k

func TestPBKDF2Vectors(t *testing.T) {
	// RFC 7914 §11 / well-known PBKDF2-HMAC-SHA256 vectors
	for _, v := range []struct {
		iter int
		want string
	}{
		{1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	} {
		if got := hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), v.iter, 32)); got != v.want {
			t.Errorf("iter %d: %s", v.iter, got)
		}
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse")
	if !checkPassword(h, "correct horse") || checkPassword(h, "correct hors") || checkPassword(h, "") {
		t.Fatal("check")
	}
	if h == hashPassword("correct horse") {
		t.Fatal("hashes must be salted")
	}
	for _, bad := range []string{"", "x", "plain$1$a$b", "pbkdf2-sha256$5$AA$AA", "pbkdf2-sha256$abc$AA$AA"} {
		if checkPassword(bad, "x") {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
	for pw, ok := range map[string]bool{"short": false, "admin": false, "alice1234": false, "Password": false, "good-pass-1": true} {
		if (validPassword("alice1234", pw) == nil) != ok {
			t.Errorf("validPassword(%q)", pw)
		}
	}
}

func TestUserStoreBootstrap(t *testing.T) {
	dir := t.TempDir()
	s, err := openUserStore(dir, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.users) != 1 || s.users[0].Username != "admin" || !s.users[0].Admin() || !s.users[0].MustChange {
		t.Fatalf("default admin: %+v", s.users[0])
	}
	st, _ := os.Stat(filepath.Join(dir, "users.json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("users.json mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "users.json"))
	if bytes.Contains(b, []byte(`"admin"`)) && !bytes.Contains(b, []byte("pbkdf2-sha256$")) {
		t.Error("password must be stored as a hash")
	}
	// second start: env password is ignored, nothing is recreated
	s2, err := openUserStore(dir, "root", "Another-pass-9", nil, nil)
	if err != nil || len(s2.users) != 1 || s2.users[0].Username != "admin" || !checkPassword(s2.users[0].Hash, "admin") {
		t.Fatalf("restart must keep users: %v %+v", err, s2.users)
	}
	// admin from the environment: no forced change; a weak one is refused
	s3, err := openUserStore(t.TempDir(), "ops", "Str0ng-admin-pw", nil, nil)
	if err != nil || s3.users[0].Username != "ops" || s3.users[0].MustChange {
		t.Fatalf("env admin: %v %+v", err, s3.users)
	}
	if _, err := openUserStore(t.TempDir(), "ops", "short", nil, nil); err == nil {
		t.Error("weak KUBELAMP_ADMIN_PASSWORD accepted")
	}
}

func TestMigrateSingleUserData(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfigs", "prod-abcd.yaml")
	_ = os.MkdirAll(filepath.Dir(kc), 0o700)
	_ = os.WriteFile(kc, []byte(testKubeconfig), 0o600)
	saveSettings(dir, settings{Kubeconfig: []string{kc}})
	s, err := openUserStore(dir, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := loadSettings(s.UserDir(s.users[0]))
	if len(st.Kubeconfig) != 1 || !strings.HasPrefix(st.Kubeconfig[0], s.UserDir(s.users[0])) {
		t.Fatalf("not moved to the admin: %+v", st)
	}
	if _, err := os.Stat(st.Kubeconfig[0]); err != nil {
		t.Fatal(err)
	}
}

const testKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: abc}}]
contexts: [{name: CTX, context: {cluster: c, user: u}}]
current-context: CTX
`

// ---- HTTP helpers

type tc struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newTC(t *testing.T, base string) *tc {
	jar, _ := cookiejar.New(nil)
	return &tc{t, base, &http.Client{Jar: jar}}
}

func (c *tc) do(method, path string, body any, hdr ...string) (int, map[string]any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{"_raw": string(b)}
	}
	return r.StatusCode, m
}

func (c *tc) list(path string) []any {
	req, _ := http.NewRequest("GET", c.base+path, nil)
	r, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Body.Close()
	var v []any
	_ = json.NewDecoder(r.Body).Decode(&v)
	return v
}

func (c *tc) session() string {
	req, _ := http.NewRequest("GET", c.base, nil)
	for _, k := range c.c.Jar.Cookies(req.URL) {
		if k.Name == sessionCookie {
			return k.Value
		}
	}
	return ""
}

// login returns a signed-in client (for tests that only need access).
func login(t *testing.T, base, user, pw string) *http.Client {
	c := newTC(t, base)
	if code, m := c.do("POST", "/api/auth/login", map[string]string{"username": user, "password": pw}); code != 200 {
		t.Fatalf("login %s: %d %v", user, code, m)
	}
	return c.c
}

func authServer(t *testing.T, local bool) (*httptest.Server, *UserStore) {
	dir := t.TempDir()
	us, err := openUserStore(dir, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{reg: &Registry{}, users: us, dataDir: dir, localOnly: false, allowShell: true}
	ts := httptest.NewServer(srv.routes())
	_ = local
	t.Cleanup(ts.Close)
	return ts, us
}

func TestAuthFlow(t *testing.T) {
	ts, us := authServer(t, false)
	anon := newTC(t, ts.URL)

	// public: page, scripts, healthz; private: every API
	for _, p := range []string{"/", "/js/core.js", "/style.css", "/healthz"} {
		if r, _ := http.Get(ts.URL + p); r.StatusCode != 200 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
	for _, p := range []string{"/api/clusters", "/api/kubeconfig", "/api/users", "/api/info", "/api/auth/me", "/api/shell", "/api/routes?ctx=x"} {
		if code, _ := anon.do("GET", p, nil); code != 401 {
			t.Errorf("anonymous %s: %d", p, code)
		}
	}
	// the old ?token= trick does nothing
	if code, _ := anon.do("GET", "/api/clusters?token=anything", nil); code != 401 {
		t.Error("token query accepted")
	}

	// wrong passwords, unknown user: same answer
	_, m1 := anon.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "nope"})
	_, m2 := anon.do("POST", "/api/auth/login", map[string]string{"username": "ghost", "password": "nope"})
	if m1["error"] != m2["error"] {
		t.Errorf("login errors reveal which users exist: %v / %v", m1, m2)
	}
	// cross-site login is blocked
	if code, _ := anon.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "admin"}, "Origin", "https://evil.example"); code != 403 {
		t.Errorf("cross-origin login: %d", code)
	}

	// admin/admin signs in, but must change the password before anything else
	admin := newTC(t, ts.URL)
	admin.c.Jar.SetCookies(mustURL(ts.URL), []*http.Cookie{{Name: sessionCookie, Value: "attacker-chosen"}})
	code, m := admin.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "admin"})
	if code != 200 || m["user"].(map[string]any)["mustChange"] != true {
		t.Fatalf("admin login: %d %v", code, m)
	}
	if admin.session() == "attacker-chosen" || len(admin.session()) != 64 {
		t.Error("session ID must be new and random (no fixation)")
	}
	if code, m := admin.do("GET", "/api/clusters", nil); code != 403 || m["mustChange"] != true {
		t.Errorf("must change first: %d %v", code, m)
	}
	if code, _ := admin.do("GET", "/api/auth/me", nil); code != 200 {
		t.Error("me must work before the change")
	}
	if code, _ := admin.do("POST", "/api/auth/password", map[string]string{"current": "wrong", "new": "Admin-new-pass-1"}); code != 400 {
		t.Error("wrong current password accepted")
	}
	if code, _ := admin.do("POST", "/api/auth/password", map[string]string{"current": "admin", "new": "short"}); code != 400 {
		t.Error("weak new password accepted")
	}
	old := admin.session()
	if code, m := admin.do("POST", "/api/auth/password", map[string]string{"current": "admin", "new": "Admin-new-pass-1"}); code != 200 {
		t.Fatalf("change: %d %v", code, m)
	}
	if admin.session() == old {
		t.Error("session must be renewed after a password change")
	}
	stale := newTC(t, ts.URL)
	stale.c.Jar.SetCookies(mustURL(ts.URL), []*http.Cookie{{Name: sessionCookie, Value: old}})
	if code, _ := stale.do("GET", "/api/clusters", nil); code != 401 {
		t.Error("old session must be ended after a password change")
	}
	if code, _ := admin.do("GET", "/api/clusters", nil); code != 200 {
		t.Error("admin after change")
	}
	b, _ := os.ReadFile(filepath.Join(us.dir, "users.json"))
	if bytes.Contains(b, []byte("Admin-new-pass-1")) {
		t.Fatal("plain password on disk")
	}

	// admin creates bob; bob must change the temporary password
	if code, _ := admin.do("POST", "/api/users", map[string]string{"username": "bob", "password": "temp-pass-123", "role": "user"}); code != 200 {
		t.Fatal("create bob")
	}
	if code, _ := admin.do("POST", "/api/users", map[string]string{"username": "BOB", "password": "temp-pass-123"}); code != 409 {
		t.Error("duplicate username (case-insensitive) accepted")
	}
	if code, _ := admin.do("POST", "/api/users", map[string]string{"username": "../etc", "password": "temp-pass-123"}); code != 400 {
		t.Error("bad username accepted")
	}
	bob := newTC(t, ts.URL)
	if code, _ := bob.do("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "temp-pass-123"}); code != 200 {
		t.Fatal("bob login")
	}
	if code, _ := bob.do("GET", "/api/kubeconfig", nil); code != 403 {
		t.Error("bob must change his password first")
	}
	bob.do("POST", "/api/auth/password", map[string]string{"current": "temp-pass-123", "new": "bobs-own-pass-9"})

	// bob is not an admin
	for _, c := range []struct{ m, p string }{{"GET", "/api/users"}, {"POST", "/api/users"}, {"DELETE", "/api/users/" + us.users[0].ID},
		{"PUT", "/api/users/" + us.users[0].ID}, {"POST", "/api/quit"}, {"POST", "/api/kubeconfig/path"}, {"PUT", "/api/kubeconfig"}} {
		if code, _ := bob.do(c.m, c.p, map[string]string{"path": "/etc/passwd", "paths": "/etc/passwd", "role": "admin", "username": "x", "password": "xxxxxxxxxx1"}); code != 403 {
			t.Errorf("bob %s %s: %d", c.m, c.p, code)
		}
	}
	if _, m := bob.do("GET", "/api/info", nil); m["shell"] != false || m["admin"] != false {
		t.Errorf("bob info: %v", m)
	}
	if _, m := admin.do("GET", "/api/info", nil); m["shell"] != true || m["admin"] != true {
		t.Errorf("admin info: %v", m)
	}

	// kubeconfigs are per user
	if code, m := admin.do("POST", "/api/kubeconfig/upload", map[string]string{"name": "prod", "yaml": testKubeconfig}); code != 200 {
		t.Fatalf("admin upload: %v", m)
	}
	_, ak := admin.do("GET", "/api/kubeconfig", nil)
	_, bk := bob.do("GET", "/api/kubeconfig", nil)
	if len(ak["sources"].([]any)) != 1 || len(bk["sources"].([]any)) != 0 || bk["needsSetup"] != true {
		t.Fatalf("isolation: admin %v / bob %v", ak["sources"], bk["sources"])
	}
	if n := len(bob.list("/api/clusters")); n != 0 {
		t.Errorf("bob sees %d of admin's clusters", n)
	}
	if code, _ := bob.do("GET", "/api/overview?ctx=CTX", nil); code != 404 {
		t.Errorf("bob reached admin's context: %d", code)
	}
	adminPath := ak["sources"].([]any)[0].(map[string]any)["path"].(string)
	if code, _ := bob.do("DELETE", "/api/kubeconfig?path="+adminPath, nil); code != 404 {
		t.Error("bob removed admin's kubeconfig")
	}
	if _, err := os.Stat(adminPath); err != nil || !strings.HasPrefix(adminPath, filepath.Join(us.dir, "users")) {
		t.Errorf("admin's kubeconfig: %s %v", adminPath, err)
	}
	// bob's uploads must not run commands or read server files
	evil := "contexts: [{name: e, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {exec: {command: sh, args: [-c, id]}}}]\n"
	if code, _ := bob.do("POST", "/api/kubeconfig/upload", map[string]string{"name": "e", "yaml": evil}); code != 400 {
		t.Error("exec sh accepted from a user")
	}
	if code, _ := bob.do("POST", "/api/kubeconfig/upload", map[string]string{"name": "../../x", "yaml": testKubeconfig}); code != 200 {
		t.Error("bob upload")
	}
	_, bk = bob.do("GET", "/api/kubeconfig", nil)
	bobPath := bk["sources"].([]any)[0].(map[string]any)["path"].(string)
	if !strings.HasPrefix(bobPath, filepath.Join(us.dir, "users")) || strings.Contains(filepath.Base(bobPath), "..") {
		t.Errorf("upload name escaped the user folder: %s", bobPath)
	}

	// admin resets bob's password: bob is signed out and must change it again
	bobID := ""
	for _, u := range admin.list("/api/users") {
		if u.(map[string]any)["username"] == "bob" {
			bobID = u.(map[string]any)["id"].(string)
		}
	}
	if code, _ := admin.do("PUT", "/api/users/"+bobID, map[string]string{"password": "reset-pass-77"}); code != 200 {
		t.Fatal("reset")
	}
	if code, _ := bob.do("GET", "/api/kubeconfig", nil); code != 401 {
		t.Error("bob must be signed out after a reset")
	}
	bob.do("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "reset-pass-77"})
	if _, m := bob.do("GET", "/api/auth/me", nil); m["user"].(map[string]any)["mustChange"] != true {
		t.Error("reset password must be temporary")
	}

	// guard rails
	adminID := us.users[0].ID
	if code, _ := admin.do("DELETE", "/api/users/"+adminID, nil); code != 400 {
		t.Error("admin deleted themselves")
	}
	if code, _ := admin.do("PUT", "/api/users/"+adminID, map[string]string{"role": "user"}); code != 400 {
		t.Error("last admin demoted")
	}
	// conntest jobs belong to their owner
	connMgr.add(&ConnJob{ID: "job-of-admin", owner: adminID})
	if code, _ := bob.do("GET", "/api/tools/conntest/job-of-admin", nil); code == 200 {
		t.Error("bob read admin's job")
	}

	// delete bob: signed out, folder gone
	if code, _ := admin.do("DELETE", "/api/users/"+bobID, nil); code != 200 {
		t.Fatal("delete bob")
	}
	if code, _ := bob.do("GET", "/api/auth/me", nil); code != 401 {
		t.Error("deleted user still signed in")
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(bobPath))); !os.IsNotExist(err) {
		t.Error("deleted user's folder remains")
	}

	// logout ends the session
	s := admin.session()
	admin.do("POST", "/api/auth/logout", nil)
	stale.c.Jar.SetCookies(mustURL(ts.URL), []*http.Cookie{{Name: sessionCookie, Value: s}})
	if code, _ := stale.do("GET", "/api/clusters", nil); code != 401 {
		t.Error("session valid after logout")
	}
}

func TestLoginThrottle(t *testing.T) {
	ts, _ := authServer(t, false)
	c := newTC(t, ts.URL)
	for i := 0; i < 5; i++ {
		c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "bad"})
	}
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "admin"}); code != 429 {
		t.Errorf("after 5 failures even the right password must wait: %d", code)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	ts, _ := authServer(t, false)
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin"})
	req, _ := http.NewRequest("POST", ts.URL+"/api/auth/login", bytes.NewReader(b))
	req.Header.Set("X-Forwarded-Proto", "https")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	var ck *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", ck)
	}
}

func TestLocalAdminMayUseExecPlugins(t *testing.T) {
	dir := t.TempDir()
	us, _ := openUserStore(dir, "admin", "Admin-pass-123", nil, nil)
	ts := httptest.NewServer((&Server{reg: &Registry{}, users: us, dataDir: dir, localOnly: true}).routes())
	defer ts.Close()
	c := newTC(t, strings.Replace(ts.URL, "127.0.0.1", "localhost", 1))
	c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "Admin-pass-123"})
	y := "contexts: [{name: e, context: {cluster: c, user: u}}]\nclusters: [{name: c, cluster: {server: https://127.0.0.1:1}}]\nusers: [{name: u, user: {exec: {command: my-plugin}}}]\n"
	if code, m := c.do("POST", "/api/kubeconfig/upload", map[string]string{"name": "e", "yaml": y}); code != 200 {
		t.Errorf("local admin: %d %v", code, m)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
