package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Namespace → Overview: a dashboard for one namespace (KubeLamp 1.5). Everything comes from the
// cluster snapshot that is already collected, plus a handful of small reads for this namespace
// only (quotas, volume claims, object counts, events).

type NSCountDef struct {
	Key, Label, Group, Version, Resource string
}

// objects counted on the namespace page; the UI links each to its list
var nsCountDefs = []NSCountDef{
	{"services", "Services", "", "v1", "services"},
	{"ingresses", "Ingresses", "networking.k8s.io", "v1", "ingresses"},
	{"httproutes", "HTTPRoutes", "gateway.networking.k8s.io", "v1", "httproutes"},
	{"configmaps", "ConfigMaps", "", "v1", "configmaps"},
	{"secrets", "Secrets", "", "v1", "secrets"},
	{"pvcs", "Volume claims", "", "v1", "persistentvolumeclaims"},
	{"jobs", "Jobs", "batch", "v1", "jobs"},
	{"cronjobs", "CronJobs", "batch", "v1", "cronjobs"},
	{"hpas", "Autoscalers", "autoscaling", "v2", "horizontalpodautoscalers"},
	{"netpols", "Network policies", "networking.k8s.io", "v1", "networkpolicies"},
	{"ciliumpols", "Cilium policies", "cilium.io", "v2", "ciliumnetworkpolicies"},
	{"serviceaccounts", "Service accounts", "", "v1", "serviceaccounts"},
}

type NSCount struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Group    string `json:"group"`
	Resource string `json:"resource"`
	Count    *int   `json:"count"`             // nil: not readable / not installed
	Err      string `json:"err,omitempty"`     // forbidden, …
	Missing  bool   `json:"missing,omitempty"` // the API is not served by this cluster
}

type NSQuotaItem struct {
	Resource string  `json:"resource"`
	Hard     string  `json:"hard"`
	Used     string  `json:"used"`
	Pct      float64 `json:"pct"` // used/hard when both parse, else -1
}

type NSQuota struct {
	Name  string        `json:"name"`
	Items []NSQuotaItem `json:"items"`
}

type NSPVC struct {
	Name         string    `json:"name"`
	Phase        string    `json:"phase"`
	StorageClass string    `json:"storageClass"`
	Requested    float64   `json:"requested"`
	Usage        *PVCUsage `json:"usage,omitempty"` // a cached reading, if any
}

type NSDash struct {
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	Created    *time.Time        `json:"created,omitempty"`
	Labels     map[string]string `json:"labels"`
	Summary    NamespaceView     `json:"summary"`
	Phases     map[string]int    `json:"phases"` // pod phase → count
	Restarts   int               `json:"restarts"`
	Containers int               `json:"containers"`
	Metrics    bool              `json:"metrics"`
	History    HistoryView       `json:"history"`
	Workloads  []WorkloadView    `json:"workloads"`
	TopCPU     []PodView         `json:"topCPU"`
	TopMem     []PodView         `json:"topMem"`
	Problems   []PodView         `json:"problems"`
	Nodes      []NSNode          `json:"nodes"`
	Counts     []NSCount         `json:"counts"`
	Quotas     []NSQuota         `json:"quotas"`
	QuotaErr   string            `json:"quotaErr,omitempty"`
	PVCs       []NSPVC           `json:"pvcs"`
	PVCErr     string            `json:"pvcErr,omitempty"`
	Health     NSOverview        `json:"health"`
	Events     []EventView       `json:"events"`
	Warnings1h int               `json:"warnings1h"`
	WarnPct    float64           `json:"warnPct"`
	CritPct    float64           `json:"critPct"`
	CanExec    bool              `json:"exec"`
	At         time.Time         `json:"at"`
}

type NSNode struct {
	Node string `json:"node"`
	Pods int    `json:"pods"`
}

// countObjects returns how many objects of a kind the namespace has, reading metadata only
// (limit=1 plus remainingItemCount), so no Secret contents ever reach KubeLamp.
func countObjects(ctx context.Context, c *Client, ns string, d NSCountDef) NSCount {
	out := NSCount{Key: d.Key, Label: d.Label, Group: d.Group, Resource: d.Resource}
	base := "/api/" + d.Version
	if d.Group != "" {
		base = "/apis/" + d.Group + "/" + d.Version
	}
	path := base + "/namespaces/" + url.PathEscape(ns) + "/" + d.Resource
	resp, err := c.request(ctx, http.MethodGet, path, url.Values{"limit": {"1"}}, nil, "",
		"application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1,application/json")
	if err != nil {
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Code == 404:
			out.Missing = true
		case errors.As(err, &ae) && ae.Code == 403:
			out.Err = "no permission"
		default:
			out.Err = friendlyErr(err)
		}
		return out
	}
	defer resp.Body.Close()
	var l struct {
		Metadata struct {
			RemainingItemCount *int `json:"remainingItemCount"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		out.Err = "unreadable answer"
		return out
	}
	n := len(l.Items)
	if l.Metadata.RemainingItemCount != nil {
		n += *l.Metadata.RemainingItemCount
	}
	out.Count = &n
	return out
}

// quantityPct compares two resource quantities ("2", "500m", "4Gi", "10") as used/hard × 100.
func quantityPct(used, hard string) float64 {
	h := parseQuantity(hard)
	if h <= 0 || used == "" {
		return -1
	}
	return parseQuantity(used) / h * 100
}

func (s *Server) buildNSDash(ctx context.Context, c *Cluster, snap *Snapshot, ns string) (*NSDash, error) {
	d := &NSDash{Name: ns, Labels: map[string]string{}, Phases: map[string]int{}, Metrics: snap.MetricsAvailable,
		Workloads: []WorkloadView{}, TopCPU: []PodView{}, TopMem: []PodView{}, Problems: []PodView{}, Nodes: []NSNode{},
		Counts: []NSCount{}, Quotas: []NSQuota{}, PVCs: []NSPVC{}, Events: []EventView{},
		WarnPct: pvcWarnPct, CritPct: pvcCritPct, CanExec: !s.readOnly, At: time.Now()}
	found := false
	for _, n := range snap.Namespaces {
		if n.Metadata.Name == ns {
			found = true
			d.Phase = n.Status.Phase
			t := n.Metadata.CreationTimestamp
			if !t.IsZero() {
				d.Created = &t
			}
			for k, v := range n.Metadata.Labels {
				d.Labels[k] = v
			}
		}
	}
	for _, v := range namespaceViews(snap) {
		if v.Name == ns {
			d.Summary = v
			found = true
		}
	}
	if !found {
		return nil, errors.New("namespace not found (or not readable with your credentials)")
	}
	if d.Phase == "" {
		d.Phase = "Active"
	}
	if q, ok := snap.Quotas[ns]; ok { // namespaced scope keeps the quota with the snapshot
		d.Summary.CPU.Quota, d.Summary.Mem.Quota = q.CPU, q.Mem
	}

	// pods
	nodes := map[string]int{}
	var pods []PodView
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Metadata.Namespace != ns {
			continue
		}
		ph := p.Status.Phase
		if ph == "" {
			ph = "Unknown"
		}
		d.Phases[ph]++
		for _, cs := range p.Status.ContainerStatuses {
			d.Restarts += cs.RestartCount
		}
		d.Containers += len(p.Spec.Containers)
		if ph == "Running" || ph == "Pending" {
			pv := podView(snap, p, false)
			pods = append(pods, pv)
			if pv.Problem {
				d.Problems = append(d.Problems, pv)
			}
			if p.Spec.NodeName != "" {
				nodes[p.Spec.NodeName]++
			}
		} else if ph == "Failed" && len(d.Problems) < 50 {
			d.Problems = append(d.Problems, podView(snap, p, false))
		}
	}
	top := func(less func(a, b PodView) bool) []PodView {
		cp := append([]PodView(nil), pods...)
		sort.SliceStable(cp, func(i, j int) bool { return less(cp[i], cp[j]) })
		if len(cp) > 8 {
			cp = cp[:8]
		}
		if cp == nil {
			cp = []PodView{}
		}
		return cp
	}
	d.TopCPU = top(func(a, b PodView) bool { return a.CPU.Used > b.CPU.Used })
	d.TopMem = top(func(a, b PodView) bool { return a.Mem.Used > b.Mem.Used })
	for n, k := range nodes {
		d.Nodes = append(d.Nodes, NSNode{n, k})
	}
	sort.Slice(d.Nodes, func(i, j int) bool {
		if d.Nodes[i].Pods != d.Nodes[j].Pods {
			return d.Nodes[i].Pods > d.Nodes[j].Pods
		}
		return d.Nodes[i].Node < d.Nodes[j].Node
	})

	// workloads
	byOwner := podsByOwner(snap)
	for _, wr := range workloadRefs(snap) {
		if wr.ns == ns {
			d.Workloads = append(d.Workloads, workloadView(c, snap, wr, byOwner))
		}
	}
	sort.SliceStable(d.Workloads, func(i, j int) bool {
		hi, hj := d.Workloads[i].Health != "healthy", d.Workloads[j].Health != "healthy"
		if hi != hj {
			return hi
		}
		if d.Workloads[i].Kind != d.Workloads[j].Kind {
			return d.Workloads[i].Kind < d.Workloads[j].Kind
		}
		return d.Workloads[i].Name < d.Workloads[j].Name
	})
	d.History = c.History("ns/" + ns)

	// small reads for this namespace, in parallel
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	counts := make([]NSCount, len(nsCountDefs))
	for i, def := range nsCountDefs {
		wg.Add(1)
		go func(i int, def NSCountDef) {
			defer wg.Done()
			counts[i] = countObjects(rctx, c.client, ns, def)
		}(i, def)
	}
	var pvcs []PVC
	var pvcErr, quotaErr error
	var quotas []ResourceQuota
	var events []EventView
	wg.Add(3)
	go func() { defer wg.Done(); pvcs, pvcErr = s.listPVCs(rctx, c, ns) }()
	go func() {
		defer wg.Done()
		quotas, quotaErr = list[ResourceQuota](rctx, c.client, "/api/v1/namespaces/"+url.PathEscape(ns)+"/resourcequotas", nil)
	}()
	go func() { defer wg.Done(); events = s.liveEvents(rctx, c, ns, "", "") }()
	wg.Wait()
	for _, ct := range counts {
		if !ct.Missing {
			d.Counts = append(d.Counts, ct)
		}
	}

	if pvcErr != nil {
		d.PVCErr = friendlyErr(pvcErr)
	}
	for i := range pvcs {
		p := &pvcs[i]
		np := NSPVC{Name: p.Metadata.Name, Phase: p.Status.Phase, Requested: parseQuantity(p.Spec.Resources.Requests["storage"])}
		if p.Spec.StorageClassName != nil {
			np.StorageClass = *p.Spec.StorageClassName
		}
		if u, ok := c.pvcPeek(ns, p.Metadata.Name); ok {
			u := u
			np.Usage = &u
		}
		d.PVCs = append(d.PVCs, np)
	}
	sort.Slice(d.PVCs, func(i, j int) bool { return d.PVCs[i].Name < d.PVCs[j].Name })

	if quotaErr != nil {
		var ae *APIError
		if !(errors.As(quotaErr, &ae) && ae.Code == 404) {
			d.QuotaErr = friendlyErr(quotaErr)
		}
	}
	for _, q := range quotas {
		nq := NSQuota{Name: q.Metadata.Name}
		keys := make([]string, 0, len(q.Status.Hard))
		for k := range q.Status.Hard {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			nq.Items = append(nq.Items, NSQuotaItem{Resource: k, Hard: q.Status.Hard[k], Used: q.Status.Used[k], Pct: quantityPct(q.Status.Used[k], q.Status.Hard[k])})
		}
		d.Quotas = append(d.Quotas, nq)
		// the first quota with CPU/memory becomes the capacity line of the charts
		if d.Summary.CPU.Quota == 0 {
			for _, k := range []string{"requests.cpu", "cpu", "limits.cpu"} {
				if v := q.Status.Hard[k]; v != "" {
					d.Summary.CPU.Quota = parseQuantity(v)
					break
				}
			}
		}
		if d.Summary.Mem.Quota == 0 {
			for _, k := range []string{"requests.memory", "memory", "limits.memory"} {
				if v := q.Status.Hard[k]; v != "" {
					d.Summary.Mem.Quota = parseQuantity(v)
					break
				}
			}
		}
	}

	// health items for this namespace (same rules as Namespace → Health)
	byNS, get := nsBase(snap)
	s.addPVCItems(c, get, pvcs)
	for _, o := range finalizeNS(byNS) {
		if o.Name == ns {
			d.Health = o
		}
	}
	if d.Health.Name == "" {
		d.Health = NSOverview{Name: ns, Sev: "ok", Items: []AttnItem{}}
	}

	sort.SliceStable(events, func(i, j int) bool {
		if (events[i].Type == "Warning") != (events[j].Type == "Warning") {
			return events[i].Type == "Warning"
		}
		return events[i].When.After(events[j].When)
	})
	for _, e := range events {
		if e.Type == "Warning" && time.Since(e.When) < time.Hour {
			d.Warnings1h++
		}
	}
	if len(events) > 40 {
		events = events[:40]
	}
	if events != nil {
		d.Events = events
	}
	return d, nil
}

func (s *Server) nsDashRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/nsdash", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ns := strings.TrimSpace(r.URL.Query().Get("ns"))
		if ns == "" {
			writeErr(w, 400, errors.New("namespace required"))
			return
		}
		d, err := s.buildNSDash(r.Context(), c, snap, ns)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, d)
	})
}
