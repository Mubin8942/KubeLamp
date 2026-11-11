package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTraefikHosts(t *testing.T) {
	got := traefikHosts("Host(`a.example.com`, `b.example.com`) && PathPrefix(`/api`) || HostRegexp(`{sub:[a-z]+}.example.org`)")
	want := []string{"a.example.com", "b.example.com", "{sub:[a-z]+}.example.org"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestIngressRouteView(t *testing.T) {
	var ir traefikIR
	js := `{"metadata":{"name":"web","namespace":"apps"},"spec":{"entryPoints":["websecure"],
	  "routes":[{"match":"Host(` + "`shop.example.com`" + `) && PathPrefix(` + "`/api`" + `)","services":[{"name":"api","port":8080,"weight":3},{"name":"api-canary","port":"http","namespace":"canary"}],
	  "middlewares":[{"name":"strip"},{"name":"auth","namespace":"infra"}]}],"tls":{"certResolver":"le"}}}`
	if err := json.Unmarshal([]byte(js), &ir); err != nil {
		t.Fatal(err)
	}
	v := ir.view("traefik.io")
	if v.Hosts[0] != "shop.example.com" || v.TLS != "resolver le" || v.Entry[0] != "websecure" {
		t.Fatalf("bad view %+v", v)
	}
	r := v.Rules[0]
	if r.Backends[0].Port != "8080" || *r.Backends[0].Weight != 3 || r.Backends[1].Port != "http" || r.Backends[1].Namespace != "canary" {
		t.Fatalf("bad backends %+v", r.Backends)
	}
	if !reflect.DeepEqual(r.Middlewares, []string{"strip", "infra/auth"}) {
		t.Fatalf("bad middlewares %v", r.Middlewares)
	}
}

func TestHTTPRouteView(t *testing.T) {
	var hr gwHTTPRoute
	js := `{"metadata":{"name":"r","namespace":"apps"},"spec":{"parentRefs":[{"name":"gw","namespace":"infra","sectionName":"https"}],
	  "hostnames":["a.example.com"],"rules":[{"matches":[{"path":{"type":"PathPrefix","value":"/v1"},"method":"GET"}],
	  "backendRefs":[{"name":"svc","port":80}],"filters":[{"type":"RequestHeaderModifier"}]},{"backendRefs":[{"name":"default","port":8080}]}]},
	  "status":{"parents":[{"parentRef":{"name":"gw"},"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"False","reason":"BackendNotFound"}]}]}}`
	if err := json.Unmarshal([]byte(js), &hr); err != nil {
		t.Fatal(err)
	}
	v := hr.view("gateway.networking.k8s.io")
	if v.Entry[0] != "infra/gw · https" || v.Rules[0].Match != "PathPrefix /v1 GET" || v.Rules[1].Match != "PathPrefix /" {
		t.Fatalf("bad view %+v", v)
	}
	if v.Status != "critical" || v.Rules[0].Middlewares[0] != "RequestHeaderModifier" {
		t.Fatalf("bad status %q %v", v.Status, v.Rules[0].Middlewares)
	}
}

func TestIngressView(t *testing.T) {
	var in netIngress
	js := `{"metadata":{"name":"i","namespace":"apps"},"spec":{"ingressClassName":"nginx","tls":[{"secretName":"tls"}],
	  "rules":[{"host":"x.example.com","http":{"paths":[{"path":"/","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]},
	  "status":{"loadBalancer":{"ingress":[{"ip":"10.0.0.1"}]}}}`
	if err := json.Unmarshal([]byte(js), &in); err != nil {
		t.Fatal(err)
	}
	v := in.view()
	if v.Rules[0].Match != "x.example.com/" || v.Rules[0].Backends[0].Port != "80" || v.Status != "good" || v.TLS != "secret tls" {
		t.Fatalf("bad view %+v", v)
	}
}

func TestCheckUploadedKubeconfig(t *testing.T) {
	ok := "contexts: [{name: a, context: {cluster: c, user: u}}]\nclusters: [{name: c, cluster: {server: https://x}}]\nusers: [{name: u, user: {token: t}}]\n"
	if _, err := checkUploadedKubeconfig([]byte(ok), false); err != nil {
		t.Fatalf("plain token config rejected: %v", err)
	}
	if _, err := checkUploadedKubeconfig([]byte("users: []\n"), true); err == nil {
		t.Fatal("config without contexts accepted")
	}
	bad := map[string]string{
		"tokenFile": "users: [{name: u, user: {tokenFile: /data/token}}]",
		"cert file": "users: [{name: u, user: {client-key: /etc/key}}]",
		"ca file":   "clusters: [{name: c, cluster: {certificate-authority: /etc/ca}}]",
		"exec sh":   "users: [{name: u, user: {exec: {command: sh, args: [-c, id]}}}]",
		"exec path": "users: [{name: u, user: {exec: {command: /tmp/aws}}}]",
	}
	for name, extra := range bad {
		y := "contexts: [{name: a, context: {cluster: c, user: u}}]\n" + extra + "\n"
		if _, err := checkUploadedKubeconfig([]byte(y), false); err == nil {
			t.Errorf("%s: accepted from a remote browser", name)
		}
		if _, err := checkUploadedKubeconfig([]byte(y), true); err != nil {
			t.Errorf("%s: rejected on localhost: %v", name, err)
		}
	}
	aws := "contexts: [{name: a, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {exec: {command: aws, args: [eks, get-token]}}}]\n"
	if _, err := checkUploadedKubeconfig([]byte(aws), false); err != nil {
		t.Fatalf("aws exec rejected: %v", err)
	}
}
