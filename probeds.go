package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The probe DaemonSet: the connectivity tester's default way to test from every node.
// For one test KubeLamp creates a DaemonSet in the chosen namespace (one small pod per
// node), waits until its pods run, probes from them, and deletes it again, also when the
// test fails or is cancelled. DaemonSets it left behind (KubeLamp stopped mid-test) are
// removed by the next test in that namespace.
//
// Admins can change the YAML (e.g. to pull the image from an internal registry); it is
// kept in <data>/probe-daemonset.yaml. KubeLamp sets the name, namespace and labels.

const defaultProbeDS = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: kubelamp-probe          # KubeLamp adds a random suffix per test
  labels:
    app.kubernetes.io/name: kubelamp-probe
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kubelamp-probe
  template:
    metadata:
      labels:
        app.kubernetes.io/name: kubelamp-probe
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      terminationGracePeriodSeconds: 2
      # run on every node, tainted ones (control plane) too; remove to skip tainted nodes
      tolerations:
        - operator: Exists
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        runAsGroup: 65534
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: probe
          # needs sh plus nc, curl or wget; use an internal mirror if Docker Hub is blocked
          image: curlimages/curl:8.11.1
          imagePullPolicy: IfNotPresent
          command: ["sh", "-c", "trap 'exit 0' TERM INT; while true; do sleep 3600 & wait $!; done"]
          resources:
            requests: {cpu: 5m, memory: 16Mi}
            limits: {cpu: 200m, memory: 64Mi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: tmp
          emptyDir: {sizeLimit: 16Mi}
`

const (
	probeLabel     = "kubelamp.io/probe"
	probeRunLabel  = "kubelamp.io/probe-run"
	probeWait      = 2 * time.Minute // for the probe pods to start
	probeStaleTime = 30 * time.Minute
)

func (s *Server) probeTemplatePath() string { return filepath.Join(s.dataDir, "probe-daemonset.yaml") }

// probeTemplate is the admin's YAML, or the default.
func (s *Server) probeTemplate() (string, bool) {
	if b, err := os.ReadFile(s.probeTemplatePath()); err == nil && strings.TrimSpace(string(b)) != "" {
		return string(b), false
	}
	return defaultProbeDS, true
}

// buildProbeDS turns the template into this test's DaemonSet: its own name, the
// namespace, and labels that tie its pods to this test only.
func buildProbeDS(tmpl, ns, run string, nodes ...string) ([]byte, string, error) {
	_, m, err := fromYAML(tmpl)
	if err != nil {
		return nil, "", err
	}
	if m["kind"] != "DaemonSet" || m["apiVersion"] != "apps/v1" {
		return nil, "", errors.New("the probe template must be an apps/v1 DaemonSet")
	}
	obj := func(parent map[string]any, key string) map[string]any {
		v, ok := parent[key].(map[string]any)
		if !ok {
			v = map[string]any{}
			parent[key] = v
		}
		return v
	}
	label := func(parent map[string]any) {
		l := obj(parent, "labels")
		l[probeLabel] = "true"
		l[probeRunLabel] = run
	}
	md := obj(m, "metadata")
	base, _ := md["name"].(string)
	if base == "" || len(base) > 40 {
		base = "kubelamp-probe"
	}
	name := base + "-" + run
	md["name"], md["namespace"] = name, ns
	delete(md, "resourceVersion")
	delete(md, "uid")
	label(md)
	obj(md, "annotations")["kubelamp.io/created"] = time.Now().UTC().Format(time.RFC3339)
	spec := obj(m, "spec")
	obj(obj(spec, "selector"), "matchLabels")[probeRunLabel] = run
	tpl := obj(spec, "template")
	label(obj(tpl, "metadata"))
	ps := obj(tpl, "spec")
	if cs, _ := ps["containers"].([]any); len(cs) == 0 {
		return nil, "", errors.New("the probe template has no containers")
	}
	if len(nodes) > 0 { // only on these nodes (re-run of failed checks)
		// a node field selector takes one value, so one term per node (terms are ORed)
		terms := make([]any, len(nodes))
		for i, n := range nodes {
			terms[i] = map[string]any{"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "In", "values": []any{n}}}}
		}
		obj(obj(ps, "affinity"), "nodeAffinity")["requiredDuringSchedulingIgnoredDuringExecution"] = map[string]any{"nodeSelectorTerms": terms}
	}
	b, err := json.Marshal(m)
	return b, name, err
}

func checkProbeTemplate(y string) error {
	if len(y) > 64<<10 {
		return errors.New("the template is too large")
	}
	_, _, err := buildProbeDS(y, "x", "check")
	return err
}

func dsPath(ns, name string) string {
	p := "/apis/apps/v1/namespaces/" + url.PathEscape(ns) + "/daemonsets"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func deleteProbeDS(c *Cluster, ns, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := []byte(`{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background","gracePeriodSeconds":0}`)
	_, err := c.client.send(ctx, http.MethodDelete, dsPath(ns, name), nil, body, "application/json")
	return err
}

// removeStaleProbes deletes probe DaemonSets older than 30 minutes (KubeLamp stopped mid-test).
func removeStaleProbes(ctx context.Context, c *Cluster, ns string) {
	type ds struct {
		Metadata struct {
			Name              string    `json:"name"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
	}
	items, err := list[ds](ctx, c.client, dsPath(ns, ""), url.Values{"labelSelector": {probeLabel + "=true"}})
	if err != nil {
		return
	}
	for _, d := range items {
		if time.Since(d.Metadata.CreationTimestamp) > probeStaleTime {
			if deleteProbeDS(c, ns, d.Metadata.Name) == nil {
				log.Printf("removed leftover probe DaemonSet %s/%s", ns, d.Metadata.Name)
			}
		}
	}
}

func (j *ConnJob) setPrep(msg string) {
	j.mu.Lock()
	j.Prep = msg
	j.mu.Unlock()
}

// waitingReason explains why a pod isn't running (image pull errors and the like).
func waitingReason(p *Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			msg := w.Reason
			if w.Message != "" {
				msg += ": " + w.Message
			}
			if len(msg) > 220 {
				msg = msg[:220] + "…"
			}
			return msg
		}
	}
	for _, cd := range p.Status.Conditions {
		if cd.Type == "PodScheduled" && cd.Status == "False" && cd.Message != "" {
			return "not scheduled: " + cd.Message
		}
	}
	return ""
}

// runWithProbeDS creates the DaemonSet, waits for its pods, tests from them, and removes it.
func (j *ConnJob) runWithProbeDS(ctx context.Context, c *Cluster, snap *Snapshot, tmpl string) {
	fail := func(msg string) {
		j.mu.Lock()
		j.State, j.Err, j.Finished = "failed", msg, time.Now()
		j.mu.Unlock()
	}
	run := newJobID()[:6]
	body, name, err := buildProbeDS(tmpl, j.NS, run, j.Nodes...)
	if err != nil {
		fail(err.Error())
		return
	}
	removeStaleProbes(ctx, c, j.NS)
	j.setPrep("Creating DaemonSet " + name + "…")
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = c.client.send(cctx, http.MethodPost, dsPath(j.NS, ""), nil, body, "application/json")
	cancel()
	if err != nil {
		fail("Cannot create the probe DaemonSet: " + friendlyErr(err))
		return
	}
	log.Printf("conntest %s: created probe DaemonSet %s/%s", j.ID, j.NS, name)
	defer func() {
		if err := deleteProbeDS(c, j.NS, name); err != nil {
			j.mu.Lock()
			j.Warnings = append(j.Warnings, fmt.Sprintf("The probe DaemonSet %s could not be removed (%s); the next test in this namespace removes it.", name, friendlyErr(err)))
			j.mu.Unlock()
		}
	}()

	// wait until every scheduled probe pod runs (or the time is up: test from those that do)
	var ready []*Pod
	reasons := map[string]string{}
	deadline := time.Now().Add(probeWait)
	for {
		var st struct {
			Status struct {
				Desired int `json:"desiredNumberScheduled"`
			} `json:"status"`
		}
		_ = c.client.get(ctx, dsPath(j.NS, name), nil, &st)
		pods, _ := c.FreshPods(ctx, j.NS)
		ready = ready[:0]
		reasons = map[string]string{}
		for i := range pods {
			p := &pods[i]
			if p.Metadata.Labels[probeRunLabel] != run || (len(j.Nodes) > 0 && !contains(j.Nodes, p.Spec.NodeName)) {
				continue
			}
			if p.Status.Phase == "Running" && len(runningContainers(p)) > 0 && podReadyCount(p) == len(p.Spec.Containers) {
				ready = append(ready, p)
			} else if why := waitingReason(p); why != "" {
				reasons[p.Spec.NodeName] = why
			}
		}
		want := st.Status.Desired
		j.setPrep(fmt.Sprintf("Starting probe pods: %d of %d running…", len(ready), want))
		if want > 0 && len(ready) >= want {
			break
		}
		if ctx.Err() != nil {
			j.mu.Lock()
			j.State, j.Finished = "cancelled", time.Now()
			j.mu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if len(ready) == 0 {
		msg := "No probe pod started within 2 minutes."
		if len(reasons) > 0 {
			var why []string
			for n, r := range reasons {
				why = append(why, n+": "+r)
			}
			sort.Strings(why)
			msg += " " + strings.Join(why[:min(len(why), 3)], "; ")
		}
		if strings.Contains(msg, "Image") || strings.Contains(msg, "image") {
			msg += " An admin can change the image (for example to an internal registry) under DaemonSet YAML."
		}
		fail(msg)
		return
	}
	if len(reasons) > 0 {
		var why []string
		for n, r := range reasons {
			why = append(why, n+": "+r)
		}
		sort.Strings(why)
		j.mu.Lock()
		j.Warnings = append(j.Warnings, fmt.Sprintf("%d probe pod(s) did not start, so their nodes were not tested: %s", len(reasons), strings.Join(why, "; ")))
		j.mu.Unlock()
	}
	sort.Slice(ready, func(a, b int) bool { return ready[a].Spec.NodeName < ready[b].Spec.NodeName })
	if len(ready) > connMaxSources {
		ready = ready[:connMaxSources]
	}
	j.mu.Lock()
	j.pods = ready
	j.Sources = make([]ConnSource, 0, len(ready))
	for _, p := range ready {
		j.Sources = append(j.Sources, j.source(p))
	}
	j.Total = len(ready) * len(j.Checks)
	j.Prep = ""
	j.mu.Unlock()
	j.run(ctx, c, snap)
}

// ---------------------------------------------------------------- template routes

func (s *Server) probeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools/probe-template", func(w http.ResponseWriter, r *http.Request) {
		y, def := s.probeTemplate()
		writeJSON(w, 200, map[string]any{"yaml": y, "default": def, "defaultYaml": defaultProbeDS, "canEdit": s.isAdmin(r)})
	})
	mux.HandleFunc("PUT /api/tools/probe-template", func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			writeErr(w, 403, errors.New("only admins can change the probe DaemonSet"))
			return
		}
		var req struct {
			YAML string `json:"yaml"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		if strings.TrimSpace(req.YAML) == "" { // reset to the default
			_ = os.Remove(s.probeTemplatePath())
			writeJSON(w, 200, map[string]any{"ok": true, "default": true})
			return
		}
		if err := checkProbeTemplate(req.YAML); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
			writeErr(w, 500, err)
			return
		}
		if err := os.WriteFile(s.probeTemplatePath(), []byte(req.YAML), 0o600); err != nil {
			writeErr(w, 500, err)
			return
		}
		if u := reqUser(r); u != nil {
			log.Printf("user %s changed the probe DaemonSet template", u.Username)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "default": false})
	})
}
