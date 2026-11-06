package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// APIResource is one listable resource type discovered on the cluster (incl. CRDs).
type APIResource struct {
	Group      string   `json:"group"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"` // plural, e.g. "httproutes"
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	Verbs      []string `json:"verbs"`
	ShortNames []string `json:"shortNames,omitempty"`
}

type discoveryCache struct {
	mu  sync.Mutex
	at  time.Time
	res []APIResource
}

type apiResourceList struct {
	GroupVersion string `json:"groupVersion"`
	Resources    []struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		Namespaced bool     `json:"namespaced"`
		Verbs      []string `json:"verbs"`
		ShortNames []string `json:"shortNames"`
	} `json:"resources"`
}

// Discover lists every resource type the API server serves (cached 10 min).
// Discovery is allowed for any authenticated user, so this works with Role-only access.
func (c *Cluster) Discover(ctx context.Context) ([]APIResource, error) {
	c.disc.mu.Lock()
	defer c.disc.mu.Unlock()
	if c.disc.res != nil && time.Since(c.disc.at) < 10*time.Minute {
		return c.disc.res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out []APIResource
	var mu sync.Mutex
	add := func(group, version string, l apiResourceList) {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range l.Resources {
			if strings.Contains(r.Name, "/") || !contains(r.Verbs, "list") {
				continue // subresources (pods/log) and non-listable types
			}
			out = append(out, APIResource{Group: group, Version: version, Resource: r.Name, Kind: r.Kind,
				Namespaced: r.Namespaced, Verbs: r.Verbs, ShortNames: r.ShortNames})
		}
	}
	var core apiResourceList
	if err := c.client.get(ctx, "/api/v1", nil, &core); err != nil {
		return nil, err
	}
	add("", "v1", core)
	var groups struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := c.client.get(ctx, "/apis", nil, &groups); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for _, g := range groups.Groups {
		wg.Add(1)
		go func(name, gv, v string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var l apiResourceList
			if err := c.client.get(ctx, "/apis/"+gv, nil, &l); err == nil {
				add(name, v, l)
			}
		}(g.Name, g.PreferredVersion.GroupVersion, g.PreferredVersion.Version)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Resource < out[j].Resource
	})
	c.disc.res, c.disc.at = out, time.Now()
	return out, nil
}

func (c *Cluster) findResource(ctx context.Context, group, resource string) (APIResource, error) {
	all, err := c.Discover(ctx)
	if err != nil {
		return APIResource{}, err
	}
	for _, r := range all {
		if r.Group == group && r.Resource == resource {
			return r, nil
		}
	}
	return APIResource{}, fmt.Errorf("resource %q (group %q) is not served by this cluster", resource, group)
}

func resPath(r APIResource, ns, name string) string {
	p := "/api/v1"
	if r.Group != "" {
		p = "/apis/" + r.Group + "/" + r.Version
	}
	if r.Namespaced && ns != "" {
		p += "/namespaces/" + url.PathEscape(ns)
	}
	p += "/" + r.Resource
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// ---------------------------------------------------------------- tables (kubectl get)

type TableColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
}

type TableRow struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Created   time.Time         `json:"created"`
	Labels    map[string]string `json:"labels,omitempty"`
	Cells     []any             `json:"cells"`
}

type Table struct {
	Resource APIResource   `json:"resource"`
	Columns  []TableColumn `json:"columns"`
	Rows     []TableRow    `json:"rows"`
	Warnings []string      `json:"warnings,omitempty"`
}

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"

// listTable asks the API server for the same columns `kubectl get` prints —
// including CRD printer columns — so any resource renders sensibly.
func (c *Cluster) listTable(ctx context.Context, r APIResource, ns string) (*Table, error) {
	t := &Table{Resource: r}
	fetch := func(ns string) error {
		q := url.Values{"limit": {"500"}}
		for page := 0; page < 40; page++ {
			resp, err := c.client.request(ctx, http.MethodGet, resPath(r, ns, ""), q, nil, "", tableAccept)
			if err != nil {
				return err
			}
			var tb struct {
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
				ColumnDefinitions []TableColumn `json:"columnDefinitions"`
				Rows              []struct {
					Cells  []any `json:"cells"`
					Object struct {
						Metadata Meta `json:"metadata"`
					} `json:"object"`
				} `json:"rows"`
				Items []struct {
					Metadata Meta `json:"metadata"`
				} `json:"items"`
			}
			err = json.NewDecoder(resp.Body).Decode(&tb)
			resp.Body.Close()
			if err != nil {
				return err
			}
			if len(tb.ColumnDefinitions) == 0 && len(tb.Items) > 0 {
				// server without Table support: plain list
				tb.ColumnDefinitions = []TableColumn{{Name: "Name", Type: "string"}}
				for _, it := range tb.Items {
					tb.Rows = append(tb.Rows, struct {
						Cells  []any `json:"cells"`
						Object struct {
							Metadata Meta `json:"metadata"`
						} `json:"object"`
					}{Cells: []any{it.Metadata.Name}, Object: struct {
						Metadata Meta `json:"metadata"`
					}{it.Metadata}})
				}
			}
			if t.Columns == nil && len(tb.ColumnDefinitions) > 0 {
				t.Columns = tb.ColumnDefinitions
			}
			for _, row := range tb.Rows {
				m := row.Object.Metadata
				t.Rows = append(t.Rows, TableRow{Name: m.Name, Namespace: m.Namespace, Created: m.CreationTimestamp, Labels: m.Labels, Cells: row.Cells})
			}
			if tb.Metadata.Continue == "" {
				break
			}
			q.Set("continue", tb.Metadata.Continue)
		}
		return nil
	}
	snap := c.Snap()
	if r.Namespaced && ns == "" && snap != nil && snap.Scope == "namespaced" {
		// no cluster-wide rights: read each allowed namespace
		for _, n := range snap.ScopeNamespaces {
			if err := fetch(n); err != nil {
				t.Warnings = append(t.Warnings, n+": "+friendlyErr(err))
			}
		}
		if len(t.Rows) == 0 && len(t.Warnings) == len(snap.ScopeNamespaces) {
			return nil, errors.New(strings.Join(t.Warnings, "; "))
		}
	} else if err := fetch(ns); err != nil {
		return nil, err
	}
	if t.Columns == nil {
		t.Columns = []TableColumn{{Name: "Name", Type: "string"}, {Name: "Age", Type: "string"}}
	}
	if t.Rows == nil {
		t.Rows = []TableRow{}
	}
	return t, nil
}

// ---------------------------------------------------------------- YAML round trip

// toYAML renders JSON as block YAML, preserving the server's key order and
// dropping metadata.managedFields noise.
func toYAML(js []byte) (string, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(js, &n); err != nil {
		return "", err
	}
	var walk func(*yaml.Node, int)
	walk = func(n *yaml.Node, depth int) {
		switch n.Kind {
		case yaml.MappingNode, yaml.SequenceNode:
			n.Style = 0
		case yaml.ScalarNode:
			if n.Tag == "!!str" && strings.Contains(n.Value, "\n") && !strings.ContainsAny(n.Value, "\t") {
				n.Style = yaml.LiteralStyle
			} else {
				n.Style = 0
			}
		}
		if n.Kind == yaml.MappingNode && depth == 1 {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "metadata" {
					md := n.Content[i+1]
					for j := 0; j+1 < len(md.Content); j += 2 {
						if md.Content[j].Value == "managedFields" {
							md.Content = append(md.Content[:j], md.Content[j+2:]...)
							break
						}
					}
				}
			}
		}
		for _, ch := range n.Content {
			walk(ch, depth+1)
		}
	}
	walk(&n, 0)
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // kubectl style
	if err := enc.Encode(&n); err != nil {
		return "", err
	}
	_ = enc.Close()
	return buf.String(), nil
}

// fromYAML converts edited YAML to JSON for the API server.
func fromYAML(src string) ([]byte, map[string]any, error) {
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		return nil, nil, fmt.Errorf("YAML: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, errors.New("YAML must describe a single object")
	}
	b, err := json.Marshal(m)
	return b, m, err
}

// ---------------------------------------------------------------- HTTP handlers

type objRef struct {
	Ctx      string `json:"ctx"`
	Group    string `json:"group"`
	Resource string `json:"resource"`
	NS       string `json:"ns"`
	Name     string `json:"name"`
}

func refFromQuery(q url.Values) objRef {
	return objRef{Ctx: q.Get("ctx"), Group: q.Get("group"), Resource: q.Get("resource"), NS: q.Get("ns"), Name: q.Get("name")}
}

func (s *Server) resourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/resources", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		res, err := c.Discover(r.Context())
		if err != nil {
			writeErr(w, 502, errors.New(friendlyErr(err)))
			return
		}
		writeJSON(w, 200, map[string]any{"resources": res, "readOnly": s.readOnly, "shell": s.shellAllowed()})
	})

	mux.HandleFunc("GET /api/table", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ref := refFromQuery(r.URL.Query())
		res, err := c.findResource(r.Context(), ref.Group, ref.Resource)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		t, err := c.listTable(ctx, res, ref.NS)
		if err != nil {
			writeErr(w, 502, errors.New(friendlyErr(err)))
			return
		}
		writeJSON(w, 200, t)
	})

	mux.HandleFunc("GET /api/object", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ref := refFromQuery(r.URL.Query())
		res, err := c.findResource(r.Context(), ref.Group, ref.Resource)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		resp, err := c.client.request(ctx, http.MethodGet, resPath(res, ref.NS, ref.Name), nil, nil, "", "application/json")
		if err != nil {
			writeErr(w, apiStatus(err), errors.New(friendlyErr(err)))
			return
		}
		js, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		y, err := toYAML(js)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		var obj map[string]any
		_ = json.Unmarshal(js, &obj)
		if md, ok := obj["metadata"].(map[string]any); ok {
			delete(md, "managedFields")
		}
		evs := []EventView{}
		if res.Kind != "Event" {
			evs = s.liveEvents(r.Context(), c, ref.NS, res.Kind, ref.Name)
		}
		writeJSON(w, 200, map[string]any{"resource": res, "yaml": y, "object": obj, "events": evs, "readOnly": s.readOnly})
	})

	mux.HandleFunc("PUT /api/object", func(w http.ResponseWriter, r *http.Request) {
		if s.denyWrite(w) {
			return
		}
		var req struct {
			objRef
			YAML string `json:"yaml"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		c := s.reg.Get(req.Ctx)
		if c == nil || c.client == nil {
			writeErr(w, 404, errors.New("unknown context"))
			return
		}
		res, err := c.findResource(r.Context(), req.Group, req.Resource)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		body, obj, err := fromYAML(req.YAML)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		md, _ := obj["metadata"].(map[string]any)
		if md == nil || md["name"] != req.Name || (res.Namespaced && md["namespace"] != nil && md["namespace"] != req.NS) {
			writeErr(w, 400, errors.New("metadata.name / namespace must stay the same — create a new object with kubectl instead"))
			return
		}
		if obj["kind"] != res.Kind {
			writeErr(w, 400, fmt.Errorf("kind must stay %s", res.Kind))
			return
		}
		out, err := c.client.send(r.Context(), http.MethodPut, resPath(res, req.NS, req.Name), nil, body, "application/json")
		if err != nil {
			msg := friendlyErr(err)
			if e, ok := err.(*APIError); ok && e.Code == 409 {
				msg = "the object changed since you opened it — reload and re-apply your edit (" + e.Message + ")"
			}
			writeErr(w, apiStatus(err), errors.New(msg))
			return
		}
		y, _ := toYAML(out)
		c.poke()
		writeJSON(w, 200, map[string]any{"ok": true, "yaml": y})
	})

	mux.HandleFunc("DELETE /api/object", func(w http.ResponseWriter, r *http.Request) {
		if s.denyWrite(w) {
			return
		}
		ref := refFromQuery(r.URL.Query())
		c := s.reg.Get(ref.Ctx)
		if c == nil || c.client == nil {
			writeErr(w, 404, errors.New("unknown context"))
			return
		}
		res, err := c.findResource(r.Context(), ref.Group, ref.Resource)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		body := []byte(`{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background"}`)
		if _, err := c.client.send(r.Context(), http.MethodDelete, resPath(res, ref.NS, ref.Name), nil, body, "application/json"); err != nil {
			writeErr(w, apiStatus(err), errors.New(friendlyErr(err)))
			return
		}
		c.poke()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// Quick actions: scale, rollout restart, cordon / uncordon.
	mux.HandleFunc("POST /api/action", func(w http.ResponseWriter, r *http.Request) {
		if s.denyWrite(w) {
			return
		}
		var req struct {
			objRef
			Action   string `json:"action"`
			Replicas *int   `json:"replicas"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		c := s.reg.Get(req.Ctx)
		if c == nil || c.client == nil {
			writeErr(w, 404, errors.New("unknown context"))
			return
		}
		res, err := c.findResource(r.Context(), req.Group, req.Resource)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		path := resPath(res, req.NS, req.Name)
		var body []byte
		ctype := "application/merge-patch+json"
		switch req.Action {
		case "scale":
			if req.Replicas == nil || *req.Replicas < 0 || *req.Replicas > 10000 {
				writeErr(w, 400, errors.New("replicas must be between 0 and 10000"))
				return
			}
			path += "/scale"
			body = []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, *req.Replicas))
		case "restart":
			if res.Kind != "Deployment" && res.Kind != "StatefulSet" && res.Kind != "DaemonSet" {
				writeErr(w, 400, errors.New("restart works on Deployments, StatefulSets and DaemonSets"))
				return
			}
			ctype = "application/strategic-merge-patch+json"
			body = []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, time.Now().Format(time.RFC3339)))
		case "cordon", "uncordon":
			if res.Kind != "Node" {
				writeErr(w, 400, errors.New("cordon works on nodes"))
				return
			}
			body = []byte(fmt.Sprintf(`{"spec":{"unschedulable":%v}}`, req.Action == "cordon"))
		default:
			writeErr(w, 400, errors.New("unknown action"))
			return
		}
		if _, err := c.client.send(r.Context(), http.MethodPatch, path, nil, body, ctype); err != nil {
			writeErr(w, apiStatus(err), errors.New(friendlyErr(err)))
			return
		}
		c.poke()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}

func (s *Server) denyWrite(w http.ResponseWriter) bool {
	if s.readOnly {
		writeErr(w, 403, errors.New("KubeDeck is running in read-only mode (-read-only)"))
		return true
	}
	return false
}

func apiStatus(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 502
}
