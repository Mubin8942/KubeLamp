package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildProbeDS(t *testing.T) {
	b, name, err := buildProbeDS(defaultProbeDS, "payments", "a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	var ds struct {
		Metadata struct {
			Name, Namespace string
			Labels          map[string]string
		}
		Spec struct {
			Selector struct{ MatchLabels map[string]string } `json:"selector"`
			Template struct {
				Metadata struct{ Labels map[string]string }
				Spec     struct {
					AutomountServiceAccountToken bool `json:"automountServiceAccountToken"`
					SecurityContext              struct {
						RunAsNonRoot bool `json:"runAsNonRoot"`
					} `json:"securityContext"`
					Containers []struct {
						Image           string
						SecurityContext struct {
							AllowPrivilegeEscalation bool `json:"allowPrivilegeEscalation"`
							ReadOnlyRootFilesystem   bool `json:"readOnlyRootFilesystem"`
						} `json:"securityContext"`
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &ds); err != nil {
		t.Fatal(err)
	}
	if name != "kubelamp-probe-a1b2c3" || ds.Metadata.Name != name || ds.Metadata.Namespace != "payments" || ds.Metadata.Labels[probeLabel] != "true" {
		t.Errorf("metadata: %+v", ds.Metadata)
	}
	sel, pl := ds.Spec.Selector.MatchLabels, ds.Spec.Template.Metadata.Labels
	if sel[probeRunLabel] != "a1b2c3" || pl[probeRunLabel] != "a1b2c3" || sel["app.kubernetes.io/name"] != "kubelamp-probe" {
		t.Errorf("selector %v / pod labels %v must tie the pods to this test", sel, pl)
	}
	sp := ds.Spec.Template.Spec
	c := sp.Containers[0]
	if sp.AutomountServiceAccountToken || !sp.SecurityContext.RunAsNonRoot || c.SecurityContext.AllowPrivilegeEscalation || !c.SecurityContext.ReadOnlyRootFilesystem || !strings.HasPrefix(c.Image, "curlimages/curl:") {
		t.Errorf("default must be locked down: %+v", sp)
	}
	for name, bad := range map[string]string{
		"not yaml":      "a: [",
		"a deployment":  "apiVersion: apps/v1\nkind: Deployment\nspec: {template: {spec: {containers: [{name: x, image: y}]}}}\n",
		"no containers": "apiVersion: apps/v1\nkind: DaemonSet\nspec: {template: {spec: {}}}\n",
		"not one doc":   "- a\n- b\n",
	} {
		if checkProbeTemplate(bad) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if checkProbeTemplate(strings.Replace(defaultProbeDS, "curlimages/curl:8.11.1", "harbor.example.com/library/curl:8.11.1", 1)) != nil {
		t.Error("a changed image must be accepted")
	}
}

func TestBuildProbeDSOnlyNodes(t *testing.T) {
	b, _, err := buildProbeDS(defaultProbeDS, "payments", "a1b2c3", "worker-1", "worker-4")
	if err != nil {
		t.Fatal(err)
	}
	var ds struct {
		Spec struct {
			Template struct {
				Spec struct {
					Affinity struct {
						NodeAffinity struct {
							Req struct {
								Terms []struct {
									MatchFields []struct {
										Key, Operator string
										Values        []string
									} `json:"matchFields"`
								} `json:"nodeSelectorTerms"`
							} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
						} `json:"nodeAffinity"`
					} `json:"affinity"`
					Tolerations []any `json:"tolerations"`
				}
			}
		}
	}
	if err := json.Unmarshal(b, &ds); err != nil {
		t.Fatal(err)
	}
	ts := ds.Spec.Template.Spec.Affinity.NodeAffinity.Req.Terms
	var got []string
	for _, tm := range ts { // the API server takes exactly one value per node field selector
		if len(tm.MatchFields) != 1 || tm.MatchFields[0].Key != "metadata.name" || tm.MatchFields[0].Operator != "In" || len(tm.MatchFields[0].Values) != 1 {
			t.Fatalf("affinity term: %+v", tm)
		}
		got = append(got, tm.MatchFields[0].Values[0])
	}
	if strings.Join(got, ",") != "worker-1,worker-4" {
		t.Errorf("affinity: %+v", ts)
	}
	if len(ds.Spec.Template.Spec.Tolerations) == 0 {
		t.Error("tolerations must be kept")
	}
	// no nodes: no affinity
	b, _, _ = buildProbeDS(defaultProbeDS, "payments", "a1b2c3")
	if strings.Contains(string(b), "nodeAffinity") {
		t.Error("affinity set without nodes")
	}
}
