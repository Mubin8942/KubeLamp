package main

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Nodes seen through your pods: for users without rights to list nodes, the node
// names come from the pods they can read (spec.nodeName), like `kubectl get pods -o
// wide`. A pod only knows its node's registered InternalIP (status.hostIP), which on
// some clusters is an overlay address; the node's other addresses come, in order, from
// the node objects (if the user may list them), Rancher's node list (Steve /v1/nodes,
// then the management API /v3/nodes, which records the node's IP and external IP), and
// otherwise the pods. Lookups that fail are not retried for 10 minutes.

type PodNode struct {
	Name        string   `json:"name"`
	IP          string   `json:"ip"`         // the address to show: ExternalIP if known, else InternalIP
	InternalIP  string   `json:"internalIP"` // as registered with Kubernetes (status.hostIP of its pods)
	ExternalIP  string   `json:"externalIP,omitempty"`
	ExternalSrc string   `json:"externalSrc,omitempty"` // manual | node | rancher
	Pods        int      `json:"pods"`
	Running     int      `json:"running"`
	Namespaces  []string `json:"namespaces"`
	PodNames    []string `json:"podNames"` // ns/name, at most 50
}

type PodNodesResult struct {
	Nodes      []PodNode `json:"nodes"`
	NodeAccess bool      `json:"nodeAccess"` // the user may list nodes (the full Nodes view works)
	IPSource   string    `json:"ipSource"`   // nodes | rancher | pods
	Scope      string    `json:"scope"`
	Unplaced   int       `json:"unplaced"` // pods not scheduled on a node yet
}

type nodeAddr struct{ internal, external string }

// node addresses from Rancher, cached per cluster
var rancherAddrCache = struct {
	sync.Mutex
	m map[*Cluster]struct {
		at    time.Time
		addrs map[string]nodeAddr
	}
}{m: map[*Cluster]struct {
	at    time.Time
	addrs map[string]nodeAddr
}{}}

func rancherNodeAddrs(ctx context.Context, c *Cluster) map[string]nodeAddr {
	if c == nil || c.client == nil || !strings.Contains(c.client.base.Path, "/k8s/clusters/") {
		return nil
	}
	rancherAddrCache.Lock()
	e, ok := rancherAddrCache.m[c]
	rancherAddrCache.Unlock()
	if ok && time.Since(e.at) < 10*time.Minute {
		return e.addrs
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs := map[string]nodeAddr{}
	// 1. Steve: Kubernetes node objects through Rancher, if Rancher lets this user see them
	var steve struct {
		Data []Node `json:"data"`
	}
	if err := c.client.get(ctx, "/v1/nodes", nil, &steve); err == nil {
		for _, n := range steve.Data {
			addrs[n.Metadata.Name] = addrOf(n)
		}
	}
	// 2. Rancher's management API: the IPs Rancher recorded for each node
	if len(addrs) == 0 {
		id := c.client.base.Path[strings.Index(c.client.base.Path, "/k8s/clusters/")+len("/k8s/clusters/"):]
		id = strings.Trim(strings.SplitN(id, "/", 2)[0], "/")
		var v3 struct {
			Data []struct {
				NodeName          string `json:"nodeName"`
				Hostname          string `json:"hostname"`
				IPAddress         string `json:"ipAddress"`
				ExternalIPAddress string `json:"externalIpAddress"`
			} `json:"data"`
		}
		if id != "" && c.client.get(ctx, rootPath("/v3/nodes"), url.Values{"clusterId": {id}, "limit": {"-1"}}, &v3) == nil {
			for _, n := range v3.Data {
				name := n.NodeName
				if name == "" {
					name = n.Hostname
				}
				a := nodeAddr{internal: n.IPAddress, external: n.ExternalIPAddress}
				if a.external == a.internal {
					a.external = ""
				}
				addrs[strings.ToLower(name)] = a
			}
		}
	}
	rancherAddrCache.Lock()
	rancherAddrCache.m[c] = struct {
		at    time.Time
		addrs map[string]nodeAddr
	}{time.Now(), addrs}
	rancherAddrCache.Unlock()
	return addrs
}

func addrOf(n Node) nodeAddr {
	var a nodeAddr
	for _, x := range n.Status.Addresses {
		switch x.Type {
		case "InternalIP":
			if a.internal == "" {
				a.internal = x.Address
			}
		case "ExternalIP":
			if a.external == "" {
				a.external = x.Address
			}
		}
	}
	return a
}

func podNodes(snap *Snapshot, rancher map[string]nodeAddr) PodNodesResult {
	byName := map[string]*PodNode{}
	nsSeen := map[string]map[string]bool{}
	res := PodNodesResult{Nodes: []PodNode{}, NodeAccess: len(snap.Nodes) > 0, Scope: snap.Scope, IPSource: "pods"}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		n := p.Spec.NodeName
		if n == "" {
			res.Unplaced++
			continue
		}
		pn := byName[n]
		if pn == nil {
			pn = &PodNode{Name: n, Namespaces: []string{}, PodNames: []string{}}
			byName[n] = pn
			nsSeen[n] = map[string]bool{}
		}
		if pn.InternalIP == "" && p.Status.HostIP != "" {
			pn.InternalIP = p.Status.HostIP
		}
		pn.Pods++
		if p.Status.Phase == "Running" {
			pn.Running++
		}
		if ns := p.Metadata.Namespace; !nsSeen[n][ns] {
			nsSeen[n][ns] = true
			pn.Namespaces = append(pn.Namespaces, ns)
		}
		if len(pn.PodNames) < 50 {
			pn.PodNames = append(pn.PodNames, p.Metadata.Namespace+"/"+p.Metadata.Name)
		}
	}
	// better addresses: the node objects, else Rancher's list
	known := map[string]nodeAddr{}
	for _, nd := range snap.Nodes {
		known[nd.Metadata.Name] = addrOf(nd)
	}
	if len(known) > 0 {
		res.IPSource = "nodes"
	} else if len(rancher) > 0 {
		res.IPSource = "rancher"
		known = rancher
	}
	for name, pn := range byName {
		a, ok := known[name]
		if !ok {
			a, ok = known[strings.ToLower(name)]
		}
		if ok {
			if a.internal != "" {
				pn.InternalIP = a.internal
			}
			if a.external != "" {
				pn.ExternalIP, pn.ExternalSrc = a.external, res.IPSource
			}
		}
		pn.IP = pn.InternalIP
		if pn.ExternalIP != "" {
			pn.IP = pn.ExternalIP
		}
		sort.Strings(pn.Namespaces)
		sort.Strings(pn.PodNames)
		res.Nodes = append(res.Nodes, *pn)
	}
	sort.Slice(res.Nodes, func(i, j int) bool { return res.Nodes[i].Name < res.Nodes[j].Name })
	return res
}

func (s *Server) podNodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/podnodes", func(w http.ResponseWriter, r *http.Request) {
		c, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		var rancher map[string]nodeAddr
		if len(snap.Nodes) == 0 {
			rancher = rancherNodeAddrs(r.Context(), c)
		}
		res := podNodes(snap, rancher)
		for i := range res.Nodes { // an admin-set external IP wins
			if ip := s.nodeIPs.Lookup(res.Nodes[i].Name); ip != "" {
				n := &res.Nodes[i]
				n.ExternalIP, n.ExternalSrc, n.IP = ip, "manual", ip
			}
		}
		writeJSON(w, 200, res)
	})
}

// nodeExternalIPs returns a lookup of node → external IP, the same order the Nodes page
// uses: set by an admin, the node's own ExternalIP, Rancher's.
func (s *Server) nodeExternalIPs(ctx context.Context, c *Cluster, snap *Snapshot) func(string) string {
	known := map[string]string{}
	for _, nd := range snap.Nodes {
		if a := addrOf(nd); a.external != "" {
			known[strings.ToLower(nd.Metadata.Name)] = a.external
		}
	}
	if len(snap.Nodes) == 0 {
		for n, a := range rancherNodeAddrs(ctx, c) {
			if a.external != "" {
				known[strings.ToLower(n)] = a.external
			}
		}
	}
	return func(node string) string {
		if ip := s.nodeIPs.Lookup(node); ip != "" {
			return ip
		}
		return known[strings.ToLower(node)]
	}
}
