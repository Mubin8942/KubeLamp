package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	reg        *Registry
	dataDir    string
	token      string // optional shared token when exposed beyond localhost
	localOnly  bool
	readOnly   bool
	ftMaxDown  int64 // file transfer limits in bytes (0 = default)
	ftMaxUp    int64
	ftRate     int64 // bytes per second per transfer
	allowShell bool
	quit       func()
}

func itoa(i int) string { return strconv.Itoa(i) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) cluster(w http.ResponseWriter, r *http.Request) (*Cluster, *Snapshot, bool) {
	c := s.reg.Get(r.URL.Query().Get("ctx"))
	if c == nil {
		writeErr(w, 404, errors.New("unknown context"))
		return nil, nil, false
	}
	c.touch()
	snap := c.Snap()
	if snap == nil {
		// first load still running: wait briefly so the first page isn't empty
		for i := 0; i < 40 && snap == nil; i++ {
			time.Sleep(250 * time.Millisecond)
			snap = c.Snap()
		}
		if snap == nil {
			writeErr(w, 503, errors.New("still connecting to the cluster…"))
			return nil, nil, false
		}
	}
	if c.client == nil || !snap.Readable {
		writeErr(w, 502, errors.New(snap.Err))
		return nil, nil, false
	}
	return c, snap, true
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/kubeconfig", func(w http.ResponseWriter, r *http.Request) {
		s.reg.mu.RLock()
		paths, loadErr := s.reg.paths, s.reg.loadErr
		s.reg.mu.RUnlock()
		ctxs := []ContextInfo{}
		for _, c := range s.reg.All() {
			ctxs = append(ctxs, c.Info)
		}
		writeJSON(w, 200, map[string]any{"paths": paths, "error": loadErr, "contexts": ctxs,
			"sources": s.kubeconfigSources(), "inCluster": s.reg.inCluster != nil,
			"needsSetup": len(paths) == 0 && s.reg.inCluster == nil, "local": s.localOnly, "version": version})
	})

	mux.HandleFunc("PUT /api/kubeconfig", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paths string `json:"paths"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		paths := splitPaths(req.Paths)
		if len(paths) == 0 {
			writeErr(w, 400, errors.New("enter the path to a kubeconfig file"))
			return
		}
		for _, p := range paths {
			if st, err := os.Stat(p); err != nil {
				writeErr(w, 400, errors.New("cannot read "+p+": "+err.Error()))
				return
			} else if st.IsDir() {
				writeErr(w, 400, errors.New(p+" is a folder — point to the config file inside it"))
				return
			}
		}
		if _, err := loadKubeconfig(paths); err != nil {
			writeErr(w, 400, err)
			return
		}
		st := loadSettings(s.dataDir)
		st.Kubeconfig = paths
		s.reg.Load(paths, st.Access)
		saveSettings(s.dataDir, st)
		writeJSON(w, 200, map[string]any{"ok": true, "contexts": len(s.reg.All())})
	})

	// Per-context access: auto / cluster-wide / only these namespaces (Role + RoleBinding).
	mux.HandleFunc("PUT /api/access", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Context    string   `json:"context"`
			Mode       string   `json:"mode"`
			Namespaces []string `json:"namespaces"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		c := s.reg.Get(req.Context)
		if c == nil {
			writeErr(w, 404, errors.New("unknown context"))
			return
		}
		acc := normalizeAccess(AccessSettings{Mode: req.Mode, Namespaces: req.Namespaces})
		for _, n := range req.Namespaces {
			for _, part := range strings.FieldsFunc(n, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == ';' }) {
				if !validDNSLabel(strings.TrimSpace(part)) {
					writeErr(w, 400, errors.New("not a valid namespace name: "+part))
					return
				}
			}
		}
		c.SetAccess(acc)
		c.touch()
		st := loadSettings(s.dataDir)
		if len(st.Kubeconfig) == 0 {
			s.reg.mu.RLock()
			st.Kubeconfig = s.reg.paths
			s.reg.mu.RUnlock()
		}
		if st.Access == nil {
			st.Access = map[string]AccessSettings{}
		}
		st.Access[req.Context] = acc
		saveSettings(s.dataDir, st)
		writeJSON(w, 200, acc)
	})

	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		if s.quit != nil {
			go s.quit()
		}
	})

	mux.HandleFunc("GET /api/clusters", func(w http.ResponseWriter, r *http.Request) {
		out := []ClusterSummary{}
		for _, c := range s.reg.All() {
			out = append(out, summarize(c))
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, r *http.Request) {
		if c := s.reg.Get(r.URL.Query().Get("ctx")); c != nil {
			c.touch()
			c.poke()
		} else {
			for _, c := range s.reg.All() {
				c.poke()
			}
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		pods := make([]PodView, 0, len(snap.Pods))
		for i := range snap.Pods {
			if snap.Pods[i].Status.Phase == "Running" || snap.Pods[i].Status.Phase == "Pending" {
				pods = append(pods, podView(snap, &snap.Pods[i], false))
			}
		}
		top := func(less func(a, b PodView) bool) []PodView {
			cp := append([]PodView(nil), pods...)
			sort.Slice(cp, func(i, j int) bool { return less(cp[i], cp[j]) })
			if len(cp) > 10 {
				cp = cp[:10]
			}
			return cp
		}
		problems := []PodView{}
		for _, p := range pods {
			if p.Problem {
				problems = append(problems, p)
			}
		}
		for i := range snap.Pods {
			if snap.Pods[i].Status.Phase == "Failed" && len(problems) < 50 {
				problems = append(problems, podView(snap, &snap.Pods[i], false))
			}
		}
		warn := []EventView{}
		for i := range snap.Warnings {
			if i >= 40 {
				break
			}
			warn = append(warn, eventView(&snap.Warnings[i]))
		}
		writeJSON(w, 200, map[string]any{
			"summary":    summarize(c),
			"history":    c.History("cluster"),
			"nodes":      nodeViews(c, snap, false),
			"namespaces": namespaceViews(snap),
			"topCPU":     top(func(a, b PodView) bool { return a.CPU.Used > b.CPU.Used }),
			"topMem":     top(func(a, b PodView) bool { return a.Mem.Used > b.Mem.Used }),
			"problems":   problems,
			"warnings":   warn,
		})
	})

	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		writeJSON(w, 200, map[string]any{"nodes": nodeViews(c, snap, false), "metrics": snap.MetricsAvailable})
	})

	mux.HandleFunc("GET /api/node", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		name := r.URL.Query().Get("name")
		var nv *NodeView
		for _, v := range nodeViews(c, snap, true) {
			if v.Name == name {
				v := v
				nv = &v
			}
		}
		if nv == nil {
			writeErr(w, 404, errors.New("node not found"))
			return
		}
		pods := []PodView{}
		for i := range snap.Pods {
			if snap.Pods[i].Spec.NodeName == name {
				pods = append(pods, podView(snap, &snap.Pods[i], false))
			}
		}
		sort.Slice(pods, func(i, j int) bool { return pods[i].CPU.Used > pods[j].CPU.Used })
		evs := s.liveEvents(r.Context(), c, "", "Node", name)
		writeJSON(w, 200, map[string]any{"node": nv, "pods": pods, "history": c.History("node/" + name), "events": evs})
	})

	mux.HandleFunc("GET /api/namespaces", func(w http.ResponseWriter, r *http.Request) {
		_, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		writeJSON(w, 200, namespaceViews(snap))
	})

	mux.HandleFunc("GET /api/workloads", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ns, kind := r.URL.Query().Get("ns"), r.URL.Query().Get("kind")
		byOwner := podsByOwner(snap)
		out := []WorkloadView{}
		for _, wr := range workloadRefs(snap) {
			if (ns == "" || wr.ns == ns) && (kind == "" || wr.kind == kind) {
				out = append(out, workloadView(c, snap, wr, byOwner))
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Namespace != out[j].Namespace {
				return out[i].Namespace < out[j].Namespace
			}
			return out[i].Name < out[j].Name
		})
		writeJSON(w, 200, map[string]any{"workloads": out, "metrics": snap.MetricsAvailable})
	})

	mux.HandleFunc("GET /api/workload", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		wr := findWorkload(snap, q.Get("kind"), q.Get("ns"), q.Get("name"))
		if wr == nil {
			writeErr(w, 404, errors.New("workload not found (it may have just been created — refresh in a few seconds)"))
			return
		}
		v := workloadView(c, snap, *wr, nil)
		pods := []PodView{}
		names := map[string]bool{wr.name: true}
		for _, p := range workloadPods(snap, wr.kind, wr.ns, wr.name, wr.sel) {
			pods = append(pods, podView(snap, p, false))
			names[p.Metadata.Name] = true
			for _, o := range p.Metadata.OwnerReferences {
				names[o.Name] = true
			}
		}
		var conds []Condition
		strategy := ""
		if wr.kind == "Deployment" {
			for i := range snap.Deployments {
				d := &snap.Deployments[i]
				if d.Metadata.Namespace == wr.ns && d.Metadata.Name == wr.name {
					conds, strategy = d.Status.Conditions, d.Spec.Strategy.Type
				}
			}
		}
		evs := []EventView{}
		for _, e := range s.liveEvents(r.Context(), c, wr.ns, "", "") {
			if names[e.Name] {
				evs = append(evs, e)
			}
		}
		containers := []map[string]any{}
		for _, ct := range wr.tpl.Spec.Containers {
			containers = append(containers, map[string]any{
				"name": ct.Name, "image": ct.Image,
				"requests": ct.Resources.Requests, "limits": ct.Resources.Limits,
			})
		}
		writeJSON(w, 200, map[string]any{
			"workload": v, "pods": pods, "conditions": conds, "strategy": strategy,
			"events": evs, "containers": containers, "labels": wr.meta.Labels,
			"history": c.History("wl/" + wr.kind + "/" + wr.ns + "/" + wr.name),
		})
	})

	mux.HandleFunc("GET /api/pods", func(w http.ResponseWriter, r *http.Request) {
		_, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ns := r.URL.Query().Get("ns")
		pods, stale := snap.Pods, false
		if r.URL.Query().Get("fresh") == "1" {
			// live list so 1s/5s polling shows real changes, not the 15s snapshot
			if c := s.reg.Get(r.URL.Query().Get("ctx")); c != nil && c.client != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
				if fp, err := c.FreshPods(ctx, ns); err == nil {
					pods = fp
				} else {
					stale = true
				}
				cancel()
			}
		}
		out := make([]PodView, 0, len(pods))
		for i := range pods {
			if ns == "" || pods[i].Metadata.Namespace == ns {
				out = append(out, podView(snap, &pods[i], false))
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Namespace != out[j].Namespace {
				return out[i].Namespace < out[j].Namespace
			}
			return out[i].Name < out[j].Name
		})
		writeJSON(w, 200, map[string]any{"pods": out, "metrics": snap.MetricsAvailable, "stale": stale, "at": time.Now()})
	})

	mux.HandleFunc("GET /api/pod", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ns, name := r.URL.Query().Get("ns"), r.URL.Query().Get("name")
		for i := range snap.Pods {
			p := &snap.Pods[i]
			if p.Metadata.Namespace == ns && p.Metadata.Name == name {
				writeJSON(w, 200, map[string]any{
					"pod": podView(snap, p, true), "statuses": p.Status.ContainerStatuses,
					"conditions": p.Status.Conditions, "spec": p.Spec,
					"history": c.History("pod/" + ns + "/" + name),
					"events":  s.liveEvents(r.Context(), c, ns, "Pod", name),
				})
				return
			}
		}
		writeErr(w, 404, errors.New("pod not found"))
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		writeJSON(w, 200, s.liveEvents(r.Context(), c, r.URL.Query().Get("ns"), "", ""))
	})

	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		writeJSON(w, 200, c.History(r.URL.Query().Get("key")))
	})

	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		s.handleLogs(w, r, c)
	})

	mux.HandleFunc("GET /api/logs/download", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		s.handleLogDownload(w, r, c)
	})

	s.resourceRoutes(mux)
	s.kubeconfigRoutes(mux)
	s.routeRoutes(mux)
	s.terminalRoutes(mux)
	s.pvcRoutes(mux)
	s.overviewRoutes(mux)
	s.nsDashRoutes(mux)
	s.connRoutes(mux)
	s.ftRoutes(mux)
	s.dnsRoutes(mux)

	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"version": version, "readOnly": s.readOnly, "shell": s.shellAllowed(), "os": runtime.GOOS})
	})

	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	return s.guard(mux)
}

// liveEvents reads events straight from the API (newest first, max 300).
func (s *Server) liveEvents(ctx context.Context, c *Cluster, ns, kind, name string) []EventView {
	if snap := c.Snap(); ns == "" && snap != nil && snap.Scope == "namespaced" {
		// no cluster-wide permission: gather from each allowed namespace
		var all []EventView
		for _, n := range snap.ScopeNamespaces {
			all = append(all, s.liveEvents(ctx, c, n, kind, name)...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].When.After(all[j].When) })
		if len(all) > 300 {
			all = all[:300]
		}
		if all == nil {
			all = []EventView{}
		}
		return all
	}
	path := "/api/v1/events"
	if ns != "" {
		path = "/api/v1/namespaces/" + url.PathEscape(ns) + "/events"
	}
	q := url.Values{}
	if name != "" {
		fs := "involvedObject.name=" + name
		if kind != "" {
			fs += ",involvedObject.kind=" + kind
		}
		q.Set("fieldSelector", fs)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	evs, err := list[Event](ctx, c.client, path, q)
	out := []EventView{}
	if err != nil {
		return out
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].When().After(evs[j].When()) })
	for i := range evs {
		if i >= 300 {
			break
		}
		out = append(out, eventView(&evs[i]))
	}
	return out
}

// guard: localhost-only by default (with DNS-rebinding protection), or a bearer
// token / ?token= when bound to other interfaces.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			// liveness/readiness for Docker and Kubernetes: no token, no cluster data
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(`{"status":"ok","version":"` + version + `"}`))
			return
		}
		if s.localOnly {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "[::1]" {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		if s.token != "" {
			got := ""
			if c, err := r.Cookie("kubelamp_token"); err == nil {
				got = c.Value
			}
			if t := r.URL.Query().Get("token"); t != "" {
				got = t
				http.SetCookie(w, &http.Cookie{Name: "kubelamp_token", Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
				got = strings.TrimPrefix(a, "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				http.Error(w, "unauthorized: open the URL printed at startup (it contains ?token=…)", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request blocked", http.StatusForbidden)
					return
				}
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- settings

type settings struct {
	Kubeconfig []string                  `json:"kubeconfig"`
	Access     map[string]AccessSettings `json:"access,omitempty"` // per context
}

func settingsPath(dir string) string { return filepath.Join(dir, "settings.json") }

func loadSettings(dir string) settings {
	var st settings
	if b, err := os.ReadFile(settingsPath(dir)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveSettings(dir string, st settings) {
	_ = os.MkdirAll(dir, 0o700)
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(settingsPath(dir), b, 0o600)
}
