package main

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// AccessSettings decide how KubeDeck reads a context.
//
//	auto        try cluster-wide reads; if RBAC forbids them, fall back to namespaces
//	cluster     cluster-wide only (ClusterRole / ClusterRoleBinding)
//	namespaced  only the listed namespaces (Role / RoleBinding), no cluster-scoped calls
type AccessSettings struct {
	Mode       string   `json:"mode"`
	Namespaces []string `json:"namespaces"`
}

type Quota struct {
	CPU float64 `json:"cpu"` // hard requests.cpu (or limits.cpu if only that is set)
	Mem float64 `json:"mem"`
}

type ResourceQuota struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Hard ResourceList `json:"hard"`
		Used ResourceList `json:"used"`
	} `json:"status"`
}

func normalizeAccess(a AccessSettings) AccessSettings {
	switch a.Mode {
	case "cluster", "namespaced":
	default:
		a.Mode = "auto"
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range a.Namespaces {
		for _, part := range strings.FieldsFunc(n, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == ';' }) {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] && validDNSLabel(part) {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	a.Namespaces = out
	return a
}

func validDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(s)-1
		if !ok {
			return false
		}
	}
	return true
}

func isForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == 403
}

func (c *Cluster) Access() AccessSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a := c.access
	a.Namespaces = append([]string(nil), a.Namespaces...)
	return a
}

func (c *Cluster) SetAccess(a AccessSettings) {
	c.mu.Lock()
	c.access = normalizeAccess(a)
	c.detected, c.detectedAt = "", time.Time{}
	c.mu.Unlock()
	c.poke()
}

// scopeNamespaces: configured namespaces plus the context's default namespace.
func (c *Cluster) scopeNamespaces() []string {
	a := c.Access()
	nss := a.Namespaces
	if ns := c.Info.Namespace; ns != "" && !contains(nss, ns) {
		nss = append([]string{ns}, nss...)
	}
	if len(nss) == 0 {
		nss = []string{"default"}
	}
	return nss
}

// decideScope returns "cluster" or "namespaced". In auto mode it probes once
// (a cheap list of one pod across all namespaces) and re-checks every 10 minutes,
// so newly granted ClusterRoles are picked up.
func (c *Cluster) decideScope(ctx context.Context) string {
	a := c.Access()
	switch a.Mode {
	case "cluster", "namespaced":
		return a.Mode
	}
	c.mu.RLock()
	d, at := c.detected, c.detectedAt
	c.mu.RUnlock()
	if d != "" && time.Since(at) < 10*time.Minute {
		return d
	}
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var res struct{}
	err := c.client.get(pctx, "/api/v1/pods", url.Values{"limit": {"1"}}, &res)
	d = "cluster"
	if isForbidden(err) {
		d = "namespaced"
	}
	c.mu.Lock()
	c.detected, c.detectedAt = d, time.Now()
	c.mu.Unlock()
	return d
}

// refreshNamespaced reads each allowed namespace with namespaced endpoints only —
// exactly what a Role + RoleBinding grants. Forbidden resources are reported, not fatal.
func (c *Cluster) refreshNamespaced(ctx context.Context, s *Snapshot) []string {
	nss := c.scopeNamespaces()
	s.Scope, s.ScopeNamespaces = "namespaced", nss
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		errs    []string
		sem     = make(chan struct{}, 8)
		metrics int
		mErr    string
		events  []Event
	)
	type job struct {
		ns, what string
		fn       func(ns string) error
	}
	base := func(ns string) string { return "/api/v1/namespaces/" + url.PathEscape(ns) }
	apps := func(ns, r string) string { return "/apis/apps/v1/namespaces/" + url.PathEscape(ns) + "/" + r }
	var jobs []job
	for _, ns := range nss {
		jobs = append(jobs,
			job{ns, "pods", func(ns string) error {
				v, err := list[Pod](ctx, c.client, base(ns)+"/pods", nil)
				mu.Lock()
				s.Pods = append(s.Pods, v...)
				mu.Unlock()
				return err
			}},
			job{ns, "deployments", func(ns string) error {
				v, err := list[Deployment](ctx, c.client, apps(ns, "deployments"), nil)
				mu.Lock()
				s.Deployments = append(s.Deployments, v...)
				mu.Unlock()
				return err
			}},
			job{ns, "statefulsets", func(ns string) error {
				v, err := list[StatefulSet](ctx, c.client, apps(ns, "statefulsets"), nil)
				mu.Lock()
				s.StatefulSets = append(s.StatefulSets, v...)
				mu.Unlock()
				return err
			}},
			job{ns, "daemonsets", func(ns string) error {
				v, err := list[DaemonSet](ctx, c.client, apps(ns, "daemonsets"), nil)
				mu.Lock()
				s.DaemonSets = append(s.DaemonSets, v...)
				mu.Unlock()
				return err
			}},
			job{ns, "events", func(ns string) error {
				v, err := list[Event](ctx, c.client, base(ns)+"/events", url.Values{"fieldSelector": {"type=Warning"}})
				mu.Lock()
				events = append(events, v...)
				mu.Unlock()
				return err
			}},
			job{ns, "resourcequotas", func(ns string) error {
				v, err := list[ResourceQuota](ctx, c.client, base(ns)+"/resourcequotas", nil)
				var q Quota
				for _, rq := range v {
					h := rq.Status.Hard
					cpu := parseQuantity(firstNonEmpty(h["requests.cpu"], h["cpu"], h["limits.cpu"]))
					mem := parseQuantity(firstNonEmpty(h["requests.memory"], h["memory"], h["limits.memory"]))
					q.CPU = maxf(q.CPU, cpu)
					q.Mem = maxf(q.Mem, mem)
				}
				mu.Lock()
				if q.CPU > 0 || q.Mem > 0 {
					s.Quotas[ns] = q
				}
				mu.Unlock()
				return err
			}},
			job{ns, "metrics", func(ns string) error {
				v, err := list[PodMetrics](ctx, c.client, "/apis/metrics.k8s.io/v1beta1/namespaces/"+url.PathEscape(ns)+"/pods", nil)
				mu.Lock()
				if err == nil {
					addPodMetrics(s, v)
					metrics++
				} else if mErr == "" {
					mErr = friendlyErr(err)
				}
				mu.Unlock()
				return nil // metrics are optional; reported via MetricsErr
			}},
		)
	}
	// Cluster-scoped extras that some setups still allow (e.g. Rancher project members
	// can often list nodes). Try once; silently skip if forbidden.
	wg.Add(1)
	go func() {
		defer wg.Done()
		nodes, err := list[Node](ctx, c.client, "/api/v1/nodes", nil)
		if err != nil {
			return
		}
		nm, _ := list[NodeMetrics](ctx, c.client, "/apis/metrics.k8s.io/v1beta1/nodes", nil)
		mu.Lock()
		s.Nodes = nodes
		for _, n := range nm {
			s.NodeUsage[n.Metadata.Name] = Usage{n.Usage.cpu(), n.Usage.mem()}
		}
		mu.Unlock()
	}()
	okLists := 0
	forbidden := map[string][]string{}
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := j.fn(j.ns)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				switch {
				case isForbidden(err) && (j.what == "resourcequotas" || j.what == "events"):
					// nice-to-have; don't nag about them
				case isForbidden(err):
					forbidden[j.ns] = append(forbidden[j.ns], j.what)
				default:
					errs = append(errs, j.ns+"/"+j.what+": "+friendlyErr(err))
				}
				return
			}
			if j.what != "metrics" && j.what != "resourcequotas" && j.what != "events" {
				okLists++
			}
		}(j)
	}
	wg.Wait()
	for ns, what := range forbidden {
		sort.Strings(what)
		if len(what) >= 4 {
			errs = append(errs, "no access to namespace "+ns+" (check the namespace name or your RoleBinding)")
		} else {
			errs = append(errs, ns+": not allowed to list "+strings.Join(what, ", "))
		}
	}
	s.Readable = okLists > 0
	s.MetricsAvailable = metrics > 0
	s.MetricsErr = mErr
	s.Warnings = newestEvents(events, 300)
	for _, ns := range nss {
		var n Namespace
		n.Metadata.Name = ns
		n.Status.Phase = "Active"
		s.Namespaces = append(s.Namespaces, n)
	}
	sort.Strings(errs)
	return errs
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
