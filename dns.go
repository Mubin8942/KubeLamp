package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tools → Networking → DNS Checker: resolve names from inside pods (so the answer is what the
// application sees: the pod's resolv.conf, search domains, cluster DNS and the node's upstreams)
// and report the IPs each pod got. One exec per pod covers every name.

const (
	dnsMaxNames   = 30
	dnsMaxSources = 40
)

// The script prints raw facts; Go does all the parsing. Names and settings arrive as
// positional arguments, never spliced into the script.
const dnsScript = `# kl-dns
to=$1; srv=$2; shift 2
t=; for x in getent nslookup dig host timeout; do command -v $x >/dev/null 2>&1 && t="$t $x"; done
echo "@@tools|$t"
if [ -r /etc/resolv.conf ]; then grep -E '^(nameserver|search|options)' /etc/resolv.conf 2>/dev/null | while IFS= read -r l; do echo "@@resolv|$l"; done; fi
T=; command -v timeout >/dev/null 2>&1 && T="timeout $((to+3))"
i=0
for h in "$@"; do
  i=$((i+1)); tool=none; rc=0; out=
  if [ -n "$srv" ]; then
    if command -v dig >/dev/null 2>&1; then tool=dig; out=$($T dig +time=$to +tries=1 +noall +answer "$h" A "@$srv" 2>&1); rc=$?
    elif command -v nslookup >/dev/null 2>&1; then tool=nslookup; out=$($T nslookup "$h" "$srv" 2>&1); rc=$?
    fi
  else
    if command -v getent >/dev/null 2>&1; then tool=getent; out=$($T getent ahosts "$h" 2>&1); rc=$?
    elif command -v nslookup >/dev/null 2>&1; then tool=nslookup; out=$($T nslookup "$h" 2>&1); rc=$?
    elif command -v dig >/dev/null 2>&1; then tool=dig; out=$($T dig +time=$to +tries=1 +noall +answer "$h" A 2>&1); rc=$?
    elif command -v host >/dev/null 2>&1; then tool=host; out=$($T host -W "$to" "$h" 2>&1); rc=$?
    fi
  fi
  echo "@@d|$i|$tool|$rc"
  printf '%s\n' "$out" | head -n 40 | while IFS= read -r l; do echo "@@o|$i|$l"; done
done
`

type DNSResolv struct {
	Nameservers []string `json:"nameservers"`
	Search      []string `json:"search"`
	Options     []string `json:"options"`
}

type DNSSource struct {
	Pod         string    `json:"pod"`
	Container   string    `json:"container,omitempty"`
	Node        string    `json:"node"`
	PodIP       string    `json:"podIP"`
	HostNetwork bool      `json:"hostNetwork,omitempty"`
	State       string    `json:"state"` // done | noshell | notool | error
	Tool        string    `json:"tool,omitempty"`
	Err         string    `json:"err,omitempty"`
	Resolv      DNSResolv `json:"resolv"`
}

type DNSResult struct {
	Src    int      `json:"src"`
	Name   string   `json:"name"`
	State  string   `json:"state"` // ok | notfound | timeout | servfail | error | notool | noshell
	Tool   string   `json:"tool,omitempty"`
	IPs    []string `json:"ips"`
	CNAMEs []string `json:"cnames,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

var (
	dnsNameRE = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_-]{0,62})(\.[A-Za-z0-9_]([A-Za-z0-9_-]{0,62}))*\.?$`)
)

func parseDNSNames(s string) (names []string, err error) {
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' || r == '\r' }) {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[tok] {
			continue
		}
		if len(tok) > 253 || !dnsNameRE.MatchString(tok) {
			return nil, fmt.Errorf("%q is not a valid host name", tok)
		}
		if net.ParseIP(tok) != nil {
			return nil, fmt.Errorf("%q is an IP address: enter host names (reverse lookups are not supported)", tok)
		}
		seen[tok] = true
		names = append(names, tok)
	}
	if len(names) > dnsMaxNames {
		return nil, fmt.Errorf("%d names (limit %d)", len(names), dnsMaxNames)
	}
	return names, nil
}

// parseExpect reads "10.0.0.5, 10.0.1.0/24" into matchers.
func parseExpect(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == ';' }) {
		if _, n, err := net.ParseCIDR(tok); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(tok)
		if ip == nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", tok)
		}
		bits := 128
		if ip.To4() != nil {
			ip, bits = ip.To4(), 32
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

func cleanIP(tok string) string {
	tok = strings.TrimSpace(tok)
	if i := strings.IndexAny(tok, "#%"); i >= 0 {
		tok = tok[:i]
	}
	if ip := net.ParseIP(tok); ip != nil {
		return ip.String()
	}
	return ""
}

// parseDNSAnswer extracts the resolved IPs (and CNAMEs) from one tool's raw output.
func parseDNSAnswer(tool string, lines []string) (ips, cnames []string) {
	seen := map[string]bool{}
	add := func(s string) {
		if ip := cleanIP(s); ip != "" && !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	switch tool {
	case "getent":
		for _, l := range lines { // "10.96.0.1  STREAM kubernetes.default.svc.cluster.local"
			f := strings.Fields(l)
			if len(f) >= 2 && (f[1] == "STREAM" || f[1] == "DGRAM" || f[1] == "RAW") {
				add(f[0])
			} else if len(f) >= 1 && len(f) < 4 {
				add(f[0])
			}
		}
	case "nslookup":
		afterName := false
		for _, l := range lines {
			l = strings.TrimSpace(l)
			lw := strings.ToLower(l)
			if strings.HasPrefix(lw, "name:") {
				afterName = true
				continue
			}
			if !afterName {
				continue
			}
			if strings.HasPrefix(lw, "address") { // "Address: 1.2.3.4", "Address 1: 1.2.3.4 name"
				rest := l[strings.Index(l, ":")+1:]
				for _, tok := range strings.Fields(rest) {
					if ip := cleanIP(tok); ip != "" {
						add(ip)
						break
					}
				}
			} else if i := strings.Index(lw, "canonical name = "); i >= 0 {
				cnames = append(cnames, strings.TrimSuffix(strings.TrimSpace(l[i+len("canonical name = "):]), "."))
			}
		}
	case "dig": // +noall +answer: "name. 30 IN A 1.2.3.4" / "name. 30 IN CNAME other."
		for _, l := range lines {
			f := strings.Fields(l)
			if len(f) >= 5 && f[2] == "IN" {
				switch f[3] {
				case "A", "AAAA":
					add(f[4])
				case "CNAME":
					cnames = append(cnames, strings.TrimSuffix(f[4], "."))
				}
			} else if len(f) == 1 {
				add(f[0])
			}
		}
	case "host":
		for _, l := range lines {
			if i := strings.Index(l, " has address "); i >= 0 {
				add(l[i+len(" has address "):])
			} else if i := strings.Index(l, " has IPv6 address "); i >= 0 {
				add(l[i+len(" has IPv6 address "):])
			} else if i := strings.Index(l, " is an alias for "); i >= 0 {
				cnames = append(cnames, strings.TrimSuffix(strings.TrimSpace(l[i+len(" is an alias for "):]), "."))
			}
		}
	}
	return
}

func classifyDNS(tool string, rc int, ips []string, lines []string) (state, detail string) {
	txt := strings.ToLower(strings.Join(lines, " "))
	if len(ips) > 0 {
		return "ok", ""
	}
	short := strings.TrimSpace(strings.Join(lines, " "))
	if len(short) > 200 {
		short = short[:200] + "…"
	}
	switch {
	case rc == 124 || rc == 137 || strings.Contains(txt, "timed out") || strings.Contains(txt, "no servers could be reached") ||
		strings.Contains(txt, "communications error") || strings.Contains(txt, "connection refused") && strings.Contains(txt, "server"):
		return "timeout", "no answer from the DNS server in time" + sep(short)
	case strings.Contains(txt, "servfail") || strings.Contains(txt, "refused"):
		return "servfail", "the DNS server returned an error" + sep(short)
	case strings.Contains(txt, "nxdomain") || strings.Contains(txt, "can't find") || strings.Contains(txt, "cant find") ||
		strings.Contains(txt, "not found") || strings.Contains(txt, "name or service not known") || strings.Contains(txt, "no address associated") ||
		strings.Contains(txt, "bad address") || strings.Contains(txt, "non-existent") || (tool == "getent" && rc == 2) || (tool == "dig" && rc == 0 && short == ""):
		return "notfound", "the name does not resolve (NXDOMAIN or no address record)"
	}
	return "error", "could not read an answer" + sep(short)
}

func sep(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

type dnsParsed struct {
	Tools  []string
	Resolv DNSResolv
	Names  map[int]*DNSResult // by 1-based index
}

func parseDNSOutput(out string, names []string) dnsParsed {
	p := dnsParsed{Names: map[int]*DNSResult{}}
	raw := map[int][]string{}
	rcs := map[int]int{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		switch {
		case strings.HasPrefix(ln, "@@tools|"):
			p.Tools = strings.Fields(ln[8:])
		case strings.HasPrefix(ln, "@@resolv|"):
			f := strings.Fields(ln[9:])
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "nameserver":
				p.Resolv.Nameservers = append(p.Resolv.Nameservers, f[1])
			case "search":
				p.Resolv.Search = append(p.Resolv.Search, f[1:]...)
			case "options":
				p.Resolv.Options = append(p.Resolv.Options, f[1:]...)
			}
		case strings.HasPrefix(ln, "@@d|"):
			f := strings.SplitN(ln[4:], "|", 3)
			if len(f) < 3 {
				continue
			}
			i, _ := strconv.Atoi(f[0])
			rc, _ := strconv.Atoi(strings.SplitN(f[2], "|", 2)[0])
			if i >= 1 && i <= len(names) {
				p.Names[i] = &DNSResult{Name: names[i-1], Tool: f[1], IPs: []string{}}
				rcs[i] = rc
			}
		case strings.HasPrefix(ln, "@@o|"):
			f := strings.SplitN(ln[4:], "|", 2)
			if len(f) == 2 {
				i, _ := strconv.Atoi(f[0])
				raw[i] = append(raw[i], f[1])
			}
		}
	}
	for i, r := range p.Names {
		if r.Tool == "none" {
			r.State, r.Detail = "notool", "no getent, nslookup, dig or host in this container"
			continue
		}
		var lines []string
		for _, l := range raw[i] {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		ips, cn := parseDNSAnswer(r.Tool, lines)
		sort.Strings(ips)
		r.IPs, r.CNAMEs = append([]string{}, ips...), cn
		r.State, r.Detail = classifyDNS(r.Tool, rcs[i], ips, lines)
	}
	return p
}

type dnsRequest struct {
	NS      string   `json:"ns"`
	PodMode string   `json:"podMode"`
	Pods    []string `json:"pods"`
	Names   string   `json:"names"`
	Expect  string   `json:"expect"`
	Server  string   `json:"server"`
	Timeout int      `json:"timeout"`
}

// dnsRunSource resolves every name from one pod, trying its containers in turn when one has no shell
// or no DNS tool.
func dnsRunSource(ctx context.Context, c *Cluster, p *Pod, si int, names []string, timeout int, server string) (DNSSource, []DNSResult) {
	src := DNSSource{Pod: p.Metadata.Name, Node: p.Spec.NodeName, PodIP: p.Status.PodIP, HostNetwork: p.Spec.HostNetwork}
	fill := func(state, detail string) []DNSResult {
		rs := make([]DNSResult, len(names))
		for i, n := range names {
			rs[i] = DNSResult{Src: si, Name: n, State: state, IPs: []string{}, Detail: detail}
		}
		return rs
	}
	per := len(names)*(timeout+4) + 20
	if per > 300 {
		per = 300
	}
	lastErr, noTool := "", false
	for _, ctr := range runningContainers(p) {
		if ctx.Err() != nil {
			break
		}
		args := append([]string{"sh", "-c", dnsScript, "kl", strconv.Itoa(timeout), server}, names...)
		out, err := c.execWith(ctx, toolExecSem, p.Metadata.Namespace, p.Metadata.Name, ctr, args, time.Duration(per)*time.Second)
		if err != nil && out == "" {
			lastErr = err.Error()
			if !noShellRE.MatchString(lastErr) {
				break
			}
			continue
		}
		parsed := parseDNSOutput(out, names)
		if len(parsed.Names) == 0 {
			lastErr = "the command produced no output (no usable sh?)"
			continue
		}
		if first := parsed.Names[1]; first != nil && first.Tool == "none" {
			noTool = true
			src.Resolv = parsed.Resolv
			continue
		}
		res := make([]DNSResult, len(names))
		for i, n := range names {
			if r := parsed.Names[i+1]; r != nil {
				r.Src = si
				res[i] = *r
			} else {
				res[i] = DNSResult{Src: si, Name: n, State: "error", IPs: []string{}, Detail: "lookup did not finish (exec timed out or was cut off)"}
			}
		}
		src.State, src.Container, src.Resolv = "done", ctr, parsed.Resolv
		src.Tool = res[0].Tool
		return src, res
	}
	switch {
	case ctx.Err() != nil:
		src.State, src.Err = "error", "cancelled"
		return src, fill("error", "cancelled")
	case lastErr != "" && !noShellRE.MatchString(lastErr):
		src.State, src.Err = "error", friendlyErr(errors.New(lastErr))
		return src, fill("error", src.Err)
	case noTool:
		src.State, src.Err = "notool", "no getent, nslookup, dig or host in any container"
		return src, fill("notool", src.Err)
	}
	src.State, src.Err = "noshell", "no shell in any container (distroless?)"
	return src, fill("noshell", src.Err)
}

func (s *Server) dnsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tools/dns", func(w http.ResponseWriter, r *http.Request) {
		if s.denyExecTool(w) {
			return
		}
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		var req dnsRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.NS == "" {
			writeErr(w, 400, errors.New("namespace required"))
			return
		}
		names, err := parseDNSNames(req.Names)
		if err != nil || len(names) == 0 {
			if err == nil {
				err = errors.New("enter at least one host name")
			}
			writeErr(w, 400, err)
			return
		}
		expect, err := parseExpect(req.Expect)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		server := strings.TrimSpace(req.Server)
		if server != "" && net.ParseIP(server) == nil {
			writeErr(w, 400, fmt.Errorf("DNS server %q must be an IP address", server))
			return
		}
		if req.Timeout < 1 || req.Timeout > 10 {
			req.Timeout = 3
		}
		pods, err := c.FreshPods(r.Context(), req.NS)
		if err != nil {
			writeErr(w, 502, errors.New(friendlyErr(err)))
			return
		}
		srcPods, err := pickSources(pods, connRequest{NS: req.NS, PodMode: req.PodMode, Pods: req.Pods})
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if len(srcPods) > dnsMaxSources {
			writeErr(w, 400, fmt.Errorf("%d pods selected (limit %d): use \"One pod per node\" or pick pods", len(srcPods), dnsMaxSources))
			return
		}
		sources := make([]DNSSource, len(srcPods))
		results := make([][]DNSResult, len(srcPods))
		var wg sync.WaitGroup
		for i, p := range srcPods {
			wg.Add(1)
			go func(i int, p *Pod) {
				defer wg.Done()
				sources[i], results[i] = dnsRunSource(r.Context(), c, p, i, names, req.Timeout, server)
			}(i, p)
		}
		wg.Wait()
		flat := []DNSResult{}
		for _, rs := range results {
			flat = append(flat, rs...)
		}
		// flag answers outside the expected set
		resp := map[string]any{"names": names, "sources": sources, "results": flat, "server": server, "at": time.Now()}
		if len(expect) > 0 {
			var unexpected []string
			seen := map[string]bool{}
			for _, rs := range flat {
				for _, ip := range rs.IPs {
					if seen[ip] {
						continue
					}
					seen[ip] = true
					in := false
					for _, n := range expect {
						if n.Contains(net.ParseIP(ip)) {
							in = true
						}
					}
					if !in {
						unexpected = append(unexpected, ip)
					}
				}
			}
			sort.Strings(unexpected)
			resp["expect"] = strings.TrimSpace(req.Expect)
			resp["unexpected"] = unexpected
		}
		writeJSON(w, 200, resp)
	})
}
