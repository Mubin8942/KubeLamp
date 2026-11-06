// mockapi is a tiny fake Kubernetes API server used to test KubeLamp without a
// real cluster. It serves realistic objects, metrics that move over time, and
// streaming pod logs, behind real TLS with client-certificate or bearer auth.
//
//	go run ./tools/mockapi -dir /tmp/mock     # writes /tmp/mock/kubeconfig
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var start = time.Now()

type obj = map[string]any

type pod struct {
	ns, name, deploy, kind, node, status string
	labels                               map[string]string
	containers                           []string
	cpuBase, memBase                     float64 // millicores, MiB
	reqCPU, reqMem, limCPU, limMem       float64
	restarts                             int
	created                              time.Time
	ready                                bool
}

type cluster struct {
	mu               sync.Mutex
	name             string
	nodes            []string
	nodeCPU, nodeMem float64
	pods             []*pod
	deps             []obj
	sts              []obj
	ds               []obj
	nss              []string
	events           []obj
	rng              *mrand.Rand
	types            []*resType
	cordoned         map[string]bool
}

func hash(s string) string {
	h := 0
	for _, c := range s {
		h = h*31 + int(c)
	}
	const al = "bcdfghjklmnpqrstvwxz2456789"
	out := ""
	for i := 0; i < 10; i++ {
		out += string(al[(h>>uint(i*3))&0xffff%len(al)])
		h = h*7 + 13
		if h < 0 {
			h = -h
		}
	}
	return out
}

func suffix(r *mrand.Rand) string {
	const al = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = al[r.Intn(len(al))]
	}
	return string(b)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func newCluster(name string, nodes int, seed int64, big bool) *cluster {
	c := &cluster{name: name, rng: mrand.New(mrand.NewSource(seed)), nodeCPU: 8, nodeMem: 32}
	for i := 0; i < nodes; i++ {
		role := "worker"
		if i == 0 {
			role = "control-plane"
		}
		c.nodes = append(c.nodes, fmt.Sprintf("%s-%s-%02d", name, role, i+1))
	}
	c.nss = []string{"default", "kube-system", "monitoring", "payments", "ott-streaming", "ecrm", "ingress"}
	type spec struct {
		ns, name   string
		replicas   int
		containers []string
		cpu, mem   float64
		req        [2]float64
		image      string
	}
	specs := []spec{
		{"payments", "payments-api", 4, []string{"api", "envoy"}, 220, 410, [2]float64{250, 512}, "registry.example.local/payments/api:2.14.1"},
		{"payments", "payment-gateway", 3, []string{"gateway"}, 140, 300, [2]float64{200, 384}, "registry.example.local/payments/gateway:1.9.0"},
		{"payments", "ledger-worker", 2, []string{"worker"}, 90, 620, [2]float64{100, 768}, "registry.example.local/payments/ledger:3.2.0"},
		{"ott-streaming", "binge-catalog", 3, []string{"catalog"}, 310, 700, [2]float64{300, 1024}, "registry.example.local/ott/catalog:5.0.3"},
		{"ott-streaming", "binge-transcoder", 2, []string{"transcoder"}, 1300, 1800, [2]float64{1000, 2048}, "registry.example.local/ott/transcoder:2.1.7"},
		{"ott-streaming", "binge-web", 4, []string{"nginx"}, 40, 90, [2]float64{50, 128}, "nginx:1.27-alpine"},
		{"ecrm", "ecrm-backend", 2, []string{"app"}, 180, 900, [2]float64{250, 1024}, "registry.example.local/ecrm/backend:4.4.0"},
		{"ecrm", "ecrm-frontend", 2, []string{"web"}, 25, 80, [2]float64{50, 128}, "registry.example.local/ecrm/frontend:4.4.0"},
		{"monitoring", "grafana", 1, []string{"grafana"}, 60, 210, [2]float64{100, 256}, "grafana/grafana:11.3.0"},
		{"monitoring", "kube-state-metrics", 1, []string{"kube-state-metrics"}, 20, 70, [2]float64{50, 128}, "registry.k8s.io/kube-state-metrics:v2.13.0"},
		{"kube-system", "coredns", 2, []string{"coredns"}, 8, 30, [2]float64{100, 70}, "registry.k8s.io/coredns/coredns:v1.11.3"},
		{"kube-system", "metrics-server", 1, []string{"metrics-server"}, 6, 25, [2]float64{100, 200}, "registry.k8s.io/metrics-server/metrics-server:v0.7.2"},
		{"ingress", "traefik", 2, []string{"traefik"}, 70, 120, [2]float64{100, 128}, "traefik:v3.2"},
		{"default", "notifier", 1, []string{"notifier"}, 10, 60, [2]float64{50, 64}, "registry.example.local/tools/notifier:0.3.1"},
	}
	if !big {
		specs = specs[:9]
	}
	for _, s := range specs {
		labels := map[string]string{"app.kubernetes.io/name": s.name, "app.kubernetes.io/part-of": s.ns}
		h := hash(s.name)[:10]
		var ctrs []obj
		for _, ct := range s.containers {
			ctrs = append(ctrs, obj{"name": ct, "image": s.image, "resources": obj{
				"requests": obj{"cpu": fmt.Sprintf("%dm", int(s.req[0])), "memory": fmt.Sprintf("%dMi", int(s.req[1]))},
				"limits":   obj{"cpu": fmt.Sprintf("%dm", int(s.req[0]*2)), "memory": fmt.Sprintf("%dMi", int(s.req[1]*2))},
			}})
		}
		created := start.Add(-time.Duration(c.rng.Intn(60*24)+30) * time.Hour)
		ready := s.replicas
		degraded := s.name == "ledger-worker"
		if degraded {
			ready = 1
		}
		cond := []obj{{"type": "Available", "status": "True", "reason": "MinimumReplicasAvailable"},
			{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable"}}
		c.deps = append(c.deps, obj{
			"metadata": obj{"name": s.name, "namespace": s.ns, "uid": hash(s.ns + s.name), "creationTimestamp": ts(created), "labels": labels, "generation": 3},
			"spec": obj{"replicas": s.replicas, "selector": obj{"matchLabels": map[string]string{"app.kubernetes.io/name": s.name}},
				"strategy": obj{"type": "RollingUpdate"},
				"template": obj{"metadata": obj{"labels": labels}, "spec": obj{"containers": ctrs}}},
			"status": obj{"observedGeneration": 3, "replicas": s.replicas, "readyReplicas": ready, "updatedReplicas": s.replicas,
				"availableReplicas": ready, "unavailableReplicas": s.replicas - ready, "conditions": cond},
		})
		for i := 0; i < s.replicas; i++ {
			pl := map[string]string{"pod-template-hash": h}
			for k, v := range labels {
				pl[k] = v
			}
			p := &pod{ns: s.ns, name: s.name + "-" + h + "-" + suffix(c.rng), deploy: s.name, kind: "ReplicaSet",
				node: c.nodes[(i+len(s.name))%len(c.nodes)], status: "Running", labels: pl, containers: s.containers,
				cpuBase: s.cpu * (0.7 + c.rng.Float64()*0.6), memBase: s.mem * (0.8 + c.rng.Float64()*0.4),
				reqCPU: s.req[0], reqMem: s.req[1], limCPU: s.req[0] * 2, limMem: s.req[1] * 2,
				created: created.Add(time.Duration(c.rng.Intn(500)) * time.Hour), ready: true}
			if degraded && i == 1 {
				p.status, p.ready, p.restarts = "CrashLoopBackOff", false, 37
			}
			c.pods = append(c.pods, p)
		}
	}
	// statefulset + daemonset
	for i := 0; i < 3; i++ {
		c.pods = append(c.pods, &pod{ns: "payments", name: fmt.Sprintf("redis-%d", i), deploy: "redis", kind: "StatefulSet",
			node: c.nodes[i%len(c.nodes)], status: "Running", labels: map[string]string{"app": "redis"}, containers: []string{"redis"},
			cpuBase: 45, memBase: 380, reqCPU: 100, reqMem: 512, limCPU: 500, limMem: 1024, created: start.Add(-400 * time.Hour), ready: true})
	}
	c.sts = append(c.sts, obj{"metadata": obj{"name": "redis", "namespace": "payments", "creationTimestamp": ts(start.Add(-400 * time.Hour))},
		"spec":   obj{"replicas": 3, "selector": obj{"matchLabels": obj{"app": "redis"}}, "template": obj{"spec": obj{"containers": []obj{{"name": "redis", "image": "redis:7.4"}}}}},
		"status": obj{"replicas": 3, "readyReplicas": 3, "updatedReplicas": 3, "availableReplicas": 3}})
	for i, n := range c.nodes {
		c.pods = append(c.pods, &pod{ns: "kube-system", name: "calico-node-" + suffix(c.rng), deploy: "calico-node", kind: "DaemonSet",
			node: n, status: "Running", labels: map[string]string{"k8s-app": "calico-node"}, containers: []string{"calico-node"},
			cpuBase: 30 + float64(i)*3, memBase: 140, reqCPU: 250, reqMem: 0, created: start.Add(-800 * time.Hour), ready: true})
	}
	c.ds = append(c.ds, obj{"metadata": obj{"name": "calico-node", "namespace": "kube-system", "creationTimestamp": ts(start.Add(-800 * time.Hour))},
		"spec":   obj{"selector": obj{"matchLabels": obj{"k8s-app": "calico-node"}}, "template": obj{"spec": obj{"containers": []obj{{"name": "calico-node", "image": "calico/node:v3.28.2"}}}}},
		"status": obj{"desiredNumberScheduled": len(c.nodes), "numberReady": len(c.nodes), "updatedNumberScheduled": len(c.nodes), "numberAvailable": len(c.nodes)}})
	// a pending pod
	c.pods = append(c.pods, &pod{ns: "ott-streaming", name: "binge-transcoder-" + hash("binge-transcoder")[:10] + "-pend1", deploy: "binge-transcoder", kind: "ReplicaSet",
		status: "Pending", labels: map[string]string{"app.kubernetes.io/name": "binge-transcoder", "pod-template-hash": hash("binge-transcoder")[:10]},
		containers: []string{"transcoder"}, reqCPU: 1000, reqMem: 2048, created: start.Add(-20 * time.Minute)})

	now := time.Now()
	ev := func(ns, kind, name, reason, msg string, ago time.Duration, count int) {
		c.events = append(c.events, obj{"metadata": obj{"name": name + "." + suffix(c.rng), "namespace": ns},
			"involvedObject": obj{"kind": kind, "name": name, "namespace": ns}, "reason": reason, "message": msg,
			"type": "Warning", "count": count, "lastTimestamp": ts(now.Add(-ago)), "firstTimestamp": ts(now.Add(-ago - time.Hour)),
			"source": obj{"component": "kubelet"}})
	}
	for _, p := range c.pods {
		if p.status == "CrashLoopBackOff" {
			ev(p.ns, "Pod", p.name, "BackOff", "Back-off restarting failed container worker in pod "+p.name, 40*time.Second, 37)
			ev(p.ns, "Pod", p.name, "Unhealthy", "Liveness probe failed: Get \"http://10.42.3.17:8080/healthz\": context deadline exceeded", 3*time.Minute, 112)
		}
		if p.status == "Pending" {
			ev(p.ns, "Pod", p.name, "FailedScheduling", "0/"+strconv.Itoa(len(c.nodes))+" nodes are available: "+strconv.Itoa(len(c.nodes))+" Insufficient cpu. preemption: 0/"+strconv.Itoa(len(c.nodes))+" nodes are available.", 2*time.Minute, 14)
		}
	}
	ev("ott-streaming", "HorizontalPodAutoscaler", "binge-transcoder", "FailedGetResourceMetric", "failed to get cpu utilization: missing request for cpu in container transcoder", 25*time.Minute, 4)
	ev("monitoring", "Node", c.nodes[len(c.nodes)-1], "SystemOOM", "System OOM encountered, victim process: java, pid: 28371", 48*time.Minute, 1)
	c.initGeneric()
	return c
}

func (c *cluster) wave(base float64, key string, amp float64) float64 {
	h := 0.0
	for _, ch := range key {
		h += float64(ch)
	}
	t := time.Since(start).Seconds()
	v := base * (1 + amp*math.Sin(t/45+h) + amp*0.5*math.Sin(t/11+h*2) + (mrand.Float64()-0.5)*amp*0.4)
	return math.Max(v, base*0.05)
}

func (c *cluster) podObj(p *pod) obj {
	var ctrs, statuses []obj
	var volumes []obj
	mounts := map[string][]string{} // container → mount entries "vol|path"
	for _, pm := range pvcMounts[p.deploy] {
		volumes = append(volumes, obj{"name": pm.claim, "persistentVolumeClaim": obj{"claimName": pm.claim}})
		for _, ct := range pm.ctrs {
			mounts[ct] = append(mounts[ct], pm.claim+"|"+pm.path)
		}
	}
	// declare mounting containers first so the first df attempt can hit a container without `sh`
	ordered := append([]string(nil), p.containers...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i] == "envoy" && ordered[j] != "envoy" })
	for _, ct := range ordered {
		var vms []obj
		for _, e := range mounts[ct] {
			parts := strings.SplitN(e, "|", 2)
			vms = append(vms, obj{"name": parts[0], "mountPath": parts[1]})
		}
		ctrs = append(ctrs, obj{"name": ct, "image": "img/" + ct, "volumeMounts": vms, "resources": obj{
			"requests": obj{"cpu": fmt.Sprintf("%dm", int(p.reqCPU/float64(len(p.containers)))), "memory": fmt.Sprintf("%dMi", int(p.reqMem/float64(len(p.containers))))},
			"limits":   obj{"cpu": fmt.Sprintf("%dm", int(p.limCPU/float64(len(p.containers)))), "memory": fmt.Sprintf("%dMi", int(p.limMem/float64(len(p.containers))))},
		}})
		st := obj{"name": ct, "ready": p.ready, "restartCount": p.restarts, "image": "img/" + ct}
		switch p.status {
		case "Running":
			started := p.created
			if p.restarts > 0 {
				started = time.Now().Add(-time.Duration(1+len(p.name)%5*9) * time.Minute)
			}
			st["state"] = obj{"running": obj{"startedAt": ts(started)}}
		case "CrashLoopBackOff":
			st["state"] = obj{"waiting": obj{"reason": "CrashLoopBackOff", "message": "back-off 5m0s restarting failed container"}}
			st["lastState"] = obj{"terminated": obj{"reason": "Error", "exitCode": 1, "finishedAt": ts(time.Now().Add(-time.Minute))}}
		}
		if p.restarts > 0 && p.status == "Running" {
			reason, code := "OOMKilled", 137
			if p.deploy != "ledger-worker" {
				reason, code = "Error", 1
			}
			st["lastState"] = obj{"terminated": obj{"reason": reason, "exitCode": code,
				"finishedAt": ts(time.Now().Add(-time.Duration(2+len(p.name)%5*9) * time.Minute))}}
		}
		if p.status != "Pending" {
			statuses = append(statuses, st)
		}
	}
	phase := "Running"
	if p.status == "Pending" {
		phase = "Pending"
	}
	o := obj{
		"metadata": obj{"name": p.name, "namespace": p.ns, "labels": p.labels, "creationTimestamp": ts(p.created),
			"ownerReferences": []obj{{"kind": p.kind, "name": func() string {
				if p.kind == "ReplicaSet" {
					return p.deploy + "-" + p.labels["pod-template-hash"]
				}
				return p.deploy
			}()}}},
		"spec":   obj{"nodeName": p.node, "containers": ctrs, "volumes": volumes},
		"status": obj{"phase": phase, "podIP": "10.42." + strconv.Itoa(len(p.name)%9) + "." + strconv.Itoa(len(p.name)*7%250), "hostIP": c.nodeIP(p.node), "qosClass": "Burstable", "containerStatuses": statuses},
	}
	if p.status != "Pending" {
		o["status"].(obj)["startTime"] = ts(p.created)
	}
	return o
}

func matchSel(sel string, labels map[string]string) bool {
	if sel == "" {
		return true
	}
	for _, part := range strings.Split(sel, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || labels[strings.TrimSpace(k)] != strings.TrimSpace(v) {
			return false
		}
	}
	return true
}

func page(w http.ResponseWriter, r *http.Request, items []obj) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	off, _ := strconv.Atoi(r.URL.Query().Get("continue"))
	cont := ""
	if limit > 0 && off+limit < len(items) {
		cont = strconv.Itoa(off + limit)
		items = items[off : off+limit]
	} else if off > 0 && off < len(items) {
		items = items[off:]
	}
	writeJSON(w, obj{"kind": "List", "metadata": obj{"continue": cont}, "items": items})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

var logTemplates = []string{
	`level=info msg="request completed" method=GET path=/api/v2/orders status=200 duration_ms=%d trace_id=%s`,
	`level=info msg="request completed" method=POST path=/api/v2/payments status=201 duration_ms=%d trace_id=%s`,
	`level=debug msg="cache hit" key=user:%d:profile ttl=%s`,
	`level=info msg="kafka message consumed" topic=payment-events partition=%d offset=%s`,
	`level=warn msg="slow query" table=transactions duration_ms=%d query_id=%s`,
	`level=info msg="health check ok" uptime_s=%d build=%s`,
	`level=error msg="upstream timeout" upstream=bkash-gateway attempt=%d request_id=%s`,
}

func logLine(r *mrand.Rand, p *pod, ct string) string {
	if p.status == "CrashLoopBackOff" {
		msgs := []string{
			`level=info msg="starting ledger worker" version=3.2.0`,
			`level=info msg="connecting to postgres" host=ledger-db.payments.svc port=5432`,
			`level=error msg="failed to connect to database" err="dial tcp 10.43.12.9:5432: connect: connection refused"`,
			`level=fatal msg="exiting" code=1`,
		}
		return msgs[r.Intn(len(msgs))]
	}
	t := logTemplates[r.Intn(len(logTemplates))]
	if ct == "envoy" {
		return fmt.Sprintf(`[%s] "GET /api/v2/orders HTTP/1.1" 200 - 0 %d %d - "10.42.1.%d"`, time.Now().UTC().Format(time.RFC3339), r.Intn(9000), r.Intn(120), r.Intn(250))
	}
	return fmt.Sprintf(t, r.Intn(900)+3, fmt.Sprintf("%x", r.Int63())[:12])
}

func (c *cluster) handler(auth func(*http.Request) bool, allow func(path string) bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, obj{"gitVersion": "v1.31.4", "platform": "linux/amd64"})
	})
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		var items []obj
		for i, n := range c.nodes {
			role := "node-role.kubernetes.io/worker"
			if i == 0 {
				role = "node-role.kubernetes.io/control-plane"
			}
			conds := []obj{{"type": "Ready", "status": "True"}, {"type": "MemoryPressure", "status": "False"}, {"type": "DiskPressure", "status": "False"}}
			if i == len(c.nodes)-1 && len(c.nodes) > 3 {
				conds[2]["status"] = "True"
			}
			items = append(items, obj{
				"metadata": obj{"name": n, "labels": obj{role: "", "kubernetes.io/os": "linux", "topology.kubernetes.io/zone": "dhaka-" + string(rune('a'+i%3))}, "creationTimestamp": ts(start.Add(-2000 * time.Hour))},
				"spec":     obj{"unschedulable": c.cordoned[n]},
				"status": obj{"capacity": obj{"cpu": "8", "memory": "32876544Ki", "pods": "110"},
					"allocatable": obj{"cpu": "7800m", "memory": "31588352Ki", "pods": "110"}, "conditions": conds,
					"addresses": []obj{{"type": "InternalIP", "address": fmt.Sprintf("10.20.30.%d", 11+i)}},
					"nodeInfo":  obj{"kubeletVersion": "v1.31.4", "osImage": "Ubuntu 24.04.1 LTS", "kernelVersion": "6.8.0-48-generic", "containerRuntimeVersion": "containerd://1.7.22", "architecture": "amd64"}},
			})
		}
		page(w, r, items)
	})
	listPods := func(w http.ResponseWriter, r *http.Request, ns string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var items []obj
		for _, p := range c.pods {
			fs := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
			if (ns == "" || p.ns == ns) && matchSel(r.URL.Query().Get("labelSelector"), p.labels) && (fs == "" || fs == p.name) {
				items = append(items, c.podObj(p))
			}
		}
		page(w, r, items)
	}
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) { listPods(w, r, "") })
	mux.HandleFunc("/api/v1/namespaces", func(w http.ResponseWriter, r *http.Request) {
		var items []obj
		for _, n := range c.nss {
			items = append(items, obj{"metadata": obj{"name": n, "creationTimestamp": ts(start.Add(-3000 * time.Hour))}, "status": obj{"phase": "Active"}})
		}
		page(w, r, items)
	})
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) { page(w, r, c.events) })
	mux.HandleFunc("/apis/apps/v1/deployments", func(w http.ResponseWriter, r *http.Request) { page(w, r, c.deps) })
	mux.HandleFunc("/apis/apps/v1/statefulsets", func(w http.ResponseWriter, r *http.Request) { page(w, r, c.sts) })
	mux.HandleFunc("/apis/apps/v1/daemonsets", func(w http.ResponseWriter, r *http.Request) { page(w, r, c.ds) })
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/nodes", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		per := map[string][2]float64{}
		for _, p := range c.pods {
			if p.status == "Running" {
				a := per[p.node]
				a[0] += c.wave(p.cpuBase, p.name, 0.35)
				a[1] += c.wave(p.memBase, p.name, 0.08)
				per[p.node] = a
			}
		}
		var items []obj
		for _, n := range c.nodes {
			a := per[n]
			items = append(items, obj{"metadata": obj{"name": n}, "usage": obj{
				"cpu": fmt.Sprintf("%dn", int64((a[0]+350)*1e6)), "memory": fmt.Sprintf("%dKi", int64((a[1]+2100)*1024))}})
		}
		page(w, r, items)
	})
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/pods", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var items []obj
		for _, p := range c.pods {
			if p.status != "Running" {
				continue
			}
			var ctrs []obj
			for _, ct := range p.containers {
				ctrs = append(ctrs, obj{"name": ct, "usage": obj{
					"cpu":    fmt.Sprintf("%dn", int64(c.wave(p.cpuBase, p.name+ct, 0.35)/float64(len(p.containers))*1e6)),
					"memory": fmt.Sprintf("%dKi", int64(c.wave(p.memBase, p.name+ct, 0.08)/float64(len(p.containers))*1024))}})
			}
			items = append(items, obj{"metadata": obj{"name": p.name, "namespace": p.ns}, "containers": ctrs})
		}
		page(w, r, items)
	})
	byNS := func(list []obj, ns string) []obj {
		var out []obj
		for _, o := range list {
			if o["metadata"].(obj)["namespace"] == ns {
				out = append(out, o)
			}
		}
		return out
	}
	mux.HandleFunc("/apis/apps/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/apps/v1/namespaces/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		src := map[string][]obj{"deployments": c.deps, "statefulsets": c.sts, "daemonsets": c.ds}[parts[1]]
		items := byNS(src, parts[0])
		if len(parts) == 3 {
			for _, o := range items {
				if o["metadata"].(obj)["name"] == parts[2] {
					writeJSON(w, o)
					return
				}
			}
			w.WriteHeader(404)
			writeJSON(w, obj{"message": "not found"})
			return
		}
		page(w, r, items)
	})
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		ns := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/metrics.k8s.io/v1beta1/namespaces/"), "/")[0]
		c.mu.Lock()
		defer c.mu.Unlock()
		var items []obj
		for _, p := range c.pods {
			if p.ns != ns || p.status != "Running" {
				continue
			}
			var ctrs []obj
			for _, ct := range p.containers {
				ctrs = append(ctrs, obj{"name": ct, "usage": obj{
					"cpu":    fmt.Sprintf("%dn", int64(c.wave(p.cpuBase, p.name+ct, 0.35)/float64(len(p.containers))*1e6)),
					"memory": fmt.Sprintf("%dKi", int64(c.wave(p.memBase, p.name+ct, 0.08)/float64(len(p.containers))*1024))}})
			}
			items = append(items, obj{"metadata": obj{"name": p.name, "namespace": p.ns}, "containers": ctrs})
		}
		page(w, r, items)
	})
	// namespaced routes
	mux.HandleFunc("/api/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/")
		ns := parts[0]
		switch {
		case len(parts) == 2 && parts[1] == "pods":
			listPods(w, r, ns)
		case len(parts) == 2 && parts[1] == "resourcequotas":
			var items []obj
			switch ns {
			case "payments":
				items = append(items, obj{"metadata": obj{"name": "team-quota", "namespace": ns},
					"status": obj{"hard": obj{"requests.cpu": "4", "requests.memory": "8Gi", "limits.cpu": "8", "limits.memory": "16Gi"}}})
			case "ecrm":
				items = append(items, obj{"metadata": obj{"name": "team-quota", "namespace": ns},
					"status": obj{"hard": obj{"requests.cpu": "2", "requests.memory": "6Gi"}}})
			}
			page(w, r, items)
		case len(parts) == 2 && parts[1] == "events":
			fsel := r.URL.Query().Get("fieldSelector")
			var items []obj
			for _, e := range c.events {
				io := e["involvedObject"].(obj)
				if io["namespace"] != ns {
					continue
				}
				if strings.Contains(fsel, "involvedObject.name=") && !strings.Contains(fsel, "involvedObject.name="+io["name"].(string)) {
					continue
				}
				items = append(items, e)
			}
			page(w, r, items)
		case len(parts) == 4 && parts[1] == "pods" && parts[3] == "log":
			c.serveLog(w, r, ns, parts[2])
		default:
			http.NotFound(w, r)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			writeJSON(w, obj{"kind": "Status", "message": "Unauthorized", "code": 401})
			return
		}
		if allow != nil && !allow(r.URL.Path) && !isDiscovery(r.URL.Path) {
			w.WriteHeader(403)
			writeJSON(w, obj{"kind": "Status", "code": 403, "reason": "Forbidden",
				"message": "forbidden: User \"dev-team\" cannot list resource at " + r.URL.Path})
			return
		}
		if c.discovery(w, r) || c.generic(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func isDiscovery(p string) bool {
	p = strings.TrimSuffix(p, "/")
	return p == "/api" || p == "/api/v1" || p == "/apis" || (strings.HasPrefix(p, "/apis/") && strings.Count(p, "/") == 3)
}

func (c *cluster) serveLog(w http.ResponseWriter, r *http.Request, ns, name string) {
	c.mu.Lock()
	var p *pod
	for _, x := range c.pods {
		if x.ns == ns && x.name == name {
			p = x
		}
	}
	c.mu.Unlock()
	if p == nil {
		w.WriteHeader(404)
		writeJSON(w, obj{"message": "pods \"" + name + "\" not found"})
		return
	}
	if p.status == "Pending" {
		w.WriteHeader(400)
		writeJSON(w, obj{"message": "container \"" + p.containers[0] + "\" in pod \"" + name + "\" is waiting to start: ContainerCreating"})
		return
	}
	q := r.URL.Query()
	ct := q.Get("container")
	rng := mrand.New(mrand.NewSource(int64(len(name)*131 + len(ct))))
	// deterministic history: one line every ~2s for the last 30 min
	now := time.Now()
	var hist []string
	var since time.Time
	if s := q.Get("sinceTime"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	for t := now.Add(-30 * time.Minute); t.Before(now); t = t.Add(time.Duration(1500+rng.Intn(1500)) * time.Millisecond) {
		if !since.IsZero() && t.Before(since) {
			rng.Int63()
			continue
		}
		hist = append(hist, t.UTC().Format(time.RFC3339Nano)+" "+logLine(rng, p, ct))
	}
	if n, err := strconv.Atoi(q.Get("tailLines")); err == nil && n < len(hist) {
		hist = hist[len(hist)-n:]
	}
	w.Header().Set("Content-Type", "text/plain")
	for _, l := range hist {
		fmt.Fprintln(w, l)
	}
	if q.Get("follow") != "true" {
		return
	}
	fl := w.(http.Flusher)
	fl.Flush()
	lr := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Duration(400+lr.Intn(1800)) * time.Millisecond):
			fmt.Fprintln(w, time.Now().UTC().Format(time.RFC3339Nano)+" "+logLine(lr, p, ct))
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------- TLS plumbing

func mkCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mock-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func mkLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tpl.DNSNames = []string{"localhost"}
	} else {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func main() {
	dir := flag.String("dir", "/tmp/kubelamp-mock", "where to write the kubeconfig")
	flag.Parse()
	os.MkdirAll(*dir, 0o755)
	ca, caKey, caPEM := mkCA()
	srvCert, srvKey := mkLeaf(ca, caKey, "kube-apiserver", true)
	cliCert, cliKey := mkLeaf(ca, caKey, "admin", false)
	pair, _ := tls.X509KeyPair(srvCert, srvKey)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	const token = "mock-eks-token-123"
	serve := func(c *cluster, requireClientCert bool, tok string, allow func(string) bool) string {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		cfg := &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven}
		auth := func(r *http.Request) bool {
			if requireClientCert {
				return r.TLS != nil && len(r.TLS.PeerCertificates) > 0
			}
			return r.Header.Get("Authorization") == "Bearer "+tok
		}
		go (&http.Server{Handler: c.handler(auth, allow), TLSConfig: cfg}).Serve(tls.NewListener(ln, cfg))
		return "https://" + ln.Addr().String()
	}
	prod := newCluster("prod-dc1", 6, 1, true)
	eks := newCluster("eks-binge", 4, 2, false)
	u1 := serve(prod, true, "", nil)
	u2 := serve(eks, false, token, nil)
	// Role/RoleBinding-only user: may read payments + ecrm, nothing cluster-scoped
	team := newCluster("team-dc1", 5, 3, true)
	teamAllow := func(p string) bool {
		if p == "/version" || p == "/api/v1/nodes" || p == "/apis/metrics.k8s.io/v1beta1/nodes" {
			return true // like Rancher: project members may list nodes
		}
		for _, ns := range []string{"payments", "ecrm"} {
			for _, pre := range []string{"/api/v1/namespaces/", "/apis/apps/v1/namespaces/", "/apis/metrics.k8s.io/v1beta1/namespaces/"} {
				if strings.HasPrefix(p, pre+ns+"/") {
					return true
				}
			}
		}
		return false
	}
	u3 := serve(team, false, "team-token", teamAllow)

	// rollout simulation: every 90s replace one payments-api pod
	go func() {
		for range time.Tick(90 * time.Second) {
			prod.mu.Lock()
			for i, p := range prod.pods {
				if p.deploy == "payments-api" {
					np := *p
					np.name = p.deploy + "-" + p.labels["pod-template-hash"] + "-" + suffix(prod.rng)
					np.created = time.Now()
					prod.pods[i] = &np
					break
				}
			}
			prod.mu.Unlock()
		}
	}()

	// exec plugin script that mimics `aws eks get-token`
	plugin := filepath.Join(*dir, "fake-aws-token.sh")
	os.WriteFile(plugin, []byte(`#!/bin/sh
echo '{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1beta1","status":{"token":"`+token+`","expirationTimestamp":"`+time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339)+`"}}'
`), 0o755)
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: prod-dc1
clusters:
- name: prod-dc1
  cluster:
    server: %s
    certificate-authority-data: %s
- name: arn:aws:eks:ap-southeast-1:111122223333:cluster/eks-binge
  cluster:
    server: %s
    certificate-authority-data: %s
- name: staging-dc2
  cluster:
    server: https://127.0.0.1:1
    insecure-skip-tls-verify: true
- name: team-dc1
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: kubernetes-admin
  user:
    client-certificate-data: %s
    client-key-data: %s
- name: arn:aws:eks:ap-southeast-1:111122223333:cluster/eks-binge
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: %s
      args: ["eks", "get-token", "--cluster-name", "eks-binge"]
- name: staging-admin
  user:
    token: abc
- name: dev-team
  user:
    token: team-token
contexts:
- name: prod-dc1
  context: {cluster: prod-dc1, user: kubernetes-admin}
- name: arn:aws:eks:ap-southeast-1:111122223333:cluster/eks-binge
  context: {cluster: "arn:aws:eks:ap-southeast-1:111122223333:cluster/eks-binge", user: "arn:aws:eks:ap-southeast-1:111122223333:cluster/eks-binge", namespace: ott-streaming}
- name: staging-dc2
  context: {cluster: staging-dc2, user: staging-admin}
- name: team-payments
  context: {cluster: team-dc1, user: dev-team, namespace: payments}
`, u1, b64(caPEM), u2, b64(caPEM), u3, b64(caPEM), b64(cliCert), b64(cliKey), plugin)
	os.WriteFile(filepath.Join(*dir, "kubeconfig"), []byte(kc), 0o600)
	names := []string{}
	for _, p := range prod.pods {
		names = append(names, p.name)
	}
	sort.Strings(names)
	log.Printf("mock clusters up: %s (client cert), %s (exec token); kubeconfig: %s", u1, u2, filepath.Join(*dir, "kubeconfig"))
	select {}
}

type pvcMount struct {
	claim, path string
	ctrs        []string
}

// which deployments mount which PVC (used by pod objects and the fake `df`)
var pvcMounts = map[string][]pvcMount{
	"payments-api":  {{"api-data", "/data", []string{"envoy", "api"}}},
	"ledger-worker": {{"ledger-data", "/var/ledger", []string{"worker"}}, {"ledger-cache", "/var/cache", []string{"worker"}}},
	"ecrm-backend":  {{"ecrm-uploads", "/uploads", []string{"app"}}},
}

// pvc → (size KiB, used KiB) reported by the fake df
var pvcDf = map[string][2]int{
	"api-data":     {10485760, 3355443},
	"ledger-data":  {20971520, 18245222},
	"ledger-cache": {5242880, 1048576},
	"ecrm-uploads": {5242880, 4876000},
}

// nodeIP matches the InternalIP the /api/v1/nodes handler reports.
func (c *cluster) nodeIP(node string) string {
	for i, n := range c.nodes {
		if n == node {
			return fmt.Sprintf("10.20.30.%d", 11+i)
		}
	}
	return ""
}
