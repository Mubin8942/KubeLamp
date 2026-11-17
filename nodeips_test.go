package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeKey(t *testing.T) {
	for in, want := range map[string]string{
		"gen1-w01.example.com": "gen1-w01", "STG-W11": "stg-w11", " stg-w01 ": "stg-w01", "10.0.0.1": "10.0.0.1",
	} {
		if got := nodeKey(in); got != want {
			t.Errorf("nodeKey(%q) = %q", in, got)
		}
	}
}

func TestParseNodeIPText(t *testing.T) {
	text := "Hostname\t IP\nstg-w01\t192.0.2.24\nSTG-W11, 198.51.100.87\ngen1-w01.example.com;192.0.2.11\nw02 10.0.0.2\n\nnot a row\nhost-x\tnot-an-ip\n"
	got, skipped := parseNodeIPRows(textRows(text))
	want := map[string]string{"stg-w01": "192.0.2.24", "STG-W11": "198.51.100.87", "gen1-w01.example.com": "192.0.2.11", "w02": "10.0.0.2"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q", k, got[k])
		}
	}
	if len(skipped) != 3 { // header, "not a row", host without IP
		t.Errorf("skipped %v", skipped)
	}
}

// a two-sheet workbook like the one admins keep: shared strings and an inline string
func testXLSX(t *testing.T) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	add := func(name, body string) { w, _ := z.Create(name); io.WriteString(w, body) }
	add("xl/sharedStrings.xml", `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Hostname</t></si><si><t> IP</t></si>`+
		`<si><t>stg-w01</t></si><si><t>192.0.2.24</t></si><si><r><t>STG-</t></r><r><t>W11</t></r></si><si><t>198.51.100.87</t></si></sst>`)
	add("xl/worksheets/sheet1.xml", `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`+
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>`+
		`<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2" t="s"><v>3</v></c></row>`+
		`<row r="3"><c r="A3" t="s"><v>4</v></c><c r="B3" t="s"><v>5</v></c></row></sheetData></worksheet>`)
	add("xl/worksheets/sheet2.xml", `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`+
		`<row r="2"><c r="A2" t="inlineStr"><is><t>gen2-w01.example.com</t></is></c><c r="B2" t="inlineStr"><is><t>192.0.2.16</t></is></c></row></sheetData></worksheet>`)
	z.Close()
	return buf.Bytes()
}

func TestXLSXRows(t *testing.T) {
	rows, err := xlsxRows(testXLSX(t))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := parseNodeIPRows(rows)
	if got["stg-w01"] != "192.0.2.24" || got["STG-W11"] != "198.51.100.87" || got["gen2-w01.example.com"] != "192.0.2.16" || len(got) != 3 {
		t.Fatalf("%v", got)
	}
	if _, err := xlsxRows([]byte("nope")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestNodeIPsHTTP(t *testing.T) {
	dir := t.TempDir()
	us, _ := openUserStore(dir, "admin", "Admin-pass-123", nil, nil)
	us.users = append(us.users, &User{ID: "bob1", Username: "bob", Role: "user", Hash: hashPassword("bobs-pass-123")})
	srv := &Server{reg: &Registry{}, users: us, dataDir: dir, nodeIPs: openNodeIPs(dir)}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	admin, bob := newTC(t, ts.URL), newTC(t, ts.URL)
	admin.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "Admin-pass-123"})
	bob.do("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "bobs-pass-123"})

	// only admins
	for _, c := range []struct{ m, p string }{{"GET", "/api/nodeips"}, {"PUT", "/api/nodeips"}, {"POST", "/api/nodeips/import"}} {
		if code, _ := bob.do(c.m, c.p, map[string]string{"host": "w1", "ip": "1.2.3.4", "text": "w1 1.2.3.4"}); code != 403 {
			t.Errorf("bob %s %s: %d", c.m, c.p, code)
		}
	}
	if code, _ := admin.do("PUT", "/api/nodeips", map[string]string{"host": "gen1-w01.example.com", "ip": "not-ip"}); code != 400 {
		t.Error("bad IP accepted")
	}
	if code, _ := admin.do("PUT", "/api/nodeips", map[string]string{"host": "a b", "ip": "1.2.3.4"}); code != 400 {
		t.Error("bad host accepted")
	}
	if code, _ := admin.do("PUT", "/api/nodeips", map[string]string{"host": "gen1-w01.example.com", "ip": "192.0.2.11"}); code != 200 {
		t.Fatal("set")
	}
	if srv.nodeIPs.Lookup("GEN1-W01") != "192.0.2.11" || srv.nodeIPs.Lookup("gen1-w01.other.domain") != "192.0.2.11" {
		t.Error("lookup ignores case and domain")
	}
	// import an .xlsx (raw body) and pasted text
	req, _ := http.NewRequest("POST", ts.URL+"/api/nodeips/import", bytes.NewReader(testXLSX(t)))
	r, err := admin.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.NewDecoder(r.Body).Decode(&m)
	r.Body.Close()
	if r.StatusCode != 200 || m["imported"].(float64) != 3 {
		t.Fatalf("xlsx import: %d %v", r.StatusCode, m)
	}
	if code, m := admin.do("POST", "/api/nodeips/import", map[string]string{"text": "w9\t10.9.9.9\nw10,10.9.9.10"}); code != 200 || m["imported"].(float64) != 2 {
		t.Fatalf("text import: %v", m)
	}
	// a protected / old workbook gets a clear message
	req, _ = http.NewRequest("POST", ts.URL+"/api/nodeips/import", bytes.NewReader([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1}))
	r, _ = admin.c.Do(req)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 400 || !strings.Contains(string(b), "save a copy as .xlsx") {
		t.Errorf("protected workbook: %d %s", r.StatusCode, b)
	}
	// saved, mode 600, survives a restart; removing works
	st, _ := os.Stat(filepath.Join(dir, "node-ips.json"))
	if st.Mode().Perm() != 0o600 || openNodeIPs(dir).Lookup("stg-w11") != "198.51.100.87" {
		t.Error("persisted")
	}
	admin.do("PUT", "/api/nodeips", map[string]string{"host": "w9", "ip": ""})
	if srv.nodeIPs.Lookup("w9") != "" {
		t.Error("remove")
	}
	if code, m := admin.do("GET", "/api/nodeips", nil); code != 200 || m["_raw"] == nil && false {
		t.Error("list")
	}
	if n := len(admin.list("/api/nodeips")); n != 5 {
		t.Errorf("list has %d", n)
	}
}

func TestNodeIPsMerge(t *testing.T) {
	mk := func(name, host string) Pod {
		var p Pod
		p.Metadata.Namespace, p.Metadata.Name, p.Spec.NodeName, p.Status.HostIP, p.Status.Phase = "app", "p-"+name, name, host, "Running"
		return p
	}
	store := openNodeIPs(t.TempDir())
	store.set(map[string]string{"GEN1-W01": "192.0.2.11"}, "admin")
	res := podNodes(&Snapshot{Pods: []Pod{mk("gen1-w01.example.com", "100.64.1.26"), mk("gen1-w02.example.com", "100.64.1.27")}}, nil)
	// what the handler does
	for i := range res.Nodes {
		if ip := store.Lookup(res.Nodes[i].Name); ip != "" {
			res.Nodes[i].ExternalIP, res.Nodes[i].ExternalSrc, res.Nodes[i].IP = ip, "manual", ip
		}
	}
	if n := res.Nodes[0]; n.ExternalIP != "192.0.2.11" || n.InternalIP != "100.64.1.26" || n.ExternalSrc != "manual" {
		t.Errorf("w01: %+v", n)
	}
	if n := res.Nodes[1]; n.ExternalIP != "" || n.InternalIP != "100.64.1.27" {
		t.Errorf("w02: %+v", n)
	}
	var nilStore *NodeIPStore
	if nilStore.Lookup("x") != "" {
		t.Error("nil store")
	}
}
