package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Checking one rule: which objects match its condition right now. Pod problems are
// grouped by the workload that owns the pods, so a crashing Deployment is one alert
// (listing its pods) and a rollout that replaces the pods doesn't start a new one.

type alertHit struct {
	Key     string // object key, unique within the rule
	NS      string
	Object  string // "Deployment/api", "Node/w01"
	Summary string
}

// errNoData: the cluster's data can't be read right now. Alerts keep their state (nothing
// resolves) until it can.
type errNoData struct{ why string }

func (e errNoData) Error() string { return e.why }

type evalEnv struct {
	ctx      context.Context
	srv      *Server
	c        *Cluster
	snap     *Snapshot
	now      time.Time
	restarts *restartBook
}

type ruleScope struct {
	ns    []string
	kind  string
	names *nameMatcher
	sel   *LabelSelector
}

func scopeOf(r *AlertRule) *ruleScope {
	sc := &ruleScope{ns: r.Namespaces, kind: r.Kind}
	sc.names, _ = newNameMatcher(r.Workload)
	if r.Labels != "" {
		if s, err := parseSelector(r.Labels); err == nil {
			sc.sel = &s
		}
	}
	return sc
}

func (sc *ruleScope) nsOK(ns string) bool {
	if len(sc.ns) == 0 {
		return true
	}
	for _, p := range sc.ns {
		if ok, _ := path.Match(p, ns); ok {
			return true
		}
	}
	return false
}

func (sc *ruleScope) labelsOK(l map[string]string) bool { return sc.sel == nil || sc.sel.Matches(l) }

// the namespaces to list one by one (rule names plain namespaces), or nil for all
func (sc *ruleScope) literalNS() []string {
	for _, p := range sc.ns {
		if strings.ContainsAny(p, "*?[") {
			return nil
		}
	}
	return sc.ns
}

var cronRunRe = regexp.MustCompile(`^(.+?)-(\d{6,}|manual-\d{8}-\d{6})$`)

// cronOf returns the CronJob a Job name was made from ("" when it doesn't look like one).
func cronOf(job string) string {
	if m := cronRunRe.FindStringSubmatch(job); m != nil {
		return m[1]
	}
	return ""
}

// ---------------------------------------------------------------- restart counting

type restartSample struct {
	at    time.Time
	count int
}

type restartBook struct {
	mu sync.Mutex
	m  map[string][]restartSample
}

func newRestartBook() *restartBook { return &restartBook{m: map[string][]restartSample{}} }

// delta records a sample and returns the restarts within the window. A pod seen for the
// first time counts from now, unless it started inside the window.
func (b *restartBook) delta(key string, now time.Time, count int, created time.Time, window time.Duration) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.m[key]
	if len(s) == 0 || now.Sub(s[len(s)-1].at) >= 10*time.Second {
		s = append(s, restartSample{now, count})
	}
	for len(s) > 1 && now.Sub(s[0].at) > 2*time.Hour {
		s = s[1:]
	}
	b.m[key] = s
	if !created.IsZero() && now.Sub(created) <= window {
		return count
	}
	// the count at the start of the window: the last sample taken before it, else the
	// first one inside it
	base := s[0].count
	for _, x := range s {
		if now.Sub(x.at) < window {
			break
		}
		base = x.count
	}
	if base > count { // the counter went down (statuses were reset): count from zero
		base = 0
	}
	return count - base
}

// prune drops pods not seen for 2 hours
func (b *restartBook) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, s := range b.m {
		if now.Sub(s[len(s)-1].at) > 2*time.Hour {
			delete(b.m, k)
		}
	}
}

// ---------------------------------------------------------------- evaluation

func evalRule(env *evalEnv, r *AlertRule) ([]alertHit, error) {
	d := condByID[r.Condition]
	if d == nil {
		return nil, fmt.Errorf("unknown condition %q", r.Condition)
	}
	snap := env.snap
	if snap == nil {
		return nil, errNoData{"the cluster has not been read yet"}
	}
	if d.Target == "cluster" {
		return evalCluster(env, r, d)
	}
	if !snap.Readable {
		return nil, errNoData{"cannot read the cluster: " + snap.Err}
	}
	if d.Metrics && !snap.MetricsAvailable {
		return nil, errNoData{"no metrics: " + firstNonEmpty(snap.MetricsErr, "metrics-server is not answering")}
	}
	sc := scopeOf(r)
	switch d.Target {
	case "pod":
		return evalPods(env, r, sc), nil
	case "workload":
		return evalWorkloads(env, r, sc), nil
	case "node":
		if len(snap.Nodes) == 0 {
			return nil, errNoData{"your credentials for this cluster cannot list nodes"}
		}
		return evalNodes(env, r, sc), nil
	case "event":
		return evalEvents(env, r, sc), nil
	case "job", "cronjob":
		return evalJobs(env, r, sc, d)
	case "pvc":
		return evalPVCs(env, r, sc)
	case "externalsecret":
		return evalExternalSecrets(env, r, sc)
	}
	return nil, fmt.Errorf("condition %q has no check", r.Condition)
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// ---------------------------------------------------------------- pods

var (
	badWaiting = map[string]bool{"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true,
		"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true, "ContainerCannotRun": true}
	pullWaiting = map[string]bool{"ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true}
)

func evalPods(env *evalEnv, r *AlertRule, sc *ruleScope) []alertHit {
	snap, now := env.snap, env.now
	window := time.Duration(r.Window * float64(time.Minute))
	recent := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) <= window }
	var check func(p *Pod) string
	switch r.Condition {
	case "pod_error":
		check = func(p *Pod) string {
			if p.Status.Phase == "Failed" {
				return "failed: " + short(firstNonEmpty(p.Status.Reason, p.Status.Message, "Failed"), 80)
			}
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && badWaiting[w.Reason] {
					return fmt.Sprintf("%s (%s)", w.Reason, cs.Name)
				}
				if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && p.Status.Phase != "Succeeded" {
					return fmt.Sprintf("%s exited with code %d (%s)", cs.Name, t.ExitCode, firstNonEmpty(t.Reason, "Error"))
				}
				if t := cs.LastState.Terminated; t != nil && t.ExitCode != 0 && recent(t.FinishedAt) {
					return fmt.Sprintf("%s exited with code %d (%s) %s ago", cs.Name, t.ExitCode, firstNonEmpty(t.Reason, "Error"), ago(now.Sub(t.FinishedAt)))
				}
			}
			return ""
		}
	case "pod_crashloop":
		check = func(p *Pod) string {
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
					return fmt.Sprintf("CrashLoopBackOff (%s, %d restarts)", cs.Name, cs.RestartCount)
				}
			}
			return ""
		}
	case "pod_imagepull":
		check = func(p *Pod) string {
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && pullWaiting[w.Reason] {
					return fmt.Sprintf("%s: %s", w.Reason, cs.Image)
				}
			}
			return ""
		}
	case "pod_oom":
		check = func(p *Pod) string {
			for _, cs := range p.Status.ContainerStatuses {
				for _, t := range []*struct {
					Reason     string    `json:"reason"`
					ExitCode   int       `json:"exitCode"`
					FinishedAt time.Time `json:"finishedAt"`
				}{cs.State.Terminated, cs.LastState.Terminated} {
					if t != nil && t.Reason == "OOMKilled" && recent(t.FinishedAt) {
						return fmt.Sprintf("%s OOMKilled %s ago", cs.Name, ago(now.Sub(t.FinishedAt)))
					}
				}
			}
			return ""
		}
	case "pod_restarts":
		check = func(p *Pod) string {
			key := env.c.Info.Name + "/" + p.Metadata.Namespace + "/" + p.Metadata.Name + "/" + p.Metadata.UID
			n := env.restarts.delta(key, now, podRestarts(p), p.Metadata.CreationTimestamp, window)
			if float64(n) >= r.Threshold {
				return fmt.Sprintf("%d restarts in %s", n, ago(window))
			}
			return ""
		}
	case "pod_pending":
		check = func(p *Pod) string {
			if p.Status.Phase != "Pending" || p.Metadata.DeletionTimestamp != nil {
				return ""
			}
			for _, c := range p.Status.Conditions {
				if c.Type == "PodScheduled" && c.Status == "False" {
					return "not scheduled: " + short(firstNonEmpty(c.Message, c.Reason), 90)
				}
			}
			return "Pending: " + podStatus(p)
		}
	case "pod_not_ready":
		check = func(p *Pod) string {
			if p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != nil {
				return ""
			}
			if rd, tot := podReady(p); rd < tot {
				return fmt.Sprintf("ready %d/%d", rd, tot)
			}
			return ""
		}
	case "pod_failed":
		check = func(p *Pod) string {
			if p.Status.Phase == "Failed" {
				return short(firstNonEmpty(p.Status.Reason, "Failed")+": "+p.Status.Message, 100)
			}
			return ""
		}
	case "pod_cpu", "pod_mem":
		cpu := r.Condition == "pod_cpu"
		check = func(p *Pod) string {
			if p.Status.Phase != "Running" {
				return ""
			}
			u, ok := snap.PodUsage[p.Metadata.Namespace+"/"+p.Metadata.Name]
			if !ok {
				return ""
			}
			reqC, reqM, limC, limM := podResources(&p.Spec)
			use, base, of := u.Mem, limM, "limit"
			if cpu {
				use, base = u.CPU, limC
			}
			if base == 0 {
				base, of = reqM, "request"
				if cpu {
					base = reqC
				}
			}
			if base == 0 {
				return ""
			}
			pct := use / base * 100
			if pct < r.Threshold {
				return ""
			}
			if cpu {
				return fmt.Sprintf("CPU %.0f%% of %s (%.2f of %.2f cores)", pct, of, use, base)
			}
			return fmt.Sprintf("memory %.0f%% of %s (%s of %s)", pct, of, fmtBytes(use), fmtBytes(base))
		}
	}
	if check == nil {
		return nil
	}
	type group struct {
		ns, object string
		pods       []string
	}
	groups := map[string]*group{}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		ns := p.Metadata.Namespace
		if !sc.nsOK(ns) || !sc.labelsOK(p.Metadata.Labels) {
			continue
		}
		okind, oname, _ := strings.Cut(snap.owner[ns+"/"+p.Metadata.Name], "/")
		switch {
		case sc.kind == "":
			if !sc.names.match(oname, p.Metadata.Name) {
				continue
			}
		case sc.kind == okind:
			if !sc.names.match(oname) {
				continue
			}
		case sc.kind == "CronJob" && okind == "Job" && cronOf(oname) != "":
			if !sc.names.match(cronOf(oname)) {
				continue
			}
		default:
			continue
		}
		why := check(p)
		if why == "" {
			continue
		}
		key, obj := "Pod/"+ns+"/"+p.Metadata.Name, "Pod/"+p.Metadata.Name
		if oname != "" && okind != "Node" {
			key, obj = okind+"/"+ns+"/"+oname, okind+"/"+oname
		}
		g := groups[key]
		if g == nil {
			g = &group{ns: ns, object: obj}
			groups[key] = g
		}
		g.pods = append(g.pods, p.Metadata.Name+" "+why)
	}
	var hits []alertHit
	for key, g := range groups {
		sort.Strings(g.pods)
		sum := "pod " + g.pods[0]
		if len(g.pods) > 1 {
			shown := g.pods
			if len(shown) > 3 {
				shown = shown[:3]
			}
			sum = fmt.Sprintf("%d pods: %s", len(g.pods), strings.Join(shown, "; "))
			if len(g.pods) > 3 {
				sum += fmt.Sprintf("; +%d more", len(g.pods)-3)
			}
		}
		hits = append(hits, alertHit{Key: key, NS: g.ns, Object: g.object, Summary: sum})
	}
	return hits
}

func fmtBytes(b float64) string {
	units := []string{"B", "Ki", "Mi", "Gi", "Ti"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", b, units[i])
	}
	return fmt.Sprintf("%.1f%s", b, units[i])
}

// ---------------------------------------------------------------- workloads

func evalWorkloads(env *evalEnv, r *AlertRule, sc *ruleScope) []alertHit {
	snap := env.snap
	var hits []alertHit
	add := func(kind string, m *Meta, sum string) {
		hits = append(hits, alertHit{Key: kind + "/" + m.Namespace + "/" + m.Name, NS: m.Namespace, Object: kind + "/" + m.Name, Summary: sum})
	}
	want := func(kind string, m *Meta) bool {
		return (sc.kind == "" || sc.kind == kind) && sc.nsOK(m.Namespace) && sc.names.match(m.Name) && sc.labelsOK(m.Labels)
	}
	replicas := func(p *int32) int32 {
		if p == nil {
			return 1
		}
		return *p
	}
	for i := range snap.Deployments {
		d := &snap.Deployments[i]
		if !want("Deployment", &d.Metadata) {
			continue
		}
		n := replicas(d.Spec.Replicas)
		switch r.Condition {
		case "workload_unavailable":
			if n > 0 && d.Status.AvailableReplicas < n {
				add("Deployment", &d.Metadata, fmt.Sprintf("%d of %d replicas available", d.Status.AvailableReplicas, n))
			}
		case "deploy_stuck":
			for _, c := range d.Status.Conditions {
				if c.Type == "Progressing" && c.Reason == "ProgressDeadlineExceeded" {
					add("Deployment", &d.Metadata, "rollout stuck: "+short(c.Message, 100))
				}
			}
		case "workload_zero":
			if d.Spec.Replicas != nil && n == 0 {
				add("Deployment", &d.Metadata, "scaled to 0 replicas")
			}
		}
	}
	for i := range snap.StatefulSets {
		s := &snap.StatefulSets[i]
		if !want("StatefulSet", &s.Metadata) {
			continue
		}
		n := replicas(s.Spec.Replicas)
		switch r.Condition {
		case "workload_unavailable":
			if n > 0 && s.Status.ReadyReplicas < n {
				add("StatefulSet", &s.Metadata, fmt.Sprintf("%d of %d replicas ready", s.Status.ReadyReplicas, n))
			}
		case "workload_zero":
			if s.Spec.Replicas != nil && n == 0 {
				add("StatefulSet", &s.Metadata, "scaled to 0 replicas")
			}
		}
	}
	if r.Condition == "workload_unavailable" {
		for i := range snap.DaemonSets {
			d := &snap.DaemonSets[i]
			if want("DaemonSet", &d.Metadata) && d.Status.NumberReady < d.Status.DesiredNumberScheduled {
				add("DaemonSet", &d.Metadata, fmt.Sprintf("%d of %d pods ready", d.Status.NumberReady, d.Status.DesiredNumberScheduled))
			}
		}
	}
	return hits
}

// ---------------------------------------------------------------- nodes

func evalNodes(env *evalEnv, r *AlertRule, sc *ruleScope) []alertHit {
	snap := env.snap
	var hits []alertHit
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		name := n.Metadata.Name
		if !sc.names.match(name) || !sc.labelsOK(n.Metadata.Labels) {
			continue
		}
		sum := ""
		switch r.Condition {
		case "node_not_ready":
			if !nodeReady(n) {
				sum = "NotReady"
				for _, c := range n.Status.Conditions {
					if c.Type == "Ready" && c.Message != "" {
						sum += ": " + short(c.Message, 90)
					}
				}
			}
		case "node_cpu", "node_mem":
			u, ok := snap.NodeUsage[name]
			if !ok {
				continue
			}
			if r.Condition == "node_cpu" {
				if a := n.Status.Allocatable.cpu(); a > 0 && u.CPU/a*100 >= r.Threshold {
					sum = fmt.Sprintf("CPU %.0f%% (%.1f of %.1f cores)", u.CPU/a*100, u.CPU, a)
				}
			} else if a := n.Status.Allocatable.mem(); a > 0 && u.Mem/a*100 >= r.Threshold {
				sum = fmt.Sprintf("memory %.0f%% (%s of %s)", u.Mem/a*100, fmtBytes(u.Mem), fmtBytes(a))
			}
		case "node_pressure":
			var p []string
			for _, c := range n.Status.Conditions {
				if strings.HasSuffix(c.Type, "Pressure") && c.Status == "True" {
					p = append(p, c.Type)
				}
			}
			if len(p) > 0 {
				sum = strings.Join(p, ", ")
			}
		case "node_cordoned":
			if n.Spec.Unschedulable {
				sum = "cordoned (unschedulable)"
			}
		}
		if sum != "" {
			hits = append(hits, alertHit{Key: "Node/" + name, Object: "Node/" + name, Summary: sum})
		}
	}
	return hits
}

// ---------------------------------------------------------------- events

func evalEvents(env *evalEnv, r *AlertRule, sc *ruleScope) []alertHit {
	window := time.Duration(r.Window * float64(time.Minute))
	m, _ := newNameMatcher(r.Match)
	seen := map[string]bool{}
	var hits []alertHit
	for i := range env.snap.Warnings {
		e := &env.snap.Warnings[i]
		ns := firstNonEmpty(e.InvolvedObject.Namespace, e.Metadata.Namespace)
		if env.now.Sub(e.When()) > window || !sc.nsOK(ns) || (sc.kind != "" && e.InvolvedObject.Kind != sc.kind) ||
			!sc.names.match(e.InvolvedObject.Name) || !m.matchText(e.Reason, e.Message) {
			continue
		}
		key := "Event/" + ns + "/" + e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name + "/" + e.Reason
		if seen[key] {
			continue
		}
		hits = append(hits, alertHit{Key: key, NS: ns, Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Summary: e.Reason + ": " + short(e.Message, 140)})
		seen[key] = true
	}
	return hits
}

// matchText: like match, but a plain word (no wildcards) matches anywhere in the text.
func (m *nameMatcher) matchText(s ...string) bool {
	if m.re != nil || m.any() {
		return m.match(s...)
	}
	for _, v := range s {
		lv := strings.ToLower(v)
		for _, g := range m.globs {
			if strings.ContainsAny(g, "*?[") {
				if ok, _ := path.Match(g, lv); ok {
					return true
				}
			} else if strings.Contains(lv, g) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- objects listed on demand

// listScoped lists a resource in the rule's namespaces: one call per namespace when the
// credentials are namespace-scoped or the rule names plain namespaces, else cluster-wide.
func listScoped[T any](env *evalEnv, sc *ruleScope, group, resource string) ([]T, error) {
	c := env.c
	if c.client == nil {
		return nil, errNoData{"no connection to this cluster"}
	}
	res, err := c.findResource(env.ctx, group, resource)
	if err != nil {
		return nil, err
	}
	var nss []string
	if env.snap.Scope == "namespaced" {
		for _, ns := range env.snap.ScopeNamespaces {
			if sc.nsOK(ns) {
				nss = append(nss, ns)
			}
		}
	} else if lit := sc.literalNS(); len(lit) > 0 && len(lit) <= 20 {
		nss = lit
	} else {
		return list[T](env.ctx, c.client, resPath(res, "", ""), nil)
	}
	var out []T
	var errs []string
	for _, ns := range nss {
		v, err := list[T](env.ctx, c.client, resPath(res, ns, ""), nil)
		if err != nil {
			errs = append(errs, ns+": "+friendlyErr(err))
			continue
		}
		out = append(out, v...)
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

type alertJob struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Active         int        `json:"active"`
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Conditions     []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			Reason             string    `json:"reason"`
			Message            string    `json:"message"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

type alertCronJob struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Suspend *bool `json:"suspend"`
	} `json:"spec"`
	Status struct {
		LastScheduleTime   *time.Time `json:"lastScheduleTime"`
		LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
	} `json:"status"`
}

func evalJobs(env *evalEnv, r *AlertRule, sc *ruleScope, d *condDef) ([]alertHit, error) {
	now := env.now
	var hits []alertHit
	if d.Target == "cronjob" {
		cjs, err := listScoped[alertCronJob](env, sc, "batch", "cronjobs")
		if err != nil {
			return nil, err
		}
		limit := time.Duration(r.Threshold * float64(time.Minute))
		for _, cj := range cjs {
			m := &cj.Metadata
			if (sc.kind != "" && sc.kind != "CronJob") || !sc.nsOK(m.Namespace) || !sc.names.match(m.Name) || !sc.labelsOK(m.Labels) ||
				(cj.Spec.Suspend != nil && *cj.Spec.Suspend) {
				continue
			}
			last, what := m.CreationTimestamp, "never succeeded"
			if t := cj.Status.LastSuccessfulTime; t != nil {
				last, what = *t, "last success "+ago(now.Sub(*t))+" ago"
			}
			if now.Sub(last) > limit {
				hits = append(hits, alertHit{Key: "CronJob/" + m.Namespace + "/" + m.Name, NS: m.Namespace, Object: "CronJob/" + m.Name, Summary: what})
			}
		}
		return hits, nil
	}
	jobs, err := listScoped[alertJob](env, sc, "batch", "jobs")
	if err != nil {
		return nil, err
	}
	window := time.Duration(r.Window * float64(time.Minute))
	for _, j := range jobs {
		m := &j.Metadata
		cron := cronOf(m.Name)
		for _, o := range m.OwnerReferences {
			if o.Kind == "CronJob" {
				cron = o.Name
			}
		}
		okName := sc.names.match(m.Name, cron)
		if sc.kind == "CronJob" {
			okName = cron != "" && sc.names.match(cron)
		} else if sc.kind != "" && sc.kind != "Job" {
			continue
		}
		if !okName || !sc.nsOK(m.Namespace) || !sc.labelsOK(m.Labels) {
			continue
		}
		hit := alertHit{Key: "Job/" + m.Namespace + "/" + m.Name, NS: m.Namespace, Object: "Job/" + m.Name}
		switch r.Condition {
		case "job_failed":
			for _, c := range j.Status.Conditions {
				if c.Type == "Failed" && c.Status == "True" && now.Sub(c.LastTransitionTime) <= window {
					hit.Summary = "failed: " + short(firstNonEmpty(c.Reason, "")+" "+c.Message, 120)
				}
			}
		case "job_long":
			if j.Status.Active > 0 && j.Status.CompletionTime == nil && j.Status.StartTime != nil &&
				now.Sub(*j.Status.StartTime) > time.Duration(r.Threshold*float64(time.Minute)) {
				hit.Summary = "running for " + ago(now.Sub(*j.Status.StartTime))
			}
		}
		if hit.Summary != "" {
			hits = append(hits, hit)
		}
	}
	return hits, nil
}

func evalPVCs(env *evalEnv, r *AlertRule, sc *ruleScope) ([]alertHit, error) {
	var hits []alertHit
	if r.Condition == "pvc_pending" {
		pvcs, err := listScoped[PVC](env, sc, "", "persistentvolumeclaims")
		if err != nil {
			return nil, err
		}
		for _, p := range pvcs {
			m := &p.Metadata
			if (p.Status.Phase == "Pending" || p.Status.Phase == "Lost") && sc.nsOK(m.Namespace) && sc.names.match(m.Name) && sc.labelsOK(m.Labels) {
				hits = append(hits, alertHit{Key: "PVC/" + m.Namespace + "/" + m.Name, NS: m.Namespace, Object: "PVC/" + m.Name, Summary: "claim is " + p.Status.Phase})
			}
		}
		return hits, nil
	}
	// pvc_usage: df inside pods (cached 5 minutes), one namespace at a time
	var nss []string
	if env.snap.Scope == "namespaced" {
		nss = env.snap.ScopeNamespaces
	} else {
		for _, n := range env.snap.Namespaces {
			nss = append(nss, n.Metadata.Name)
		}
	}
	var want []string
	for _, ns := range nss {
		if sc.nsOK(ns) {
			want = append(want, ns)
		}
	}
	if len(want) > 30 {
		return nil, fmt.Errorf("%d namespaces match: name at most 30 for volume usage, measuring needs a command in a pod per volume", len(want))
	}
	if env.srv == nil {
		return nil, errNoData{"volume usage is not available here"}
	}
	var errs []string
	for _, ns := range want {
		us, err := env.srv.usageFor(env.ctx, env.c, ns, "", false)
		if err != nil {
			errs = append(errs, ns+": "+err.Error())
			continue
		}
		for _, u := range us {
			if u.State == "ok" && u.Pct >= r.Threshold && sc.names.match(u.Name) {
				hits = append(hits, alertHit{Key: "PVC/" + ns + "/" + u.Name, NS: ns, Object: "PVC/" + u.Name,
					Summary: fmt.Sprintf("%.0f%% used (%s of %s free)", math.Round(u.Pct), fmtBytes(u.Avail), fmtBytes(u.Size))})
			}
		}
	}
	if len(hits) == 0 && len(errs) > 0 && len(errs) == len(want) {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return hits, nil
}

type alertExternalSecret struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

func evalExternalSecrets(env *evalEnv, r *AlertRule, sc *ruleScope) ([]alertHit, error) {
	ess, err := listScoped[alertExternalSecret](env, sc, "external-secrets.io", "externalsecrets")
	if err != nil {
		return nil, err
	}
	var hits []alertHit
	for _, e := range ess {
		m := &e.Metadata
		if !sc.nsOK(m.Namespace) || !sc.names.match(m.Name) || !sc.labelsOK(m.Labels) {
			continue
		}
		for _, c := range e.Status.Conditions {
			if c.Type == "Ready" && c.Status != "True" {
				hits = append(hits, alertHit{Key: "ExternalSecret/" + m.Namespace + "/" + m.Name, NS: m.Namespace, Object: "ExternalSecret/" + m.Name,
					Summary: short(firstNonEmpty(c.Reason, "not ready")+": "+c.Message, 140)})
			}
		}
	}
	return hits, nil
}

// ---------------------------------------------------------------- cluster

func evalCluster(env *evalEnv, r *AlertRule, d *condDef) ([]alertHit, error) {
	snap := env.snap
	hit := alertHit{Key: "Cluster", Object: "Cluster/" + env.c.Info.Name}
	switch r.Condition {
	case "cluster_unreachable":
		if snap.Err != "" && !snap.Readable {
			hit.Summary = "cannot reach or read the API server: " + short(snap.Err, 140)
		}
	case "metrics_down":
		if !snap.Readable {
			return nil, errNoData{"cannot read the cluster"}
		}
		if !snap.MetricsAvailable {
			hit.Summary = "metrics-server is not answering: " + short(snap.MetricsErr, 120)
		}
	case "cert_expiry":
		if env.c.client == nil {
			return nil, errNoData{"no connection to this cluster"}
		}
		rep := env.c.client.CertReport()
		if rep == nil || rep.Soonest == nil {
			return nil, errNoData{"the API server certificate has not been seen yet"}
		}
		if s := rep.Soonest; float64(s.DaysLeft) <= r.Threshold {
			hit.Key = "Cert/" + s.Subject
			hit.Summary = fmt.Sprintf("%s certificate %q expires in %d days (%s)", s.Role, s.Subject, s.DaysLeft, s.NotAfter.Format("2006-01-02"))
		}
	}
	if hit.Summary == "" {
		return nil, nil
	}
	return []alertHit{hit}, nil
}
