package main

// Network policy analysis for the connectivity tester. Given a source pod and a
// target (IP or host, port), work out whether the egress rules of Kubernetes
// NetworkPolicies and Cilium (Cluster)NetworkPolicies in the cluster would allow
// it. This reads policy objects only; nothing is sent over the network.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- types

type LabelSel struct {
	MatchLabels      map[string]string `json:"matchLabels,omitempty"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values,omitempty"`
	} `json:"matchExpressions,omitempty"`
}

// intOrStr decodes a Kubernetes IntOrString port into its text form.
type intOrStr string

func (v *intOrStr) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var t string
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		*v = intOrStr(t)
		return nil
	}
	*v = intOrStr(s)
	return nil
}

type NPPort struct {
	Protocol string   `json:"protocol,omitempty"`
	Port     intOrStr `json:"port,omitempty"`
	EndPort  *int     `json:"endPort,omitempty"`
}

type NPPeer struct {
	PodSelector       *LabelSel `json:"podSelector,omitempty"`
	NamespaceSelector *LabelSel `json:"namespaceSelector,omitempty"`
	IPBlock           *struct {
		CIDR   string   `json:"cidr"`
		Except []string `json:"except,omitempty"`
	} `json:"ipBlock,omitempty"`
}

type NetPol struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		PodSelector LabelSel `json:"podSelector"`
		PolicyTypes []string `json:"policyTypes,omitempty"`
		Egress      *[]struct {
			Ports []NPPort `json:"ports,omitempty"`
			To    []NPPeer `json:"to,omitempty"`
		} `json:"egress,omitempty"`
	} `json:"spec"`
}

type CiliumPort struct {
	Port     string `json:"port"`
	EndPort  int    `json:"endPort,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

type CiliumEgress struct {
	ToEndpoints []LabelSel `json:"toEndpoints,omitempty"`
	ToCIDR      []string   `json:"toCIDR,omitempty"`
	ToCIDRSet   []struct {
		CIDR   string   `json:"cidr"`
		Except []string `json:"except,omitempty"`
	} `json:"toCIDRSet,omitempty"`
	ToEntities []string `json:"toEntities,omitempty"`
	ToFQDNs    []struct {
		MatchName    string `json:"matchName,omitempty"`
		MatchPattern string `json:"matchPattern,omitempty"`
	} `json:"toFQDNs,omitempty"`
	ToServices []json.RawMessage `json:"toServices,omitempty"`
	ToGroups   []json.RawMessage `json:"toGroups,omitempty"`
	ToRequires []json.RawMessage `json:"toRequires,omitempty"`
	ToPorts    []struct {
		Ports []CiliumPort `json:"ports,omitempty"`
	} `json:"toPorts,omitempty"`
}

type CiliumRule struct {
	EndpointSelector *LabelSel       `json:"endpointSelector,omitempty"`
	Egress           *[]CiliumEgress `json:"egress,omitempty"`
	EgressDeny       *[]CiliumEgress `json:"egressDeny,omitempty"`
}

type CiliumPolicy struct {
	Metadata Meta         `json:"metadata"`
	Spec     *CiliumRule  `json:"spec,omitempty"`
	Specs    []CiliumRule `json:"specs,omitempty"`
	cluster  bool
}

func (p *CiliumPolicy) rules() []CiliumRule {
	var out []CiliumRule
	if p.Spec != nil {
		out = append(out, *p.Spec)
	}
	return append(out, p.Specs...)
}

// PolicyVerdict is the outcome for one (source pod, target) pair.
type PolicyVerdict struct {
	State string `json:"s"`           // none | allowed | blocked | unknown
	Why   string `json:"w,omitempty"` // human explanation
}

// policyEnv holds everything needed to evaluate verdicts for one namespace.
type policyEnv struct {
	ns       string
	netpols  []NetPol
	cilium   []CiliumPolicy
	nsLabels map[string]map[string]string
	podsByIP map[string]*Pod
	nodeIPs  map[string]bool
	Warnings []string
	// Found is set when at least one policy object exists in scope.
	Found bool
	// NPDenied is set when NetworkPolicies could not be read at all.
	NPDenied bool
}

// ---------------------------------------------------------------- loading

func isNotFoundOrForbidden(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "404") || strings.Contains(m, "403") || strings.Contains(m, "forbidden") || strings.Contains(m, "could not find the requested resource") || strings.Contains(m, "not found")
}

func (c *Cluster) loadPolicyEnv(ctx context.Context, snap *Snapshot, ns string) *policyEnv {
	e := &policyEnv{ns: ns, nsLabels: map[string]map[string]string{}, podsByIP: map[string]*Pod{}, nodeIPs: map[string]bool{}}
	for i := range snap.Namespaces {
		e.nsLabels[snap.Namespaces[i].Metadata.Name] = snap.Namespaces[i].Metadata.Labels
	}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Status.PodIP != "" && !p.Spec.HostNetwork {
			e.podsByIP[p.Status.PodIP] = p
		}
	}
	for i := range snap.Nodes {
		for _, a := range snap.Nodes[i].Status.Addresses {
			if a.Type == "InternalIP" || a.Type == "ExternalIP" {
				e.nodeIPs[a.Address] = true
			}
		}
	}
	if snap.Scope == "namespaced" {
		e.Warnings = append(e.Warnings, "Your credentials only cover some namespaces, so pods and nodes elsewhere are not visible: a policy rule that targets them cannot be matched and may show as blocked or unknown.")
	}
	nsPath := url.PathEscape(ns)
	nps, err := list[NetPol](ctx, c.client, "/apis/networking.k8s.io/v1/namespaces/"+nsPath+"/networkpolicies", nil)
	if err != nil && !isNotFoundOrForbidden(err) {
		e.Warnings = append(e.Warnings, "Could not read NetworkPolicies: "+friendlyErr(err))
	} else if err != nil {
		e.NPDenied = true
		e.Warnings = append(e.Warnings, "No access to read NetworkPolicies in this namespace; the policy check ignores them.")
	}
	e.netpols = nps
	cn, err := list[CiliumPolicy](ctx, c.client, "/apis/cilium.io/v2/namespaces/"+nsPath+"/ciliumnetworkpolicies", nil)
	if err != nil && !isNotFoundOrForbidden(err) {
		e.Warnings = append(e.Warnings, "Could not read CiliumNetworkPolicies: "+friendlyErr(err))
	}
	e.cilium = append(e.cilium, cn...)
	if snap.Scope != "namespaced" {
		cc, err := list[CiliumPolicy](ctx, c.client, "/apis/cilium.io/v2/ciliumclusterwidenetworkpolicies", nil)
		if err != nil && !isNotFoundOrForbidden(err) {
			e.Warnings = append(e.Warnings, "Could not read CiliumClusterwideNetworkPolicies: "+friendlyErr(err))
		}
		for i := range cc {
			cc[i].cluster = true
		}
		e.cilium = append(e.cilium, cc...)
	}
	e.Found = len(e.netpols) > 0 || len(e.cilium) > 0
	return e
}

// ---------------------------------------------------------------- selectors

func (s *LabelSel) matches(labels map[string]string, norm func(string) string) bool {
	if s == nil {
		return false
	}
	key := func(k string) string {
		if norm != nil {
			return norm(k)
		}
		return k
	}
	for k, v := range s.MatchLabels {
		if got, ok := labels[key(k)]; !ok || got != v {
			return false
		}
	}
	for _, ex := range s.MatchExpressions {
		got, has := labels[key(ex.Key)]
		in := false
		for _, v := range ex.Values {
			if v == got {
				in = true
			}
		}
		switch ex.Operator {
		case "In":
			if !has || !in {
				return false
			}
		case "NotIn":
			if has && in {
				return false
			}
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		}
	}
	return true
}

func ciliumNorm(k string) string {
	for _, pre := range []string{"k8s:", "any:"} {
		k = strings.TrimPrefix(k, pre)
	}
	return k
}

// ciliumLabels builds the label view Cilium matches against: pod labels plus the
// namespace name and namespace labels under their special keys.
func (e *policyEnv) ciliumLabels(p *Pod) map[string]string {
	m := map[string]string{}
	for k, v := range p.Metadata.Labels {
		m[k] = v
	}
	m["io.kubernetes.pod.namespace"] = p.Metadata.Namespace
	for k, v := range e.nsLabels[p.Metadata.Namespace] {
		m["io.cilium.k8s.namespace.labels."+k] = v
	}
	return m
}

func (e *policyEnv) k8sNSLabels(ns string) map[string]string {
	m := map[string]string{}
	for k, v := range e.nsLabels[ns] {
		m[k] = v
	}
	if _, ok := m["kubernetes.io/metadata.name"]; !ok {
		m["kubernetes.io/metadata.name"] = ns
	}
	return m
}

// ---------------------------------------------------------------- target

type polTarget struct {
	IP    netip.Addr
	HasIP bool
	Host  string
	Port  int
	Proto string // TCP
	Label string
}

func (t polTarget) known(e *policyEnv) (pod *Pod, node bool) {
	if !t.HasIP {
		return nil, false
	}
	return e.podsByIP[t.IP.String()], e.nodeIPs[t.IP.String()]
}

func inCIDR(cidr string, ip netip.Addr) bool {
	pfx, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return false
	}
	return pfx.Contains(ip)
}

func cidrAllows(cidr string, except []string, ip netip.Addr) bool {
	if !inCIDR(cidr, ip) {
		return false
	}
	for _, x := range except {
		if inCIDR(x, ip) {
			return false
		}
	}
	return true
}

// result of a tri-state match
type tri int

const (
	no tri = iota
	maybe
	yes
)

func best(a, b tri) tri {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- Kubernetes NetworkPolicy

func npPortMatch(ports []NPPort, t polTarget) tri {
	if len(ports) == 0 {
		return yes
	}
	res := no
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "TCP"
		}
		if !strings.EqualFold(proto, t.Proto) {
			continue
		}
		if p.Port == "" {
			res = best(res, yes)
			continue
		}
		n, err := strconv.Atoi(string(p.Port))
		if err != nil { // named port: needs the target's container spec
			res = best(res, maybe)
			continue
		}
		hi := n
		if p.EndPort != nil {
			hi = *p.EndPort
		}
		if t.Port >= n && t.Port <= hi {
			res = best(res, yes)
		}
	}
	return res
}

func (e *policyEnv) npPeerMatch(peers []NPPeer, polNS string, t polTarget) tri {
	if len(peers) == 0 {
		return yes
	}
	pod, _ := t.known(e)
	res := no
	for _, pr := range peers {
		switch {
		case pr.IPBlock != nil:
			if !t.HasIP {
				res = best(res, maybe) // a hostname resolves to an unknown IP
			} else if cidrAllows(pr.IPBlock.CIDR, pr.IPBlock.Except, t.IP) {
				res = best(res, yes)
			}
		default:
			if pod == nil {
				continue
			}
			if pr.NamespaceSelector != nil {
				if !pr.NamespaceSelector.matches(e.k8sNSLabels(pod.Metadata.Namespace), nil) {
					continue
				}
			} else if pod.Metadata.Namespace != polNS {
				continue
			}
			if pr.PodSelector != nil && !pr.PodSelector.matches(pod.Metadata.Labels, nil) {
				continue
			}
			res = best(res, yes)
		}
	}
	return res
}

// ---------------------------------------------------------------- Cilium

func ciliumPortMatch(rules []struct {
	Ports []CiliumPort `json:"ports,omitempty"`
}, t polTarget) tri {
	if len(rules) == 0 {
		return yes
	}
	res := no
	for _, r := range rules {
		if len(r.Ports) == 0 {
			res = best(res, yes)
			continue
		}
		for _, p := range r.Ports {
			proto := strings.ToUpper(p.Protocol)
			if proto != "" && proto != "ANY" && proto != t.Proto {
				continue
			}
			n, err := strconv.Atoi(p.Port)
			if err != nil {
				res = best(res, maybe)
				continue
			}
			hi := n
			if p.EndPort > 0 {
				hi = p.EndPort
			}
			if n == 0 || (t.Port >= n && t.Port <= hi) {
				res = best(res, yes)
			}
		}
	}
	return res
}

func fqdnPatternRE(pat string) *regexp.Regexp {
	q := regexp.QuoteMeta(strings.ToLower(strings.TrimSuffix(pat, ".")))
	q = strings.ReplaceAll(q, `\*`, `[-a-zA-Z0-9_]*`)
	return regexp.MustCompile("^" + q + "$")
}

func (e *policyEnv) ciliumPeerMatch(r *CiliumEgress, polNS string, cluster bool, t polTarget) tri {
	hasPeer := len(r.ToEndpoints)+len(r.ToCIDR)+len(r.ToCIDRSet)+len(r.ToEntities)+len(r.ToFQDNs)+len(r.ToServices)+len(r.ToGroups) > 0
	if !hasPeer {
		return yes // ports-only rule: any destination
	}
	pod, node := t.known(e)
	res := no
	for i := range r.ToEndpoints {
		sel := &r.ToEndpoints[i]
		if pod == nil {
			continue
		}
		if !cluster && sel.MatchLabels["io.kubernetes.pod.namespace"] == "" && sel.MatchLabels["k8s:io.kubernetes.pod.namespace"] == "" && pod.Metadata.Namespace != polNS {
			continue
		}
		if sel.matches(e.ciliumLabels(pod), ciliumNorm) {
			res = best(res, yes)
		}
	}
	for _, c := range r.ToCIDR {
		if t.HasIP && inCIDR(c, t.IP) {
			res = best(res, yes)
		}
	}
	for _, c := range r.ToCIDRSet {
		if t.HasIP && cidrAllows(c.CIDR, c.Except, t.IP) {
			res = best(res, yes)
		}
	}
	for _, en := range r.ToEntities {
		switch strings.ToLower(en) {
		case "all":
			res = best(res, yes)
		case "world":
			if !t.HasIP {
				res = best(res, maybe)
			} else if pod == nil && !node {
				res = best(res, yes)
			}
		case "cluster":
			if pod != nil || node {
				res = best(res, yes)
			}
		case "host", "remote-node":
			if node {
				res = best(res, yes)
			}
		default:
			res = best(res, maybe)
		}
	}
	for _, f := range r.ToFQDNs {
		if t.Host == "" {
			res = best(res, maybe) // an IP may be one of the names' resolved addresses
			continue
		}
		h := strings.ToLower(strings.TrimSuffix(t.Host, "."))
		if f.MatchName != "" && strings.EqualFold(strings.TrimSuffix(f.MatchName, "."), h) {
			res = best(res, yes)
		}
		if f.MatchPattern != "" && fqdnPatternRE(f.MatchPattern).MatchString(h) {
			res = best(res, yes)
		}
	}
	if len(r.ToServices)+len(r.ToGroups) > 0 {
		res = best(res, maybe)
	}
	return res
}

// ---------------------------------------------------------------- evaluation

type podPolicies struct {
	np       []*NetPol
	cil      []*CiliumPolicy
	cilRules map[*CiliumPolicy][]*CiliumRule // rules that select this pod
	names    []string
	restrict bool // an allow-list applies to egress
	hostNet  bool
}

// selectFor finds the policies that apply to a pod's egress.
func (e *policyEnv) selectFor(p *Pod) *podPolicies {
	pp := &podPolicies{cilRules: map[*CiliumPolicy][]*CiliumRule{}, hostNet: p.Spec.HostNetwork}
	if pp.hostNet {
		return pp
	}
	for i := range e.netpols {
		np := &e.netpols[i]
		if np.Metadata.Namespace != "" && np.Metadata.Namespace != p.Metadata.Namespace {
			continue
		}
		if !np.Spec.PodSelector.matches(p.Metadata.Labels, nil) {
			continue
		}
		egress := np.Spec.Egress != nil
		for _, t := range np.Spec.PolicyTypes {
			if t == "Egress" {
				egress = true
			}
		}
		if !egress {
			continue
		}
		pp.np = append(pp.np, np)
		pp.names = append(pp.names, "NetworkPolicy/"+np.Metadata.Name)
		pp.restrict = true
	}
	cl := e.ciliumLabels(p)
	for i := range e.cilium {
		cp := &e.cilium[i]
		if !cp.cluster && cp.Metadata.Namespace != p.Metadata.Namespace {
			continue
		}
		rules := cp.rules()
		var sel []*CiliumRule
		for j := range rules {
			r := &rules[j]
			if r.EndpointSelector == nil || (r.Egress == nil && r.EgressDeny == nil) {
				continue
			}
			if !r.EndpointSelector.matches(cl, ciliumNorm) {
				continue
			}
			sel = append(sel, r)
			if r.Egress != nil {
				pp.restrict = true
			}
		}
		if len(sel) > 0 {
			pp.cil = append(pp.cil, cp)
			pp.cilRules[cp] = sel
			kind := "CiliumNetworkPolicy/"
			if cp.cluster {
				kind = "CiliumClusterwideNetworkPolicy/"
			}
			pp.names = append(pp.names, kind+cp.Metadata.Name)
		}
	}
	return pp
}

func (e *policyEnv) evaluate(pp *podPolicies, t polTarget) PolicyVerdict {
	if pp.hostNet {
		return PolicyVerdict{State: "none", Why: "Host-network pod: pod network policies do not apply to it."}
	}
	if len(pp.names) == 0 {
		return PolicyVerdict{State: "none"}
	}
	sel := strings.Join(pp.names, ", ")
	// explicit denies win
	for _, cp := range pp.cil {
		for _, r := range pp.cilRules[cp] {
			if r.EgressDeny == nil {
				continue
			}
			for i := range *r.EgressDeny {
				d := &(*r.EgressDeny)[i]
				if e.ciliumPeerMatch(d, cp.Metadata.Namespace, cp.cluster, t) == yes && ciliumPortMatch(d.ToPorts, t) == yes {
					return PolicyVerdict{State: "blocked", Why: fmt.Sprintf("Denied by egressDeny rule in %s/%s.", ciliumKind(cp), cp.Metadata.Name)}
				}
			}
		}
	}
	if !pp.restrict {
		return PolicyVerdict{State: "none", Why: "Only deny rules select this pod: " + sel + "."}
	}
	var allow string
	unsure := false
	for _, np := range pp.np {
		if np.Spec.Egress == nil {
			continue
		}
		for i, r := range *np.Spec.Egress {
			m := minTri(npPortMatch(r.Ports, t), e.npPeerMatch(r.To, np.Metadata.Namespace, t))
			if m == yes && allow == "" {
				allow = fmt.Sprintf("NetworkPolicy/%s egress rule %d", np.Metadata.Name, i+1)
			} else if m == maybe {
				unsure = true
			}
		}
	}
	for _, cp := range pp.cil {
		for _, r := range pp.cilRules[cp] {
			if r.Egress == nil {
				continue
			}
			for i := range *r.Egress {
				er := &(*r.Egress)[i]
				if len(er.ToEndpoints)+len(er.ToCIDR)+len(er.ToCIDRSet)+len(er.ToEntities)+len(er.ToFQDNs)+len(er.ToServices)+len(er.ToGroups)+len(er.ToPorts) == 0 {
					continue // empty rule = default deny only
				}
				m := minTri(ciliumPortMatch(er.ToPorts, t), e.ciliumPeerMatch(er, cp.Metadata.Namespace, cp.cluster, t))
				if m == yes && allow == "" {
					allow = fmt.Sprintf("%s/%s egress rule %d", ciliumKind(cp), cp.Metadata.Name, i+1)
				} else if m == maybe {
					unsure = true
				}
			}
		}
	}
	switch {
	case allow != "":
		return PolicyVerdict{State: "allowed", Why: "Allowed by " + allow + "."}
	case unsure:
		why := "Selected by " + sel + ". An egress rule might allow this, but it cannot be decided from policy objects alone"
		if t.Host != "" {
			why += " (a hostname resolves to an address the policy check cannot see)"
		}
		return PolicyVerdict{State: "unknown", Why: why + "."}
	default:
		why := "No egress rule in " + sel + " allows " + t.Label + "."
		if t.Host != "" {
			why += " Also check that DNS (port 53) is allowed."
		}
		return PolicyVerdict{State: "blocked", Why: why}
	}
}

func ciliumKind(cp *CiliumPolicy) string {
	if cp.cluster {
		return "CiliumClusterwideNetworkPolicy"
	}
	return "CiliumNetworkPolicy"
}

func minTri(a, b tri) tri {
	if a < b {
		return a
	}
	return b
}

// policyTarget converts a check into the form used by evaluate.
func policyTarget(host string, port int, label string) polTarget {
	t := polTarget{Port: port, Proto: "TCP", Label: label}
	if ip, err := netip.ParseAddr(host); err == nil {
		t.IP, t.HasIP = ip, true
	} else {
		t.Host = host
	}
	return t
}

// podPolicySummary lists the policies that select each pod, for the UI.
func (e *policyEnv) summary(pods []*Pod) map[string][]string {
	out := map[string][]string{}
	for _, p := range pods {
		pp := e.selectFor(p)
		n := append([]string(nil), pp.names...)
		sort.Strings(n)
		out[p.Metadata.Name] = n
	}
	return out
}
