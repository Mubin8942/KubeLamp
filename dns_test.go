package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDNSNames(t *testing.T) {
	got, err := parseDNSNames("a.example.com, b.svc.cluster.local\nkubernetes.default a.example.com")
	if err != nil || !reflect.DeepEqual(got, []string{"a.example.com", "b.svc.cluster.local", "kubernetes.default"}) {
		t.Errorf("names: %v %v", got, err)
	}
	for _, bad := range []string{"-rf", "bad host!", "10.0.0.1", "a..b", "$(id)", "a/b"} {
		if _, err := parseDNSNames(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	var many []string
	for i := 0; i <= dnsMaxNames; i++ {
		many = append(many, "h"+strings.Repeat("a", i%5)+string(rune('a'+i%26))+".example.com")
	}
	if _, err := parseDNSNames(strings.Join(many, " ")); err == nil {
		t.Error("too many names should fail")
	}
}

func TestParseDNSAnswer(t *testing.T) {
	cases := []struct {
		tool  string
		lines []string
		ips   []string
		cn    []string
	}{
		{"getent", []string{"10.96.0.1       STREAM kubernetes.default.svc.cluster.local", "10.96.0.1       DGRAM", "10.96.0.1       RAW", "2001:db8::1 STREAM x"}, []string{"10.96.0.1", "2001:db8::1"}, nil},
		{"nslookup", []string{"Server:\t\t10.96.0.10", "Address:\t10.96.0.10:53", "", "Name:\tapi.example.com", "Address: 10.1.1.5", "Name:\tapi.example.com", "Address: 10.1.1.6"}, []string{"10.1.1.5", "10.1.1.6"}, nil},
		{"nslookup", []string{"Server:    10.96.0.10", "Address 1: 10.96.0.10 kube-dns.kube-system.svc.cluster.local", "", "Name:      kubernetes.default", "Address 1: 10.96.0.1 kubernetes.default.svc.cluster.local"}, []string{"10.96.0.1"}, nil},
		{"nslookup", []string{"Server: 8.8.8.8", "Address: 8.8.8.8#53", "", "Non-authoritative answer:", "www.x.com\tcanonical name = x.com.", "Name:\tx.com", "Address: 93.184.216.34"}, []string{"93.184.216.34"}, nil},
		{"dig", []string{"www.x.com.\t30\tIN\tCNAME\tx.com.", "x.com.\t30\tIN\tA\t93.184.216.34"}, []string{"93.184.216.34"}, []string{"x.com"}},
		{"host", []string{"x.com has address 1.2.3.4", "x.com has IPv6 address 2001:db8::2", "x.com mail is handled by 10 m.x.com."}, []string{"1.2.3.4", "2001:db8::2"}, nil},
	}
	for i, c := range cases {
		ips, cn := parseDNSAnswer(c.tool, c.lines)
		if !reflect.DeepEqual(ips, c.ips) || !reflect.DeepEqual(cn, c.cn) {
			t.Errorf("case %d (%s): ips=%v cn=%v", i, c.tool, ips, cn)
		}
	}
}

func TestClassifyDNS(t *testing.T) {
	for _, c := range []struct {
		tool  string
		rc    int
		lines []string
		want  string
	}{
		{"getent", 2, nil, "notfound"},
		{"getent", 124, nil, "timeout"},
		{"nslookup", 1, []string{"** server can't find x.invalid: NXDOMAIN"}, "notfound"},
		{"nslookup", 1, []string{";; connection timed out; no servers could be reached"}, "timeout"},
		{"nslookup", 1, []string{"** server can't find x: SERVFAIL"}, "servfail"},
		{"nslookup", 1, []string{"nslookup: can't resolve 'x'"}, "error"},
		{"host", 1, []string{"Host x not found: 3(NXDOMAIN)"}, "notfound"},
	} {
		if st, _ := classifyDNS(c.tool, c.rc, nil, c.lines); st != c.want {
			t.Errorf("%s rc=%d %v: got %s, want %s", c.tool, c.rc, c.lines, st, c.want)
		}
	}
	if st, _ := classifyDNS("getent", 0, []string{"1.1.1.1"}, nil); st != "ok" {
		t.Error("answers mean ok")
	}
}

func TestParseExpect(t *testing.T) {
	n, err := parseExpect("10.0.0.5, 10.1.0.0/24 2001:db8::/32")
	if err != nil || len(n) != 3 {
		t.Fatalf("%v %v", n, err)
	}
	if !n[0].Contains([]byte{10, 0, 0, 5}) || n[0].Contains([]byte{10, 0, 0, 6}) || !n[1].Contains([]byte{10, 1, 0, 200}) {
		t.Error("matchers")
	}
	if _, err := parseExpect("nonsense"); err == nil {
		t.Error("bad expect should fail")
	}
}

func TestDNSScriptLocal(t *testing.T) {
	out := runSh(t, dnsScript, nil, "1", "", "localhost", "no-such-host.invalid")
	p := parseDNSOutput(out, []string{"localhost", "no-such-host.invalid"})
	if len(p.Tools) == 0 || len(p.Names) != 2 {
		t.Fatalf("parsed: %+v\n%s", p, out)
	}
	if r := p.Names[1]; r.State != "ok" || len(r.IPs) == 0 {
		t.Errorf("localhost: %+v", r)
	}
	if r := p.Names[2]; r.State == "ok" {
		t.Errorf("invalid name resolved: %+v", r)
	}
}
