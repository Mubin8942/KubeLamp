package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// End-to-end: browser API → KubeLamp → exec WebSocket → the pod-side scripts, against the
// mock cluster (which runs them with a real sh on a directory standing in for the volume).
func TestFileTransferAgainstMock(t *testing.T) {
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
	// small limits so the tests can hit them: 1 MB down, 1 MB up, fast throttle
	srv := &Server{reg: reg, dataDir: dir, localOnly: true, ftMaxDown: 1 << 20, ftMaxUp: 1 << 20, ftRate: 200 << 20}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	ro := httptest.NewServer((&Server{reg: reg, dataDir: dir, localOnly: true, readOnly: true}).routes())
	defer ro.Close()

	// find the ecrm pods
	var pods struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	resp, err := http.Get(ts.URL + "/api/pods?ctx=prod-dc1&ns=ecrm")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var raw any
	_ = json.Unmarshal(b, &raw)
	var backend, ncdu string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if n, ok := x["name"].(string); ok {
				if strings.HasPrefix(n, "ecrm-backend-") && backend == "" {
					backend = n
				}
				if strings.HasPrefix(n, "ncdu-") && ncdu == "" {
					ncdu = n
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(raw)
	_ = pods
	if backend == "" || ncdu == "" {
		t.Fatalf("pods not found (backend=%q ncdu=%q): %s", backend, ncdu, string(b)[:min(400, len(b))])
	}
	q := func(pod, ctr string, kv ...string) string {
		v := url.Values{"ctx": {"prod-dc1"}, "ns": {"ecrm"}, "pod": {pod}, "container": {ctr}}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v.Encode()
	}
	getJSON := func(base, p string) (int, map[string]any) {
		r, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	upload := func(base, pod, ctr string, body []byte, kv ...string) (int, map[string]any) {
		kv = append(kv, "size", strconv.Itoa(len(body)))
		r, err := http.Post(base+"/api/tools/files/upload?"+q(pod, ctr, kv...), "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}

	// read-only mode disables the whole tool
	if code, _ := getJSON(ro.URL, "/api/tools/files/list?"+q(backend, "app", "path", "/uploads")); code != 403 {
		t.Errorf("read-only list: %d", code)
	}
	if code, _ := upload(ro.URL, backend, "app", []byte("x"), "dir", "/uploads", "name", "a.txt"); code != 403 {
		t.Errorf("read-only upload: %d", code)
	}

	// pod/mount info
	code, m := getJSON(ts.URL, "/api/tools/files/pod?"+q(backend, "app"))
	if code != 200 || !strings.Contains(toJSON(m), `"claim":"ecrm-uploads"`) {
		t.Fatalf("pod info: %d %v", code, m)
	}

	// list
	code, m = getJSON(ts.URL, "/api/tools/files/list?"+q(backend, "app", "path", "/uploads"))
	if code != 200 || m["error"] != nil {
		t.Fatalf("list: %d %v", code, m)
	}
	names := []string{}
	for _, e := range m["entries"].([]any) {
		names = append(names, e.(map[string]any)["n"].(string))
	}
	for _, want := range []string{"notes.txt", "reports", "storage", "latest", "odd name & spaces"} {
		if !contains(names, want) {
			t.Errorf("list is missing %q: %v", want, names)
		}
	}
	if _, m = getJSON(ts.URL, "/api/tools/files/list?"+q(backend, "app", "path", "/uploads/nope")); m["error"] == nil {
		t.Errorf("listing a missing dir should report an error: %v", m)
	}
	if code, _ = getJSON(ts.URL, "/api/tools/files/list?"+q(backend, "app", "path", "relative")); code != 400 {
		t.Errorf("relative path: %d", code)
	}
	if code, _ = getJSON(ts.URL, "/api/tools/files/list?"+q(backend, "envoy", "path", "/uploads")); code == 200 {
		t.Errorf("container without a shell should fail")
	}

	// file explorer: view text (head and tail), binary preview, folder, missing, who we run as
	if _, m = getJSON(ts.URL, "/api/tools/files/list?"+q(backend, "app", "path", "/uploads")); m["user"] == "" || m["user"] == nil {
		t.Errorf("list should report the container user: %v", m["user"])
	}
	code, m = getJSON(ts.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/notes.txt"))
	if code != 200 || m["text"] != "hello from the mock volume\n" || m["truncated"] != false || m["binary"] != false {
		t.Errorf("view text: %d %v", code, m)
	}
	code, m = getJSON(ts.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/storage/logs/laravel.log", "mode", "tail", "bytes", "2048"))
	if txt, _ := m["text"].(string); code != 200 || len(txt) != 2048 || !strings.HasSuffix(txt, "LAST-LINE-MARKER\n") || m["truncated"] != true {
		t.Errorf("view tail: %d shown=%v truncated=%v", code, m["shown"], m["truncated"])
	}
	code, m = getJSON(ts.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/storage/logs/laravel.log", "bytes", "99999999"))
	if sh, _ := m["shown"].(float64); code != 200 || sh != 512<<10 {
		t.Errorf("view must cap at 512 KiB: %d shown=%v", code, m["shown"])
	}
	code, m = getJSON(ts.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/images/pixel.bin"))
	if hx, _ := m["hex"].(string); code != 200 || m["binary"] != true || !strings.Contains(hx, "89 50 4e 47") || m["text"] != "" {
		t.Errorf("view binary: %d %v", code, m)
	}
	if code, _ = getJSON(ts.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/reports")); code != 400 {
		t.Errorf("view folder: %d", code)
	}
	if code, _ = getJSON(ts.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/nope.txt")); code != 404 {
		t.Errorf("view missing: %d", code)
	}
	if code, _ = getJSON(ro.URL, "/api/tools/files/view?"+q(backend, "app", "path", "/uploads/notes.txt")); code != 403 {
		t.Errorf("view in read-only mode: %d", code)
	}

	// download: file, tail, over the limit, folder, missing
	dl := func(p string, kv ...string) (int, http.Header, []byte) {
		r, err := http.Get(ts.URL + "/api/tools/files/download?" + q(backend, "app", append([]string{"path", p}, kv...)...))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, r.Header, b
	}
	code, h, body := dl("/uploads/notes.txt")
	if code != 200 || string(body) != "hello from the mock volume\n" || !strings.Contains(h.Get("Content-Disposition"), "notes.txt") || h.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Errorf("download file: %d %q %v", code, body, h)
	}
	if code, _, body = dl("/uploads/storage/logs/laravel.log"); code != 413 || !strings.Contains(string(body), "download limit") {
		t.Errorf("over-limit file: %d %s", code, body)
	}
	code, _, body = dl("/uploads/storage/logs/laravel.log", "tail", "1")
	if code != 200 || len(body) != 1<<20 || !strings.HasSuffix(string(body), "LAST-LINE-MARKER\n") {
		t.Errorf("tail: %d len=%d", code, len(body))
	}
	if code, _, _ = dl("/uploads/missing.txt"); code != 404 {
		t.Errorf("missing file: %d", code)
	}
	if code, _, body = dl("/uploads/storage"); code != 413 { // ~2.7 MB of logs > 1 MB limit
		t.Errorf("folder over the limit: %d %s", code, body)
	}
	code, h, body = dl("/uploads/reports", "gz", "1")
	if code != 200 || len(body) < 2 || body[0] != 0x1f || !strings.HasSuffix(h.Get("Content-Disposition"), "reports.tar.gz\"; filename*=UTF-8''reports.tar.gz") {
		t.Errorf("folder download: %d %v len=%d", code, h, len(body))
	}
	if code, _, _ = dl("/"); code == 200 {
		t.Errorf("downloading / must be refused")
	}
	// stat for the UI
	code, m = getJSON(ts.URL, "/api/tools/files/stat?"+q(backend, "app", "path", "/uploads/storage/logs/laravel.log"))
	if code != 200 || m["ok"] != false || !strings.Contains(m["why"].(string), "limit") || m["tail"] != true {
		t.Errorf("stat: %d %v", code, m)
	}

	// upload: normal, verify, exists, overwrite, atomic (no part files left)
	payload := bytes.Repeat([]byte("kubelamp-"), 20000) // 180 KB
	sum := sha256.Sum256(payload)
	code, m = upload(ts.URL, backend, "app", payload, "dir", "/uploads/reports", "name", "big.dat")
	if code != 200 || m["sha256"] != hex.EncodeToString(sum[:]) || m["verified"] != "sha256" {
		t.Fatalf("upload: %d %v", code, m)
	}
	onDisk, _ := os.ReadFile(filepath.Join(dir, "pvc", "ecrm-uploads", "reports", "big.dat"))
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("file on the volume differs (%d bytes)", len(onDisk))
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "pvc", "ecrm-uploads", "reports")); func() bool {
		for _, e := range ents {
			if strings.Contains(e.Name(), "kl-part") {
				return true
			}
		}
		return false
	}() {
		t.Error("a .kl-part file was left behind")
	}
	if code, m = upload(ts.URL, backend, "app", []byte("again"), "dir", "/uploads/reports", "name", "big.dat"); code != 409 || m["code"] != "exists" {
		t.Errorf("existing file: %d %v", code, m)
	}
	if code, m = upload(ts.URL, backend, "app", []byte("again"), "dir", "/uploads/reports", "name", "big.dat", "overwrite", "1"); code != 200 {
		t.Errorf("overwrite: %d %v", code, m)
	}
	if onDisk, _ = os.ReadFile(filepath.Join(dir, "pvc", "ecrm-uploads", "reports", "big.dat")); string(onDisk) != "again" {
		t.Errorf("overwrite content %q", onDisk)
	}
	// the same volume seen from the ncdu pod (mounted at /data)
	code, m = getJSON(ts.URL, "/api/tools/files/list?"+q(ncdu, "ncdu", "path", "/data/reports"))
	if code != 200 || !strings.Contains(toJSON(m), "big.dat") {
		t.Errorf("ncdu view of the uploaded file: %d %v", code, m)
	}

	// rules: too big, not on a volume, bad names, size mismatch, missing dir
	if code, m = upload(ts.URL, backend, "app", make([]byte, 1<<20+1), "dir", "/uploads", "name", "huge"); code != 413 || m["code"] != "toobig" {
		t.Errorf("over upload limit: %d %v", code, m)
	}
	code, m = upload(ts.URL, backend, "app", []byte("x"), "dir", "/tmp", "name", "x.txt")
	if code != 409 || m["code"] != "layer" {
		t.Errorf("container layer must need confirmation: %d %v", code, m)
	}
	if code, m = upload(ts.URL, backend, "app", []byte("x"), "dir", "/tmp", "name", "x.txt", "allowLayer", "1"); code != 200 {
		t.Errorf("container layer with allowLayer: %d %v", code, m)
	}
	for _, bad := range []string{"../x", "a/b", "..", ""} {
		if code, _ = upload(ts.URL, backend, "app", []byte("x"), "dir", "/uploads", "name", bad); code != 400 {
			t.Errorf("name %q: %d", bad, code)
		}
	}
	if code, m = upload(ts.URL, backend, "app", []byte("x"), "dir", "/uploads/nodir", "name", "a"); code != 404 || m["code"] != "nodir" {
		t.Errorf("missing target dir: %d %v", code, m)
	}
	r, _ := http.Post(ts.URL+"/api/tools/files/upload?"+q(backend, "app", "dir", "/uploads", "name", "z", "size", "99"), "application/octet-stream", strings.NewReader("short"))
	if r.StatusCode != 400 {
		t.Errorf("size mismatch: %d", r.StatusCode)
	}
	r.Body.Close()

	// alternatives: from the app container the volume is also reachable through the ncdu pod
	code, m = getJSON(ts.URL, "/api/tools/files/targets?"+q(backend, "app", "path", "/uploads/reports"))
	alts, _ := m["alternatives"].([]any)
	var viaNcdu string
	for _, a := range alts {
		if a.(map[string]any)["pod"] == ncdu {
			viaNcdu, _ = a.(map[string]any)["path"].(string)
		}
	}
	// the other replica of the app and the ncdu pod both mount this volume
	if code != 200 || len(alts) != 2 || viaNcdu != "/data/reports" {
		t.Errorf("targets: %d %v", code, m)
	}
	// ...and uploading through the ncdu pod lands in the same volume
	if code, m = upload(ts.URL, ncdu, "ncdu", []byte("via ncdu"), "dir", "/data/reports", "name", "via-ncdu.txt"); code != 200 {
		t.Errorf("upload via ncdu: %d %v", code, m)
	}
	if code, _, body = dl("/uploads/reports/via-ncdu.txt"); code != 200 || string(body) != "via ncdu" {
		t.Errorf("file written via ncdu, read via app: %d %q", code, body)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
