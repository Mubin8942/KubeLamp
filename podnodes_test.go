package main

import "testing"

func TestPodNodes(t *testing.T) {
	mk := func(ns, name, node, hostIP, phase string) Pod {
		var p Pod
		p.Metadata.Namespace, p.Metadata.Name, p.Spec.NodeName, p.Status.HostIP, p.Status.Phase = ns, name, node, hostIP, phase
		return p
	}
	snap := &Snapshot{Scope: "namespaced", Pods: []Pod{
		mk("app", "web-1", "w-02", "10.0.0.12", "Running"),
		mk("app", "web-2", "w-01", "10.0.0.11", "Running"),
		mk("db", "pg-0", "w-01", "10.0.0.11", "Pending"),
		mk("app", "web-3", "", "", "Pending"), // not scheduled yet
		mk("app", "job-x", "w-02", "", "Succeeded"),
	}}
	r := podNodes(snap)
	if r.NodeAccess || r.Unplaced != 1 || len(r.Nodes) != 2 {
		t.Fatalf("%+v", r)
	}
	n1, n2 := r.Nodes[0], r.Nodes[1]
	if n1.Name != "w-01" || n1.IP != "10.0.0.11" || n1.Pods != 2 || n1.Running != 1 || len(n1.Namespaces) != 2 || n1.Namespaces[0] != "app" {
		t.Errorf("w-01: %+v", n1)
	}
	if n2.Name != "w-02" || n2.IP != "10.0.0.12" || n2.Pods != 2 || n2.PodNames[0] != "app/job-x" {
		t.Errorf("w-02: %+v", n2)
	}
	// with node rights the node's InternalIP wins
	var nd Node
	nd.Metadata.Name = "w-01"
	nd.Status.Addresses = append(nd.Status.Addresses, struct {
		Type    string `json:"type"`
		Address string `json:"address"`
	}{"InternalIP", "192.168.1.11"})
	snap.Nodes = []Node{nd}
	if r := podNodes(snap); !r.NodeAccess || r.Nodes[0].IP != "192.168.1.11" {
		t.Errorf("internal IP: %+v", r.Nodes[0])
	}
	if r := podNodes(&Snapshot{}); r.Nodes == nil || len(r.Nodes) != 0 {
		t.Error("no pods → empty list, not null")
	}
}
