package main

import (
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Only the fields KubeDeck uses — keeps decoding cheap and memory small.

type OwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type Meta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	OwnerReferences   []OwnerRef        `json:"ownerReferences,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type ResourceList map[string]string

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type Volume struct {
	Name string `json:"name"`
	PVC  *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim,omitempty"`
}

type Container struct {
	Name         string        `json:"name"`
	Image        string        `json:"image"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
	Resources    struct {
		Requests ResourceList `json:"requests,omitempty"`
		Limits   ResourceList `json:"limits,omitempty"`
	} `json:"resources"`
}

type PodSpec struct {
	NodeName       string      `json:"nodeName,omitempty"`
	Containers     []Container `json:"containers"`
	InitContainers []Container `json:"initContainers,omitempty"`
	Volumes        []Volume    `json:"volumes,omitempty"`
}

type ContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message,omitempty"`
	} `json:"waiting,omitempty"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running,omitempty"`
	Terminated *struct {
		Reason     string    `json:"reason"`
		ExitCode   int       `json:"exitCode"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated,omitempty"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	Image        string         `json:"image"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

type Pod struct {
	Metadata Meta    `json:"metadata"`
	Spec     PodSpec `json:"spec"`
	Status   struct {
		Phase             string            `json:"phase"`
		Reason            string            `json:"reason,omitempty"`
		Message           string            `json:"message,omitempty"`
		PodIP             string            `json:"podIP,omitempty"`
		StartTime         *time.Time        `json:"startTime,omitempty"`
		QOSClass          string            `json:"qosClass,omitempty"`
		Conditions        []Condition       `json:"conditions,omitempty"`
		ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
	} `json:"status"`
}

type LabelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels,omitempty"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values,omitempty"`
	} `json:"matchExpressions,omitempty"`
}

type PodTemplate struct {
	Metadata Meta    `json:"metadata"`
	Spec     PodSpec `json:"spec"`
}

type Deployment struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Replicas *int32        `json:"replicas,omitempty"`
		Selector LabelSelector `json:"selector"`
		Template PodTemplate   `json:"template"`
		Strategy struct {
			Type string `json:"type"`
		} `json:"strategy"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration  int64       `json:"observedGeneration"`
		Replicas            int32       `json:"replicas"`
		ReadyReplicas       int32       `json:"readyReplicas"`
		UpdatedReplicas     int32       `json:"updatedReplicas"`
		AvailableReplicas   int32       `json:"availableReplicas"`
		UnavailableReplicas int32       `json:"unavailableReplicas"`
		Conditions          []Condition `json:"conditions,omitempty"`
	} `json:"status"`
}

type StatefulSet struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Replicas *int32        `json:"replicas,omitempty"`
		Selector LabelSelector `json:"selector"`
		Template PodTemplate   `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas          int32 `json:"replicas"`
		ReadyReplicas     int32 `json:"readyReplicas"`
		UpdatedReplicas   int32 `json:"updatedReplicas"`
		AvailableReplicas int32 `json:"availableReplicas"`
	} `json:"status"`
}

type DaemonSet struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Selector LabelSelector `json:"selector"`
		Template PodTemplate   `json:"template"`
	} `json:"spec"`
	Status struct {
		DesiredNumberScheduled int32 `json:"desiredNumberScheduled"`
		NumberReady            int32 `json:"numberReady"`
		UpdatedNumberScheduled int32 `json:"updatedNumberScheduled"`
		NumberAvailable        int32 `json:"numberAvailable"`
	} `json:"status"`
}

type Node struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable,omitempty"`
		Taints        []struct {
			Key    string `json:"key"`
			Effect string `json:"effect"`
		} `json:"taints,omitempty"`
	} `json:"spec"`
	Status struct {
		Capacity    ResourceList `json:"capacity"`
		Allocatable ResourceList `json:"allocatable"`
		Conditions  []Condition  `json:"conditions"`
		Addresses   []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		NodeInfo struct {
			KubeletVersion          string `json:"kubeletVersion"`
			OSImage                 string `json:"osImage"`
			KernelVersion           string `json:"kernelVersion"`
			ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
			Architecture            string `json:"architecture"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type Namespace struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type Event struct {
	Metadata       Meta `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"involvedObject"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Type           string     `json:"type"`
	Count          int        `json:"count"`
	FirstTimestamp *time.Time `json:"firstTimestamp"`
	LastTimestamp  *time.Time `json:"lastTimestamp"`
	EventTime      *time.Time `json:"eventTime"`
	Source         struct {
		Component string `json:"component"`
	} `json:"source"`
	Series *struct {
		Count            int        `json:"count"`
		LastObservedTime *time.Time `json:"lastObservedTime"`
	} `json:"series,omitempty"`
}

func (e *Event) When() time.Time {
	for _, t := range []*time.Time{e.LastTimestamp, seriesTime(e), e.EventTime, e.FirstTimestamp} {
		if t != nil && !t.IsZero() {
			return *t
		}
	}
	return e.Metadata.CreationTimestamp
}

func seriesTime(e *Event) *time.Time {
	if e.Series != nil {
		return e.Series.LastObservedTime
	}
	return nil
}

type NodeMetrics struct {
	Metadata Meta         `json:"metadata"`
	Usage    ResourceList `json:"usage"`
}

type PodMetrics struct {
	Metadata   Meta `json:"metadata"`
	Containers []struct {
		Name  string       `json:"name"`
		Usage ResourceList `json:"usage"`
	} `json:"containers"`
}

// ---------------------------------------------------------------- quantities

var suffixes = map[string]float64{
	"n": 1e-9, "u": 1e-6, "m": 1e-3, "": 1,
	"k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

// parseQuantity turns "250m", "1.5", "128Mi", "1e3", "123456n" into a float in base units
// (cores for CPU, bytes for memory).
func parseQuantity(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	i := len(s)
	for i > 0 && !(s[i-1] >= '0' && s[i-1] <= '9' || s[i-1] == '.') {
		i--
	}
	num, suf := s[:i], s[i:]
	if strings.ContainsAny(suf, "eE") && suf != "E" && suf != "Ei" {
		// exponent form like 1e3
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	if m, ok := suffixes[suf]; ok {
		return v * m
	}
	return v
}

func (r ResourceList) cpu() float64 { return parseQuantity(r["cpu"]) }
func (r ResourceList) mem() float64 { return parseQuantity(r["memory"]) }

// ---------------------------------------------------------------- selectors

func (s LabelSelector) String() string {
	var parts []string
	keys := make([]string, 0, len(s.MatchLabels))
	for k := range s.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+s.MatchLabels[k])
	}
	for _, e := range s.MatchExpressions {
		switch e.Operator {
		case "In":
			parts = append(parts, e.Key+" in ("+strings.Join(e.Values, ",")+")")
		case "NotIn":
			parts = append(parts, e.Key+" notin ("+strings.Join(e.Values, ",")+")")
		case "Exists":
			parts = append(parts, e.Key)
		case "DoesNotExist":
			parts = append(parts, "!"+e.Key)
		}
	}
	return strings.Join(parts, ",")
}

func (s LabelSelector) Empty() bool { return len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0 }

func (s LabelSelector) Matches(l map[string]string) bool {
	if s.Empty() {
		return false
	}
	for k, v := range s.MatchLabels {
		if l[k] != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, ok := l[e.Key]
		switch e.Operator {
		case "In":
			if !ok || !contains(e.Values, v) {
				return false
			}
		case "NotIn":
			if ok && contains(e.Values, v) {
				return false
			}
		case "Exists":
			if !ok {
				return false
			}
		case "DoesNotExist":
			if ok {
				return false
			}
		}
	}
	return true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// parseSelector parses the simple "k=v,k2=v2" / "k!=v" form typed by users into a
// selector we can also evaluate locally. Complex forms are passed through to the API.
func parseSelector(s string) (LabelSelector, error) {
	var sel LabelSelector
	sel.MatchLabels = map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "=="); ok {
			sel.MatchLabels[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else if k, v, ok := strings.Cut(part, "!="); ok {
			sel.MatchExpressions = append(sel.MatchExpressions, struct {
				Key      string   `json:"key"`
				Operator string   `json:"operator"`
				Values   []string `json:"values,omitempty"`
			}{strings.TrimSpace(k), "NotIn", []string{strings.TrimSpace(v)}})
		} else if k, v, ok := strings.Cut(part, "="); ok {
			sel.MatchLabels[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else {
			return sel, errors.New("only key=value / key!=value selectors are supported here")
		}
	}
	if sel.Empty() {
		return sel, errors.New("empty selector")
	}
	return sel, nil
}

// ---------------------------------------------------------------- pod helpers

// podStatus mirrors the STATUS column of `kubectl get pods`.
func podStatus(p *Pod) string {
	if p.Metadata.DeletionTimestamp != nil {
		return "Terminating"
	}
	reason := p.Status.Phase
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			reason = cs.State.Waiting.Reason
		} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && p.Status.Phase != "Succeeded" {
			reason = cs.State.Terminated.Reason
		}
	}
	return reason
}

func podReady(p *Pod) (ready, total int) {
	total = len(p.Spec.Containers)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return
}

func podRestarts(p *Pod) int {
	n := 0
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}

// podRequests sums container requests/limits (CPU cores, memory bytes).
func podResources(spec *PodSpec) (reqCPU, reqMem, limCPU, limMem float64) {
	for _, c := range spec.Containers {
		reqCPU += c.Resources.Requests.cpu()
		reqMem += c.Resources.Requests.mem()
		limCPU += c.Resources.Limits.cpu()
		limMem += c.Resources.Limits.mem()
	}
	// init containers run before app containers; effective request is the max
	for _, c := range spec.InitContainers {
		reqCPU = math.Max(reqCPU, c.Resources.Requests.cpu())
		reqMem = math.Max(reqMem, c.Resources.Requests.mem())
	}
	return
}

func nodeReady(n *Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func nodeRoles(n *Node) []string {
	var roles []string
	for k := range n.Metadata.Labels {
		if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && r != "" {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		roles = []string{"worker"}
	}
	return roles
}

func ioReadFull(r io.Reader, b []byte) (int, error) {
	n, err := io.ReadFull(r, b)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}
