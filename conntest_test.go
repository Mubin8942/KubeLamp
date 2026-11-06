package main

import (
	"encoding/json"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func labels(cs []ConnCheck) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Label)
	}
	return out
}

func TestParseTargets(t *testing.T) {
	tests := []struct {
		name, targets, ports string
		want                 []string
		errs                 int
	}{
		{"single ip with ports", "10.0.0.5", "22,443", []string{"10.0.0.5:22", "10.0.0.5:443"}, 0},
		{"last octet range x port range", "172.18.1.32-34", "22-23", []string{"172.18.1.32:22", "172.18.1.32:23", "172.18.1.33:22", "172.18.1.33:23", "172.18.1.34:22", "172.18.1.34:23"}, 0},
		{"full range", "10.0.0.254-10.0.1.1", "80", []string{"10.0.0.254:80", "10.0.0.255:80", "10.0.1.0:80", "10.0.1.1:80"}, 0},
		{"comma list mixed", "10.0.0.1, 10.0.0.2;10.0.0.3\n10.0.0.4", "22", []string{"10.0.0.1:22", "10.0.0.2:22", "10.0.0.3:22", "10.0.0.4:22"}, 0},
		{"cidr skips network and broadcast", "10.0.0.0/30", "22", []string{"10.0.0.1:22", "10.0.0.2:22"}, 0},
		{"token port overrides field", "10.0.0.1:5432 10.0.0.2", "22", []string{"10.0.0.1:5432", "10.0.0.2:22"}, 0},
		{"token port range", "10.0.0.1:22-23", "", []string{"10.0.0.1:22", "10.0.0.1:23"}, 0},
		{"domain without port is https", "Example.COM", "", []string{"example.com (https)"}, 0},
		{"domain with port is tcp", "db.internal:5432", "", []string{"db.internal:5432"}, 0},
		{"domain uses ports field", "db.internal", "5432", []string{"db.internal:5432"}, 0},
		{"url keeps path", "https://api.example.com/health", "", []string{"https://api.example.com/health"}, 0},
		{"http url default port", "http://10.0.0.9/x", "", []string{"http://10.0.0.9/x"}, 0},
		{"duplicates removed", "10.0.0.1 10.0.0.1-1", "22 22", []string{"10.0.0.1:22"}, 0},
		{"ip without port", "10.0.0.1", "", nil, 1},
		{"ipv6 rejected", "::1", "22", nil, 1},
		{"bad range", "10.0.0.9-3", "22", nil, 1},
		{"bad octet end", "10.0.0.1-300", "22", nil, 1},
		{"cidr too large", "10.0.0.0/16", "22", nil, 1},
		{"bad port", "10.0.0.1", "99999", nil, 1},
		{"bad hostname", "bad_host!", "22", nil, 1},
		{"bad scheme", "ftp://x.com", "", nil, 1},
		{"url with unsafe chars", "http://x.com/a$b", "", nil, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs, errs := parseTargets(tc.targets, tc.ports)
			if len(errs) != tc.errs {
				t.Fatalf("errors = %v, want %d", errs, tc.errs)
			}
			if tc.errs == 0 {
				got := strings.Join(labels(cs), " ")
				if got != strings.Join(tc.want, " ") {
					t.Fatalf("got  %s\nwant %s", got, strings.Join(tc.want, " "))
				}
			}
			for i, c := range cs {
				if c.ID != i {
					t.Fatalf("check IDs must be sequential, got %d at %d", c.ID, i)
				}
			}
		})
	}
	// the user's own example: 172.18.1.32-40 with ports 22-25 is 9 hosts x 4 ports
	cs, errs := parseTargets("172.18.1.32-40", "22-25")
	if len(errs) != 0 || len(cs) != 36 {
		t.Fatalf("got %d checks, errs %v", len(cs), errs)
	}
	// limit
	if _, errs := parseTargets("10.0.0.0/22", "1-2"); len(errs) == 0 {
		t.Fatal("expected a limit error")
	}
}

func TestClassifyProbe(t *testing.T) {
	tests := []struct {
		name, mode, tool string
		rc, ms           int
		msg              string
		state            string
		http             int
	}{
		{"notool", "tcp", "none", 0, 0, "", "notool", 0},
		{"killed by timeout", "tcp", "nc", 124, 6000, "", "timeout", 0},
		{"openbsd nc open", "tcp", "nc", 0, 5, "Connection to 10.0.0.5 22 port [tcp/ssh] succeeded!", "open", 0},
		{"openbsd nc refused", "tcp", "nc", 1, 3, "nc: connect to 10.0.0.5 port 23 (tcp) failed: Connection refused", "refused", 0},
		{"openbsd nc timeout", "tcp", "nc", 1, 3001, "nc: connect to 10.0.0.5 port 24 (tcp) timed out: Operation now in progress", "timeout", 0},
		{"nc no route", "tcp", "nc", 1, 20, "nc: connect to 10.0.0.5 port 25 (tcp) failed: No route to host", "unreachable", 0},
		{"busybox nc refused", "tcp", "nc", 1, 4, "nc: 10.0.0.5 (10.0.0.5:22): Connection refused", "refused", 0},
		{"busybox nc timeout", "tcp", "nc", 1, 3000, "nc: timeout", "timeout", 0},
		{"nc silent fast failure", "tcp", "nc", 1, 10, "", "error", 0},
		{"nc silent slow failure", "tcp", "nc", 1, 3000, "", "timeout", 0},
		{"nc dns", "tcp", "nc", 1, 10, "nc: getaddrinfo for host \"x.invalid\" port 22: Name or service not known", "dns", 0},
		{"bash refused", "tcp", "bash", 1, 2, "bash: connect: Connection refused", "refused", 0},
		{"bash open", "tcp", "bash", 0, 2, "", "open", 0},
		{"curl 200", "http", "curl", 0, 40, "200 0.012000", "open", 200},
		{"curl 404 still open", "http", "curl", 0, 40, "404 0.010000", "open", 404},
		{"curl refused", "http", "curl", 7, 5, "curl: (7) Failed to connect to 10.0.0.5 port 443 after 1 ms: Couldn't connect to server000 0.000000", "error", 0},
		{"curl refused text", "http", "curl", 7, 5, "curl: (7) Failed to connect to 10.0.0.5 port 443: Connection refused000 0.000000", "refused", 0},
		{"curl timeout", "http", "curl", 28, 3001, "curl: (28) Connection timed out after 3001 milliseconds000 0.000000", "timeout", 0},
		{"curl connected but slow reply is open", "http", "curl", 28, 6000, "curl: (28) Operation timed out after 6000 milliseconds with 0 bytes received000 0.010000", "open", 0},
		{"curl dns", "http", "curl", 6, 20, "curl: (6) Could not resolve host: x.invalid000 0.000000", "dns", 0},
		{"curl tls", "http", "curl", 60, 80, "curl: (60) SSL certificate problem: self-signed certificate000 0.011000", "tls", 0},
		{"curl handshake", "http", "curl", 35, 80, "curl: (35) OpenSSL SSL_connect: Connection reset by peer in connection to x:443 000 0.011000", "tls", 0},
		{"curl empty reply is open", "http", "curl", 52, 80, "curl: (52) Empty reply from server000 0.010000", "open", 0},
		{"curl unreachable", "http", "curl", 7, 30, "curl: (7) Failed to connect to 10.0.0.5 port 80: No route to host000 0.000000", "unreachable", 0},
		{"wget ok", "http", "wget", 0, 30, "HTTP/1.1 200 OK", "open", 200},
		{"wget 403", "http", "wget", 1, 30, "wget: server returned error: HTTP/1.1 403 Forbidden", "open", 403},
		{"wget no tls", "http", "wget", 1, 5, "wget: note: TLS certificate validation not implemented ssl_client: not found wget: error getting response", "error", 0},
		{"wget bad address", "http", "wget", 1, 5, "wget: bad address 'x.invalid'", "dns", 0},
		{"wget timeout", "http", "wget", 1, 3000, "wget: download timed out", "timeout", 0},
		{"http via nc says tcp only", "http", "nc", 0, 5, "", "open", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, code, detail := classifyProbe(tc.mode, tc.tool, tc.rc, tc.ms, 3000, tc.msg)
			if st != tc.state || code != tc.http {
				t.Fatalf("got %s/%d (%s), want %s/%d", st, code, detail, tc.state, tc.http)
			}
		})
	}
}

func TestParseProbeOutput(t *testing.T) {
	out := "noise\n@@tools|nc curl \n@@r|0|nc|0|12|ok\n@@r|x|bad\n@@r|1|curl|7|3|curl: (7) boom|with pipe\r\n@@r|2|nc|1|9\n"
	lines, tools, ended := parseProbeOutput(out)
	if tools != "nc curl" || ended || len(lines) != 3 {
		t.Fatalf("tools=%q ended=%v lines=%+v", tools, ended, lines)
	}
	if lines[1].Msg != "curl: (7) boom|with pipe" || lines[2].Msg != "" || lines[0].MS != 12 {
		t.Fatalf("bad parse: %+v", lines)
	}
	if _, _, e := parseProbeOutput("@@end\n"); !e {
		t.Fatal("@@end not detected")
	}
}

func TestConnBatches(t *testing.T) {
	var cs []ConnCheck
	for i := 0; i < 600; i++ {
		cs = append(cs, ConnCheck{ID: i, Mode: "tcp", Host: "172.18.100.200", Port: 22})
	}
	bs := connBatches(cs)
	n := 0
	for _, b := range bs {
		size := 0
		for _, c := range b {
			size += len(connArg(c)) + 8
		}
		if size > connBatchBytes+60 {
			t.Fatalf("batch too large: %d", size)
		}
		n += len(b)
	}
	if n != 600 || len(bs) < 2 {
		t.Fatalf("batches=%d total=%d", len(bs), n)
	}
}

// Runs the real probe script with /bin/sh against local sockets.
func TestProbeScriptLocal(t *testing.T) {
	have := false
	for _, tool := range []string{"nc", "curl", "bash"} {
		if _, err := exec.LookPath(tool); err == nil {
			have = true
		}
	}
	if !have {
		t.Skip("no nc, curl or bash on this machine")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	open := ln.Addr().(*net.TCPAddr).Port
	cl, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := cl.Addr().(*net.TCPAddr).Port
	cl.Close()
	args := []string{"-c", probeScript, "sh", "2", "4",
		"0|tcp|127.0.0.1|" + strconv.Itoa(open) + "|",
		"1|tcp|127.0.0.1|" + strconv.Itoa(closed) + "|",
		"2|http|127.0.0.1|" + strconv.Itoa(open) + "|http://127.0.0.1:" + strconv.Itoa(open) + "/",
	}
	out, err := exec.Command("sh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	lines, tools, ended := parseProbeOutput(string(out))
	if !ended || tools == "" || len(lines) != 3 {
		t.Fatalf("unexpected output (tools=%q ended=%v):\n%s", tools, ended, out)
	}
	want := map[int]string{0: "open", 1: "refused", 2: "open"}
	for _, l := range lines {
		st, _, d := classifyProbe(map[int]string{0: "tcp", 1: "tcp", 2: "http"}[l.ID], l.Tool, l.RC, l.MS, 2000, l.Msg)
		if st != want[l.ID] {
			t.Errorf("check %d (%s rc=%d msg=%q): got %s (%s), want %s", l.ID, l.Tool, l.RC, l.Msg, st, d, want[l.ID])
		}
	}
	// hostile-looking input is just data
	out, _ = exec.Command("sh", "-c", probeScript, "sh", "1", "1", "0|tcp|127.0.0.1;touch${IFS}/tmp/pwned|1|").CombinedOutput()
	if _, err := exec.LookPath("true"); err == nil {
		if o, _ := exec.Command("sh", "-c", "test -e /tmp/pwned && echo yes").Output(); strings.TrimSpace(string(o)) == "yes" {
			t.Fatal("shell injection through a target argument")
		}
	}
}

// ---------------------------------------------------------------- policies

func mkPod(ns, name, ip string, lbl map[string]string) *Pod {
	p := &Pod{}
	p.Metadata.Name, p.Metadata.Namespace, p.Metadata.Labels = name, ns, lbl
	p.Status.PodIP = ip
	p.Status.Phase = "Running"
	return p
}

func mustJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNetworkPolicyEgress(t *testing.T) {
	src := mkPod("pay", "api-1", "10.1.0.5", map[string]string{"app": "api"})
	db := mkPod("data", "db-0", "10.2.0.9", map[string]string{"app": "db"})
	env := &policyEnv{ns: "pay", podsByIP: map[string]*Pod{db.Status.PodIP: db, src.Status.PodIP: src},
		nodeIPs:  map[string]bool{"192.168.1.10": true},
		nsLabels: map[string]map[string]string{"data": {"team": "data"}, "pay": {}}}
	env.netpols = []NetPol{mustJSON[NetPol](t, `{"metadata":{"name":"api-egress","namespace":"pay"},"spec":{"podSelector":{"matchLabels":{"app":"api"}},"policyTypes":["Egress"],"egress":[
		{"to":[{"ipBlock":{"cidr":"172.18.1.0/24","except":["172.18.1.40/32"]}}],"ports":[{"protocol":"TCP","port":22,"endPort":25}]},
		{"to":[{"namespaceSelector":{"matchLabels":{"team":"data"}},"podSelector":{"matchLabels":{"app":"db"}}}],"ports":[{"port":5432}]},
		{"ports":[{"protocol":"UDP","port":53},{"protocol":"TCP","port":53}]}]}}`),
		mustJSON[NetPol](t, `{"metadata":{"name":"ingress-only","namespace":"pay"},"spec":{"podSelector":{},"policyTypes":["Ingress"]}}`)}
	pp := env.selectFor(src)
	if len(pp.names) != 1 || pp.names[0] != "NetworkPolicy/api-egress" {
		t.Fatalf("selected: %v", pp.names)
	}
	check := func(host string, port int, want string) {
		t.Helper()
		v := env.evaluate(pp, policyTarget(host, port, host+":"+strconv.Itoa(port)))
		if v.State != want {
			t.Errorf("%s:%d = %s (%s), want %s", host, port, v.State, v.Why, want)
		}
	}
	check("172.18.1.33", 22, "allowed")
	check("172.18.1.33", 25, "allowed")
	check("172.18.1.33", 26, "blocked")    // port outside endPort
	check("172.18.1.40", 22, "blocked")    // in except
	check("172.18.2.1", 22, "blocked")     // other subnet
	check("10.2.0.9", 5432, "allowed")     // selector peer: known pod in matching namespace
	check("10.2.0.9", 22, "blocked")       // wrong port for that rule
	check("8.8.8.8", 53, "allowed")        // DNS rule has no "to"
	check("db.example.com", 22, "unknown") // ipBlock cannot be evaluated for a hostname
	// a pod nobody selects has no restriction
	free := mkPod("pay", "web-1", "10.1.0.6", map[string]string{"app": "web"})
	if v := env.evaluate(env.selectFor(free), policyTarget("1.1.1.1", 443, "x")); v.State != "none" {
		t.Errorf("unselected pod: %s", v.State)
	}
	// host-network pods are not covered
	hn := mkPod("pay", "agent", "192.168.1.10", map[string]string{"app": "api"})
	hn.Spec.HostNetwork = true
	if v := env.evaluate(env.selectFor(hn), policyTarget("1.1.1.1", 443, "x")); v.State != "none" {
		t.Errorf("host network pod: %s", v.State)
	}
	// default deny: Egress type with no rules blocks everything
	env.netpols = []NetPol{mustJSON[NetPol](t, `{"metadata":{"name":"deny","namespace":"pay"},"spec":{"podSelector":{},"policyTypes":["Egress"]}}`)}
	if v := env.evaluate(env.selectFor(src), policyTarget("1.1.1.1", 443, "x")); v.State != "blocked" {
		t.Errorf("default deny: %s", v.State)
	}
}

func TestCiliumEgress(t *testing.T) {
	src := mkPod("pay", "api-1", "10.1.0.5", map[string]string{"app": "api"})
	db := mkPod("data", "db-0", "10.2.0.9", map[string]string{"app": "db"})
	env := &policyEnv{ns: "pay", podsByIP: map[string]*Pod{db.Status.PodIP: db}, nodeIPs: map[string]bool{"192.168.1.10": true},
		nsLabels: map[string]map[string]string{"data": {"team": "data"}}}
	env.cilium = []CiliumPolicy{
		mustJSON[CiliumPolicy](t, `{"metadata":{"name":"api","namespace":"pay"},"spec":{"endpointSelector":{"matchLabels":{"app":"api"}},"egress":[
			{"toCIDRSet":[{"cidr":"172.18.0.0/16","except":["172.18.9.0/24"]}],"toPorts":[{"ports":[{"port":"22","protocol":"TCP"}]}]},
			{"toEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"data","app":"db"}}],"toPorts":[{"ports":[{"port":"5432"}]}]},
			{"toFQDNs":[{"matchName":"api.example.com"},{"matchPattern":"*.corp.example.com"}]},
			{"toEntities":["kube-apiserver"]}]}}`),
		mustJSON[CiliumPolicy](t, `{"metadata":{"name":"deny-ssh","namespace":"pay"},"spec":{"endpointSelector":{},"egressDeny":[{"toCIDR":["172.18.5.0/24"],"toPorts":[{"ports":[{"port":"22"}]}]}]}}`),
	}
	pp := env.selectFor(src)
	if len(pp.names) != 2 {
		t.Fatalf("selected: %v", pp.names)
	}
	check := func(host string, port int, want string) {
		t.Helper()
		v := env.evaluate(pp, policyTarget(host, port, host))
		if v.State != want {
			t.Errorf("%s:%d = %s (%s), want %s", host, port, v.State, v.Why, want)
		}
	}
	check("172.18.1.5", 22, "allowed")
	check("172.18.1.5", 23, "unknown") // the kube-apiserver entity rule and FQDN rules cannot be ruled out for a bare IP
	check("172.18.5.5", 22, "blocked") // explicit deny wins
	check("172.18.9.9", 22, "unknown")
	check("10.2.0.9", 5432, "allowed")
	check("api.example.com", 443, "allowed")
	check("x.corp.example.com", 443, "allowed")
	check("evil.example.org", 443, "unknown")
}

func TestCiliumOnlyDenyDoesNotRestrict(t *testing.T) {
	src := mkPod("pay", "api-1", "10.1.0.5", nil)
	env := &policyEnv{ns: "pay", podsByIP: map[string]*Pod{}, nodeIPs: map[string]bool{}}
	env.cilium = []CiliumPolicy{mustJSON[CiliumPolicy](t, `{"metadata":{"name":"d","namespace":"pay"},"spec":{"endpointSelector":{},"egressDeny":[{"toCIDR":["10.9.0.0/16"]}]}}`)}
	pp := env.selectFor(src)
	if v := env.evaluate(pp, policyTarget("1.1.1.1", 443, "x")); v.State != "none" {
		t.Errorf("deny-only policy should not default-deny: %s", v.State)
	}
	if v := env.evaluate(pp, policyTarget("10.9.1.1", 443, "x")); v.State != "blocked" {
		t.Errorf("explicit deny: %s", v.State)
	}
}
