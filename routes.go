package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Routing overview: Traefik IngressRoutes, Gateway API HTTPRoutes and plain
// Ingresses, normalised to one shape (hosts → matches → backends) so the UI can
// show how traffic reaches services without reading three different specs.

type RouteBackend struct {
	Kind      string `json:"kind,omitempty"` // Service (default), TraefikService, …
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Port      string `json:"port,omitempty"`
	Weight    *int64 `json:"weight,omitempty"`
}

type RouteRule struct {
	Match       string         `json:"match"`
	Backends    []RouteBackend `json:"backends"`
	Middlewares []string       `json:"middlewares,omitempty"` // Traefik middlewares / Gateway filters
}

type RouteView struct {
	Kind      string      `json:"kind"`
	Group     string      `json:"group"`
	Resource  string      `json:"resource"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Created   time.Time   `json:"created"`
	Hosts     []string    `json:"hosts"`
	Entry     []string    `json:"entry"` // entry points, parent gateways or ingress class
	TLS       string      `json:"tls,omitempty"`
	Rules     []RouteRule `json:"rules"`
	Status    string      `json:"status,omitempty"` // good | warning | critical | ""
	StatusMsg string      `json:"statusMsg,omitempty"`
}

type RoutesResult struct {
	IngressRoutes []RouteView `json:"ingressRoutes"`
	HTTPRoutes    []RouteView `json:"httpRoutes"`
	Ingresses     []RouteView `json:"ingresses"`
	Has           struct {
		IngressRoutes bool `json:"ingressRoutes"`
		HTTPRoutes    bool `json:"httpRoutes"`
		Ingresses     bool `json:"ingresses"`
	} `json:"has"`
	Warnings []string `json:"warnings,omitempty"`
}

// ---------------------------------------------------------------- raw shapes

type portVal struct{ s string }

func (p *portVal) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		p.s = n.String()
		return nil
	}
	return json.Unmarshal(b, &p.s)
}

type nsRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type traefikIR struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		EntryPoints []string `json:"entryPoints"`
		Routes      []struct {
			Match    string `json:"match"`
			Services []struct {
				Kind      string  `json:"kind"`
				Name      string  `json:"name"`
				Namespace string  `json:"namespace"`
				Port      portVal `json:"port"`
				Weight    *int64  `json:"weight"`
			} `json:"services"`
			Middlewares []nsRef `json:"middlewares"`
		} `json:"routes"`
		TLS *struct {
			SecretName   string `json:"secretName"`
			CertResolver string `json:"certResolver"`
		} `json:"tls"`
	} `json:"spec"`
}

type gwHTTPRoute struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		ParentRefs []struct {
			Kind        string  `json:"kind"`
			Name        string  `json:"name"`
			Namespace   string  `json:"namespace"`
			SectionName string  `json:"sectionName"`
			Port        portVal `json:"port"`
		} `json:"parentRefs"`
		Hostnames []string `json:"hostnames"`
		Rules     []struct {
			Matches []struct {
				Path *struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"path"`
				Method  string `json:"method"`
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"matches"`
			Filters []struct {
				Type string `json:"type"`
			} `json:"filters"`
			BackendRefs []struct {
				Kind      string  `json:"kind"`
				Name      string  `json:"name"`
				Namespace string  `json:"namespace"`
				Port      portVal `json:"port"`
				Weight    *int64  `json:"weight"`
			} `json:"backendRefs"`
		} `json:"rules"`
	} `json:"spec"`
	Status struct {
		Parents []struct {
			ParentRef struct {
				Name string `json:"name"`
			} `json:"parentRef"`
			Conditions []Condition `json:"conditions"`
		} `json:"parents"`
	} `json:"status"`
}

type netIngress struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		IngressClassName string `json:"ingressClassName"`
		TLS              []struct {
			Hosts      []string `json:"hosts"`
			SecretName string   `json:"secretName"`
		} `json:"tls"`
		DefaultBackend *ingBackend `json:"defaultBackend"`
		Rules          []struct {
			Host string `json:"host"`
			HTTP *struct {
				Paths []struct {
					Path     string     `json:"path"`
					PathType string     `json:"pathType"`
					Backend  ingBackend `json:"backend"`
				} `json:"paths"`
			} `json:"http"`
		} `json:"rules"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type ingBackend struct {
	Service *struct {
		Name string `json:"name"`
		Port struct {
			Name   string `json:"name"`
			Number int    `json:"number"`
		} `json:"port"`
	} `json:"service"`
	Resource *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"resource"`
}

func (b ingBackend) view() RouteBackend {
	if b.Service != nil {
		p := b.Service.Port.Name
		if b.Service.Port.Number != 0 {
			p = strconv.Itoa(b.Service.Port.Number)
		}
		return RouteBackend{Kind: "Service", Name: b.Service.Name, Port: p}
	}
	if b.Resource != nil {
		return RouteBackend{Kind: b.Resource.Kind, Name: b.Resource.Name}
	}
	return RouteBackend{Name: "?"}
}

// ---------------------------------------------------------------- normalisation

var traefikHostRe = regexp.MustCompile("Host(?:SNI|Regexp)?\\(([^)]*)\\)")
var traefikArgRe = regexp.MustCompile("[`\"]([^`\"]+)[`\"]")

// traefikHosts pulls the host names out of a rule like
// Host(`a.example.com`, `b.example.com`) && PathPrefix(`/api`).
func traefikHosts(match string) []string {
	var out []string
	for _, m := range traefikHostRe.FindAllStringSubmatch(match, -1) {
		for _, a := range traefikArgRe.FindAllStringSubmatch(m[1], -1) {
			out = append(out, a[1])
		}
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (r traefikIR) view(group string) RouteView {
	v := RouteView{Kind: "IngressRoute", Group: group, Resource: "ingressroutes", Namespace: r.Metadata.Namespace, Name: r.Metadata.Name,
		Created: r.Metadata.CreationTimestamp, Entry: uniq(r.Spec.EntryPoints), Rules: []RouteRule{}}
	var hosts []string
	for _, rt := range r.Spec.Routes {
		rule := RouteRule{Match: rt.Match, Backends: []RouteBackend{}}
		hosts = append(hosts, traefikHosts(rt.Match)...)
		for _, s := range rt.Services {
			kind := s.Kind
			if kind == "" {
				kind = "Service"
			}
			rule.Backends = append(rule.Backends, RouteBackend{Kind: kind, Namespace: s.Namespace, Name: s.Name, Port: s.Port.s, Weight: s.Weight})
		}
		for _, m := range rt.Middlewares {
			n := m.Name
			if m.Namespace != "" && m.Namespace != r.Metadata.Namespace {
				n = m.Namespace + "/" + n
			}
			rule.Middlewares = append(rule.Middlewares, n)
		}
		v.Rules = append(v.Rules, rule)
	}
	v.Hosts = uniq(hosts)
	if t := r.Spec.TLS; t != nil {
		switch {
		case t.SecretName != "":
			v.TLS = "secret " + t.SecretName
		case t.CertResolver != "":
			v.TLS = "resolver " + t.CertResolver
		default:
			v.TLS = "default certificate"
		}
	}
	return v
}

func (r gwHTTPRoute) view(group string) RouteView {
	v := RouteView{Kind: "HTTPRoute", Group: group, Resource: "httproutes", Namespace: r.Metadata.Namespace, Name: r.Metadata.Name,
		Created: r.Metadata.CreationTimestamp, Hosts: uniq(r.Spec.Hostnames), Entry: []string{}, Rules: []RouteRule{}}
	for _, p := range r.Spec.ParentRefs {
		n := p.Name
		if p.Namespace != "" && p.Namespace != r.Metadata.Namespace {
			n = p.Namespace + "/" + n
		}
		if p.Kind != "" && p.Kind != "Gateway" {
			n = p.Kind + " " + n
		}
		if p.SectionName != "" {
			n += " · " + p.SectionName
		}
		v.Entry = append(v.Entry, n)
	}
	for _, rl := range r.Spec.Rules {
		var ms []string
		for _, m := range rl.Matches {
			var parts []string
			if m.Path != nil {
				t := m.Path.Type
				if t == "" {
					t = "PathPrefix"
				}
				parts = append(parts, t+" "+m.Path.Value)
			}
			if m.Method != "" {
				parts = append(parts, m.Method)
			}
			for _, h := range m.Headers {
				parts = append(parts, "header "+h.Name+"="+h.Value)
			}
			if len(parts) > 0 {
				ms = append(ms, strings.Join(parts, " "))
			}
		}
		match := strings.Join(ms, " | ")
		if match == "" {
			match = "PathPrefix /"
		}
		rule := RouteRule{Match: match, Backends: []RouteBackend{}}
		for _, b := range rl.BackendRefs {
			kind := b.Kind
			if kind == "" {
				kind = "Service"
			}
			rule.Backends = append(rule.Backends, RouteBackend{Kind: kind, Namespace: b.Namespace, Name: b.Name, Port: b.Port.s, Weight: b.Weight})
		}
		for _, f := range rl.Filters {
			rule.Middlewares = append(rule.Middlewares, f.Type)
		}
		v.Rules = append(v.Rules, rule)
	}
	// status: every parent must have accepted the route and resolved its refs
	if len(r.Status.Parents) == 0 {
		v.Status, v.StatusMsg = "warning", "No gateway has reported on this route yet"
	} else {
		v.Status = "good"
		for _, p := range r.Status.Parents {
			for _, c := range p.Conditions {
				if (c.Type == "Accepted" || c.Type == "ResolvedRefs") && c.Status != "True" {
					v.Status = "critical"
					msg := c.Message
					if msg == "" {
						msg = c.Reason
					}
					v.StatusMsg = p.ParentRef.Name + ": " + c.Type + " " + c.Status + " — " + msg
				}
			}
		}
	}
	return v
}

func (r netIngress) view() RouteView {
	v := RouteView{Kind: "Ingress", Group: "networking.k8s.io", Resource: "ingresses", Namespace: r.Metadata.Namespace, Name: r.Metadata.Name,
		Created: r.Metadata.CreationTimestamp, Entry: []string{}, Rules: []RouteRule{}}
	if r.Spec.IngressClassName != "" {
		v.Entry = append(v.Entry, "class "+r.Spec.IngressClassName)
	}
	var hosts []string
	for _, rl := range r.Spec.Rules {
		hosts = append(hosts, rl.Host)
		if rl.HTTP == nil {
			continue
		}
		for _, p := range rl.HTTP.Paths {
			path := p.Path
			if path == "" {
				path = "/"
			}
			m := path
			if rl.Host != "" {
				m = rl.Host + path
			}
			v.Rules = append(v.Rules, RouteRule{Match: m, Backends: []RouteBackend{p.Backend.view()}})
		}
	}
	if r.Spec.DefaultBackend != nil {
		v.Rules = append(v.Rules, RouteRule{Match: "default", Backends: []RouteBackend{r.Spec.DefaultBackend.view()}})
	}
	v.Hosts = uniq(hosts)
	var secrets []string
	for _, t := range r.Spec.TLS {
		secrets = append(secrets, t.SecretName)
	}
	if len(r.Spec.TLS) > 0 {
		v.TLS = "secret " + strings.Join(uniq(secrets), ", ")
	}
	var addrs []string
	for _, a := range r.Status.LoadBalancer.Ingress {
		addrs = append(addrs, a.IP+a.Hostname)
	}
	if len(addrs) > 0 {
		v.Status, v.StatusMsg = "good", "Address "+strings.Join(addrs, ", ")
	} else {
		v.Status, v.StatusMsg = "warning", "No address assigned yet"
	}
	return v
}

// ---------------------------------------------------------------- listing

// listRaw lists a resource across the namespaces this user may read.
func listRaw[T any](ctx context.Context, c *Cluster, r APIResource, ns string) ([]T, []string, error) {
	snap := c.Snap()
	if r.Namespaced && ns == "" && snap != nil && snap.Scope == "namespaced" {
		var all []T
		var warns []string
		for _, n := range snap.ScopeNamespaces {
			items, err := list[T](ctx, c.client, resPath(r, n, ""), nil)
			if err != nil {
				warns = append(warns, n+": "+friendlyErr(err))
				continue
			}
			all = append(all, items...)
		}
		if len(all) == 0 && len(warns) > 0 && len(warns) == len(snap.ScopeNamespaces) {
			return nil, nil, errors.New(strings.Join(warns, "; "))
		}
		return all, warns, nil
	}
	items, err := list[T](ctx, c.client, resPath(r, ns, ""), nil)
	return items, nil, err
}

func (c *Cluster) Routes(ctx context.Context, ns string) (*RoutesResult, error) {
	all, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	find := func(group, resource string) (APIResource, bool) {
		for _, r := range all {
			if r.Group == group && r.Resource == resource {
				return r, true
			}
		}
		return APIResource{}, false
	}
	out := &RoutesResult{IngressRoutes: []RouteView{}, HTTPRoutes: []RouteView{}, Ingresses: []RouteView{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	warn := func(what string, err error) {
		mu.Lock()
		out.Warnings = append(out.Warnings, what+": "+friendlyErr(err))
		mu.Unlock()
	}
	seenIR := map[string]bool{}
	// Traefik serves IngressRoutes under traefik.io (v3+) and, on older installs, traefik.containo.us
	for _, g := range []string{"traefik.io", "traefik.containo.us"} {
		r, ok := find(g, "ingressroutes")
		if !ok {
			continue
		}
		out.Has.IngressRoutes = true
		items, warns, err := listRaw[traefikIR](ctx, c, r, ns)
		if err != nil {
			warn("IngressRoutes ("+g+")", err)
			continue
		}
		out.Warnings = append(out.Warnings, warns...)
		for _, it := range items {
			k := it.Metadata.Namespace + "/" + it.Metadata.Name
			if seenIR[k] {
				continue // same object served by both API groups
			}
			seenIR[k] = true
			out.IngressRoutes = append(out.IngressRoutes, it.view(g))
		}
	}
	if r, ok := find("gateway.networking.k8s.io", "httproutes"); ok {
		out.Has.HTTPRoutes = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, warns, err := listRaw[gwHTTPRoute](ctx, c, r, ns)
			if err != nil {
				warn("HTTPRoutes", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			out.Warnings = append(out.Warnings, warns...)
			for _, it := range items {
				out.HTTPRoutes = append(out.HTTPRoutes, it.view(r.Group))
			}
		}()
	}
	if r, ok := find("networking.k8s.io", "ingresses"); ok {
		out.Has.Ingresses = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, warns, err := listRaw[netIngress](ctx, c, r, ns)
			if err != nil {
				warn("Ingresses", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			out.Warnings = append(out.Warnings, warns...)
			for _, it := range items {
				out.Ingresses = append(out.Ingresses, it.view())
			}
		}()
	}
	wg.Wait()
	for _, l := range [][]RouteView{out.IngressRoutes, out.HTTPRoutes, out.Ingresses} {
		sort.Slice(l, func(i, j int) bool {
			if l[i].Namespace != l[j].Namespace {
				return l[i].Namespace < l[j].Namespace
			}
			return l[i].Name < l[j].Name
		})
	}
	return out, nil
}

func (s *Server) routeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/routes", func(w http.ResponseWriter, r *http.Request) {
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		res, err := c.Routes(ctx, r.URL.Query().Get("ns"))
		if err != nil {
			writeErr(w, 502, errors.New(friendlyErr(err)))
			return
		}
		writeJSON(w, 200, res)
	})
}
