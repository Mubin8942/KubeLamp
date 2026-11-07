package main

import (
	"context"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	activeInterval = 15 * time.Second // cluster open in the UI (-interval)
	idleInterval   = 60 * time.Second // everything else
	historyPoints  = 240              // 1 hour at 15 s
)

const activeWindow = 10 * time.Minute

// Snapshot is one consistent read of a cluster. The UI is served from it, so
// page loads are instant and the API server sees one list per resource per interval.
type Snapshot struct {
	At               time.Time
	Err              string
	Version          string
	Nodes            []Node
	Pods             []Pod
	Deployments      []Deployment
	StatefulSets     []StatefulSet
	DaemonSets       []DaemonSet
	Namespaces       []Namespace
	Warnings         []Event
	NodeUsage        map[string]Usage
	PodUsage         map[string]Usage // ns/name
	MetricsAvailable bool
	MetricsErr       string
	Readable         bool              // at least one workload/pod list succeeded
	Scope            string            // "cluster" or "namespaced"
	ScopeNamespaces  []string          // namespaces read in namespaced scope
	Quotas           map[string]Quota  // per namespace (namespaced scope)
	owner            map[string]string // ns/pod → "Deployment/name"
}

type Usage struct {
	CPU float64 `json:"cpu"` // cores
	Mem float64 `json:"mem"` // bytes
}

// ---------------------------------------------------------------- history

type series struct {
	t        []int64
	cpu, mem []float32
	last     time.Time
}

func (s *series) add(t time.Time, u Usage) {
	s.t = append(s.t, t.Unix())
	s.cpu = append(s.cpu, float32(u.CPU))
	s.mem = append(s.mem, float32(u.Mem))
	if len(s.t) > historyPoints {
		n := len(s.t) - historyPoints
		s.t, s.cpu, s.mem = s.t[n:], s.cpu[n:], s.mem[n:]
	}
	s.last = t
}

type HistoryView struct {
	T   []int64   `json:"t"`
	CPU []float32 `json:"cpu"`
	Mem []float32 `json:"mem"`
}

// ---------------------------------------------------------------- cluster

type Cluster struct {
	Info    ContextInfo
	client  *Client
	initErr string

	disc       discoveryCache
	access     AccessSettings // guarded by mu
	detected   string         // auto-detected scope
	detectedAt time.Time

	fpMu     sync.Mutex // live pod lists (see FreshPods)
	fp       map[string]freshPods
	pvcMu    sync.Mutex // PVC usage cache (see pvcusage.go)
	pvcCache map[string]PVCUsage
	pvcLocks sync.Map // namespace → *sync.Mutex

	mu         sync.RWMutex
	snap       *Snapshot
	history    map[string]*series
	lastViewed time.Time
	refreshing bool
	wake       chan struct{}
	stop       chan struct{}
}

func newCluster(rc resolvedContext, acc AccessSettings) *Cluster {
	c := &Cluster{
		Info:    rc.ContextInfo,
		access:  acc,
		history: map[string]*series{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
	cl, err := newClient(rc)
	if err != nil {
		c.initErr = err.Error()
	} else {
		c.client = cl
	}
	go c.loop()
	return c
}

func (c *Cluster) touch() {
	c.mu.Lock()
	wasIdle := time.Since(c.lastViewed) > activeWindow
	c.lastViewed = time.Now()
	c.mu.Unlock()
	if wasIdle {
		c.poke()
	}
}

func (c *Cluster) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Cluster) Snap() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap
}

func (c *Cluster) loop() {
	if c.client == nil {
		c.mu.Lock()
		c.snap = &Snapshot{At: time.Now(), Err: c.initErr}
		c.mu.Unlock()
		return
	}
	for {
		c.refresh()
		c.mu.RLock()
		interval := idleInterval
		if time.Since(c.lastViewed) < activeWindow {
			interval = activeInterval
		}
		failed := c.snap != nil && c.snap.Err != ""
		c.mu.RUnlock()
		if failed {
			interval = idleInterval
		}
		t := time.NewTimer(interval)
		select {
		case <-c.stop:
			t.Stop()
			return
		case <-c.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

func (c *Cluster) Close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}

func (c *Cluster) refresh() {
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s := &Snapshot{At: time.Now(), NodeUsage: map[string]Usage{}, PodUsage: map[string]Usage{}, Quotas: map[string]Quota{}}

	// Reachability + version first so unreachable clusters fail fast.
	// A 403 here only means /version is locked down — keep going.
	var ver struct {
		GitVersion string `json:"gitVersion"`
	}
	vctx, vcancel := context.WithTimeout(ctx, 12*time.Second)
	err := c.client.get(vctx, "/version", nil, &ver)
	vcancel()
	if err != nil && !isForbidden(err) {
		s.Err = friendlyErr(err)
		c.store(s)
		return
	}
	s.Version = ver.GitVersion

	var errs []string
	if c.decideScope(ctx) == "namespaced" {
		errs = c.refreshNamespaced(ctx, s)
	} else {
		s.Scope = "cluster"
		errs = c.refreshClusterWide(ctx, s)
	}
	if len(errs) > 0 {
		// partial data is still useful (e.g. RBAC forbids one resource)
		s.Err = strings.Join(errs, "; ")
	}
	if !s.Readable {
		if s.Err == "" {
			s.Err = "nothing readable with these credentials"
		}
		c.store(s)
		return
	}
	s.owner = buildOwners(s)
	c.store(s)
	c.record(s)
}

func (c *Cluster) refreshClusterWide(ctx context.Context, s *Snapshot) []string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, name+": "+friendlyErr(err))
			} else if name != "nodes" {
				s.Readable = true
			}
		}()
	}
	run("nodes", func() (err error) { s.Nodes, err = list[Node](ctx, c.client, "/api/v1/nodes", nil); return })
	run("pods", func() (err error) { s.Pods, err = list[Pod](ctx, c.client, "/api/v1/pods", nil); return })
	run("deployments", func() (err error) {
		s.Deployments, err = list[Deployment](ctx, c.client, "/apis/apps/v1/deployments", nil)
		return
	})
	run("statefulsets", func() (err error) {
		s.StatefulSets, err = list[StatefulSet](ctx, c.client, "/apis/apps/v1/statefulsets", nil)
		return
	})
	run("daemonsets", func() (err error) {
		s.DaemonSets, err = list[DaemonSet](ctx, c.client, "/apis/apps/v1/daemonsets", nil)
		return
	})
	run("namespaces", func() (err error) {
		s.Namespaces, err = list[Namespace](ctx, c.client, "/api/v1/namespaces", nil)
		return
	})
	run("events", func() error {
		ev, err := list[Event](ctx, c.client, "/api/v1/events", url.Values{"fieldSelector": {"type=Warning"}})
		if err != nil {
			return err
		}
		s.Warnings = newestEvents(ev, 300)
		return nil
	})
	// metrics-server is optional
	wg.Add(1)
	go func() {
		defer wg.Done()
		nm, err := list[NodeMetrics](ctx, c.client, "/apis/metrics.k8s.io/v1beta1/nodes", nil)
		if err != nil {
			s.MetricsErr = friendlyErr(err)
			return
		}
		pm, err := list[PodMetrics](ctx, c.client, "/apis/metrics.k8s.io/v1beta1/pods", nil)
		if err != nil {
			s.MetricsErr = friendlyErr(err)
			return
		}
		for _, n := range nm {
			s.NodeUsage[n.Metadata.Name] = Usage{n.Usage.cpu(), n.Usage.mem()}
		}
		addPodMetrics(s, pm)
		s.MetricsAvailable = true
	}()
	wg.Wait()
	return errs
}

func addPodMetrics(s *Snapshot, pm []PodMetrics) {
	for _, p := range pm {
		var u Usage
		for _, ct := range p.Containers {
			u.CPU += ct.Usage.cpu()
			u.Mem += ct.Usage.mem()
		}
		s.PodUsage[p.Metadata.Namespace+"/"+p.Metadata.Name] = u
	}
}

func newestEvents(ev []Event, n int) []Event {
	sort.Slice(ev, func(i, j int) bool { return ev[i].When().After(ev[j].When()) })
	if len(ev) > n {
		ev = ev[:n]
	}
	return ev
}

func (c *Cluster) store(s *Snapshot) {
	c.mu.Lock()
	c.snap = s
	c.mu.Unlock()
}

func friendlyErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "i/o timeout"):
		return "timed out — cluster unreachable (VPN? firewall?)"
	case strings.Contains(msg, "connection refused"):
		return "connection refused by the API server"
	case strings.Contains(msg, "no such host"):
		return "cannot resolve the API server hostname"
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return "TLS certificate problem: " + msg
	}
	if e, ok := err.(*APIError); ok && e.Code == 403 {
		return "forbidden by RBAC: " + e.Message
	}
	if e, ok := err.(*APIError); ok && e.Code == 401 {
		return "unauthorized — credentials expired or invalid"
	}
	if e, ok := err.(*APIError); ok && e.Code == 404 {
		return "not found (API not installed)"
	}
	return msg
}

// buildOwners maps every pod to its top-level workload.
func buildOwners(s *Snapshot) map[string]string {
	deps := map[string]bool{}
	for _, d := range s.Deployments {
		deps[d.Metadata.Namespace+"/"+d.Metadata.Name] = true
	}
	own := make(map[string]string, len(s.Pods))
	for i := range s.Pods {
		p := &s.Pods[i]
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		for _, o := range p.Metadata.OwnerReferences {
			switch o.Kind {
			case "ReplicaSet":
				name := o.Name
				if h := p.Metadata.Labels["pod-template-hash"]; h != "" {
					name = strings.TrimSuffix(name, "-"+h)
				}
				if deps[p.Metadata.Namespace+"/"+name] {
					own[key] = "Deployment/" + name
				} else {
					own[key] = "ReplicaSet/" + o.Name
				}
			case "StatefulSet", "DaemonSet":
				own[key] = o.Kind + "/" + o.Name
			case "Job":
				own[key] = "Job/" + o.Name
			default:
				own[key] = o.Kind + "/" + o.Name
			}
		}
	}
	return own
}

// record appends one history point per tracked series.
func (c *Cluster) record(s *Snapshot) {
	if !s.MetricsAvailable && len(s.NodeUsage) == 0 {
		return
	}
	t := s.At
	agg := map[string]Usage{}
	var total Usage
	if s.Scope != "namespaced" {
		for _, u := range s.NodeUsage {
			total.CPU += u.CPU
			total.Mem += u.Mem
		}
	} else { // namespaced scope: the "cluster" line is the sum of visible pods
		for _, u := range s.PodUsage {
			total.CPU += u.CPU
			total.Mem += u.Mem
		}
	}
	agg["cluster"] = total
	for n, u := range s.NodeUsage {
		agg["node/"+n] = u
	}
	for key, u := range s.PodUsage {
		ns, _, _ := strings.Cut(key, "/")
		a := agg["ns/"+ns]
		a.CPU += u.CPU
		a.Mem += u.Mem
		agg["ns/"+ns] = a
		agg["pod/"+key] = u
		if o := s.owner[key]; o != "" {
			kind, name, _ := strings.Cut(o, "/")
			wk := "wl/" + kind + "/" + ns + "/" + name
			w := agg[wk]
			w.CPU += u.CPU
			w.Mem += u.Mem
			agg[wk] = w
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, u := range agg {
		sr := c.history[k]
		if sr == nil {
			sr = &series{}
			c.history[k] = sr
		}
		sr.add(t, u)
	}
	for k, sr := range c.history {
		if t.Sub(sr.last) > 15*time.Minute {
			delete(c.history, k)
		}
	}
}

func (c *Cluster) History(key string) HistoryView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sr := c.history[key]
	if sr == nil {
		return HistoryView{T: []int64{}, CPU: []float32{}, Mem: []float32{}}
	}
	return HistoryView{
		T:   append([]int64(nil), sr.t...),
		CPU: append([]float32(nil), sr.cpu...),
		Mem: append([]float32(nil), sr.mem...),
	}
}

// Spark returns just the last n CPU/mem points for compact sparklines.
func (c *Cluster) Spark(key string, n int) HistoryView {
	h := c.History(key)
	if len(h.T) > n {
		k := len(h.T) - n
		h.T, h.CPU, h.Mem = h.T[k:], h.CPU[k:], h.Mem[k:]
	}
	return h
}

// ---------------------------------------------------------------- registry of clusters

type Registry struct {
	inCluster *resolvedContext // set when KubeLamp runs inside Kubernetes
	mu        sync.RWMutex
	paths     []string
	loadErr   string
	clusters  []*Cluster
	byName    map[string]*Cluster
}

func (r *Registry) Load(paths []string, access map[string]AccessSettings) {
	ctxs, err := loadKubeconfig(paths)
	if r.inCluster != nil {
		ic := *r.inCluster
		for _, c := range ctxs {
			if c.Name == ic.Name {
				ic.Name += " (in-cluster)"
				ic.ContextInfo.Name = ic.Name
			}
		}
		if len(ctxs) > 0 {
			ic.Current = false
		}
		ctxs = append([]resolvedContext{ic}, ctxs...)
		if len(paths) == 0 {
			err = nil // in-cluster only: no kubeconfig is expected
		}
	}
	r.mu.Lock()
	old := r.clusters
	r.paths = paths
	r.clusters = nil
	r.byName = map[string]*Cluster{}
	r.loadErr = ""
	if err != nil {
		r.loadErr = err.Error()
	}
	for _, rc := range ctxs {
		c := newCluster(rc, access[rc.Name])
		r.clusters = append(r.clusters, c)
		r.byName[rc.Name] = c
	}
	r.mu.Unlock()
	for _, c := range old {
		c.Close()
	}
	if err != nil {
		log.Printf("kubeconfig problem: %v", err)
	}
	log.Printf("loaded %d contexts from %v", len(ctxs), paths)
}

func (r *Registry) All() []*Cluster {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*Cluster(nil), r.clusters...)
}

func (r *Registry) Get(name string) *Cluster {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byName[name]
}
