package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Namespace attention overview: for every namespace, only what needs a human.

type AttnItem struct {
	Sev    string `json:"sev"`  // critical | warning
	Kind   string `json:"kind"` // pod | workload | pvc | event
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Ref    string `json:"ref,omitempty"` // workload kind for links
}

type NSOverview struct {
	Name         string     `json:"name"`
	Pods         int        `json:"pods"`
	Running      int        `json:"running"`
	Restarts     int        `json:"restarts"`
	PVCs         int        `json:"pvcs"`
	PVCUnchecked int        `json:"pvcUnchecked"` // bound PVCs without a recent usage reading
	Sev          string     `json:"sev"`          // critical | warning | ok
	Items        []AttnItem `json:"items"`
	Checked      int        `json:"pvcChecked"`
}

const (
	pvcWarnPct = 80.0
	pvcCritPct = 90.0
)

func sevRank(s string) int {
	switch s {
	case "critical":
		return 2
	case "warning":
		return 1
	}
	return 0
}

func (s *Server) buildNSOverview(ctx context.Context, c *Cluster, snap *Snapshot) []NSOverview {
	byNS := map[string]*NSOverview{}
	get := func(ns string) *NSOverview {
		o := byNS[ns]
		if o == nil {
			o = &NSOverview{Name: ns, Items: []AttnItem{}}
			byNS[ns] = o
		}
		return o
	}
	for _, n := range snap.Namespaces {
		get(n.Metadata.Name)
	}
	for _, n := range snap.ScopeNamespaces {
		get(n)
	}

	// pods
	for i := range snap.Pods {
		p := &snap.Pods[i]
		o := get(p.Metadata.Namespace)
		o.Pods++
		if p.Status.Phase == "Running" {
			o.Running++
		}
		pv := podView(snap, p, false)
		o.Restarts += pv.Restarts
		recent := pv.LastRestart != nil && time.Since(*pv.LastRestart) < time.Hour
		switch {
		case pv.Problem:
			d := pv.Status
			if pv.LastReason != "" && pv.LastExit != nil {
				d += " · last exit " + pv.LastReason + " (" + itoa(*pv.LastExit) + ")"
			}
			o.Items = append(o.Items, AttnItem{"critical", "pod", pv.Name, d, ""})
		case recent:
			d := itoa(pv.Restarts) + " restarts · " + pv.LastReason
			if pv.LastExit != nil {
				d += " (" + itoa(*pv.LastExit) + ")"
			}
			o.Items = append(o.Items, AttnItem{"warning", "pod", pv.Name, d + " · " + shortAgo(*pv.LastRestart), ""})
		}
	}

	// workloads
	for _, w := range workloadRefs(snap) {
		var health string
		var want, avail int32
		switch w.kind {
		case "Deployment":
			for i := range snap.Deployments {
				d := &snap.Deployments[i]
				if d.Metadata.Namespace == w.ns && d.Metadata.Name == w.name {
					health, avail = deploymentHealth(d), d.Status.AvailableReplicas
					want = 1
					if d.Spec.Replicas != nil {
						want = *d.Spec.Replicas
					}
				}
			}
		case "StatefulSet":
			for i := range snap.StatefulSets {
				d := &snap.StatefulSets[i]
				if d.Metadata.Namespace == w.ns && d.Metadata.Name == w.name {
					want = 1
					if d.Spec.Replicas != nil {
						want = *d.Spec.Replicas
					}
					avail = d.Status.ReadyReplicas
					health = "healthy"
					if want > 0 && avail == 0 {
						health = "degraded"
					} else if avail < want {
						health = "progressing"
					}
				}
			}
		case "DaemonSet":
			for i := range snap.DaemonSets {
				d := &snap.DaemonSets[i]
				if d.Metadata.Namespace == w.ns && d.Metadata.Name == w.name {
					want, avail = d.Status.DesiredNumberScheduled, d.Status.NumberReady
					health = "healthy"
					if want > 0 && avail == 0 {
						health = "degraded"
					} else if avail < want {
						health = "progressing"
					}
				}
			}
		}
		if health == "degraded" || health == "progressing" {
			sev := "warning"
			if health == "degraded" {
				sev = "critical"
			}
			get(w.ns).Items = append(get(w.ns).Items, AttnItem{sev, "workload", w.name,
				w.kind + " " + itoa(int(avail)) + "/" + itoa(int(want)) + " available", w.kind})
		}
	}

	// warning events in the last hour, grouped by reason
	type ek struct{ ns, reason string }
	evs := map[ek][]Event{}
	for _, e := range snap.Warnings {
		if time.Since(e.When()) > time.Hour {
			continue
		}
		ns := e.InvolvedObject.Namespace
		if ns == "" {
			ns = e.Metadata.Namespace
		}
		evs[ek{ns, e.Reason}] = append(evs[ek{ns, e.Reason}], e)
	}
	for k, list := range evs {
		if k.ns == "" {
			continue
		}
		cnt := 0
		for _, e := range list {
			if e.Count > 0 {
				cnt += e.Count
			} else {
				cnt++
			}
		}
		e := list[0]
		d := k.reason + " ×" + itoa(cnt) + " · " + e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
		get(k.ns).Items = append(get(k.ns).Items, AttnItem{"warning", "event", k.reason, d, ""})
	}

	// storage: PVC phase + cached usage (the browser asks /api/pvcusage to fill the gaps)
	var nss []string
	if snap.Scope == "namespaced" {
		nss = snap.ScopeNamespaces
	} else {
		pvcs, err := list[PVC](ctx, c.client, "/api/v1/persistentvolumeclaims", nil)
		if err == nil {
			seen := map[string]bool{}
			for i := range pvcs {
				seen[pvcs[i].Metadata.Namespace] = true
			}
			for n := range seen {
				nss = append(nss, n)
			}
			s.addPVCItems(c, get, pvcs)
			nss = nil
		}
	}
	for _, ns := range nss {
		pvcs, err := s.listPVCs(ctx, c, ns)
		if err != nil {
			continue
		}
		s.addPVCItems(c, get, pvcs)
	}

	out := make([]NSOverview, 0, len(byNS))
	for _, o := range byNS {
		sort.SliceStable(o.Items, func(i, j int) bool {
			if a, b := sevRank(o.Items[i].Sev), sevRank(o.Items[j].Sev); a != b {
				return a > b
			}
			return o.Items[i].Name < o.Items[j].Name
		})
		o.Sev = "ok"
		for _, it := range o.Items {
			if sevRank(it.Sev) > sevRank(o.Sev) {
				o.Sev = it.Sev
			}
		}
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := sevRank(out[i].Sev), sevRank(out[j].Sev); a != b {
			return a > b
		}
		if len(out[i].Items) != len(out[j].Items) {
			return len(out[i].Items) > len(out[j].Items)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Server) addPVCItems(c *Cluster, get func(string) *NSOverview, pvcs []PVC) {
	for i := range pvcs {
		p := &pvcs[i]
		o := get(p.Metadata.Namespace)
		o.PVCs++
		if p.Status.Phase != "Bound" {
			sev := "warning"
			if p.Status.Phase == "Lost" {
				sev = "critical"
			}
			if p.Status.Phase == "Lost" || time.Since(p.Metadata.CreationTimestamp) > 5*time.Minute {
				o.Items = append(o.Items, AttnItem{sev, "pvc", p.Metadata.Name, "PVC " + strings.ToLower(p.Status.Phase), ""})
			}
			continue
		}
		u, ok := c.pvcPeek(p.Metadata.Namespace, p.Metadata.Name)
		if !ok {
			if cu, has := c.pvcCached(p.Metadata.Namespace+"/"+p.Metadata.Name, false); has && cu.State == "unmounted" {
				continue // not mounted: nothing to measure
			}
			o.PVCUnchecked++
			continue
		}
		o.Checked++
		if u.Pct >= pvcWarnPct {
			sev := "warning"
			if u.Pct >= pvcCritPct {
				sev = "critical"
			}
			o.Items = append(o.Items, AttnItem{sev, "pvc", p.Metadata.Name,
				fmtPct(u.Pct) + " full · " + fmtBytesShort(u.Used) + " of " + fmtBytesShort(u.Size), ""})
		}
	}
}

func fmtPct(p float64) string { return itoa(int(p+0.5)) + "%" }

func fmtBytesShort(b float64) string {
	units := []string{"B", "Ki", "Mi", "Gi", "Ti"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if b >= 100 || i == 0 {
		return itoa(int(b+0.5)) + units[i]
	}
	return strings.TrimRight(strings.TrimRight(ftoa1(b), "0"), ".") + units[i]
}

func ftoa1(f float64) string {
	n := int(f*10 + 0.5)
	return itoa(n/10) + "." + itoa(n%10)
}

func shortAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m ago"
	}
	return itoa(int(d.Hours())) + "h ago"
}

func (s *Server) overviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/nsoverview", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		writeJSON(w, 200, map[string]any{"namespaces": s.buildNSOverview(ctx, c, snap), "exec": !s.readOnly,
			"warnPct": pvcWarnPct, "critPct": pvcCritPct})
	})
}
