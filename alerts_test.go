package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- fixtures

func i32(v int32) *int32 { return &v }

func mkAlertPod(ns, name, owner, phase string) Pod {
	var p Pod
	p.Metadata.Namespace, p.Metadata.Name, p.Metadata.UID = ns, name, ns+"-"+name
	p.Metadata.Labels = map[string]string{"app": owner}
	p.Metadata.CreationTimestamp = time.Now().Add(-24 * time.Hour)
	if owner != "" {
		p.Metadata.OwnerReferences = []OwnerRef{{Kind: "ReplicaSet", Name: owner + "-5d4f8"}}
		p.Metadata.Labels["pod-template-hash"] = "5d4f8"
	}
	p.Status.Phase = phase
	p.Spec.Containers = []Container{{Name: "app"}}
	p.Status.ContainerStatuses = []ContainerStatus{{Name: "app", Ready: phase == "Running", Image: "registry.example/app:1"}}
	return p
}

func waiting(p *Pod, reason string) {
	p.Status.ContainerStatuses[0].Ready = false
	p.Status.ContainerStatuses[0].State.Waiting = &struct {
		Reason  string `json:"reason"`
		Message string `json:"message,omitempty"`
	}{Reason: reason}
}

func mkDeploy(ns, name string, want, avail int32) Deployment {
	var d Deployment
	d.Metadata.Namespace, d.Metadata.Name = ns, name
	d.Spec.Replicas = i32(want)
	d.Status.AvailableReplicas = avail
	return d
}

func mkSnap(pods []Pod, deps []Deployment) *Snapshot {
	s := &Snapshot{At: time.Now(), Readable: true, Scope: "cluster", Pods: pods, Deployments: deps, MetricsAvailable: true,
		PodUsage: map[string]Usage{}, NodeUsage: map[string]Usage{}}
	s.owner = buildOwners(s)
	return s
}

type testEngine struct {
	*AlertEngine
	mu    sync.Mutex
	clock time.Time
	sent  []notice
	c     *Cluster
}

func newTestEngine(t *testing.T) *testEngine {
	t.Helper()
	e, err := newAlertEngine(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	te := &testEngine{AlertEngine: e, clock: time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), c: &Cluster{Info: ContextInfo{Name: "prod"}}}
	e.now = func() time.Time { te.mu.Lock(); defer te.mu.Unlock(); return te.clock }
	e.cluster = func(*AlertRule) (*Cluster, error) { return te.c, nil }
	e.deliver = func(n notice) []HistoryEntry {
		te.mu.Lock()
		te.sent = append(te.sent, n)
		te.mu.Unlock()
		return []HistoryEntry{{At: n.At, Kind: "sent", Rule: n.Rule.ID, Text: n.Kind}}
	}
	t.Cleanup(func() { close(e.stop) })
	return te
}

func (te *testEngine) setSnap(s *Snapshot) { te.c.mu.Lock(); te.c.snap = s; te.c.mu.Unlock() }
func (te *testEngine) advance(d time.Duration) {
	te.mu.Lock()
	te.clock = te.clock.Add(d)
	te.mu.Unlock()
}

// step advances the clock, checks, waits for sending and returns what was sent.
func (te *testEngine) step(d time.Duration) []notice {
	te.advance(d)
	te.tick()
	te.flush()
	te.mu.Lock()
	defer te.mu.Unlock()
	out := te.sent
	te.sent = nil
	return out
}

func (te *testEngine) addRule(t *testing.T, r AlertRule) *AlertRule {
	t.Helper()
	if r.Name == "" {
		r.Name = "rule " + r.Condition
	}
	r.Cluster, r.Enabled = "prod", true
	if r.SMS == nil && r.Email == nil {
		r.SMS = []string{"+15550100000"}
	}
	if err := r.normalize(); err != nil {
		t.Fatal(err)
	}
	r.ID = newID()
	te.AlertEngine.mu.Lock()
	te.rules = append(te.rules, &r)
	te.AlertEngine.mu.Unlock()
	return &r
}

func (te *testEngine) activeFor(id string) []*Alert {
	te.AlertEngine.mu.Lock()
	defer te.AlertEngine.mu.Unlock()
	var out []*Alert
	for _, a := range te.active {
		if a.Rule == id {
			x := *a
			out = append(out, &x)
		}
	}
	return out
}

// ---------------------------------------------------------------- engine behaviour

func TestAlertFiresGroupedAndResolves(t *testing.T) {
	te := newTestEngine(t)
	p1, p2, ok := mkAlertPod("pay", "api-5d4f8-a", "api", "Running"), mkAlertPod("pay", "api-5d4f8-b", "api", "Running"), mkAlertPod("pay", "web-5d4f8-c", "web", "Running")
	waiting(&p1, "CrashLoopBackOff")
	waiting(&p2, "CrashLoopBackOff")
	deps := []Deployment{mkDeploy("pay", "api", 2, 0), mkDeploy("pay", "web", 1, 1)}
	te.setSnap(mkSnap([]Pod{p1, p2, ok}, deps))
	r := te.addRule(t, AlertRule{Condition: "pod_crashloop", Namespaces: []string{"pay"}, NotifyResolved: true})

	sent := te.step(0)
	if len(sent) != 1 || sent[0].Kind != "firing" || len(sent[0].Alerts) != 1 {
		t.Fatalf("one message with one alert (the Deployment), got %+v", sent)
	}
	a := sent[0].Alerts[0]
	if a.Object != "Deployment/api" || !strings.HasPrefix(a.Summary, "2 pods:") || a.Namespace != "pay" {
		t.Errorf("grouped by workload: %+v", a)
	}
	if sent := te.step(15 * time.Second); len(sent) != 0 {
		t.Errorf("no repeat without reminders: %+v", sent)
	}
	// pods replaced by a rollout but still crashing: same alert, nothing new
	p3 := mkAlertPod("pay", "api-5d4f8-z", "api", "Running")
	waiting(&p3, "CrashLoopBackOff")
	te.setSnap(mkSnap([]Pod{p3, ok}, deps))
	if sent := te.step(15 * time.Second); len(sent) != 0 || len(te.activeFor(r.ID)) != 1 {
		t.Errorf("rollout must not re-fire: %+v", sent)
	}
	// fixed: resolved message
	te.setSnap(mkSnap([]Pod{mkAlertPod("pay", "api-5d4f8-z", "api", "Running"), ok}, deps))
	sent = te.step(15 * time.Second)
	if len(sent) != 1 || sent[0].Kind != "resolved" || len(te.activeFor(r.ID)) != 0 {
		t.Fatalf("resolved: %+v", sent)
	}
	hist := te.st.readHistory(0)
	kinds := []string{}
	for _, h := range hist {
		kinds = append(kinds, h.Kind)
	}
	if strings.Join(kinds, ",") != "fired,sent,resolved,sent" {
		t.Errorf("history: %v", kinds)
	}
}

func TestAlertForDurationRepeatAndNoData(t *testing.T) {
	te := newTestEngine(t)
	p := mkAlertPod("pay", "api-5d4f8-a", "api", "Pending")
	te.setSnap(mkSnap([]Pod{p}, []Deployment{mkDeploy("pay", "api", 1, 0)}))
	r := te.addRule(t, AlertRule{Condition: "pod_pending", For: 300, Repeat: 60})
	if sent := te.step(0); len(sent) != 0 || te.activeFor(r.ID)[0].State != "pending" {
		t.Fatal("pending first")
	}
	if sent := te.step(299 * time.Second); len(sent) != 0 {
		t.Fatal("not before 5 minutes")
	}
	if sent := te.step(time.Second); len(sent) != 1 || sent[0].Kind != "firing" {
		t.Fatalf("fires after 5 minutes: %+v", sent)
	}
	// cluster unreadable: nothing resolves, nothing sent
	te.setSnap(&Snapshot{Err: "connection refused"})
	if sent := te.step(30 * time.Minute); len(sent) != 0 || len(te.activeFor(r.ID)) != 1 {
		t.Fatal("no data must keep the alert")
	}
	te.AlertEngine.mu.Lock()
	st := te.status[r.ID]
	te.AlertEngine.mu.Unlock()
	if !st.NoData || !strings.Contains(st.Err, "connection refused") {
		t.Errorf("status: %+v", st)
	}
	te.setSnap(mkSnap([]Pod{p}, []Deployment{mkDeploy("pay", "api", 1, 0)}))
	if sent := te.step(30*time.Minute + time.Second); len(sent) != 1 || sent[0].Kind != "reminder" {
		t.Fatalf("reminder after 60 minutes: %+v", sent)
	}
}

func TestAlertSilenceQuietHoursRateLimit(t *testing.T) {
	te := newTestEngine(t)
	n := Node{}
	n.Metadata.Name = "w01"
	n.Status.Conditions = []Condition{{Type: "Ready", Status: "False", Message: "kubelet stopped posting"}}
	snap := mkSnap(nil, nil)
	snap.Nodes = []Node{n}
	te.setSnap(snap)

	// silenced for 30 minutes: held, then sent when the silence ends
	r := te.addRule(t, AlertRule{Condition: "node_not_ready"})
	te.AlertEngine.mu.Lock()
	te.silences = append(te.silences, Silence{ID: "s1", Rule: r.ID, Until: te.clock.Add(30 * time.Minute)})
	te.AlertEngine.mu.Unlock()
	if sent := te.step(0); len(sent) != 0 {
		t.Fatal("silenced")
	}
	if sent := te.step(31 * time.Minute); len(sent) != 1 || !strings.Contains(sent[0].Alerts[0].Summary, "NotReady") {
		t.Fatalf("sent once the silence ended: %+v", sent)
	}

	// quiet hours: a warning waits, a critical goes out
	te2 := newTestEngine(t)
	te2.setSnap(snap)
	from, to := te2.clock.Add(-time.Hour).Format("15:04"), te2.clock.Add(time.Hour).Format("15:04")
	w := te2.addRule(t, AlertRule{Condition: "node_not_ready", Severity: "warning", QuietFrom: from, QuietTo: to})
	te2.addRule(t, AlertRule{Condition: "node_not_ready", Severity: "critical", QuietFrom: from, QuietTo: to})
	sent := te2.step(0)
	if len(sent) != 1 || sent[0].Rule.Severity != "critical" {
		t.Fatalf("only critical during quiet hours: %+v", sent)
	}
	if sent := te2.step(61 * time.Minute); len(sent) != 1 || sent[0].Rule.ID != w.ID {
		t.Fatalf("the warning after quiet hours: %+v", sent)
	}

	// rate limit: 20 messages per rule per hour
	te3 := newTestEngine(t)
	te3.setSnap(snap)
	te3.addRule(t, AlertRule{Condition: "node_not_ready", Repeat: 1})
	total := 0
	for i := 0; i < 40; i++ {
		total += len(te3.step(time.Minute))
	}
	if total != ruleSendsPerHr {
		t.Errorf("rate limit: %d messages in 40 minutes", total)
	}
	held := 0
	for _, h := range te3.st.readHistory(0) {
		if h.Kind == "held" {
			held++
		}
	}
	if held != 1 {
		t.Errorf("one note about holding, got %d", held)
	}
	if !inQuiet(&AlertRule{QuietFrom: "22:00", QuietTo: "07:00"}, time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)) ||
		inQuiet(&AlertRule{QuietFrom: "22:00", QuietTo: "07:00"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Error("quiet hours over midnight")
	}
}

func TestAlertNoRecipientsAndInstant(t *testing.T) {
	te := newTestEngine(t)
	ev := Event{Reason: "FailedMount", Message: "MountVolume.SetUp failed for volume data", Type: "Warning"}
	ev.InvolvedObject.Kind, ev.InvolvedObject.Name, ev.InvolvedObject.Namespace = "Pod", "api-1", "pay"
	lt := te.clock.Add(-2 * time.Minute)
	ev.LastTimestamp = &lt
	snap := mkSnap(nil, nil)
	snap.Warnings = []Event{ev}
	te.setSnap(snap)
	quiet := te.addRule(t, AlertRule{Condition: "event_warning", Match: "failedmount", Email: []string{}, SMS: []string{}})
	quiet.SMS = nil
	inst := te.addRule(t, AlertRule{Condition: "event_warning", Match: "/MountVolume\\.SetUp/", NotifyResolved: true})
	other := te.addRule(t, AlertRule{Condition: "event_warning", Match: "BackOff"})
	sent := te.step(0)
	if len(sent) != 1 || sent[0].Rule.ID != inst.ID {
		t.Fatalf("only the rule with recipients sends: %+v", sent)
	}
	if len(te.activeFor(quiet.ID)) != 1 || len(te.activeFor(other.ID)) != 0 {
		t.Error("the rule without recipients still shows the alert; non-matching pattern doesn't")
	}
	// the event ages out of the window: resolves without a "resolved" message (instant)
	if sent := te.step(10 * time.Minute); len(sent) != 0 || len(te.activeFor(inst.ID)) != 0 {
		t.Errorf("instant conditions don't send resolved: %+v", sent)
	}
}

// ---------------------------------------------------------------- conditions

func hitsFor(t *testing.T, snap *Snapshot, r AlertRule) []alertHit {
	t.Helper()
	r.Cluster = "prod"
	if r.Name == "" {
		r.Name = "x"
	}
	if err := r.normalize(); err != nil {
		t.Fatal(err)
	}
	hits, err := evalRule(&evalEnv{c: &Cluster{Info: ContextInfo{Name: "prod"}}, snap: snap, now: time.Now(), restarts: newRestartBook()}, &r)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestAlertConditions(t *testing.T) {
	now := time.Now()
	crash := mkAlertPod("pay", "api-5d4f8-a", "api", "Running")
	waiting(&crash, "CrashLoopBackOff")
	pull := mkAlertPod("shop", "web-5d4f8-a", "web", "Pending")
	waiting(&pull, "ImagePullBackOff")
	oom := mkAlertPod("pay", "worker-5d4f8-a", "worker", "Running")
	oom.Status.ContainerStatuses[0].LastState.Terminated = &struct {
		Reason     string    `json:"reason"`
		ExitCode   int       `json:"exitCode"`
		FinishedAt time.Time `json:"finishedAt"`
	}{"OOMKilled", 137, now.Add(-5 * time.Minute)}
	evicted := mkAlertPod("pay", "batch-x", "", "Failed")
	evicted.Status.Reason = "Evicted"
	notReady := mkAlertPod("pay", "db-0", "", "Running")
	notReady.Status.ContainerStatuses[0].Ready = false
	hot := mkAlertPod("pay", "cpu-5d4f8-a", "cpu", "Running")
	hot.Spec.Containers[0].Resources.Limits = ResourceList{"cpu": "500m", "memory": "100Mi"}
	healthy := mkAlertPod("kube-system", "dns-5d4f8-a", "dns", "Running")

	deps := []Deployment{mkDeploy("pay", "api", 3, 1), mkDeploy("shop", "web", 2, 2), mkDeploy("pay", "worker", 1, 1), mkDeploy("pay", "cpu", 1, 1), mkDeploy("pay", "idle", 0, 0)}
	deps[1].Status.Conditions = []Condition{{Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet web-abc has timed out progressing."}}
	snap := mkSnap([]Pod{crash, pull, oom, evicted, notReady, hot, healthy}, deps)
	snap.PodUsage["pay/cpu-5d4f8-a"] = Usage{CPU: 0.48, Mem: 50 << 20}
	var n1, n2 Node
	n1.Metadata.Name, n2.Metadata.Name = "w01", "w02"
	n1.Metadata.Labels = map[string]string{"role": "worker"}
	n1.Status.Allocatable = ResourceList{"cpu": "4", "memory": "8Gi"}
	n1.Status.Conditions = []Condition{{Type: "Ready", Status: "True"}, {Type: "DiskPressure", Status: "True"}}
	n2.Status.Conditions = []Condition{{Type: "Ready", Status: "False"}}
	n2.Spec.Unschedulable = true
	snap.Nodes = []Node{n1, n2}
	snap.NodeUsage["w01"] = Usage{CPU: 3.8, Mem: 2 << 30}

	objs := func(h []alertHit) string {
		var s []string
		for _, x := range h {
			s = append(s, x.Object)
		}
		return strings.Join(sortedCopy(s), ",")
	}
	cases := []struct {
		rule AlertRule
		want string
	}{
		{AlertRule{Condition: "pod_error"}, "Deployment/api,Deployment/web,Deployment/worker,Pod/batch-x"},
		{AlertRule{Condition: "pod_error", Namespaces: []string{"sh*"}}, "Deployment/web"},
		{AlertRule{Condition: "pod_error", Kind: "Deployment", Workload: "/^(api|worker)$/"}, "Deployment/api,Deployment/worker"},
		{AlertRule{Condition: "pod_error", Workload: "batch-*"}, "Pod/batch-x"},
		{AlertRule{Condition: "pod_error", Labels: "app=web"}, "Deployment/web"},
		{AlertRule{Condition: "pod_crashloop"}, "Deployment/api"},
		{AlertRule{Condition: "pod_imagepull"}, "Deployment/web"},
		{AlertRule{Condition: "pod_oom", Window: 15}, "Deployment/worker"},
		{AlertRule{Condition: "pod_oom", Window: 2}, ""},
		{AlertRule{Condition: "pod_pending"}, "Deployment/web"},
		{AlertRule{Condition: "pod_not_ready"}, "Deployment/api,Pod/db-0"},
		{AlertRule{Condition: "pod_failed"}, "Pod/batch-x"},
		{AlertRule{Condition: "pod_cpu", Threshold: 90}, "Deployment/cpu"},
		{AlertRule{Condition: "pod_cpu", Threshold: 99}, ""},
		{AlertRule{Condition: "pod_mem", Threshold: 40}, "Deployment/cpu"},
		{AlertRule{Condition: "workload_unavailable"}, "Deployment/api"},
		{AlertRule{Condition: "deploy_stuck"}, "Deployment/web"},
		{AlertRule{Condition: "workload_zero"}, "Deployment/idle"},
		{AlertRule{Condition: "node_not_ready"}, "Node/w02"},
		{AlertRule{Condition: "node_cpu", Threshold: 90}, "Node/w01"},
		{AlertRule{Condition: "node_mem", Threshold: 20}, "Node/w01"},
		{AlertRule{Condition: "node_pressure"}, "Node/w01"},
		{AlertRule{Condition: "node_pressure", Labels: "role=db"}, ""},
		{AlertRule{Condition: "node_cordoned"}, "Node/w02"},
		{AlertRule{Condition: "metrics_down"}, ""},
	}
	for _, c := range cases {
		if got := objs(hitsFor(t, snap, c.rule)); got != c.want {
			t.Errorf("%s %v %s %s %s: got %q, want %q", c.rule.Condition, c.rule.Namespaces, c.rule.Kind, c.rule.Workload, c.rule.Labels, got, c.want)
		}
	}
	if h := hitsFor(t, snap, AlertRule{Condition: "pod_cpu", Threshold: 90}); !strings.Contains(h[0].Summary, "CPU 96% of limit") {
		t.Errorf("cpu summary: %s", h[0].Summary)
	}
	// metrics gone: CPU rules have no data instead of silently resolving
	snap.MetricsAvailable, snap.MetricsErr = false, "metrics.k8s.io not found"
	r := AlertRule{Name: "x", Cluster: "prod", Condition: "pod_cpu"}
	_ = r.normalize()
	if _, err := evalRule(&evalEnv{c: &Cluster{}, snap: snap, now: now}, &r); err == nil || !strings.Contains(err.Error(), "no metrics") {
		t.Errorf("metrics missing: %v", err)
	}
	if got := objs(hitsFor(t, snap, AlertRule{Condition: "metrics_down"})); got != "Cluster/prod" {
		t.Errorf("metrics_down: %q", got)
	}
	if got := objs(hitsFor(t, &Snapshot{Err: "dial tcp: i/o timeout"}, AlertRule{Condition: "cluster_unreachable"})); got != "Cluster/prod" {
		t.Errorf("cluster_unreachable: %q", got)
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestRestartBookAndCronOf(t *testing.T) {
	b := newRestartBook()
	t0 := time.Now()
	old := t0.Add(-24 * time.Hour)
	if n := b.delta("p", t0, 40, old, 15*time.Minute); n != 0 {
		t.Errorf("first look counts from now: %d", n)
	}
	b.delta("p", t0.Add(5*time.Minute), 42, old, 15*time.Minute)
	if n := b.delta("p", t0.Add(10*time.Minute), 45, old, 15*time.Minute); n != 5 {
		t.Errorf("5 restarts in the window, got %d", n)
	}
	if n := b.delta("p", t0.Add(26*time.Minute), 46, old, 15*time.Minute); n != 1 {
		t.Errorf("older samples drop out of the window: %d", n)
	}
	if n := b.delta("new", t0, 4, t0.Add(-3*time.Minute), 15*time.Minute); n != 4 {
		t.Errorf("a pod started inside the window counts all its restarts: %d", n)
	}
	for in, want := range map[string]string{"backup-29371234": "backup", "report-manual-20261124-101500": "report", "plain-job": "", "x-12": ""} {
		if got := cronOf(in); got != want {
			t.Errorf("cronOf(%s) = %q", in, got)
		}
	}
}

func TestAlertRuleNormalize(t *testing.T) {
	ok := AlertRule{Name: "api down", Cluster: "prod", Condition: "pod_restarts", SMS: []string{"+1 555-0100-000"}, Email: []string{"Ops <ops@example.com>"}}
	if err := ok.normalize(); err != nil {
		t.Fatal(err)
	}
	if ok.Threshold != 3 || ok.Window != 15 || ok.SMS[0] != "+15550100000" || ok.Email[0] != "ops@example.com" || ok.Severity != "warning" || ok.SMSText != defSMSText {
		t.Errorf("defaults: %+v", ok)
	}
	empty := AlertRule{Name: "x", Cluster: "prod", Condition: "node_not_ready"}
	_ = empty.normalize()
	if b, _ := json.Marshal(empty); !strings.Contains(string(b), `"namespaces":[]`) || !strings.Contains(string(b), `"sms":[]`) {
		t.Errorf("lists are [] in JSON, never null: %s", b)
	}
	bad := []AlertRule{
		{Cluster: "prod", Condition: "pod_error"},
		{Name: "x", Cluster: "prod", Condition: "nope"},
		{Name: "x", Cluster: "prod", Condition: "pod_cpu", Threshold: 150},
		{Name: "x", Cluster: "prod", Condition: "pod_error", Workload: "/(/"},
		{Name: "x", Cluster: "prod", Condition: "pod_error", SMS: []string{"call me"}},
		{Name: "x", Cluster: "prod", Condition: "pod_error", Email: []string{"a@b\r\nBcc: x@y"}},
		{Name: "x", Cluster: "prod", Condition: "pod_error", QuietFrom: "22:00"},
		{Name: "x", Cluster: "prod", Condition: "pod_error", Labels: "a b"},
		{Name: "x", Cluster: "prod", Condition: "pod_error", Kind: "Pod"},
		{Name: "x", Condition: "pod_error"},
	}
	for i, r := range bad {
		if r.normalize() == nil {
			t.Errorf("case %d should be refused: %+v", i, r)
		}
	}
}

// ---------------------------------------------------------------- messages

func TestAlertTemplates(t *testing.T) {
	at := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	n := notice{Kind: "firing", At: at, Rule: AlertRule{Name: "API crash", Severity: "critical", Cluster: "prod", Description: "Call the payments team."},
		Alerts: []Alert{{Namespace: "pay", Object: "Deployment/api", Summary: "2 pods: a CrashLoopBackOff", FiredAt: at}}}
	v := noticeVars(n)
	got := renderTpl("[{{status}}] {{ severity }} {{rule}}: {{summary}} ({{cluster}}/{{namespace}}) {{time}} {{unknown}}", v, nil)
	if got != "[FIRING] CRITICAL API crash: 2 pods: a CrashLoopBackOff (prod/pay) 2026-11-24 09:30 {{unknown}}" {
		t.Errorf("rendered: %q", got)
	}
	if body := renderTpl(defEmailBody, v, nil); !strings.Contains(body, "Call the payments team.") || !strings.Contains(body, "- pay/Deployment/api: 2 pods") {
		t.Errorf("email body: %s", body)
	}
	n.Alerts = append(n.Alerts, Alert{Namespace: "shop", Object: "Node/w01", Summary: "NotReady"})
	v = noticeVars(n)
	if v["count"] != "2" || v["namespace"] != "pay, shop" || !strings.HasPrefix(v["summary"], "2 alerts: ") || v["object"] != "2 objects" {
		t.Errorf("many: %v", v)
	}
}

// ---------------------------------------------------------------- SMS

func TestSMSRequestAndSend(t *testing.T) {
	te := newTestEngine(t)
	user, _ := te.st.box.seal("gw-user")
	pass, _ := te.st.box.seal("s3cret\"pw")
	var got struct {
		sync.Mutex
		r    *http.Request
		body string
	}
	status := 201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Lock()
		got.r, got.body = r, string(b)
		got.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"result":"x"}`))
	}))
	defer srv.Close()
	cfg := SMSConfig{Enabled: true, URL: srv.URL + "/1/smsmessaging/outbound/{{sender}}/requests", Method: "POST", ContentType: "application/json",
		Auth: "basic", Username: user, Password: pass, ServiceID: "1000000001", Sender: "alerts", LinkID: "L1",
		Headers: "X-Service: {{service_id}}\nX-Link: {{link_id}}\r\nX-Evil: a\r\n", Body: presetByID("oneapi")["body"]}
	if err := checkSMS(&cfg); err != nil {
		t.Fatal(err)
	}
	msg := `Pod "api" down: 100% CPU` + "\nline 2"
	if err := te.sendSMS(cfg, "+15550100000", msg); err != nil {
		t.Fatal(err)
	}
	got.Lock()
	r, body := got.r, got.body
	got.Unlock()
	if r.URL.Path != "/1/smsmessaging/outbound/alerts/requests" || r.Header.Get("X-Service") != "1000000001" || r.Header.Get("X-Link") != "L1" {
		t.Errorf("url/headers: %s %v", r.URL.Path, r.Header)
	}
	if u, p, ok := r.BasicAuth(); !ok || u != "gw-user" || p != "s3cret\"pw" {
		t.Errorf("basic auth: %v %q %q", ok, u, p)
	}
	var parsed struct {
		Req struct {
			Address []string `json:"address"`
			Text    struct {
				Message string `json:"message"`
			} `json:"outboundSMSTextMessage"`
		} `json:"outboundSMSMessageRequest"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Req.Text.Message != msg || parsed.Req.Address[0] != "tel:+15550100000" {
		t.Errorf("body must stay valid JSON with the message intact: %v %s", err, body)
	}
	// WSSE: the digest is sha256(nonce + created + password)
	cfg.Auth = "wsse"
	if err := te.sendSMS(cfg, "+15550100000", "hi"); err != nil {
		t.Fatal(err)
	}
	got.Lock()
	x := got.r.Header.Get("X-WSSE")
	got.Unlock()
	m := regexp.MustCompile(`Username="gw-user", PasswordDigest="([^"]+)", Nonce="([^"]+)", Created="([^"]+)"`).FindStringSubmatch(x)
	if m == nil {
		t.Fatalf("X-WSSE: %s", x)
	}
	sum := sha256.Sum256([]byte(m[2] + m[3] + "s3cret\"pw"))
	if m[1] != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Error("WSSE digest")
	}
	status = 401
	if err := te.sendSMS(cfg, "+15550100000", "hi"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("gateway errors are reported: %v", err)
	}
	cfg.Enabled = false
	if err := te.sendSMS(cfg, "+15550100000", "hi"); err == nil {
		t.Error("disabled SMS must not send")
	}
	for _, bad := range []SMSConfig{{URL: "ftp://x"}, {URL: "https://x", Auth: "magic"}, {URL: "https://x", Headers: "no colon"}} {
		if checkSMS(&bad) == nil {
			t.Errorf("refuse %+v", bad)
		}
	}
}

// ---------------------------------------------------------------- email (a tiny SMTP server)

type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	auth string
	from string
	to   []string
	data string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		up := strings.ToUpper(cmd)
		f.mu.Lock()
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-fake\r\n250-AUTH PLAIN\r\n250 8BITMIME")
		case strings.HasPrefix(up, "AUTH PLAIN"):
			b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cmd[len("AUTH PLAIN"):]))
			f.auth = string(b)
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.from = cmd[10:]
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.to = append(f.to, cmd[8:])
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var sb strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				sb.WriteString(l)
			}
			f.data = sb.String()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			f.mu.Unlock()
			return
		default:
			say("250 ok")
		}
		f.mu.Unlock()
	}
}

func TestEmailSend(t *testing.T) {
	te := newTestEngine(t)
	f := startFakeSMTP(t)
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	user, _ := te.st.box.seal("alerts@example.com")
	pass, _ := te.st.box.seal("mail-pw")
	cfg := EmailConfig{Enabled: true, Host: host, Security: "none", Username: user, Password: pass, From: "kubelamp@example.com", FromName: "KubeLamp"}
	cfg.Port = atoiT(port)
	if err := checkEmail(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := te.sendEmail(cfg, []string{"ops@example.com", "dev@example.com"}, "[FIRING] api\r\nBcc: evil@x", "line 1\nপড ডাউন"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	data, auth, from, to := f.data, f.auth, f.from, f.to
	f.mu.Unlock()
	if auth != "\x00alerts@example.com\x00mail-pw" || !strings.HasPrefix(from, "<kubelamp@example.com>") || len(to) != 2 {
		t.Errorf("auth %q from %q to %v", auth, from, to)
	}
	if strings.Contains(data, "\r\nBcc:") || !strings.Contains(data, "Subject: [FIRING] api  Bcc: evil@x") && !strings.Contains(data, "Subject: =?utf-8?") {
		t.Errorf("subject must not inject headers:\n%s", data)
	}
	if !strings.Contains(data, "Content-Transfer-Encoding: quoted-printable") || !strings.Contains(data, "line 1") {
		t.Errorf("body:\n%s", data)
	}
	cfg.Security = "starttls"
	if err := te.sendEmail(cfg, []string{"ops@example.com"}, "s", "b"); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("STARTTLS required but missing: %v", err)
	}
}

func atoiT(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// ---------------------------------------------------------------- secrets and persistence

func TestSecretsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	e, err := newAlertEngine(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret.key: %v %v", err, fi)
	}
	sealed, _ := e.st.box.seal("gw-Secret-1")
	if strings.Contains(sealed, "gw-Secret-1") || !strings.HasPrefix(sealed, secretPrefix) {
		t.Error("sealed value leaks")
	}
	if p, err := e.st.box.open(sealed); err != nil || p != "gw-Secret-1" {
		t.Error("round trip")
	}
	e.mu.Lock()
	e.ch.SMS = SMSConfig{Enabled: true, URL: "https://sms.example/x", Username: sealed, Password: sealed}
	_ = e.st.save("channels.json", e.ch)
	r := &AlertRule{ID: "r1", Name: "kept", Cluster: "prod", Condition: "node_not_ready", Enabled: true}
	e.rules = append(e.rules, r)
	_ = e.saveRulesLocked()
	e.active["r1|Node/w1"] = &Alert{Key: "r1|Node/w1", Rule: "r1", State: "firing", Notified: true}
	_ = e.saveStateLocked()
	e.mu.Unlock()
	close(e.stop)

	raw, _ := os.ReadFile(filepath.Join(dir, "alerts", "channels.json"))
	if strings.Contains(string(raw), "gw-Secret-1") {
		t.Error("channels.json holds a plain password")
	}
	v := e.channelsView()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "gw-Secret-1") || strings.Contains(string(b), secretPrefix) || !strings.Contains(string(b), `"passwordSet":true`) {
		t.Errorf("the API view must never carry secrets: %s", b)
	}

	e2, err := newAlertEngine(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer close(e2.stop)
	if len(e2.rules) != 1 || e2.rules[0].Name != "kept" || e2.active["r1|Node/w1"] == nil || !e2.active["r1|Node/w1"].Notified {
		t.Error("rules and alert state survive a restart (no re-send)")
	}
	if p, err := e2.st.box.open(e2.ch.SMS.Password); err != nil || p != "gw-Secret-1" {
		t.Error("the same key opens saved secrets after a restart")
	}
	// another key: a clear error, not garbage
	t.Setenv("KUBELAMP_SECRET_KEY", "different")
	e3, _ := newAlertEngine(nil, dir)
	defer close(e3.stop)
	if _, err := e3.st.box.open(e2.ch.SMS.Password); err == nil || !strings.Contains(err.Error(), "secret key changed") {
		t.Errorf("wrong key: %v", err)
	}
}

// ---------------------------------------------------------------- HTTP API (no user accounts: local admin)

func TestAlertAPI(t *testing.T) {
	s := &Server{reg: &Registry{}}
	e, err := newAlertEngine(s, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer close(e.stop)
	s.alerts = e
	mux := http.NewServeMux()
	s.alertRoutes(mux)
	do := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, body := do("POST", "/api/alerts/rules", `{"name":"x","cluster":"nope","condition":"pod_error"}`); code != 400 || !strings.Contains(body, "not in your kubeconfig") {
		t.Errorf("unknown cluster refused: %d %s", code, body)
	}
	if code, _ := do("PUT", "/api/alerts/channels/sms", `{"enabled":true,"url":"https://sms.example/1/x","auth":"basic","username":"u","password":"gw-Secret-1"}`); code != 200 {
		t.Fatal("save sms")
	}
	_, body := do("PUT", "/api/alerts/channels/sms", `{"enabled":true,"url":"https://sms.example/1/y","auth":"basic","username":"","password":""}`)
	if strings.Contains(body, "gw-Secret-1") || !strings.Contains(body, `"passwordSet":true`) || !strings.Contains(body, `"usernameSet":true`) || !strings.Contains(body, "/1/y") {
		t.Errorf("edit keeps the secrets without showing them: %s", body)
	}
	if p, _ := e.st.box.open(e.ch.SMS.Password); p != "gw-Secret-1" {
		t.Error("password kept")
	}
	_, body = do("GET", "/api/alerts/channels", "")
	if strings.Contains(body, "gw-Secret-1") || strings.Contains(body, secretPrefix) {
		t.Error("GET channels leaks")
	}
	if code, _ := do("PUT", "/api/alerts/channels/email", `{"enabled":true,"host":"","from":"x@example.com"}`); code != 400 {
		t.Error("email without host refused")
	}
	if code, _ := do("POST", "/api/alerts/silences", `{"rule":"nope","minutes":60}`); code != 404 {
		t.Error("silence for unknown rule")
	}
	_, body = do("GET", "/api/alerts", "")
	if !strings.Contains(body, `"canEdit":true`) || !strings.Contains(body, `"sms":true`) || !strings.Contains(body, `"pod_crashloop"`) {
		t.Errorf("overview: %s", body[:min(400, len(body))])
	}
}

func presetByID(id string) map[string]string {
	for _, p := range smsPresets {
		if p["id"] == id {
			return p
		}
	}
	return nil
}

// The Huawei SDP preset with a fixed digest sends exactly what a working curl script sends.
func TestSMSHuaweiFixedDigest(t *testing.T) {
	te := newTestEngine(t)
	var got struct {
		sync.Mutex
		h    http.Header
		body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Lock()
		got.h, got.body = r.Header.Clone(), string(b)
		got.Unlock()
		w.WriteHeader(201)
	}))
	defer srv.Close()
	p := presetByID("sdp")
	if smsPresets[0]["id"] != "sdp" {
		t.Error("a new SMS form starts from the Huawei SDP preset")
	}
	user, _ := te.st.box.seal("gw-user")
	digest, _ := te.st.box.seal("FIXED+digest/base64=")
	cfg := SMSConfig{Enabled: true, URL: srv.URL + "/1/smsmessaging/outbound/gw-user/requests", Method: p["method"], ContentType: p["contentType"], Auth: p["auth"],
		Username: user, ServiceID: "1000000001", Sender: "MyAlert", Headers: p["headers"], Body: p["body"],
		WSSENonce: "2024012223595900001", WSSECreated: "2024-01-22T23:59:59Z", WSSEDigest: digest}
	if err := checkSMS(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := te.sendSMS(cfg, "8801000000001", `disk "full"`); err != nil {
		t.Fatal(err)
	}
	got.Lock()
	h, body := got.h, got.body
	got.Unlock()
	want := map[string]string{
		"Authorization":   `WSSE realm="SDP",profile="UsernameToken"`,
		"X-Wsse":          `UsernameToken Username="gw-user", PasswordDigest="FIXED+digest/base64=", Nonce="2024012223595900001", Created="2024-01-22T23:59:59Z"`,
		"X-Requestheader": `request ServiceId="1000000001",FA="",PresentId="",LinkId=""`,
		"Accept":          "application/json",
		"Content-Type":    "application/json; charset=UTF-8",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s:\n got %q\nwant %q", k, h.Get(k), v)
		}
	}
	if body != `{"outboundSMSMessageRequest":{"address":["8801000000001"],"senderAddress":"MyAlert","outboundSMSTextMessage":{"message":"disk \"full\""}}}` {
		t.Errorf("body: %s", body)
	}
	// without a fixed digest: a fresh one from the password
	cfg.WSSEDigest, cfg.Password = "", mustSeal(t, te, "pw")
	if err := te.sendSMS(cfg, "8801000000001", "x"); err != nil {
		t.Fatal(err)
	}
	got.Lock()
	x := got.h.Get("X-WSSE")
	got.Unlock()
	if strings.Contains(x, "2024012223595900001") || strings.Contains(x, "FIXED") {
		t.Errorf("computed digest expected: %s", x)
	}
	if checkSMS(&SMSConfig{URL: "https://x", WSSENonce: `a"b`}) == nil {
		t.Error("quotes in the nonce are refused")
	}
}

func mustSeal(t *testing.T, te *testEngine, s string) string {
	v, err := te.st.box.seal(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestChannelDigestWriteOnly(t *testing.T) {
	s := &Server{reg: &Registry{}}
	e, err := newAlertEngine(s, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer close(e.stop)
	s.alerts = e
	mux := http.NewServeMux()
	s.alertRoutes(mux)
	put := func(body string) string {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("PUT", "/api/alerts/channels/sms", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	b := put(`{"enabled":true,"url":"https://sms.example/x","auth":"wsse","wsseNonce":"n1","wsseCreated":"c1","wsseDigest":"SECRET-DIGEST"}`)
	if strings.Contains(b, "SECRET-DIGEST") || !strings.Contains(b, `"wsseDigestSet":true`) || !strings.Contains(b, `"wsseNonce":"n1"`) {
		t.Errorf("digest is write-only: %s", b)
	}
	b = put(`{"enabled":true,"url":"https://sms.example/x","auth":"wsse","wsseNonce":"n1","wsseCreated":"c1"}`)
	if d, _ := e.st.box.open(e.ch.SMS.WSSEDigest); d != "SECRET-DIGEST" || !strings.Contains(b, `"wsseDigestSet":true`) {
		t.Error("an empty field keeps the saved digest")
	}
	b = put(`{"enabled":true,"url":"https://sms.example/x","auth":"wsse","clearDigest":true}`)
	if e.ch.SMS.WSSEDigest != "" || !strings.Contains(b, `"wsseDigestSet":false`) || e.ch.SMS.ClearDigest {
		t.Errorf("clearDigest forgets it: %s", b)
	}
}

func TestSMSLogMasksSecrets(t *testing.T) {
	te := newTestEngine(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	var buf strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.WriteString(string(p)) }))
	defer log.SetOutput(os.Stderr)
	p := presetByID("sdp")
	cfg := SMSConfig{Enabled: true, URL: srv.URL + "/x", Method: p["method"], ContentType: p["contentType"], Auth: "wsse", Sender: "MyAlert",
		Username: mustSeal(t, te, "gw-user"), Password: mustSeal(t, te, "Sup3r-Secret-Pw"), Headers: p["headers"], Body: p["body"],
		WSSENonce: "n1", WSSECreated: "c1", WSSEDigest: mustSeal(t, te, "DigestValueBase64==")}
	err := te.sendSMS(cfg, "8801000000001", "hello")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "no error text") {
		t.Errorf("error: %v", err)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	for _, want := range []string{"> POST " + srv.URL + "/x", `> X-Wsse: UsernameToken Username="gw-user", PasswordDigest="Dige…(19 chars)", Nonce="n1", Created="c1"`,
		`> body: {"outboundSMSMessageRequest":{"address":["8801000000001"],"senderAddress":"MyAlert"`, "gateway answered 500 Internal Server Error", "response body: (empty)"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DigestValueBase64") || strings.Contains(out, "Sup3r-Secret-Pw") {
		t.Errorf("secrets in the log:\n%s", out)
	}
	bad := SMSConfig{Enabled: true, URL: "https://x", Body: `{"s":"{{sender}}"}`}
	if err := checkSMS(&bad); err == nil || !strings.Contains(err.Error(), "Sender is empty") {
		t.Errorf("empty sender: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
