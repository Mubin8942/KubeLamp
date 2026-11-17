package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A tiny DaemonSet controller for KubeLamp's probe DaemonSets (label kubelamp.io/probe):
// creating one starts a ready pod on every node, deleting it removes them. Other
// DaemonSets keep the fixed list. /__probeds reports how many were created / deleted.

var probeDS = struct {
	sync.Mutex
	m                map[string]obj // ns/name → DaemonSet
	created, deleted atomic.Int32
}{m: map[string]obj{}}

func (c *cluster) probeDaemonSets(w http.ResponseWriter, r *http.Request, rt route) bool {
	key := rt.ns + "/" + rt.name
	switch {
	case r.Method == http.MethodPost && rt.name == "":
		var ds obj
		if json.NewDecoder(r.Body).Decode(&ds) != nil {
			return false
		}
		md, _ := ds["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if labels["kubelamp.io/probe"] != "true" {
			w.WriteHeader(403)
			writeJSON(w, obj{"kind": "Status", "code": 403, "message": "daemonsets.apps is forbidden in this mock"})
			return true
		}
		name, _ := md["name"].(string)
		tl := map[string]string{}
		if tpl, ok := ds["spec"].(map[string]any)["template"].(map[string]any); ok {
			if tm, ok := tpl["metadata"].(map[string]any); ok {
				for k, v := range tm["labels"].(map[string]any) {
					tl[k] = v.(string)
				}
			}
		}
		md["namespace"], md["creationTimestamp"] = rt.ns, time.Now().UTC().Format(time.RFC3339)
		c.mu.Lock()
		var started []*pod
		only := map[string]bool{} // spec.template.spec.affinity: matchFields metadata.name In [...]
		if b, err := json.Marshal(ds["spec"]); err == nil {
			var sp struct {
				Template struct {
					Spec struct {
						Affinity struct {
							NodeAffinity struct {
								Req struct {
									Terms []struct {
										MatchFields []struct {
											Key    string   `json:"key"`
											Values []string `json:"values"`
										} `json:"matchFields"`
									} `json:"nodeSelectorTerms"`
								} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
							} `json:"nodeAffinity"`
						} `json:"affinity"`
					} `json:"spec"`
				} `json:"template"`
			}
			json.Unmarshal(b, &sp)
			for _, t := range sp.Template.Spec.Affinity.NodeAffinity.Req.Terms {
				for _, f := range t.MatchFields {
					if len(f.Values) != 1 {
						w.WriteHeader(422)
						writeJSON(w, obj{"kind": "Status", "code": 422, "message": "matchFields[0].values: Required value: must be only one value when `operator` is 'In' or 'NotIn' for node field selector"})
						return true
					}
					for _, v := range f.Values {
						only[v] = true
					}
				}
			}
		}
		for _, n := range c.nodes {
			if len(only) > 0 && !only[n] {
				continue
			}
			p := &pod{ns: rt.ns, name: name + "-" + suffix(c.rng)[:5], deploy: name, kind: "DaemonSet", node: n, status: "Running",
				labels: tl, containers: []string{"probe"}, cpuBase: 1, memBase: 4, reqCPU: 5, reqMem: 16, limCPU: 200, limMem: 64, created: time.Now()}
			c.pods = append(c.pods, p)
			started = append(started, p)
		}
		ds["status"] = obj{"desiredNumberScheduled": len(started)}
		c.mu.Unlock()
		go func() { // like a real cluster: the image pull and start take a moment
			time.Sleep(3 * time.Second)
			c.mu.Lock()
			for _, p := range started {
				p.ready = true
			}
			c.mu.Unlock()
		}()
		probeDS.Lock()
		probeDS.m[rt.ns+"/"+name] = ds
		probeDS.Unlock()
		probeDS.created.Add(1)
		w.WriteHeader(201)
		writeJSON(w, ds)
		return true
	case r.Method == http.MethodGet && rt.name == "" && strings.Contains(r.URL.Query().Get("labelSelector"), "kubelamp.io/probe"):
		probeDS.Lock()
		items := []obj{}
		for k, d := range probeDS.m {
			if strings.HasPrefix(k, rt.ns+"/") {
				items = append(items, d)
			}
		}
		probeDS.Unlock()
		writeJSON(w, obj{"metadata": obj{}, "items": items})
		return true
	case rt.name != "":
		probeDS.Lock()
		ds, ok := probeDS.m[key]
		if ok && r.Method == http.MethodDelete {
			delete(probeDS.m, key)
		}
		probeDS.Unlock()
		if !ok {
			return false
		}
		if r.Method == http.MethodDelete {
			c.mu.Lock()
			keep := c.pods[:0]
			for _, p := range c.pods {
				if !(p.ns == rt.ns && p.deploy == rt.name) {
					keep = append(keep, p)
				}
			}
			c.pods = keep
			c.mu.Unlock()
			probeDS.deleted.Add(1)
		}
		writeJSON(w, ds)
		return true
	}
	return false
}
