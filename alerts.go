package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Alerting: rules made by admins, checked every 15 s against the clusters of the admin
// who made them (their own kubeconfig, so a rule never sees more than its owner can).
// Everyone can read rules and alerts; only admins change them. Kept in <data>/alerts:
//   rules.json     the rules
//   state.json     active alerts and silences (so a restart doesn't re-send)
//   channels.json  SMS gateway and SMTP settings, passwords encrypted (secretbox.go)
//   history.jsonl  fired / resolved / sent, newest last, trimmed to the last 3000 lines

// ---------------------------------------------------------------- the catalogue

type condParam struct {
	Label string  `json:"label"`
	Unit  string  `json:"unit"`
	Def   float64 `json:"def"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

type condDef struct {
	ID      string     `json:"id"`
	Group   string     `json:"group"`
	Label   string     `json:"label"`
	Help    string     `json:"help"`
	Target  string     `json:"target"` // pod | workload | job | cronjob | node | pvc | externalsecret | event | cluster
	Thresh  *condParam `json:"threshold,omitempty"`
	Window  *condParam `json:"window,omitempty"`
	Match   string     `json:"match,omitempty"` // label of the text pattern field ("" = none)
	Metrics bool       `json:"metrics,omitempty"`
	Instant bool       `json:"instant,omitempty"` // a one-off happening: no "resolved" message
	DefFor  int        `json:"defFor"`            // default "for", seconds
}

var (
	pctParam  = func(def float64) *condParam { return &condParam{"Threshold", "%", def, 1, 100} }
	minParam  = func(label string, def, max float64) *condParam { return &condParam{label, "min", def, 1, max} }
	condTable = []condDef{
		{ID: "pod_error", Group: "Pods", Label: "Pod has an error", Help: "CrashLoopBackOff, image pull errors, OOMKilled, config errors, failed or evicted, or a container that exited with an error in the time window.", Target: "pod", Window: minParam("Recent exits within", 15, 1440)},
		{ID: "pod_crashloop", Group: "Pods", Label: "CrashLoopBackOff", Help: "A container keeps crashing and Kubernetes waits before restarting it.", Target: "pod"},
		{ID: "pod_imagepull", Group: "Pods", Label: "Image pull error", Help: "ImagePullBackOff, ErrImagePull or InvalidImageName.", Target: "pod"},
		{ID: "pod_oom", Group: "Pods", Label: "OOMKilled", Help: "A container was killed for using more memory than its limit.", Target: "pod", Window: minParam("Within the last", 15, 1440), Instant: true},
		{ID: "pod_restarts", Group: "Pods", Label: "Restarts too often", Help: "Container restarts within the time window (counted while KubeLamp watches).", Target: "pod", Thresh: &condParam{"Restarts", "", 3, 1, 1000}, Window: minParam("Within", 15, 120)},
		{ID: "pod_pending", Group: "Pods", Label: "Pod stuck in Pending", Help: "Not scheduled or not started. Use \"For\" to set how long.", Target: "pod", DefFor: 300},
		{ID: "pod_not_ready", Group: "Pods", Label: "Pod running but not ready", Help: "Readiness probe failing. Use \"For\" to set how long.", Target: "pod", DefFor: 300},
		{ID: "pod_failed", Group: "Pods", Label: "Pod failed or evicted", Help: "Phase Failed, including evictions.", Target: "pod"},
		{ID: "pod_cpu", Group: "Resources", Label: "Pod CPU high", Help: "CPU use as a share of the pod's CPU limit (its request if it has no limit).", Target: "pod", Thresh: pctParam(90), Metrics: true, DefFor: 300},
		{ID: "pod_mem", Group: "Resources", Label: "Pod memory high", Help: "Memory use as a share of the pod's memory limit (its request if it has no limit).", Target: "pod", Thresh: pctParam(90), Metrics: true, DefFor: 120},
		{ID: "workload_unavailable", Group: "Workloads", Label: "Replicas unavailable", Help: "A Deployment, StatefulSet or DaemonSet has fewer ready pods than it wants.", Target: "workload", DefFor: 120},
		{ID: "deploy_stuck", Group: "Workloads", Label: "Rollout stuck", Help: "A Deployment passed its progress deadline (ProgressDeadlineExceeded).", Target: "workload"},
		{ID: "workload_zero", Group: "Workloads", Label: "Scaled to zero", Help: "A Deployment or StatefulSet is set to 0 replicas.", Target: "workload"},
		{ID: "job_failed", Group: "Jobs", Label: "Job failed", Help: "A Job (or a CronJob's run) failed within the time window.", Target: "job", Window: minParam("Failed within the last", 60, 1440), Instant: true},
		{ID: "job_long", Group: "Jobs", Label: "Job running too long", Help: "A Job has been running longer than the threshold.", Target: "job", Thresh: &condParam{"Longer than", "min", 60, 1, 10080}},
		{ID: "cronjob_late", Group: "Jobs", Label: "CronJob has no recent success", Help: "No successful run within the threshold. Suspended CronJobs are skipped.", Target: "cronjob", Thresh: &condParam{"No success for", "min", 1440, 5, 43200}},
		{ID: "node_not_ready", Group: "Nodes", Label: "Node not ready", Target: "node", DefFor: 60},
		{ID: "node_cpu", Group: "Nodes", Label: "Node CPU high", Help: "CPU use as a share of the node's allocatable CPU.", Target: "node", Thresh: pctParam(90), Metrics: true, DefFor: 300},
		{ID: "node_mem", Group: "Nodes", Label: "Node memory high", Help: "Memory use as a share of the node's allocatable memory.", Target: "node", Thresh: pctParam(90), Metrics: true, DefFor: 300},
		{ID: "node_pressure", Group: "Nodes", Label: "Node under pressure", Help: "MemoryPressure, DiskPressure or PIDPressure.", Target: "node"},
		{ID: "node_cordoned", Group: "Nodes", Label: "Node cordoned", Help: "Marked unschedulable.", Target: "node"},
		{ID: "pvc_usage", Group: "Storage", Label: "Volume almost full", Help: "Measured with df inside a pod that mounts the claim, at most every 5 minutes. Name the namespaces to keep the load low.", Target: "pvc", Thresh: pctParam(85)},
		{ID: "pvc_pending", Group: "Storage", Label: "Volume claim Pending or Lost", Target: "pvc", DefFor: 300},
		{ID: "externalsecret_failed", Group: "Config", Label: "ExternalSecret not synced", Help: "The ExternalSecret's Ready condition is not True (for example SecretSyncedError).", Target: "externalsecret", DefFor: 60},
		{ID: "cert_expiry", Group: "Config", Label: "API server certificate expiring", Target: "cluster", Thresh: &condParam{"Expires within", "days", 14, 1, 365}},
		{ID: "event_warning", Group: "Events", Label: "Warning event", Help: "A Warning event whose reason or message matches the pattern, e.g. FailedMount, BackOff or FailedScheduling. Empty matches all.", Target: "event", Match: "Reason or message (text, glob or /regex/)", Window: minParam("Seen within the last", 10, 1440), Instant: true},
		{ID: "cluster_unreachable", Group: "Cluster", Label: "Cluster unreachable", Help: "KubeLamp cannot reach or read the API server.", Target: "cluster", DefFor: 120},
		{ID: "metrics_down", Group: "Cluster", Label: "metrics-server not answering", Target: "cluster", DefFor: 600},
	}
	condByID = func() map[string]*condDef {
		m := map[string]*condDef{}
		for i := range condTable {
			m[condTable[i].ID] = &condTable[i]
		}
		return m
	}()
)

// ---------------------------------------------------------------- rules

type AlertRule struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Enabled     bool     `json:"enabled"`
	Severity    string   `json:"severity"` // critical | warning | info
	Description string   `json:"description"`
	Cluster     string   `json:"cluster"`
	Namespaces  []string `json:"namespaces"` // globs; empty = all
	Kind        string   `json:"kind"`       // Deployment | StatefulSet | DaemonSet | Job | CronJob | "" (any)
	Workload    string   `json:"workload"`   // names: globs separated by commas, or /regex/
	Labels      string   `json:"labels"`     // k=v,k2!=v2
	Condition   string   `json:"condition"`
	Threshold   float64  `json:"threshold"`
	Window      float64  `json:"window"` // minutes
	Match       string   `json:"match"`
	For         int      `json:"for"` // seconds the condition must hold

	SMS            []string `json:"sms"`
	Email          []string `json:"email"`
	NotifyResolved bool     `json:"notifyResolved"`
	Repeat         int      `json:"repeat"`    // minutes between reminders, 0 = no reminders
	QuietFrom      string   `json:"quietFrom"` // "HH:MM": only critical alerts are sent between these
	QuietTo        string   `json:"quietTo"`
	SMSText        string   `json:"smsText"`
	EmailSubject   string   `json:"emailSubject"`
	EmailBody      string   `json:"emailBody"`

	Owner     string    `json:"owner"` // id of the admin whose clusters it runs against
	OwnerName string    `json:"ownerName"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updatedBy"`
}

const (
	defSMSText      = "[{{status}}] {{severity}}: {{rule}} - {{summary}} ({{cluster}}/{{namespace}}) {{time}}"
	defEmailSubject = "[{{status}}] {{rule}}: {{count}} alert(s) on {{cluster}}"
	defEmailBody    = "{{status}}: {{rule}} ({{severity}})\n\n{{description}}\n\nCluster: {{cluster}}\nTime: {{time}}\n\n{{list}}\n\nSent by KubeLamp."
)

var (
	phoneRe   = regexp.MustCompile(`^\+?[0-9]{6,15}$`)
	hhmmRe    = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	kindNames = []string{"", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}
)

// normalize fills defaults and checks a rule; it returns a message for the user.
func (r *AlertRule) normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" || len(r.Name) > 100 {
		return errors.New("give the rule a name (at most 100 characters)")
	}
	d := condByID[r.Condition]
	if d == nil {
		return fmt.Errorf("unknown condition %q", r.Condition)
	}
	if r.Severity == "" {
		r.Severity = "warning"
	}
	if r.Severity != "critical" && r.Severity != "warning" && r.Severity != "info" {
		return errors.New("severity must be critical, warning or info")
	}
	if strings.TrimSpace(r.Cluster) == "" {
		return errors.New("pick a cluster")
	}
	if !contains(kindNames, r.Kind) {
		return fmt.Errorf("unknown workload kind %q", r.Kind)
	}
	clean := func(list []string) []string {
		out := []string{} // [] in JSON, never null
		for _, s := range list {
			if s = strings.TrimSpace(s); s != "" && !contains(out, s) {
				out = append(out, s)
			}
		}
		return out
	}
	r.Namespaces = clean(r.Namespaces)
	for _, ns := range r.Namespaces {
		if _, err := path.Match(ns, ""); err != nil || len(ns) > 253 {
			return fmt.Errorf("namespace pattern %q is not valid", ns)
		}
	}
	r.Workload = strings.TrimSpace(r.Workload)
	if _, err := newNameMatcher(r.Workload); err != nil {
		return err
	}
	r.Match = strings.TrimSpace(r.Match)
	if _, err := newNameMatcher(r.Match); err != nil {
		return err
	}
	r.Labels = strings.TrimSpace(r.Labels)
	if r.Labels != "" {
		if _, err := parseSelector(r.Labels); err != nil {
			return fmt.Errorf("labels: %v", err)
		}
	}
	param := func(p *condParam, v *float64, what string) error {
		if p == nil {
			*v = 0
			return nil
		}
		if *v == 0 {
			*v = p.Def
		}
		if *v < p.Min || *v > p.Max {
			return fmt.Errorf("%s must be between %g and %g", what, p.Min, p.Max)
		}
		return nil
	}
	if err := param(d.Thresh, &r.Threshold, "the threshold"); err != nil {
		return err
	}
	if err := param(d.Window, &r.Window, "the time window"); err != nil {
		return err
	}
	if d.Match == "" {
		r.Match = ""
	}
	if r.For < 0 || r.For > 86400 {
		return errors.New(`"for" must be between 0 seconds and 24 hours`)
	}
	if r.Repeat < 0 || r.Repeat > 10080 {
		return errors.New("reminders: between 0 (off) and 10080 minutes")
	}
	r.SMS = clean(r.SMS)
	for i, n := range r.SMS {
		n = strings.NewReplacer(" ", "", "-", "").Replace(n)
		if !phoneRe.MatchString(n) {
			return fmt.Errorf("%q is not a phone number (digits, optionally starting with +)", r.SMS[i])
		}
		r.SMS[i] = n
	}
	r.Email = clean(r.Email)
	for i, e := range r.Email {
		a, err := mail.ParseAddress(e)
		if err != nil || strings.ContainsAny(a.Address, "\r\n") {
			return fmt.Errorf("%q is not an email address", e)
		}
		r.Email[i] = a.Address
	}
	if len(r.SMS) > 50 || len(r.Email) > 50 {
		return errors.New("at most 50 phone numbers and 50 email addresses")
	}
	if (r.QuietFrom == "") != (r.QuietTo == "") || r.QuietFrom != "" && (!hhmmRe.MatchString(r.QuietFrom) || !hhmmRe.MatchString(r.QuietTo)) {
		return errors.New("quiet hours: give both times as HH:MM, or neither")
	}
	r.Description = strings.TrimSpace(r.Description)
	if r.SMSText = strings.TrimSpace(r.SMSText); r.SMSText == "" {
		r.SMSText = defSMSText
	}
	if r.EmailSubject = strings.TrimSpace(r.EmailSubject); r.EmailSubject == "" {
		r.EmailSubject = defEmailSubject
	}
	if r.EmailBody = strings.TrimSpace(r.EmailBody); r.EmailBody == "" {
		r.EmailBody = defEmailBody
	}
	if len(r.Description) > 2000 || len(r.SMSText) > 1000 || len(r.EmailSubject) > 300 || len(r.EmailBody) > 10000 {
		return errors.New("a message is too long (description 2000, SMS 1000, subject 300, email 10000 characters)")
	}
	return nil
}

// nameMatcher: "" matches all; "/re/" is a regular expression; else comma-separated globs
// (case-insensitive).
type nameMatcher struct {
	re    *regexp.Regexp
	globs []string
}

func newNameMatcher(p string) (*nameMatcher, error) {
	p = strings.TrimSpace(p)
	m := &nameMatcher{}
	if len(p) >= 2 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/") {
		re, err := regexp.Compile("(?i)" + p[1:len(p)-1])
		if err != nil {
			return nil, fmt.Errorf("pattern %s: %v", p, err)
		}
		m.re = re
		return m, nil
	}
	for _, g := range strings.Split(p, ",") {
		if g = strings.ToLower(strings.TrimSpace(g)); g != "" {
			if _, err := path.Match(g, ""); err != nil {
				return nil, fmt.Errorf("pattern %q is not valid", g)
			}
			m.globs = append(m.globs, g)
		}
	}
	return m, nil
}

func (m *nameMatcher) any() bool { return m.re == nil && len(m.globs) == 0 }

func (m *nameMatcher) match(s ...string) bool {
	if m.any() {
		return true
	}
	for _, v := range s {
		if v == "" {
			continue
		}
		if m.re != nil {
			if m.re.MatchString(v) {
				return true
			}
			continue
		}
		lv := strings.ToLower(v)
		for _, g := range m.globs {
			// a glob without wildcards is a contains-match for free text, exact for names
			if ok, _ := path.Match(g, lv); ok {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- active alerts, silences, history

type Alert struct {
	Key       string    `json:"key"` // rule id + "|" + object key
	Rule      string    `json:"rule"`
	RuleName  string    `json:"ruleName"`
	Severity  string    `json:"severity"`
	Cluster   string    `json:"cluster"`
	Namespace string    `json:"namespace"`
	Object    string    `json:"object"`
	Summary   string    `json:"summary"`
	State     string    `json:"state"` // pending | firing
	Since     time.Time `json:"since"` // condition first seen
	FiredAt   time.Time `json:"firedAt,omitempty"`
	Notified  bool      `json:"notified"`
	LastSent  time.Time `json:"lastSent,omitempty"`
	Silenced  bool      `json:"silenced,omitempty"` // computed for the API
	seen      bool
}

type Silence struct {
	ID      string    `json:"id"`
	Rule    string    `json:"rule"`
	Key     string    `json:"key,omitempty"` // one alert; "" = the whole rule
	Until   time.Time `json:"until"`         // zero = until the alert clears
	By      string    `json:"by"`
	Comment string    `json:"comment,omitempty"`
}

type HistoryEntry struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // fired | resolved | sent | failed | held
	Rule      string    `json:"rule"`
	RuleName  string    `json:"ruleName"`
	Severity  string    `json:"severity,omitempty"`
	Cluster   string    `json:"cluster,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	Object    string    `json:"object,omitempty"`
	Text      string    `json:"text"`
}

type alertStore struct {
	dir string
	box *secretBox
	hmu sync.Mutex // history file
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (st *alertStore) path(name string) string { return filepath.Join(st.dir, name) }

func (st *alertStore) load(name string, v any) {
	b, err := os.ReadFile(st.path(name))
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, v); err != nil {
		log.Printf("warning: %s is unreadable (%v); starting without it", st.path(name), err)
	}
}

func (st *alertStore) save(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return err
	}
	tmp := st.path(name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path(name))
}

const historyKeep = 3000

func (st *alertStore) appendHistory(es []HistoryEntry) {
	if len(es) == 0 {
		return
	}
	st.hmu.Lock()
	defer st.hmu.Unlock()
	_ = os.MkdirAll(st.dir, 0o700)
	f, err := os.OpenFile(st.path("history.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("alert history: %v", err)
		return
	}
	for _, e := range es {
		b, _ := json.Marshal(e)
		_, _ = f.Write(append(b, '\n'))
	}
	fi, _ := f.Stat()
	f.Close()
	if fi != nil && fi.Size() > 3<<20 { // trim to the newest lines
		all := st.readHistoryLocked(0)
		if len(all) > historyKeep {
			all = all[len(all)-historyKeep:]
		}
		var sb strings.Builder
		for _, e := range all {
			b, _ := json.Marshal(e)
			sb.Write(b)
			sb.WriteByte('\n')
		}
		tmp := st.path("history.jsonl.tmp")
		if os.WriteFile(tmp, []byte(sb.String()), 0o600) == nil {
			_ = os.Rename(tmp, st.path("history.jsonl"))
		}
	}
}

// readHistory returns entries oldest first (the newest `limit`, 0 = all).
func (st *alertStore) readHistory(limit int) []HistoryEntry {
	st.hmu.Lock()
	defer st.hmu.Unlock()
	return st.readHistoryLocked(limit)
}

func (st *alertStore) readHistoryLocked(limit int) []HistoryEntry {
	f, err := os.Open(st.path("history.jsonl"))
	if err != nil {
		return []HistoryEntry{}
	}
	defer f.Close()
	out := []HistoryEntry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
