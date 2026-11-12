package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI imitates what a Role/RoleBinding-only user sees.
type fakeAPI struct {
	steve      []string        // Rancher's /v1/namespaces (nil: not Rancher, 404)
	nsList     []string        // /api/v1/namespaces (nil: forbidden)
	allowed    map[string]bool // namespaces where pods may be listed
	noReviews  bool            // access reviews not available (403)
	steveHits  atomic.Int32
	reviewHits atomic.Int32
}

func (f *fakeAPI) handler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, prefix)
		forbid := func() { w.WriteHeader(403); _ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "code": 403, "message": "forbidden"}) }
		switch {
		case p == "/v1/namespaces" && r.Method == "GET":
			f.steveHits.Add(1)
			if f.steve == nil {
				http.NotFound(w, r)
				return
			}
			var data []any
			for _, n := range f.steve {
				data = append(data, map[string]any{"id": n, "metadata": map[string]any{"name": n}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "collection", "data": data})
		case p == "/api/v1/namespaces":
			if f.nsList == nil {
				forbid()
				return
			}
			var items []any
			for _, n := range f.nsList {
				items = append(items, map[string]any{"metadata": map[string]any{"name": n}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{}, "items": items})
		case p == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" && r.Method == "POST":
			f.reviewHits.Add(1)
			if f.noReviews {
				forbid()
				return
			}
			var rv struct {
				Spec struct {
					ResourceAttributes struct{ Namespace, Verb, Resource string } `json:"resourceAttributes"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&rv)
			ra := rv.Spec.ResourceAttributes
			ok := ra.Verb == "list" && ra.Resource == "pods" && f.allowed[ra.Namespace]
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"allowed": ok}})
		default:
			forbid()
		}
	})
}

func discoverCluster(t *testing.T, f *fakeAPI, rancher bool, acc AccessSettings, ctxNS string) *Cluster {
	prefix := ""
	if rancher {
		prefix = "/k8s/clusters/c-tth66"
	}
	ts := httptest.NewServer(f.handler(prefix))
	t.Cleanup(ts.Close)
	rc := resolvedContext{ContextInfo: ContextInfo{Name: "x", Namespace: ctxNS}}
	rc.cluster.Server = ts.URL + prefix
	rc.user.Token = "t"
	cl, err := newClient(rc)
	if err != nil {
		t.Fatal(err)
	}
	return &Cluster{Info: rc.ContextInfo, client: cl, access: normalizeAccess(acc)}
}

func TestDiscoverNamespacesRancher(t *testing.T) {
	f := &fakeAPI{steve: []string{"argus-mz-dev", "info360-mz-dev", "info360-mz-stage", "shared-readonly"},
		allowed: map[string]bool{"argus-mz-dev": true, "info360-mz-dev": true, "info360-mz-stage": true}}
	c := discoverCluster(t, f, true, AccessSettings{}, "")
	nss, by := c.scopeNamespaces(context.Background())
	if by != "rancher" || !reflect.DeepEqual(nss, []string{"argus-mz-dev", "info360-mz-dev", "info360-mz-stage"}) {
		t.Fatalf("got %v by %q", nss, by)
	}
	// cached: no new requests within 10 minutes
	s, rv := f.steveHits.Load(), f.reviewHits.Load()
	c.scopeNamespaces(context.Background())
	if f.steveHits.Load() != s || f.reviewHits.Load() != rv {
		t.Error("discovery must be cached")
	}
	// saving the access settings looks again
	f.allowed["shared-readonly"] = true
	c.SetAccess(AccessSettings{})
	if nss, _ := c.scopeNamespaces(context.Background()); len(nss) != 4 {
		t.Errorf("after SetAccess: %v", nss)
	}
	// and so does the 10-minute timer
	f.allowed["argus-mz-dev"] = false
	c.mu.Lock()
	c.nsFoundAt = time.Now().Add(-11 * time.Minute)
	c.mu.Unlock()
	if nss, _ := c.scopeNamespaces(context.Background()); contains(nss, "argus-mz-dev") {
		t.Errorf("a removed RoleBinding must disappear: %v", nss)
	}
}

func TestDiscoverNamespacesPlainKubernetes(t *testing.T) {
	// not Rancher: Steve is never asked; the user may list namespaces
	f := &fakeAPI{nsList: []string{"kube-system", "team-a", "team-b", "team-c"}, allowed: map[string]bool{"team-a": true, "team-c": true}}
	c := discoverCluster(t, f, false, AccessSettings{}, "default")
	nss, by := c.scopeNamespaces(context.Background())
	if by != "namespaces" || !reflect.DeepEqual(nss, []string{"team-a", "team-c"}) {
		t.Fatalf("got %v by %q (the context's default namespace must not be added when discovery works)", nss, by)
	}
	if f.steveHits.Load() != 0 {
		t.Error("Rancher API asked on a non-Rancher cluster")
	}
}

func TestDiscoverNamespacesFallbacks(t *testing.T) {
	// nothing discoverable: typed namespaces + the context's namespace, like before
	f := &fakeAPI{allowed: map[string]bool{"payments": true}}
	c := discoverCluster(t, f, false, AccessSettings{Namespaces: []string{"payments", "ecrm"}}, "web")
	nss, by := c.scopeNamespaces(context.Background())
	if by != "" || !reflect.DeepEqual(nss, []string{"ecrm", "payments", "web"}) {
		t.Fatalf("typed: %v by %q", nss, by)
	}
	// nothing at all: "default"
	c2 := discoverCluster(t, &fakeAPI{}, false, AccessSettings{}, "")
	if nss, _ := c2.scopeNamespaces(context.Background()); !reflect.DeepEqual(nss, []string{"default"}) {
		t.Errorf("empty: %v", nss)
	}
	// Rancher list works but access reviews are blocked: trust Rancher's list
	f3 := &fakeAPI{steve: []string{"a1", "a2"}, noReviews: true}
	c3 := discoverCluster(t, f3, true, AccessSettings{}, "")
	if nss, by := c3.scopeNamespaces(context.Background()); by != "rancher" || !reflect.DeepEqual(nss, []string{"a1", "a2"}) {
		t.Errorf("no reviews: %v %q", nss, by)
	}
	// found + typed are merged, no duplicates
	f4 := &fakeAPI{steve: []string{"a1", "a2"}, allowed: map[string]bool{"a1": true, "a2": true}}
	c4 := discoverCluster(t, f4, true, AccessSettings{Namespaces: []string{"a2", "extra"}}, "")
	if nss, _ := c4.scopeNamespaces(context.Background()); !reflect.DeepEqual(nss, []string{"a1", "a2", "extra"}) {
		t.Errorf("merge: %v", nss)
	}
	// candidates where the user has no access are dropped, even all of them
	f5 := &fakeAPI{nsList: []string{"x", "y"}, allowed: map[string]bool{}}
	c5 := discoverCluster(t, f5, false, AccessSettings{}, "")
	if nss, by := c5.scopeNamespaces(context.Background()); by != "" || !reflect.DeepEqual(nss, []string{"default"}) {
		t.Errorf("none allowed: %v %q", nss, by)
	}
	// Rancher-looking junk names are ignored
	f6 := &fakeAPI{steve: []string{"ok-ns", "../etc", "Bad_Name"}, allowed: map[string]bool{"ok-ns": true, "../etc": true, "Bad_Name": true}}
	c6 := discoverCluster(t, f6, true, AccessSettings{}, "")
	if nss, _ := c6.scopeNamespaces(context.Background()); !reflect.DeepEqual(nss, []string{"ok-ns"}) {
		t.Errorf("junk: %v", nss)
	}
}

func TestDiscoverNamespacesCap(t *testing.T) {
	var many []string
	allowed := map[string]bool{}
	for i := 0; i < nsDiscoverMax+200; i++ {
		n := "ns-" + itoa(i)
		many = append(many, n)
		allowed[n] = true
	}
	f := &fakeAPI{nsList: many, allowed: allowed}
	c := discoverCluster(t, f, false, AccessSettings{}, "")
	nss, _ := c.scopeNamespaces(context.Background())
	if len(nss) != nsDiscoverMax || int(f.reviewHits.Load()) != nsDiscoverMax {
		t.Errorf("cap: %d namespaces, %d reviews", len(nss), f.reviewHits.Load())
	}
}
