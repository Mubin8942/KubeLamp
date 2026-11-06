package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kubelamp/internal/ws"
)

// ---------------------------------------------------------------- live pod lists

type freshPods struct {
	at   time.Time
	pods []Pod
}

const freshTTL = 900 * time.Millisecond

// FreshPods lists pods straight from the API server (the collector snapshot only
// refreshes every few seconds). Concurrent callers are serialised and share one
// result for ~1s, so even 1s polling from several tabs costs one list per second.
func (c *Cluster) FreshPods(ctx context.Context, ns string) ([]Pod, error) {
	c.fpMu.Lock()
	defer c.fpMu.Unlock()
	if e, ok := c.fp[ns]; ok && time.Since(e.at) < freshTTL {
		return e.pods, nil
	}
	snap := c.Snap()
	var pods []Pod
	switch {
	case ns != "":
		v, err := list[Pod](ctx, c.client, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", nil)
		if err != nil {
			return nil, err
		}
		pods = v
	case snap != nil && snap.Scope == "namespaced":
		for _, n := range snap.ScopeNamespaces {
			v, err := list[Pod](ctx, c.client, "/api/v1/namespaces/"+url.PathEscape(n)+"/pods", nil)
			if err != nil && !isForbidden(err) {
				return nil, err
			}
			pods = append(pods, v...)
		}
	default:
		v, err := list[Pod](ctx, c.client, "/api/v1/pods", nil)
		if err != nil {
			return nil, err
		}
		pods = v
	}
	if c.fp == nil {
		c.fp = map[string]freshPods{}
	}
	c.fp[ns] = freshPods{time.Now(), pods}
	return pods, nil
}

// ---------------------------------------------------------------- one-shot exec

// execSem caps concurrent non-interactive execs across all clusters.
var execSem = make(chan struct{}, 3)

// execOnce runs a command in a container (no TTY, no stdin) and returns its output.
func (c *Cluster) execOnce(ctx context.Context, ns, pod, container string, cmd []string, timeout time.Duration) (string, error) {
	return c.execWith(ctx, execSem, ns, pod, container, cmd, timeout)
}

// execWith is execOnce with a caller-chosen concurrency limiter, so other features
// (the connectivity tester) don't starve PVC usage checks.
func (c *Cluster) execWith(ctx context.Context, sem chan struct{}, ns, pod, container string, cmd []string, timeout time.Duration) (string, error) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "command": cmd}
	if container != "" {
		q.Set("container", container)
	}
	rwc, _, err := c.client.upgrade(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/exec", q,
		[]string{"v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io"})
	if err != nil {
		return "", err
	}
	conn := ws.NewClient(rwc)
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()

	var out strings.Builder
	for {
		op, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return out.String(), errors.New("timed out")
			}
			return out.String(), nil // closed after the status frame
		}
		if op != ws.OpBinary || len(msg) < 2 {
			continue
		}
		switch msg[0] {
		case 1:
			if out.Len() < 1<<20 {
				out.Write(msg[1:])
			}
		case 3:
			var st struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(msg[1:], &st)
			if st.Status == "Failure" && !strings.Contains(st.Message, "exit code") {
				return out.String(), errors.New(st.Message)
			}
			return out.String(), nil
		}
	}
}

// ---------------------------------------------------------------- PVC usage

type PVC struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		StorageClassName *string `json:"storageClassName"`
		VolumeName       string  `json:"volumeName"`
		Resources        struct {
			Requests ResourceList `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type PVCUsage struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Phase     string    `json:"phase"`
	Requested float64   `json:"requested"`
	State     string    `json:"state"` // ok | unmounted | error | disabled | unbound
	Used      float64   `json:"used"`
	Size      float64   `json:"size"`
	Avail     float64   `json:"avail"`
	Pct       float64   `json:"pct"`
	Pod       string    `json:"pod,omitempty"`
	Container string    `json:"container,omitempty"`
	Path      string    `json:"path,omitempty"`
	Approx    bool      `json:"approx,omitempty"` // subPath mount: df shows the whole volume
	At        time.Time `json:"at,omitempty"`
	Err       string    `json:"err,omitempty"`
	Cached    bool      `json:"cached,omitempty"`
}

const (
	pvcOKTTL  = 5 * time.Minute
	pvcErrTTL = 60 * time.Second
)

func (c *Cluster) pvcCached(key string, refresh bool) (PVCUsage, bool) {
	c.pvcMu.Lock()
	defer c.pvcMu.Unlock()
	u, ok := c.pvcCache[key]
	if !ok {
		return u, false
	}
	// "refresh" bypasses the 5-minute cache, but never re-reads a volume more than once per 10s
	if refresh && time.Since(u.At) > 10*time.Second {
		return PVCUsage{}, false
	}
	ttl := pvcOKTTL
	if u.State != "ok" {
		ttl = pvcErrTTL
	}
	if time.Since(u.At) > ttl {
		return u, false
	}
	u.Cached = true
	return u, true
}

func (c *Cluster) pvcStore(u PVCUsage) {
	c.pvcMu.Lock()
	defer c.pvcMu.Unlock()
	if c.pvcCache == nil {
		c.pvcCache = map[string]PVCUsage{}
	}
	c.pvcCache[u.Namespace+"/"+u.Name] = u
}

// pvcPeek returns a cached (possibly stale) reading without touching the cluster.
func (c *Cluster) pvcPeek(ns, name string) (PVCUsage, bool) {
	c.pvcMu.Lock()
	defer c.pvcMu.Unlock()
	u, ok := c.pvcCache[ns+"/"+name]
	return u, ok && u.State == "ok"
}

// candidate = one container that mounts a PVC inside a running pod.
type mountCand struct {
	pod, container, path string
	subPath              bool
	pvc                  string
}

func pvcCandidates(pods []Pod, ns string) map[string][]mountCand {
	out := map[string][]mountCand{}
	for i := range pods {
		p := &pods[i]
		if p.Metadata.Namespace != ns || p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != nil {
			continue
		}
		claim := map[string]string{} // volume name → claim
		for _, v := range p.Spec.Volumes {
			if v.PVC != nil {
				claim[v.Name] = v.PVC.ClaimName
			}
		}
		if len(claim) == 0 {
			continue
		}
		running := map[string]bool{}
		for _, cs := range p.Status.ContainerStatuses {
			running[cs.Name] = cs.State.Running != nil
		}
		for _, ct := range p.Spec.Containers {
			if !running[ct.Name] {
				continue
			}
			for _, vm := range ct.VolumeMounts {
				if cl, ok := claim[vm.Name]; ok {
					out[cl] = append(out[cl], mountCand{p.Metadata.Name, ct.Name, vm.MountPath, vm.SubPath != "", cl})
				}
			}
		}
	}
	return out
}

// parseDfScript reads the output of the probe script: "@@<path>" then a `df -P -k` line (or an error).
func parseDfScript(out string) map[string]PVCUsage {
	res := map[string]PVCUsage{}
	cur := ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if p, ok := strings.CutPrefix(ln, "@@"); ok {
			cur = p
			continue
		}
		f := strings.Fields(ln)
		if cur == "" || len(f) < 6 {
			continue
		}
		n := len(f)
		blocks, e1 := strconv.ParseFloat(f[n-5], 64)
		used, e2 := strconv.ParseFloat(f[n-4], 64)
		avail, e3 := strconv.ParseFloat(f[n-3], 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		u := PVCUsage{Used: used * 1024, Size: blocks * 1024, Avail: avail * 1024}
		if u.Size > 0 {
			u.Pct = u.Used / u.Size * 100
		}
		res[cur] = u
	}
	return res
}

const dfScript = `for p in "$@"; do echo "@@$p"; df -P -k "$p" 2>&1 | tail -n +2; done`

func pvcLock(c *Cluster, ns string) *sync.Mutex {
	m, _ := c.pvcLocks.LoadOrStore(ns, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (s *Server) listPVCs(ctx context.Context, c *Cluster, ns string) ([]PVC, error) {
	return list[PVC](ctx, c.client, "/api/v1/namespaces/"+url.PathEscape(ns)+"/persistentvolumeclaims", nil)
}

// usageFor measures PVC usage in one namespace. At most one exec per pod, and only for
// PVCs without a fresh cached reading.
func (s *Server) usageFor(ctx context.Context, c *Cluster, ns, only string, refresh bool) ([]PVCUsage, error) {
	l := pvcLock(c, ns)
	l.Lock()
	defer l.Unlock()

	pvcs, err := s.listPVCs(ctx, c, ns)
	if err != nil {
		return nil, err
	}
	pods, perr := c.FreshPods(ctx, ns)
	if perr != nil {
		pods = nil
	}
	cands := pvcCandidates(pods, ns)

	res := map[string]*PVCUsage{}
	var order []string
	var todo []string
	for i := range pvcs {
		p := &pvcs[i]
		if only != "" && p.Metadata.Name != only {
			continue
		}
		u := &PVCUsage{Namespace: ns, Name: p.Metadata.Name, Phase: p.Status.Phase, Requested: parseQuantity(p.Spec.Resources.Requests["storage"])}
		res[u.Name] = u
		order = append(order, u.Name)
		switch {
		case p.Status.Phase != "Bound":
			u.State = "unbound"
		case s.readOnly:
			if cu, ok := c.pvcPeek(ns, u.Name); ok {
				cu.Cached = true
				*u = cu
			} else {
				u.State = "disabled"
			}
		default:
			if cu, ok := c.pvcCached(ns+"/"+u.Name, refresh); ok {
				*u = cu
			} else if len(cands[u.Name]) == 0 {
				u.State = "unmounted"
				u.At = time.Now()
				c.pvcStore(*u)
			} else {
				todo = append(todo, u.Name)
			}
		}
	}

	// up to two attempts; the second tries a different pod/container for failures
	for attempt := 0; attempt < 2 && len(todo) > 0; attempt++ {
		type key struct{ pod, container string }
		groups := map[key][]mountCand{}
		// prefer pods that serve the most PVCs so one exec covers many
		score := map[key]int{}
		for _, n := range todo {
			for _, cd := range cands[n] {
				score[key{cd.pod, cd.container}]++
			}
		}
		for _, n := range todo {
			cs := append([]mountCand(nil), cands[n]...)
			sort.SliceStable(cs, func(i, j int) bool {
				a, b := score[key{cs[i].pod, cs[i].container}], score[key{cs[j].pod, cs[j].container}]
				if a != b {
					return a > b
				}
				return cs[i].pod < cs[j].pod
			})
			if attempt >= len(cs) {
				continue
			}
			cd := cs[attempt]
			groups[key{cd.pod, cd.container}] = append(groups[key{cd.pod, cd.container}], cd)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := map[string]string{}
		for k, g := range groups {
			wg.Add(1)
			go func(k key, g []mountCand) {
				defer wg.Done()
				var paths []string
				seen := map[string]bool{}
				for _, cd := range g {
					if !seen[cd.path] {
						seen[cd.path] = true
						paths = append(paths, cd.path)
					}
				}
				out, err := c.execOnce(ctx, ns, k.pod, k.container, append([]string{"sh", "-c", dfScript, "sh"}, paths...), 8*time.Second)
				parsed := parseDfScript(out)
				mu.Lock()
				defer mu.Unlock()
				for _, cd := range g {
					u := res[cd.pvc]
					if r, ok := parsed[cd.path]; ok {
						u.Used, u.Size, u.Avail, u.Pct = r.Used, r.Size, r.Avail, r.Pct
						u.State, u.Pod, u.Container, u.Path, u.Approx = "ok", cd.pod, cd.container, cd.path, cd.subPath
						u.At, u.Err = time.Now(), ""
						continue
					}
					msg := "df not available in " + k.container
					if err != nil {
						msg = friendlyErr(err)
					}
					failed[cd.pvc] = msg
				}
			}(k, g)
		}
		wg.Wait()
		var next []string
		for _, n := range todo {
			if res[n].State == "ok" {
				c.pvcStore(*res[n])
				continue
			}
			if msg, bad := failed[n]; bad {
				res[n].State, res[n].Err = "error", msg
			}
			next = append(next, n)
		}
		todo = next
	}
	for _, n := range todo {
		u := res[n]
		if u.State != "error" {
			u.State, u.Err = "error", "could not read usage"
		}
		u.At = time.Now()
		c.pvcStore(*u)
	}
	out := make([]PVCUsage, 0, len(order))
	for _, n := range order {
		out = append(out, *res[n])
	}
	return out, nil
}

func (s *Server) pvcRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pvcusage", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		ns := q.Get("ns")
		if ns == "" {
			writeErr(w, 400, errors.New("ns required"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		u, err := s.usageFor(ctx, c, ns, q.Get("name"), q.Get("refresh") == "1")
		if err != nil {
			writeErr(w, 502, errors.New(friendlyErr(err)))
			return
		}
		writeJSON(w, 200, map[string]any{"pvcs": u, "exec": !s.readOnly})
	})
}
