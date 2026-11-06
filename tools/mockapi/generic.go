package main

// Generic resource handling for the mock: discovery, kubectl-style Table
// responses, get/put/delete/patch, and a fake shell behind pod exec.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"kubelamp/internal/ws"
)

type resType struct {
	group, version, name, kind string
	namespaced                 bool
	items                      *[]obj
}

func (c *cluster) initGeneric() {
	mk := func(items ...obj) *[]obj { s := items; return &s }
	meta := func(ns, name string, extra obj) obj {
		m := obj{"name": name, "namespace": ns, "uid": hash(ns + name), "resourceVersion": "1",
			"creationTimestamp": ts(start.Add(-time.Duration(len(name)*37) * time.Hour)),
			"labels":            obj{"app.kubernetes.io/part-of": ns}}
		for k, v := range extra {
			m[k] = v
		}
		if ns == "" {
			delete(m, "namespace")
		}
		return m
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	c.types = []*resType{
		{"", "v1", "configmaps", "ConfigMap", true, mk(
			obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("payments", "payments-api-config", nil),
				"data": obj{"LOG_LEVEL": "info", "DB_POOL_SIZE": "20", "FEATURE_NEW_CHECKOUT": "true",
					"application.yaml": "server:\n  port: 8080\nspring:\n  datasource:\n    url: jdbc:postgresql://ledger-db:5432/payments\n"}},
			obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("ott-streaming", "binge-catalog-config", nil),
				"data": obj{"CDN_BASE": "https://cdn.binge.example", "CACHE_TTL": "300"}},
			obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("ecrm", "ecrm-settings", nil), "data": obj{"TZ": "Asia/Dhaka"}},
		)},
		{"", "v1", "secrets", "Secret", true, mk(
			obj{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta("payments", "payments-db-credentials", nil),
				"data": obj{"username": b64("payments_app"), "password": b64("s3cr3t-Pa55!")}},
			obj{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls", "metadata": meta("ingress", "wildcard-example-tls", nil),
				"data": obj{"tls.crt": b64("-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----\n"), "tls.key": b64("-----BEGIN PRIVATE KEY-----\n...\n")}},
			obj{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta("ecrm", "ecrm-api-keys", nil), "data": obj{"SMS_API_KEY": b64("abc123")}},
		)},
		{"", "v1", "services", "Service", true, mk(
			obj{"apiVersion": "v1", "kind": "Service", "metadata": meta("payments", "payments-api", nil),
				"spec": obj{"type": "ClusterIP", "clusterIP": "10.43.18.20", "ports": []obj{{"name": "http", "port": 80, "targetPort": 8080, "protocol": "TCP"}},
					"selector": obj{"app.kubernetes.io/name": "payments-api"}}},
			obj{"apiVersion": "v1", "kind": "Service", "metadata": meta("ingress", "traefik", nil),
				"spec":   obj{"type": "LoadBalancer", "clusterIP": "10.43.0.10", "ports": []obj{{"name": "websecure", "port": 443, "protocol": "TCP"}}},
				"status": obj{"loadBalancer": obj{"ingress": []obj{{"ip": "10.20.30.200"}}}}},
			obj{"apiVersion": "v1", "kind": "Service", "metadata": meta("ecrm", "ecrm-backend", nil),
				"spec": obj{"type": "ClusterIP", "clusterIP": "10.43.77.4", "ports": []obj{{"port": 8080, "protocol": "TCP"}}}},
		)},
		{"", "v1", "serviceaccounts", "ServiceAccount", true, mk(
			obj{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": meta("payments", "payments-api", nil)},
			obj{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": meta("ecrm", "default", nil)},
		)},
		{"", "v1", "persistentvolumeclaims", "PersistentVolumeClaim", true, mk(
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("payments", "redis-data-redis-0", nil),
				"spec":   obj{"accessModes": []string{"ReadWriteOnce"}, "storageClassName": "ceph-rbd", "resources": obj{"requests": obj{"storage": "10Gi"}}},
				"status": obj{"phase": "Bound", "capacity": obj{"storage": "10Gi"}}},
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("payments", "api-data", nil),
				"spec":   obj{"accessModes": []string{"ReadWriteMany"}, "storageClassName": "cephfs-retain", "resources": obj{"requests": obj{"storage": "10Gi"}}},
				"status": obj{"phase": "Bound", "capacity": obj{"storage": "10Gi"}}},
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("payments", "ledger-data", nil),
				"spec":   obj{"accessModes": []string{"ReadWriteMany"}, "storageClassName": "cephfs-retain", "resources": obj{"requests": obj{"storage": "20Gi"}}},
				"status": obj{"phase": "Bound", "capacity": obj{"storage": "20Gi"}}},
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("payments", "ledger-cache", nil),
				"spec":   obj{"accessModes": []string{"ReadWriteMany"}, "storageClassName": "cephfs-retain", "resources": obj{"requests": obj{"storage": "5Gi"}}},
				"status": obj{"phase": "Bound", "capacity": obj{"storage": "5Gi"}}},
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("payments", "old-export", nil),
				"spec":   obj{"accessModes": []string{"ReadWriteMany"}, "storageClassName": "cephfs-retain", "resources": obj{"requests": obj{"storage": "50Gi"}}},
				"status": obj{"phase": "Bound", "capacity": obj{"storage": "50Gi"}}},
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("ecrm", "ecrm-uploads", nil),
				"spec":   obj{"accessModes": []string{"ReadWriteMany"}, "storageClassName": "cephfs-retain", "resources": obj{"requests": obj{"storage": "5Gi"}}},
				"status": obj{"phase": "Bound", "capacity": obj{"storage": "5Gi"}}},
		)},
		{"networking.k8s.io", "v1", "ingresses", "Ingress", true, mk(
			obj{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": meta("ecrm", "ecrm-frontend", nil),
				"spec": obj{"ingressClassName": "traefik", "rules": []obj{{"host": "ecrm.example.com", "http": obj{"paths": []obj{{"path": "/", "pathType": "Prefix",
					"backend": obj{"service": obj{"name": "ecrm-frontend", "port": obj{"number": 80}}}}}}}}}},
		)},
		{"networking.k8s.io", "v1", "networkpolicies", "NetworkPolicy", true, mk(
			obj{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta("payments", "default-deny", nil),
				"spec": obj{"podSelector": obj{}, "policyTypes": []string{"Ingress"}}},
			obj{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta("payments", "api-egress", nil),
				"spec": obj{"podSelector": obj{"matchLabels": obj{"app.kubernetes.io/name": "payments-api"}}, "policyTypes": []string{"Egress"},
					"egress": []obj{
						{"to": []obj{{"ipBlock": obj{"cidr": "172.18.1.0/24", "except": []string{"172.18.1.40/32"}}}}, "ports": []obj{{"protocol": "TCP", "port": 22, "endPort": 25}}},
						{"ports": []obj{{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}}},
					}}},
		)},
		{"gateway.networking.k8s.io", "v1", "gateways", "Gateway", true, mk(
			obj{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": meta("ingress", "public-gw", nil),
				"spec":   obj{"gatewayClassName": "traefik", "listeners": []obj{{"name": "https", "port": 443, "protocol": "HTTPS", "hostname": "*.example.com"}}},
				"status": obj{"addresses": []obj{{"type": "IPAddress", "value": "10.20.30.200"}}, "conditions": []obj{{"type": "Programmed", "status": "True"}}}},
		)},
		{"gateway.networking.k8s.io", "v1", "httproutes", "HTTPRoute", true, mk(
			obj{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": meta("payments", "payments-api", nil),
				"spec": obj{"parentRefs": []obj{{"name": "public-gw", "namespace": "ingress"}}, "hostnames": []string{"pay.example.com"},
					"rules": []obj{{"backendRefs": []obj{{"name": "payments-api", "port": 80}}}}}},
			obj{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": meta("ott-streaming", "binge-web", nil),
				"spec": obj{"parentRefs": []obj{{"name": "public-gw", "namespace": "ingress"}}, "hostnames": []string{"binge.example.com", "www.binge.example.com"}}},
		)},
		{"cilium.io", "v2", "ciliumnetworkpolicies", "CiliumNetworkPolicy", true, mk(
			obj{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": meta("payments", "allow-gateway-to-api", nil),
				"spec": obj{"endpointSelector": obj{"matchLabels": obj{"app.kubernetes.io/name": "payments-api"}},
					"ingress": []obj{{"fromEndpoints": []obj{{"matchLabels": obj{"io.kubernetes.pod.namespace": "ingress"}}}, "toPorts": []obj{{"ports": []obj{{"port": "8080", "protocol": "TCP"}}}}}}}},
			obj{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": meta("payments", "api-fqdn-egress", nil),
				"spec": obj{"endpointSelector": obj{"matchLabels": obj{"app.kubernetes.io/name": "payments-api"}},
					"egress": []obj{{"toFQDNs": []obj{{"matchName": "api.example.com"}}, "toPorts": []obj{{"ports": []obj{{"port": "443", "protocol": "TCP"}}}}}}}},
		)},
		{"external-secrets.io", "v1beta1", "externalsecrets", "ExternalSecret", true, mk(
			obj{"apiVersion": "external-secrets.io/v1beta1", "kind": "ExternalSecret", "metadata": meta("payments", "payments-db-credentials", nil),
				"spec": obj{"refreshInterval": "1h", "secretStoreRef": obj{"name": "vault-backend", "kind": "ClusterSecretStore"},
					"target": obj{"name": "payments-db-credentials"}, "dataFrom": []obj{{"extract": obj{"key": "kv/payments/db"}}}},
				"status": obj{"conditions": []obj{{"type": "Ready", "status": "True", "reason": "SecretSynced"}}}},
		)},
		{"batch", "v1", "cronjobs", "CronJob", true, mk(
			obj{"apiVersion": "batch/v1", "kind": "CronJob", "metadata": meta("payments", "settlement-report", nil),
				"spec": obj{"schedule": "0 2 * * *", "suspend": false, "jobTemplate": obj{"spec": obj{}}}},
		)},
		{"batch", "v1", "jobs", "Job", true, mk(
			obj{"apiVersion": "batch/v1", "kind": "Job", "metadata": meta("payments", "settlement-report-29371234", nil),
				"spec": obj{"completions": 1}, "status": obj{"succeeded": 1}},
		)},
		{"autoscaling", "v2", "horizontalpodautoscalers", "HorizontalPodAutoscaler", true, mk(
			obj{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": meta("ott-streaming", "binge-catalog", nil),
				"spec":   obj{"scaleTargetRef": obj{"kind": "Deployment", "name": "binge-catalog"}, "minReplicas": 2, "maxReplicas": 10},
				"status": obj{"currentReplicas": 3, "desiredReplicas": 3}},
		)},
		{"rbac.authorization.k8s.io", "v1", "roles", "Role", true, mk(
			obj{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": meta("payments", "kubelamp-viewer", nil),
				"rules": []obj{{"apiGroups": []string{""}, "resources": []string{"pods", "pods/log"}, "verbs": []string{"get", "list"}}}},
		)},
		{"apps", "v1", "deployments", "Deployment", true, &c.deps},
		{"apps", "v1", "statefulsets", "StatefulSet", true, &c.sts},
		{"apps", "v1", "daemonsets", "DaemonSet", true, &c.ds},
	}
	for _, d := range c.deps {
		d["apiVersion"], d["kind"] = "apps/v1", "Deployment"
		d["metadata"].(obj)["resourceVersion"] = "1"
	}
	for _, d := range c.sts {
		d["apiVersion"], d["kind"] = "apps/v1", "StatefulSet"
	}
	for _, d := range c.ds {
		d["apiVersion"], d["kind"] = "apps/v1", "DaemonSet"
	}
	c.cordoned = map[string]bool{}
}

func (c *cluster) typeOf(group, res string) *resType {
	for _, t := range c.types {
		if t.group == group && t.name == res {
			return t
		}
	}
	return nil
}

// ---------------------------------------------------------------- discovery

func (c *cluster) discovery(w http.ResponseWriter, r *http.Request) bool {
	p := strings.TrimSuffix(r.URL.Path, "/")
	type res struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		Namespaced bool     `json:"namespaced"`
		Verbs      []string `json:"verbs"`
	}
	all := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	switch {
	case p == "/api":
		writeJSON(w, obj{"kind": "APIVersions", "versions": []string{"v1"}})
	case p == "/api/v1":
		core := []res{{"pods", "Pod", true, all}, {"pods/log", "Pod", true, []string{"get"}}, {"pods/exec", "PodExecOptions", true, []string{"create", "get"}},
			{"namespaces", "Namespace", false, all}, {"nodes", "Node", false, all}, {"events", "Event", true, all}, {"resourcequotas", "ResourceQuota", true, all}}
		for _, t := range c.types {
			if t.group == "" {
				core = append(core, res{t.name, t.kind, t.namespaced, all})
			}
		}
		writeJSON(w, obj{"kind": "APIResourceList", "groupVersion": "v1", "resources": core})
	case p == "/apis":
		seen := map[string]bool{}
		var groups []obj
		add := func(g, v string) {
			if !seen[g] {
				seen[g] = true
				groups = append(groups, obj{"name": g, "preferredVersion": obj{"groupVersion": g + "/" + v, "version": v}})
			}
		}
		for _, t := range c.types {
			if t.group != "" {
				add(t.group, t.version)
			}
		}
		add("metrics.k8s.io", "v1beta1")
		writeJSON(w, obj{"kind": "APIGroupList", "groups": groups})
	case strings.HasPrefix(p, "/apis/") && strings.Count(p, "/") == 3:
		parts := strings.Split(p, "/")
		g, v := parts[2], parts[3]
		var list []res
		if g == "metrics.k8s.io" {
			list = []res{{"pods", "PodMetrics", true, []string{"get", "list"}}, {"nodes", "NodeMetrics", false, []string{"get", "list"}}}
		}
		for _, t := range c.types {
			if t.group == g && t.version == v {
				list = append(list, res{t.name, t.kind, t.namespaced, all})
				if t.kind == "Deployment" || t.kind == "StatefulSet" {
					list = append(list, res{t.name + "/scale", "Scale", true, []string{"get", "patch", "update"}})
				}
			}
		}
		if list == nil {
			return false
		}
		writeJSON(w, obj{"kind": "APIResourceList", "groupVersion": g + "/" + v, "resources": list})
	default:
		return false
	}
	return true
}

// ---------------------------------------------------------------- tables

func age(created any) string {
	t, _ := time.Parse(time.RFC3339, fmt.Sprint(created))
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func dig(o obj, path ...string) any {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(obj)
		if !ok {
			if mm, ok2 := cur.(map[string]any); ok2 {
				m = mm
			} else {
				return nil
			}
		}
		cur = m[p]
	}
	return cur
}

// asTable converts a list to a meta.k8s.io Table like the real API server does.
func asTable(kind string, items []obj) obj {
	cols := []obj{{"name": "Name", "type": "string", "priority": 0}}
	type col struct {
		name string
		f    func(obj) any
		prio int
	}
	var extra []col
	switch kind {
	case "Pod":
		extra = []col{{"Ready", func(o obj) any {
			n, r := 0, 0
			for _, s := range asList(dig(o, "status", "containerStatuses")) {
				n++
				if s["ready"] == true {
					r++
				}
			}
			return fmt.Sprintf("%d/%d", r, n)
		}, 0}, {"Status", func(o obj) any {
			for _, s := range asList(dig(o, "status", "containerStatuses")) {
				if w := dig(s, "state", "waiting", "reason"); w != nil {
					return w
				}
			}
			return dig(o, "status", "phase")
		}, 0}, {"Restarts", func(o obj) any {
			n := 0
			for _, s := range asList(dig(o, "status", "containerStatuses")) {
				n += toInt(s["restartCount"])
			}
			return n
		}, 0}, {"IP", func(o obj) any { return dig(o, "status", "podIP") }, 1}, {"Node", func(o obj) any { return dig(o, "spec", "nodeName") }, 1}}
	case "Deployment":
		extra = []col{{"Ready", func(o obj) any {
			return fmt.Sprintf("%v/%v", or(dig(o, "status", "readyReplicas"), 0), dig(o, "spec", "replicas"))
		}, 0}, {"Up-to-date", func(o obj) any { return dig(o, "status", "updatedReplicas") }, 0},
			{"Available", func(o obj) any { return dig(o, "status", "availableReplicas") }, 0}}
	case "Service":
		extra = []col{{"Type", func(o obj) any { return dig(o, "spec", "type") }, 0}, {"Cluster-IP", func(o obj) any { return dig(o, "spec", "clusterIP") }, 0},
			{"Ports", func(o obj) any {
				var p []string
				for _, x := range asList(dig(o, "spec", "ports")) {
					p = append(p, fmt.Sprintf("%v/%v", x["port"], or(x["protocol"], "TCP")))
				}
				return strings.Join(p, ",")
			}, 0}}
	case "ConfigMap":
		extra = []col{{"Data", func(o obj) any { m, _ := dig(o, "data").(obj); return len(m) }, 0}}
	case "Secret":
		extra = []col{{"Type", func(o obj) any { return o["type"] }, 0}, {"Data", func(o obj) any { m, _ := dig(o, "data").(obj); return len(m) }, 0}}
	case "Gateway":
		extra = []col{{"Class", func(o obj) any { return dig(o, "spec", "gatewayClassName") }, 0},
			{"Address", func(o obj) any {
				a := asList(dig(o, "status", "addresses"))
				if len(a) > 0 {
					return a[0]["value"]
				}
				return ""
			}, 0}, {"Programmed", func(o obj) any { return "True" }, 0}}
	case "HTTPRoute":
		extra = []col{{"Hostnames", func(o obj) any { return fmt.Sprint(dig(o, "spec", "hostnames")) }, 0}}
	case "ExternalSecret":
		extra = []col{{"Store", func(o obj) any { return dig(o, "spec", "secretStoreRef", "name") }, 0},
			{"Refresh Interval", func(o obj) any { return dig(o, "spec", "refreshInterval") }, 0}, {"Status", func(o obj) any { return "SecretSynced" }, 0}}
	case "Node":
		extra = []col{{"Status", func(o obj) any { return "Ready" }, 0}, {"Version", func(o obj) any { return dig(o, "status", "nodeInfo", "kubeletVersion") }, 0}}
	}
	for _, e := range extra {
		cols = append(cols, obj{"name": e.name, "type": "string", "priority": e.prio})
	}
	cols = append(cols, obj{"name": "Age", "type": "string", "priority": 0})
	rows := []obj{}
	for _, it := range items {
		md, _ := it["metadata"].(obj)
		cells := []any{md["name"]}
		for _, e := range extra {
			cells = append(cells, e.f(it))
		}
		cells = append(cells, age(md["creationTimestamp"]))
		rows = append(rows, obj{"cells": cells, "object": obj{"kind": "PartialObjectMetadata", "metadata": md}})
	}
	return obj{"kind": "Table", "apiVersion": "meta.k8s.io/v1", "columnDefinitions": cols, "rows": rows, "metadata": obj{}}
}

func asList(v any) []obj {
	var out []obj
	switch l := v.(type) {
	case []obj:
		return l
	case []any:
		for _, x := range l {
			if m, ok := x.(obj); ok {
				out = append(out, m)
			} else if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func or(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

func wantsTable(r *http.Request) bool { return strings.Contains(r.Header.Get("Accept"), "as=Table") }

// ---------------------------------------------------------------- object routes

type route struct {
	group, version, ns, res, name, sub string
}

func parseRoute(p string) (route, bool) {
	var rt route
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api" && parts[1] == "v1":
		rt.version, parts = "v1", parts[2:]
	case len(parts) >= 4 && parts[0] == "apis":
		rt.group, rt.version, parts = parts[1], parts[2], parts[3:]
	default:
		return rt, false
	}
	if len(parts) >= 2 && parts[0] == "namespaces" && len(parts) >= 3 {
		rt.ns, parts = parts[1], parts[2:]
	}
	if len(parts) == 0 {
		return rt, false
	}
	rt.res = parts[0]
	if len(parts) > 1 {
		rt.name = parts[1]
	}
	if len(parts) > 2 {
		rt.sub = parts[2]
	}
	return rt, true
}

func status(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	writeJSON(w, obj{"kind": "Status", "status": "Failure", "code": code, "message": msg})
}

// generic serves get/list/put/delete/patch for every resType, plus pod get/delete,
// node patch and pod exec. Returns false to fall through to the classic handlers.
func (c *cluster) generic(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/__conntestexecs" {
		writeJSON(w, obj{"count": connExecs.Load()})
		return true
	}
	if r.URL.Path == "/__dfexecs" {
		writeJSON(w, obj{"count": dfExecs.Load()})
		return true
	}
	rt, ok := parseRoute(r.URL.Path)
	if !ok {
		return false
	}
	// pod exec / single pod / pod delete
	if rt.group == "" && rt.res == "pods" && rt.name != "" {
		switch {
		case rt.sub == "exec":
			c.serveExec(w, r, rt.ns, rt.name)
			return true
		case rt.sub == "" && (r.Method == http.MethodGet || r.Method == http.MethodDelete):
			c.mu.Lock()
			defer c.mu.Unlock()
			for i, p := range c.pods {
				if p.ns == rt.ns && p.name == rt.name {
					o := c.podObj(p)
					o["apiVersion"], o["kind"] = "v1", "Pod"
					if r.Method == http.MethodDelete {
						c.pods = append(c.pods[:i], c.pods[i+1:]...)
					}
					writeJSON(w, o)
					return true
				}
			}
			status(w, 404, fmt.Sprintf("pods %q not found", rt.name))
			return true
		}
		return false
	}
	if rt.group == "" && rt.res == "nodes" && rt.name != "" && r.Method == http.MethodPatch {
		var p struct {
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		c.mu.Lock()
		c.cordoned[rt.name] = p.Spec.Unschedulable
		c.mu.Unlock()
		writeJSON(w, obj{"kind": "Node", "metadata": obj{"name": rt.name}})
		return true
	}
	// list tables for classic resources (pods, nodes, namespaces)
	if rt.name == "" && r.Method == http.MethodGet && wantsTable(r) && c.typeOf(rt.group, rt.res) == nil {
		var items []obj
		switch {
		case rt.group == "" && rt.res == "pods":
			c.mu.Lock()
			for _, p := range c.pods {
				if rt.ns == "" || p.ns == rt.ns {
					items = append(items, c.podObj(p))
				}
			}
			c.mu.Unlock()
			writeJSON(w, asTable("Pod", items))
			return true
		}
		return false
	}
	t := c.typeOf(rt.group, rt.res)
	if t == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	find := func() int {
		for i, o := range *t.items {
			md := o["metadata"].(obj)
			if md["name"] == rt.name && (!t.namespaced || md["namespace"] == rt.ns) {
				return i
			}
		}
		return -1
	}
	switch {
	case rt.name == "" && r.Method == http.MethodGet:
		var items []obj
		for _, o := range *t.items {
			if rt.ns == "" || o["metadata"].(obj)["namespace"] == rt.ns {
				items = append(items, o)
			}
		}
		if wantsTable(r) {
			writeJSON(w, asTable(t.kind, items))
		} else {
			writeJSON(w, obj{"kind": t.kind + "List", "metadata": obj{}, "items": items})
		}
	case rt.name != "" && rt.sub == "" && r.Method == http.MethodGet:
		if i := find(); i >= 0 {
			writeJSON(w, (*t.items)[i])
		} else {
			status(w, 404, fmt.Sprintf("%s %q not found", t.name, rt.name))
		}
	case rt.name != "" && r.Method == http.MethodPut:
		i := find()
		if i < 0 {
			status(w, 404, "not found")
			return true
		}
		var o obj
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
			status(w, 400, err.Error())
			return true
		}
		o = normalize(o)
		cur := (*t.items)[i]["metadata"].(obj)
		md, _ := o["metadata"].(obj)
		if md == nil || fmt.Sprint(md["resourceVersion"]) != fmt.Sprint(cur["resourceVersion"]) {
			status(w, 409, fmt.Sprintf("Operation cannot be fulfilled on %s %q: the object has been modified; please apply your changes to the latest version and try again", t.name, rt.name))
			return true
		}
		rv, _ := strconv.Atoi(fmt.Sprint(cur["resourceVersion"]))
		md["resourceVersion"] = strconv.Itoa(rv + 1)
		(*t.items)[i] = o
		writeJSON(w, o)
	case rt.name != "" && r.Method == http.MethodDelete:
		if i := find(); i >= 0 {
			o := (*t.items)[i]
			*t.items = append((*t.items)[:i], (*t.items)[i+1:]...)
			writeJSON(w, o)
		} else {
			status(w, 404, "not found")
		}
	case rt.name != "" && r.Method == http.MethodPatch:
		i := find()
		if i < 0 {
			status(w, 404, "not found")
			return true
		}
		var p obj
		_ = json.NewDecoder(r.Body).Decode(&p)
		p = normalize(p)
		o := (*t.items)[i]
		if rt.sub == "scale" {
			n := toInt(dig(p, "spec", "replicas"))
			o["spec"].(obj)["replicas"] = n
			if st, ok := o["status"].(obj); ok {
				st["replicas"], st["readyReplicas"], st["availableReplicas"], st["updatedReplicas"] = n, n, n, n
			}
			writeJSON(w, obj{"kind": "Scale", "spec": obj{"replicas": n}})
			return true
		}
		if ann := dig(p, "spec", "template", "metadata", "annotations"); ann != nil {
			tpl := o["spec"].(obj)["template"].(obj)
			tm, _ := tpl["metadata"].(obj)
			if tm == nil {
				tm = obj{}
				tpl["metadata"] = tm
			}
			tm["annotations"] = ann
		}
		writeJSON(w, o)
	default:
		return false
	}
	return true
}

// normalize converts decoded JSON maps (map[string]any) into obj recursively.
func normalize(v obj) obj {
	var conv func(any) any
	conv = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			m := obj{}
			for k, v := range t {
				m[k] = conv(v)
			}
			return m
		case []any:
			for i := range t {
				t[i] = conv(t[i])
			}
			return t
		}
		return x
	}
	return conv(map[string]any(v)).(obj)
}

// ---------------------------------------------------------------- fake exec shell

var dfExecs atomic.Int64

// batch df probe: sh -c '<script>' sh path...  (what KubeLamp's PVC usage check sends)
func (c *cluster) serveDfExec(w http.ResponseWriter, r *http.Request, ns, pod string, cmd []string) {
	dfExecs.Add(1)
	conn, _, err := ws.Accept(w, r, "v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io")
	if err != nil {
		return
	}
	defer conn.Close()
	ctr := r.URL.Query().Get("container")
	if ctr == "envoy" { // distroless sidecar: no shell
		b, _ := json.Marshal(obj{"status": "Failure", "message": "OCI runtime exec failed: exec: \"sh\": executable file not found in $PATH"})
		_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
		return
	}
	var deploy string
	c.mu.Lock()
	for _, p := range c.pods {
		if p.ns == ns && p.name == pod {
			deploy = p.deploy
		}
	}
	c.mu.Unlock()
	var sb strings.Builder
	for _, path := range cmd[4:] {
		sb.WriteString("@@" + path + "\n")
		found := false
		for _, pm := range pvcMounts[deploy] {
			if pm.path == path {
				d := pvcDf[pm.claim]
				sb.WriteString(fmt.Sprintf("10.1.2.3:6789:/volumes/csi/%s %d %d %d %d%% %s\n", pm.claim, d[0], d[1], d[0]-d[1], d[1]*100/d[0], path))
				found = true
			}
		}
		if !found {
			sb.WriteString("df: " + path + ": No such file or directory\n")
		}
	}
	_ = conn.WriteMessage(ws.OpBinary, append([]byte{1}, sb.String()...))
	b, _ := json.Marshal(obj{"status": "Success"})
	_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
}

var connExecs atomic.Int64

// fake connectivity probe: sh -c '<probe script>' sh T P id|mode|host|port|url ...
// Behaviour: pods on the 3rd node are firewalled (everything times out); otherwise port 22 is
// open, 23 refused, 24 times out and 25 has no route; *.invalid is a DNS failure; https checks
// of *.example.com return 200. redis pods are distroless (no sh); ledger-worker pods have no tools.
func (c *cluster) serveConnExec(w http.ResponseWriter, r *http.Request, ns, pod string, cmd []string) {
	connExecs.Add(1)
	conn, _, err := ws.Accept(w, r, "v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io")
	if err != nil {
		return
	}
	defer conn.Close()
	var deploy, node string
	c.mu.Lock()
	for _, p := range c.pods {
		if p.ns == ns && p.name == pod {
			deploy, node = p.deploy, p.node
		}
	}
	c.mu.Unlock()
	fail := func(msg string) {
		b, _ := json.Marshal(obj{"status": "Failure", "message": msg})
		_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
	}
	if deploy == "redis" || r.URL.Query().Get("container") == "envoy" {
		fail("OCI runtime exec failed: exec failed: unable to start container process: exec: \"sh\": executable file not found in $PATH: unknown")
		return
	}
	tmo, _ := strconv.Atoi(cmd[4])
	blocked := len(c.nodes) > 2 && node == c.nodes[2]
	var sb strings.Builder
	if deploy == "ledger-worker" {
		sb.WriteString("@@tools|\n")
	} else {
		sb.WriteString("@@tools|nc curl \n")
	}
	for _, a := range cmd[6:] {
		f := strings.SplitN(a, "|", 5)
		id, mode, host := f[0], f[1], f[2]
		port, _ := strconv.Atoi(f[3])
		line := func(tool string, rc, ms int, msg string) {
			sb.WriteString(fmt.Sprintf("@@r|%s|%s|%d|%d|%s\n", id, tool, rc, ms, msg))
		}
		switch {
		case deploy == "ledger-worker":
			line("none", 0, 0, "")
		case strings.HasSuffix(host, ".invalid") || host == "blocked.example":
			if mode == "http" {
				line("curl", 6, 14, "curl: (6) Could not resolve host: "+host+" 000 0.000000")
			} else {
				line("nc", 1, 12, "nc: getaddrinfo for host \""+host+"\" port "+f[3]+": Name or service not known")
			}
		case blocked:
			if mode == "http" {
				line("curl", 28, tmo*1000, "curl: (28) Connection timed out after "+strconv.Itoa(tmo*1000)+" milliseconds000 0.000000")
			} else {
				line("nc", 1, tmo*1000, "nc: connect to "+host+" port "+f[3]+" (tcp) timed out: Operation now in progress")
			}
		case mode == "http":
			line("curl", 0, 40, "200 0.012000")
		case port == 23:
			line("nc", 1, 3, "nc: connect to "+host+" port 23 (tcp) failed: Connection refused")
		case port == 24:
			line("nc", 1, tmo*1000, "nc: connect to "+host+" port 24 (tcp) timed out: Operation now in progress")
		case port == 25:
			line("nc", 1, 21, "nc: connect to "+host+" port 25 (tcp) failed: No route to host")
		default:
			line("nc", 0, 4, "Connection to "+host+" "+f[3]+" port [tcp/*] succeeded!")
		}
	}
	sb.WriteString("@@end\n")
	_ = conn.WriteMessage(ws.OpBinary, append([]byte{1}, sb.String()...))
	b, _ := json.Marshal(obj{"status": "Success"})
	_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
}

func (c *cluster) serveExec(w http.ResponseWriter, r *http.Request, ns, pod string) {
	if cmd := r.URL.Query()["command"]; len(cmd) >= 6 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "# kl-dns") {
		c.serveDNSExec(w, r, ns, pod, cmd)
		return
	}
	if cmd := r.URL.Query()["command"]; len(cmd) >= 4 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "# kl-ft") {
		c.serveFtExec(w, r, ns, pod, cmd)
		return
	}
	if cmd := r.URL.Query()["command"]; len(cmd) >= 7 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "@@r|") {
		c.serveConnExec(w, r, ns, pod, cmd)
		return
	}
	if cmd := r.URL.Query()["command"]; len(cmd) >= 5 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "df -P") {
		c.serveDfExec(w, r, ns, pod, cmd)
		return
	}
	conn, proto, err := ws.Accept(w, r, "v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io")
	if err != nil {
		return
	}
	defer conn.Close()
	_ = proto
	out := func(s string) { _ = conn.WriteMessage(ws.OpBinary, append([]byte{1}, s...)) }
	prompt := func() { out("\x1b[32mroot@" + pod + "\x1b[0m:/app# ") }
	out("Welcome to " + pod + " (" + ns + ")\r\n")
	prompt()
	line := ""
	for {
		op, msg, err := conn.ReadMessage()
		if err != nil || op == ws.OpClose {
			return
		}
		if len(msg) < 1 || msg[0] != 0 {
			continue // resize etc.
		}
		for _, ch := range string(msg[1:]) {
			switch ch {
			case '\r', '\n':
				out("\r\n")
				cmd := strings.TrimSpace(line)
				line = ""
				switch {
				case cmd == "":
				case cmd == "exit":
					b, _ := json.Marshal(obj{"status": "Success"})
					_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
					return
				case cmd == "hostname":
					out(pod + "\r\n")
				case cmd == "whoami":
					out("root\r\n")
				case cmd == "pwd":
					out("/app\r\n")
				case cmd == "ls" || cmd == "ls -la":
					out("app.jar  config  logs  start.sh\r\n")
				case strings.HasPrefix(cmd, "echo "):
					out(strings.TrimPrefix(cmd, "echo ") + "\r\n")
				case cmd == "env":
					out("POD_NAME=" + pod + "\r\nPOD_NAMESPACE=" + ns + "\r\nJAVA_OPTS=-Xmx512m\r\n")
				default:
					out("sh: " + strings.Fields(cmd)[0] + ": not found\r\n")
				}
				prompt()
			case 127, 8:
				if len(line) > 0 {
					line = line[:len(line)-1]
					out("\b \b")
				}
			default:
				line += string(ch)
				out(string(ch))
			}
		}
	}
}

var _ = io.EOF

// ---- file transfer tool: the pod-side scripts really run (with sh on this machine) against a
// directory tree that stands in for the container filesystem. Paths under a PVC mount map to a
// directory shared by every pod mounting that claim; everything else is private to the pod.

var mockRoot string

func seedPVC(dir, claim string) {
	marker := filepath.Join(dir, ".seeded")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	if claim == "ecrm-uploads" {
		w := func(rel, s string) {
			p := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte(s), 0o644)
		}
		w("notes.txt", "hello from the mock volume\n")
		w("images/pixel.bin", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x01")
		w("reports/2026-q3.csv", "id,amount\n1,10\n2,20\n")
		w("images/logo.svg", "<svg xmlns='http://www.w3.org/2000/svg'/>")
		w("odd name & spaces/file with 'quote'.txt", "tricky names\n")
		_ = os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
		log := strings.Repeat("[2026-11-07 03:00:00] production.INFO: request handled {\"ms\":12}\n", 40000) // ~2.7 MB
		w("storage/logs/laravel.log", log+"LAST-LINE-MARKER\n")
		_ = os.Symlink("reports", filepath.Join(dir, "latest"))
	}
	_ = os.WriteFile(marker, nil, 0o644)
}

func (c *cluster) ftMapPath(deploy, ns, pod, arg string) string {
	if !strings.HasPrefix(arg, "/") {
		return arg
	}
	for _, pm := range pvcMounts[deploy] {
		if arg == pm.path || strings.HasPrefix(arg, pm.path+"/") {
			d := filepath.Join(mockRoot, "pvc", pm.claim)
			seedPVC(d, pm.claim)
			return filepath.Join(d, strings.TrimPrefix(arg, pm.path))
		}
	}
	d := filepath.Join(mockRoot, "pods", ns, pod)
	_ = os.MkdirAll(filepath.Join(d, "tmp"), 0o755)
	return filepath.Join(d, arg)
}

func (c *cluster) serveFtExec(w http.ResponseWriter, r *http.Request, ns, pod string, cmd []string) {
	conn, _, err := ws.Accept(w, r, "v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io")
	if err != nil {
		return
	}
	defer conn.Close()
	status := func(m obj) {
		b, _ := json.Marshal(m)
		_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
	}
	if r.URL.Query().Get("container") == "envoy" {
		status(obj{"status": "Failure", "message": "OCI runtime exec failed: exec: \"sh\": executable file not found in $PATH"})
		return
	}
	var deploy string
	c.mu.Lock()
	for _, p := range c.pods {
		if p.ns == ns && p.name == pod {
			deploy = p.deploy
		}
	}
	c.mu.Unlock()
	args := append([]string(nil), cmd[1:]...)
	for i := 3; i < len(args); i++ {
		args[i] = c.ftMapPath(deploy, ns, pod, args[i])
	}
	sh := exec.Command(cmd[0], args...)
	stdin, _ := sh.StdinPipe()
	stdout, _ := sh.StdoutPipe()
	var errb strings.Builder
	sh.Stderr = &errb
	hasStdin := r.URL.Query().Get("stdin") == "true"
	if err := sh.Start(); err != nil {
		status(obj{"status": "Failure", "message": err.Error()})
		return
	}
	go func() {
		defer stdin.Close()
		if !hasStdin {
			return
		}
		for {
			op, msg, err := conn.ReadMessage()
			if err != nil || op == ws.OpClose {
				return
			}
			if len(msg) >= 1 && msg[0] == 0 {
				if _, err := stdin.Write(msg[1:]); err != nil {
					return
				}
			}
			if len(msg) >= 1 && msg[0] == 255 {
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			if conn.WriteMessage(ws.OpBinary, append([]byte{1}, buf[:n]...)) != nil {
				_ = sh.Process.Kill()
				break
			}
		}
		if err != nil {
			break
		}
	}
	werr := sh.Wait()
	if errb.Len() > 0 {
		_ = conn.WriteMessage(ws.OpBinary, append([]byte{2}, errb.String()...))
	}
	if ee, ok := werr.(*exec.ExitError); ok {
		status(obj{"status": "Failure", "reason": "NonZeroExitCode", "message": "command terminated with non-zero exit code",
			"details": obj{"causes": []obj{{"reason": "ExitCode", "message": strconv.Itoa(ee.ExitCode())}}}})
		return
	}
	status(obj{"status": "Success"})
}

// ---- DNS checker. Answers are scripted: api.example.com differs on the 3rd node (a split-horizon
// style problem), slow.example.com times out there, *.invalid and blocked.example don't exist,
// kubernetes.default is the cluster IP, everything else gets a stable fake address. binge-web uses
// nslookup output, ledger-worker has no DNS tool, redis has no shell.
func (c *cluster) serveDNSExec(w http.ResponseWriter, r *http.Request, ns, pod string, cmd []string) {
	conn, _, err := ws.Accept(w, r, "v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io")
	if err != nil {
		return
	}
	defer conn.Close()
	status := func(m obj) {
		b, _ := json.Marshal(m)
		_ = conn.WriteMessage(ws.OpBinary, append([]byte{3}, b...))
	}
	var deploy, node string
	c.mu.Lock()
	for _, p := range c.pods {
		if p.ns == ns && p.name == pod {
			deploy, node = p.deploy, p.node
		}
	}
	c.mu.Unlock()
	if deploy == "redis" || r.URL.Query().Get("container") == "envoy" {
		status(obj{"status": "Failure", "message": "OCI runtime exec failed: exec: \"sh\": executable file not found in $PATH"})
		return
	}
	srv := cmd[5]
	names := cmd[6:]
	split := len(c.nodes) > 2 && node == c.nodes[2]
	var sb strings.Builder
	tool := "getent"
	switch deploy {
	case "binge-web":
		tool = "nslookup"
	case "ledger-worker":
		tool = "none"
	}
	if tool == "none" {
		sb.WriteString("@@tools| timeout\n")
	} else {
		sb.WriteString("@@tools| getent nslookup timeout\n")
	}
	sb.WriteString("@@resolv|nameserver 10.96.0.10\n@@resolv|search " + ns + ".svc.cluster.local svc.cluster.local cluster.local\n@@resolv|options ndots:5\n")
	for i, n := range names {
		id := i + 1
		var ips []string
		rc, text := 0, ""
		switch {
		case tool == "none":
		case srv != "" && srv != "10.96.0.10":
			rc, text = 124, ";; connection timed out; no servers could be reached"
		case strings.HasSuffix(n, ".invalid") || n == "blocked.example":
			rc, text = 2, "** server can't find "+n+": NXDOMAIN"
		case n == "slow.example.com" && split:
			rc, text = 124, ""
		case n == "kubernetes.default" || n == "kubernetes.default.svc.cluster.local":
			ips = []string{"10.96.0.1"}
		case n == "api.example.com" && split:
			ips = []string{"10.1.9.9"}
		case n == "api.example.com":
			ips = []string{"10.1.1.5", "10.1.1.6"}
		default:
			h := 0
			for _, ch := range n {
				h = (h*31 + int(ch)) % 65000
			}
			ips = []string{fmt.Sprintf("10.50.%d.%d", h/250%250, h%250+1)}
		}
		fmt.Fprintf(&sb, "@@d|%d|%s|%d\n", id, tool, rc)
		switch {
		case tool == "getent":
			for _, ip := range ips {
				fmt.Fprintf(&sb, "@@o|%d|%s       STREAM %s\n@@o|%d|%s       DGRAM\n", id, ip, n, id, ip)
			}
			if text != "" {
				fmt.Fprintf(&sb, "@@o|%d|%s\n", id, text)
			}
		case tool == "nslookup":
			fmt.Fprintf(&sb, "@@o|%d|Server:\t\t10.96.0.10\n@@o|%d|Address:\t10.96.0.10:53\n@@o|%d|\n", id, id, id)
			if len(ips) > 0 {
				fmt.Fprintf(&sb, "@@o|%d|Name:\t%s\n", id, n)
				for _, ip := range ips {
					fmt.Fprintf(&sb, "@@o|%d|Address: %s\n", id, ip)
				}
			} else {
				fmt.Fprintf(&sb, "@@o|%d|%s\n", id, "** server can't find "+n+": NXDOMAIN")
			}
		}
	}
	_ = conn.WriteMessage(ws.OpBinary, append([]byte{1}, sb.String()...))
	status(obj{"status": "Success"})
}
