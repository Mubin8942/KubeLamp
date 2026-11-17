package main

import (
	"net/http"
	"sort"
)

// Nodes seen through your pods: for users without rights to list nodes, the node
// names and IPs come from the pods they can read (spec.nodeName, status.hostIP), the
// same thing `kubectl get pods -o wide` shows. No extra API calls: the pods are already
// in the snapshot.

type PodNode struct {
	Name       string   `json:"name"`
	IP         string   `json:"ip"`
	Pods       int      `json:"pods"`
	Running    int      `json:"running"`
	Namespaces []string `json:"namespaces"`
	PodNames   []string `json:"podNames"` // ns/name, at most 50
}

type PodNodesResult struct {
	Nodes      []PodNode `json:"nodes"`
	NodeAccess bool      `json:"nodeAccess"` // the user may list nodes (the full Nodes view works)
	Scope      string    `json:"scope"`
	Unplaced   int       `json:"unplaced"` // pods not scheduled on a node yet
}

func podNodes(snap *Snapshot) PodNodesResult {
	byName := map[string]*PodNode{}
	nsSeen := map[string]map[string]bool{}
	res := PodNodesResult{Nodes: []PodNode{}, NodeAccess: len(snap.Nodes) > 0, Scope: snap.Scope}
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
		if pn.IP == "" && p.Status.HostIP != "" {
			pn.IP = p.Status.HostIP
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
	// with node rights the node's own InternalIP is better than a pod's hostIP
	for _, nd := range snap.Nodes {
		if pn := byName[nd.Metadata.Name]; pn != nil {
			for _, a := range nd.Status.Addresses {
				if a.Type == "InternalIP" {
					pn.IP = a.Address
				}
			}
		}
	}
	for _, pn := range byName {
		sort.Strings(pn.Namespaces)
		sort.Strings(pn.PodNames)
		res.Nodes = append(res.Nodes, *pn)
	}
	sort.Slice(res.Nodes, func(i, j int) bool { return res.Nodes[i].Name < res.Nodes[j].Name })
	return res
}

func (s *Server) podNodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/podnodes", func(w http.ResponseWriter, r *http.Request) {
		_, snap, ok := s.cluster(w, r)
		if !ok {
			return
		}
		writeJSON(w, 200, podNodes(snap))
	})
}
