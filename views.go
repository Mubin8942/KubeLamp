package main

import (
	"sort"
	"strings"
	"time"
)

type Res struct {
	Quota float64 `json:"quota,omitempty"` // ResourceQuota hard limit (namespaced scope)
	Used  float64 `json:"used"`
	Req   float64 `json:"req"`
	Lim   float64 `json:"lim"`
	Alloc float64 `json:"alloc"`
	Cap   float64 `json:"cap"`
}

type ClusterSummary struct {
	Attention        *AttentionDigest `json:"attention,omitempty"`
	Name             string           `json:"name"`
	Server           string           `json:"server"`
	AuthType         string           `json:"authType"`
	Current          bool             `json:"current"`
	Namespace        string           `json:"namespace"`
	Status           string           `json:"status"` // ok | partial | error | loading
	Error            string           `json:"error,omitempty"`
	Version          string           `json:"version,omitempty"`
	At               time.Time        `json:"at"`
	MetricsAvailable bool             `json:"metricsAvailable"`
	MetricsErr       string           `json:"metricsErr,omitempty"`
	NodesReady       int              `json:"nodesReady"`
	NodesTotal       int              `json:"nodesTotal"`
	Pods             PodCounts        `json:"pods"`
	DeployHealthy    int              `json:"deployHealthy"`
	DeployTotal      int              `json:"deployTotal"`
	Namespaces       int              `json:"namespaces"`
	CPU              Res              `json:"cpu"`
	Mem              Res              `json:"mem"`
	Warnings1h       int              `json:"warnings1h"`
	Spark            HistoryView      `json:"spark"`
	Certs            *CertReport      `json:"certs,omitempty"`
	Scope            string           `json:"scope"`           // cluster | namespaced
	ScopeNamespaces  []string         `json:"scopeNamespaces"` // when namespaced
	ScopeFoundBy     string           `json:"scopeFoundBy"`    // rancher | namespaces | "" (typed in)
	AccessMode       string           `json:"accessMode"`      // configured: auto | cluster | namespaced
	AccessNamespaces []string         `json:"accessNamespaces"`
}

type PodCounts struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
	Succeeded int `json:"succeeded"`
	Problem   int `json:"problem"` // CrashLoopBackOff, ImagePullBackOff, Error, not ready…
}

func isProblem(p *Pod) bool {
	st := podStatus(p)
	switch st {
	case "Running", "Succeeded", "Completed", "Terminating", "ContainerCreating", "PodInitializing":
		if st == "Running" {
			r, t := podReady(p)
			// running but not ready for more than 2 minutes
			if r < t && p.Status.StartTime != nil && time.Since(*p.Status.StartTime) > 2*time.Minute {
				return true
			}
		}
		return false
	case "Pending":
		return p.Status.StartTime == nil && time.Since(p.Metadata.CreationTimestamp) > 5*time.Minute
	}
	return true
}

func summarize(c *Cluster) ClusterSummary {
	s := c.Snap()
	sum := ClusterSummary{
		Name: c.Info.Name, Server: c.Info.Server, AuthType: c.Info.AuthType,
		Current: c.Info.Current, Namespace: c.Info.Namespace, Status: "loading",
	}
	if c.client != nil {
		sum.Certs = c.client.CertReport()
	}
	acc := c.Access()
	sum.AccessMode, sum.AccessNamespaces = acc.Mode, acc.Namespaces
	if sum.AccessNamespaces == nil {
		sum.AccessNamespaces = []string{}
	}
	if s == nil {
		return sum
	}
	sum.At, sum.Version, sum.Error = s.At, s.Version, s.Err
	sum.MetricsAvailable, sum.MetricsErr = s.MetricsAvailable, s.MetricsErr
	sum.Scope, sum.ScopeNamespaces, sum.ScopeFoundBy = s.Scope, s.ScopeNamespaces, s.ScopeFoundBy
	// Quota is only a fair capacity line if every visible namespace has one;
	// otherwise usage from un-quota'd namespaces would be compared against it.
	if s.Scope == "namespaced" && len(s.Quotas) == len(s.ScopeNamespaces) {
		cpuAll, memAll := true, true
		for _, q := range s.Quotas {
			sum.CPU.Quota += q.CPU
			sum.Mem.Quota += q.Mem
			cpuAll = cpuAll && q.CPU > 0
			memAll = memAll && q.Mem > 0
		}
		if !cpuAll {
			sum.CPU.Quota = 0
		}
		if !memAll {
			sum.Mem.Quota = 0
		}
	}
	switch {
	case !s.Readable:
		sum.Status = "error"
		return sum
	case s.Err != "":
		sum.Status = "partial"
	default:
		sum.Status = "ok"
	}
	sum.NodesTotal = len(s.Nodes)
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if nodeReady(n) {
			sum.NodesReady++
		}
		if s.Scope == "namespaced" {
			continue // whole-cluster capacity isn't comparable with "my namespaces" usage
		}
		sum.CPU.Alloc += n.Status.Allocatable.cpu()
		sum.CPU.Cap += n.Status.Capacity.cpu()
		sum.Mem.Alloc += n.Status.Allocatable.mem()
		sum.Mem.Cap += n.Status.Capacity.mem()
	}
	if s.Scope != "namespaced" {
		for _, u := range s.NodeUsage {
			sum.CPU.Used += u.CPU
			sum.Mem.Used += u.Mem
		}
	} else { // namespaced scope: usage of the pods we can see
		for _, u := range s.PodUsage {
			sum.CPU.Used += u.CPU
			sum.Mem.Used += u.Mem
		}
	}
	for i := range s.Pods {
		p := &s.Pods[i]
		sum.Pods.Total++
		switch p.Status.Phase {
		case "Running":
			sum.Pods.Running++
		case "Pending":
			sum.Pods.Pending++
		case "Failed":
			sum.Pods.Failed++
		case "Succeeded":
			sum.Pods.Succeeded++
		}
		if isProblem(p) {
			sum.Pods.Problem++
		}
		if p.Status.Phase == "Running" || p.Status.Phase == "Pending" {
			rc, rm, lc, lm := podResources(&p.Spec)
			sum.CPU.Req += rc
			sum.Mem.Req += rm
			sum.CPU.Lim += lc
			sum.Mem.Lim += lm
		}
	}
	for i := range s.Deployments {
		if deploymentHealth(&s.Deployments[i]) == "healthy" {
			sum.DeployHealthy++
		}
	}
	sum.DeployTotal = len(s.Deployments)
	sum.Namespaces = len(s.Namespaces)
	for i := range s.Warnings {
		if time.Since(s.Warnings[i].When()) < time.Hour {
			sum.Warnings1h++
		}
	}
	sum.Spark = c.Spark("cluster", 60)
	att := attentionDigest(c, s)
	sum.Attention = &att
	return sum
}

func deploymentHealth(d *Deployment) string {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	for _, cnd := range d.Status.Conditions {
		if cnd.Type == "Progressing" && cnd.Reason == "ProgressDeadlineExceeded" {
			return "degraded"
		}
	}
	switch {
	case want == 0:
		return "scaled-down"
	case d.Status.AvailableReplicas >= want && d.Status.UpdatedReplicas >= want && d.Status.Replicas == want:
		return "healthy"
	case d.Status.AvailableReplicas == 0:
		return "degraded"
	}
	return "progressing"
}

// ---------------------------------------------------------------- nodes

type NodeView struct {
	Name          string            `json:"name"`
	Ready         bool              `json:"ready"`
	Roles         []string          `json:"roles"`
	Version       string            `json:"version"`
	OS            string            `json:"os"`
	Kernel        string            `json:"kernel"`
	Arch          string            `json:"arch"`
	Runtime       string            `json:"runtime"`
	InternalIP    string            `json:"internalIP"`
	Created       time.Time         `json:"created"`
	Unschedulable bool              `json:"unschedulable"`
	Taints        []string          `json:"taints"`
	Pressure      []string          `json:"pressure"`
	CPU           Res               `json:"cpu"`
	Mem           Res               `json:"mem"`
	Pods          int               `json:"pods"`
	PodCap        float64           `json:"podCap"`
	Labels        map[string]string `json:"labels,omitempty"`
	Spark         HistoryView       `json:"spark"`
}

func nodeViews(c *Cluster, s *Snapshot, withLabels bool) []NodeView {
	type acc struct {
		rc, rm, lc, lm float64
		pods           int
	}
	per := map[string]*acc{}
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		a := per[p.Spec.NodeName]
		if a == nil {
			a = &acc{}
			per[p.Spec.NodeName] = a
		}
		rc, rm, lc, lm := podResources(&p.Spec)
		a.rc += rc
		a.rm += rm
		a.lc += lc
		a.lm += lm
		a.pods++
	}
	out := make([]NodeView, 0, len(s.Nodes))
	for i := range s.Nodes {
		n := &s.Nodes[i]
		v := NodeView{
			Name: n.Metadata.Name, Ready: nodeReady(n), Roles: nodeRoles(n),
			Version: n.Status.NodeInfo.KubeletVersion, OS: n.Status.NodeInfo.OSImage,
			Kernel: n.Status.NodeInfo.KernelVersion, Arch: n.Status.NodeInfo.Architecture,
			Runtime: n.Status.NodeInfo.ContainerRuntimeVersion, Created: n.Metadata.CreationTimestamp,
			Unschedulable: n.Spec.Unschedulable, Taints: []string{}, Pressure: []string{},
			PodCap: parseQuantity(n.Status.Allocatable["pods"]),
		}
		for _, a := range n.Status.Addresses {
			if a.Type == "InternalIP" && v.InternalIP == "" {
				v.InternalIP = a.Address
			}
		}
		for _, t := range n.Spec.Taints {
			v.Taints = append(v.Taints, t.Key+":"+t.Effect)
		}
		for _, cnd := range n.Status.Conditions {
			if cnd.Type != "Ready" && cnd.Status == "True" {
				v.Pressure = append(v.Pressure, cnd.Type)
			}
		}
		v.CPU = Res{Alloc: n.Status.Allocatable.cpu(), Cap: n.Status.Capacity.cpu()}
		v.Mem = Res{Alloc: n.Status.Allocatable.mem(), Cap: n.Status.Capacity.mem()}
		if u, ok := s.NodeUsage[v.Name]; ok {
			v.CPU.Used, v.Mem.Used = u.CPU, u.Mem
		}
		if a := per[v.Name]; a != nil {
			v.CPU.Req, v.Mem.Req, v.CPU.Lim, v.Mem.Lim, v.Pods = a.rc, a.rm, a.lc, a.lm, a.pods
		}
		if withLabels {
			v.Labels = n.Metadata.Labels
		}
		v.Spark = c.Spark("node/"+v.Name, 40)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------------------------------------------------------------- pods

type PodView struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Phase       string            `json:"phase"`
	Problem     bool              `json:"problem"`
	Ready       string            `json:"ready"`
	Restarts    int               `json:"restarts"`
	LastReason  string            `json:"lastReason,omitempty"`
	LastExit    *int              `json:"lastExit,omitempty"`
	LastRestart *time.Time        `json:"lastRestart,omitempty"`
	Node        string            `json:"node"`
	IP          string            `json:"ip"`
	Created     time.Time         `json:"created"`
	Owner       string            `json:"owner"`
	QoS         string            `json:"qos"`
	CPU         Res               `json:"cpu"`
	Mem         Res               `json:"mem"`
	HasMetrics  bool              `json:"hasMetrics"`
	Containers  []string          `json:"containers"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func podView(s *Snapshot, p *Pod, withLabels bool) PodView {
	r, t := podReady(p)
	key := p.Metadata.Namespace + "/" + p.Metadata.Name
	v := PodView{
		Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, Status: podStatus(p),
		Phase: p.Status.Phase, Problem: isProblem(p), Ready: itoa(r) + "/" + itoa(t),
		Restarts: podRestarts(p), Node: p.Spec.NodeName, IP: p.Status.PodIP,
		Created: p.Metadata.CreationTimestamp, Owner: s.owner[key], QoS: p.Status.QOSClass,
	}
	// most recent container termination = "last restart"
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.LastState.Terminated; t != nil && (v.LastRestart == nil || t.FinishedAt.After(*v.LastRestart)) {
			fin, code := t.FinishedAt, t.ExitCode
			v.LastRestart, v.LastExit = &fin, &code
			v.LastReason = t.Reason
			if v.LastReason == "" {
				v.LastReason = "Error"
			}
		}
	}
	// the restart itself happened when the restarted container started running again
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > 0 && cs.State.Running != nil && (v.LastRestart == nil || cs.State.Running.StartedAt.After(*v.LastRestart)) {
			st := cs.State.Running.StartedAt
			v.LastRestart = &st
		}
	}
	for _, c := range p.Spec.Containers {
		v.Containers = append(v.Containers, c.Name)
	}
	v.CPU.Req, v.Mem.Req, v.CPU.Lim, v.Mem.Lim = podResources(&p.Spec)
	if u, ok := s.PodUsage[key]; ok {
		v.CPU.Used, v.Mem.Used, v.HasMetrics = u.CPU, u.Mem, true
	}
	if withLabels {
		v.Labels = p.Metadata.Labels
	}
	return v
}

// ---------------------------------------------------------------- workloads

type WorkloadView struct {
	Kind        string            `json:"kind"`
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Desired     int32             `json:"desired"`
	Ready       int32             `json:"ready"`
	Updated     int32             `json:"updated"`
	Available   int32             `json:"available"`
	Health      string            `json:"health"`
	Created     time.Time         `json:"created"`
	Images      []string          `json:"images"`
	Selector    string            `json:"selector"`
	MatchLabels map[string]string `json:"matchLabels"`
	Pods        int               `json:"pods"`
	Restarts    int               `json:"restarts"`
	CPU         Res               `json:"cpu"`
	Mem         Res               `json:"mem"`
	Spark       HistoryView       `json:"spark"`
	Containers  []string          `json:"containers"`
}

type workloadRef struct {
	kind, ns, name string
	sel            LabelSelector
	tpl            *PodTemplate
	meta           *Meta
}

func workloadRefs(s *Snapshot) []workloadRef {
	var out []workloadRef
	for i := range s.Deployments {
		d := &s.Deployments[i]
		out = append(out, workloadRef{"Deployment", d.Metadata.Namespace, d.Metadata.Name, d.Spec.Selector, &d.Spec.Template, &d.Metadata})
	}
	for i := range s.StatefulSets {
		d := &s.StatefulSets[i]
		out = append(out, workloadRef{"StatefulSet", d.Metadata.Namespace, d.Metadata.Name, d.Spec.Selector, &d.Spec.Template, &d.Metadata})
	}
	for i := range s.DaemonSets {
		d := &s.DaemonSets[i]
		out = append(out, workloadRef{"DaemonSet", d.Metadata.Namespace, d.Metadata.Name, d.Spec.Selector, &d.Spec.Template, &d.Metadata})
	}
	return out
}

func findWorkload(s *Snapshot, kind, ns, name string) *workloadRef {
	for _, w := range workloadRefs(s) {
		if w.kind == kind && w.ns == ns && w.name == name {
			w := w
			return &w
		}
	}
	return nil
}

// workloadPods returns pods owned by the workload (via ownerRefs), falling back to
// selector matching for pods that have no owner info.
func workloadPods(s *Snapshot, kind, ns, name string, sel LabelSelector) []*Pod {
	want := kind + "/" + name
	var out []*Pod
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.Metadata.Namespace != ns {
			continue
		}
		o := s.owner[ns+"/"+p.Metadata.Name]
		if o == want || (o == "" && sel.Matches(p.Metadata.Labels)) {
			out = append(out, p)
		}
	}
	return out
}

func workloadView(c *Cluster, s *Snapshot, w workloadRef, podsByOwner map[string][]*Pod) WorkloadView {
	v := WorkloadView{Kind: w.kind, Namespace: w.ns, Name: w.name, Created: w.meta.CreationTimestamp,
		Selector: w.sel.String(), MatchLabels: w.sel.MatchLabels, Images: []string{}}
	for _, ct := range w.tpl.Spec.Containers {
		v.Images = append(v.Images, ct.Image)
		v.Containers = append(v.Containers, ct.Name)
	}
	switch w.kind {
	case "Deployment":
		for i := range s.Deployments {
			d := &s.Deployments[i]
			if d.Metadata.Namespace == w.ns && d.Metadata.Name == w.name {
				v.Desired = 1
				if d.Spec.Replicas != nil {
					v.Desired = *d.Spec.Replicas
				}
				v.Ready, v.Updated, v.Available = d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas
				v.Health = deploymentHealth(d)
			}
		}
	case "StatefulSet":
		for i := range s.StatefulSets {
			d := &s.StatefulSets[i]
			if d.Metadata.Namespace == w.ns && d.Metadata.Name == w.name {
				v.Desired = 1
				if d.Spec.Replicas != nil {
					v.Desired = *d.Spec.Replicas
				}
				v.Ready, v.Updated, v.Available = d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas
			}
		}
	case "DaemonSet":
		for i := range s.DaemonSets {
			d := &s.DaemonSets[i]
			if d.Metadata.Namespace == w.ns && d.Metadata.Name == w.name {
				v.Desired, v.Ready, v.Updated, v.Available = d.Status.DesiredNumberScheduled, d.Status.NumberReady, d.Status.UpdatedNumberScheduled, d.Status.NumberAvailable
			}
		}
	}
	if v.Health == "" {
		switch {
		case v.Desired == 0:
			v.Health = "scaled-down"
		case v.Ready >= v.Desired && v.Updated >= v.Desired:
			v.Health = "healthy"
		case v.Ready == 0:
			v.Health = "degraded"
		default:
			v.Health = "progressing"
		}
	}
	var pods []*Pod
	if podsByOwner != nil {
		pods = podsByOwner[w.kind+"/"+w.ns+"/"+w.name]
	} else {
		pods = workloadPods(s, w.kind, w.ns, w.name, w.sel)
	}
	v.Pods = len(pods)
	for _, p := range pods {
		v.Restarts += podRestarts(p)
		rc, rm, lc, lm := podResources(&p.Spec)
		v.CPU.Req += rc
		v.Mem.Req += rm
		v.CPU.Lim += lc
		v.Mem.Lim += lm
		if u, ok := s.PodUsage[p.Metadata.Namespace+"/"+p.Metadata.Name]; ok {
			v.CPU.Used += u.CPU
			v.Mem.Used += u.Mem
		}
	}
	v.Spark = c.Spark("wl/"+w.kind+"/"+w.ns+"/"+w.name, 40)
	return v
}

func podsByOwner(s *Snapshot) map[string][]*Pod {
	m := map[string][]*Pod{}
	for i := range s.Pods {
		p := &s.Pods[i]
		if o := s.owner[p.Metadata.Namespace+"/"+p.Metadata.Name]; o != "" {
			kind, name, _ := strings.Cut(o, "/")
			k := kind + "/" + p.Metadata.Namespace + "/" + name
			m[k] = append(m[k], p)
		}
	}
	return m
}

// ---------------------------------------------------------------- events

type EventView struct {
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Count     int       `json:"count"`
	When      time.Time `json:"when"`
	Source    string    `json:"source"`
}

func eventView(e *Event) EventView {
	n := e.Count
	if e.Series != nil && e.Series.Count > n {
		n = e.Series.Count
	}
	if n == 0 {
		n = 1
	}
	return EventView{Type: e.Type, Reason: e.Reason, Message: e.Message, Kind: e.InvolvedObject.Kind,
		Name: e.InvolvedObject.Name, Namespace: e.InvolvedObject.Namespace, Count: n, When: e.When(), Source: e.Source.Component}
}

// ---------------------------------------------------------------- namespaces

type NamespaceView struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Pods    int    `json:"pods"`
	CPU     Res    `json:"cpu"`
	Mem     Res    `json:"mem"`
	Problem int    `json:"problem"`
}

func namespaceViews(s *Snapshot) []NamespaceView {
	m := map[string]*NamespaceView{}
	for _, n := range s.Namespaces {
		m[n.Metadata.Name] = &NamespaceView{Name: n.Metadata.Name, Phase: n.Status.Phase}
	}
	for i := range s.Pods {
		p := &s.Pods[i]
		v := m[p.Metadata.Namespace]
		if v == nil {
			v = &NamespaceView{Name: p.Metadata.Namespace}
			m[p.Metadata.Namespace] = v
		}
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		v.Pods++
		if isProblem(p) {
			v.Problem++
		}
		rc, rm, lc, lm := podResources(&p.Spec)
		v.CPU.Req += rc
		v.Mem.Req += rm
		v.CPU.Lim += lc
		v.Mem.Lim += lm
		if u, ok := s.PodUsage[p.Metadata.Namespace+"/"+p.Metadata.Name]; ok {
			v.CPU.Used += u.CPU
			v.Mem.Used += u.Mem
		}
	}
	out := make([]NamespaceView, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPU.Used != out[j].CPU.Used {
			return out[i].CPU.Used > out[j].CPU.Used
		}
		return out[i].Name < out[j].Name
	})
	return out
}
