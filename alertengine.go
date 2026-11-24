package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// The alert engine: every 15 s each enabled rule is checked against the snapshot of its
// cluster (refreshed every 15 s while a rule watches it). An object matching the rule is
// "pending" until it has matched for the rule's "for" time, then "firing"; it resolves when
// it stops matching. While the cluster can't be read nothing changes state.
//
// Messages: one per rule and check, listing every alert that fired (or resolved) in it.
// Firing alerts that couldn't be sent yet (quiet hours, silence, rate limit) are sent once
// that ends, if they are still firing.

const (
	alertTick       = 15 * time.Second
	ruleSendsPerHr  = 20
	totalSendsPerHr = 300
)

type ruleStatus struct {
	Checked time.Time `json:"checked"`
	Err     string    `json:"error,omitempty"`
	NoData  bool      `json:"noData,omitempty"`
	Matches int       `json:"matches"`
}

type notice struct {
	Rule   AlertRule
	Kind   string // firing | reminder | resolved | test
	Alerts []Alert
	At     time.Time
}

type AlertEngine struct {
	srv *Server
	st  *alertStore

	mu         sync.Mutex
	rules      []*AlertRule
	active     map[string]*Alert
	silences   []Silence
	ch         Channels
	status     map[string]ruleStatus
	slow       map[string]slowHits // rule id → hits of a slow condition
	sent       map[string][]time.Time
	limitNoted map[string]time.Time
	restarts   *restartBook

	now     func() time.Time
	cluster func(r *AlertRule) (*Cluster, error)
	deliver func(n notice) []HistoryEntry

	queue   chan notice
	pending sync.WaitGroup
	stop    chan struct{}
}

type slowHits struct {
	at   time.Time
	hits []alertHit
	err  error
}

// slow conditions list objects or run df in pods: checked at most this often
var slowEvery = map[string]time.Duration{"pvc_usage": 5 * time.Minute, "pvc_pending": time.Minute, "job_failed": 30 * time.Second,
	"job_long": time.Minute, "cronjob_late": time.Minute, "externalsecret_failed": 30 * time.Second, "cert_expiry": 10 * time.Minute}

func newAlertEngine(srv *Server, dataDir string) (*AlertEngine, error) {
	dir := dataDir + "/alerts"
	box, err := openSecretBox(dataDir)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	e := &AlertEngine{srv: srv, st: &alertStore{dir: dir, box: box}, active: map[string]*Alert{}, status: map[string]ruleStatus{},
		slow: map[string]slowHits{}, sent: map[string][]time.Time{}, limitNoted: map[string]time.Time{}, restarts: newRestartBook(),
		now: time.Now, queue: make(chan notice, 200), stop: make(chan struct{})}
	e.cluster = e.ownerCluster
	e.deliver = e.send
	var rf struct {
		Rules []*AlertRule `json:"rules"`
	}
	e.st.load("rules.json", &rf)
	e.rules = rf.Rules
	var sf struct {
		Active   []*Alert  `json:"active"`
		Silences []Silence `json:"silences"`
	}
	e.st.load("state.json", &sf)
	for _, a := range sf.Active {
		e.active[a.Key] = a
	}
	e.silences = sf.Silences
	e.st.load("channels.json", &e.ch)
	on := 0
	for _, r := range e.rules {
		if r.Enabled {
			on++
		}
	}
	log.Printf("alerts: %d rules (%d on), SMS %s, email %s; checked every %s", len(e.rules), on, onOff(e.ch.SMS.Enabled && e.ch.SMS.URL != ""),
		onOff(e.ch.Email.Enabled && e.ch.Email.Host != ""), alertTick)
	go e.worker()
	return e, nil
}

func (e *AlertEngine) run() {
	t := time.NewTicker(alertTick)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.tick()
		}
	}
}

// ownerCluster: the rule's cluster as its owner (an admin) sees it.
func (e *AlertEngine) ownerCluster(r *AlertRule) (*Cluster, error) {
	if e.srv == nil || e.srv.users == nil {
		return nil, errors.New("no user accounts")
	}
	e.srv.users.mu.Lock()
	u := e.srv.users.byID(r.Owner)
	isAdmin := u != nil && u.Admin()
	e.srv.users.mu.Unlock()
	if u == nil {
		return nil, fmt.Errorf("the rule's owner (%s) no longer exists: an admin should save the rule again", r.OwnerName)
	}
	if !isAdmin {
		return nil, fmt.Errorf("the rule's owner (%s) is no longer an admin: an admin should save the rule again", r.OwnerName)
	}
	c := e.srv.users.Registry(u).Get(r.Cluster)
	if c == nil {
		return nil, fmt.Errorf("cluster %q is not in %s's kubeconfig", r.Cluster, r.OwnerName)
	}
	return c, nil
}

type evalResult struct {
	hits []alertHit
	err  error
}

func (e *AlertEngine) check(r *AlertRule, now time.Time) evalResult {
	if every := slowEvery[r.Condition]; every > 0 {
		e.mu.Lock()
		c, ok := e.slow[r.ID]
		e.mu.Unlock()
		if ok && now.Sub(c.at) < every {
			return evalResult{c.hits, c.err}
		}
	}
	c, err := e.cluster(r)
	if err != nil {
		return evalResult{err: err}
	}
	c.touch() // keep this cluster on the 15 s refresh
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	hits, err := evalRule(&evalEnv{ctx: ctx, srv: e.srv, c: c, snap: c.Snap(), now: now, restarts: e.restarts}, r)
	if slowEvery[r.Condition] > 0 {
		e.mu.Lock()
		e.slow[r.ID] = slowHits{now, hits, err}
		e.mu.Unlock()
	}
	return evalResult{hits, err}
}

// tick checks every enabled rule once and sends what needs sending.
func (e *AlertEngine) tick() {
	now := e.now()
	e.mu.Lock()
	var rules []AlertRule
	for _, r := range e.rules {
		if r.Enabled {
			rules = append(rules, *r)
		}
	}
	e.mu.Unlock()

	res := make([]evalResult, len(rules))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range rules {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res[i] = e.check(&rules[i], now)
		}(i)
	}
	wg.Wait()

	e.mu.Lock()
	var hist []HistoryEntry
	var out []notice
	for i := range rules {
		h, n := e.apply(&rules[i], res[i], now)
		hist = append(hist, h...)
		out = append(out, n...)
	}
	e.expireSilencesLocked(now)
	err := e.saveStateLocked()
	e.mu.Unlock()
	if err != nil {
		log.Printf("alerts: saving state: %v", err)
	}
	e.restarts.prune(now)
	e.st.appendHistory(hist)
	for _, n := range out {
		e.enqueue(n)
	}
}

func histOf(kind string, r *AlertRule, a *Alert, at time.Time, text string) HistoryEntry {
	return HistoryEntry{At: at, Kind: kind, Rule: r.ID, RuleName: r.Name, Severity: r.Severity, Cluster: r.Cluster, Namespace: a.Namespace, Object: a.Object, Text: text}
}

// apply updates the rule's alerts from one check. Callers hold e.mu.
func (e *AlertEngine) apply(r *AlertRule, res evalResult, now time.Time) ([]HistoryEntry, []notice) {
	var nd errNoData
	st := ruleStatus{Checked: now, Matches: len(res.hits)}
	if res.err != nil {
		st.Err, st.NoData, st.Matches = res.err.Error(), errors.As(res.err, &nd), 0
	}
	if prev := e.status[r.ID]; prev.Err != st.Err {
		if st.Err != "" {
			log.Printf("alerts: rule %q cannot be checked: %s", r.Name, st.Err)
		} else if prev.Err != "" {
			log.Printf("alerts: rule %q can be checked again", r.Name)
		}
	}
	e.status[r.ID] = st
	if res.err != nil {
		return nil, nil // keep every alert as it is until the rule can be checked again
	}
	d := condByID[r.Condition]
	var hist []HistoryEntry
	prefix := r.ID + "|"
	for _, a := range e.active {
		if strings.HasPrefix(a.Key, prefix) {
			a.seen = false
		}
	}
	for _, h := range res.hits {
		key := prefix + h.Key
		a := e.active[key]
		if a == nil {
			a = &Alert{Key: key, Rule: r.ID, State: "pending", Since: now}
			e.active[key] = a
		}
		a.seen = true
		a.RuleName, a.Severity, a.Cluster, a.Namespace, a.Object, a.Summary = r.Name, r.Severity, r.Cluster, h.NS, h.Object, h.Summary
		if a.State == "pending" && now.Sub(a.Since) >= time.Duration(r.For)*time.Second {
			a.State, a.FiredAt = "firing", now
			hist = append(hist, histOf("fired", r, a, now, a.Summary))
		}
	}
	var resolved []Alert
	for key, a := range e.active {
		if !strings.HasPrefix(key, prefix) || a.seen {
			continue
		}
		if a.State == "firing" {
			hist = append(hist, histOf("resolved", r, a, now, "no longer: "+a.Summary))
			if a.Notified && r.NotifyResolved && !d.Instant {
				resolved = append(resolved, *a)
			}
		}
		delete(e.active, key)
		e.dropSilencesForLocked(key)
	}

	// what to send now
	var firing, remind []Alert
	for _, a := range e.active {
		if !strings.HasPrefix(a.Key, prefix) || a.State != "firing" {
			continue
		}
		a.Silenced = e.silencedLocked(a, now)
		switch {
		case a.Silenced:
		case !a.Notified:
			firing = append(firing, *a)
		case r.Repeat > 0 && now.Sub(a.LastSent) >= time.Duration(r.Repeat)*time.Minute:
			remind = append(remind, *a)
		}
	}
	if len(firing)+len(remind)+len(resolved) == 0 {
		return hist, nil
	}
	if len(r.SMS)+len(r.Email) == 0 { // nobody to tell: shown on the page only
		e.markSentLocked(append(firing, remind...), now)
		return hist, nil
	}
	if r.Severity != "critical" && inQuiet(r, now) {
		return hist, nil // sent when quiet hours end, if still firing
	}
	if !e.allowSendLocked(r.ID, now) {
		if now.Sub(e.limitNoted[r.ID]) > time.Hour {
			e.limitNoted[r.ID] = now
			hist = append(hist, HistoryEntry{At: now, Kind: "held", Rule: r.ID, RuleName: r.Name, Severity: r.Severity, Cluster: r.Cluster,
				Text: fmt.Sprintf("not sending: more than %d messages for this rule (or %d in all) in the last hour", ruleSendsPerHr, totalSendsPerHr)})
		}
		return hist, nil
	}
	var out []notice
	for _, n := range []notice{{Kind: "firing", Alerts: firing}, {Kind: "reminder", Alerts: remind}, {Kind: "resolved", Alerts: resolved}} {
		if len(n.Alerts) == 0 {
			continue
		}
		sortAlerts(n.Alerts)
		n.Rule, n.At = *r, now
		out = append(out, n)
		e.sent[r.ID] = append(e.sent[r.ID], now)
		e.sent[""] = append(e.sent[""], now)
	}
	e.markSentLocked(append(firing, remind...), now)
	return hist, out
}

func sortAlerts(as []Alert) {
	sort.Slice(as, func(i, j int) bool {
		if as[i].Namespace != as[j].Namespace {
			return as[i].Namespace < as[j].Namespace
		}
		return as[i].Object < as[j].Object
	})
}

func (e *AlertEngine) markSentLocked(as []Alert, now time.Time) {
	for _, x := range as {
		if a := e.active[x.Key]; a != nil {
			a.Notified, a.LastSent = true, now
		}
	}
}

func (e *AlertEngine) allowSendLocked(rule string, now time.Time) bool {
	keep := func(k string) int {
		ts := e.sent[k]
		i := 0
		for i < len(ts) && now.Sub(ts[i]) > time.Hour {
			i++
		}
		e.sent[k] = ts[i:]
		return len(ts) - i
	}
	return keep(rule) < ruleSendsPerHr && keep("") < totalSendsPerHr
}

// inQuiet: now falls in the rule's quiet hours (server time zone; may wrap midnight).
func inQuiet(r *AlertRule, now time.Time) bool {
	if r.QuietFrom == "" || r.QuietTo == "" || r.QuietFrom == r.QuietTo {
		return false
	}
	cur := now.Format("15:04")
	if r.QuietFrom < r.QuietTo {
		return cur >= r.QuietFrom && cur < r.QuietTo
	}
	return cur >= r.QuietFrom || cur < r.QuietTo
}

func (e *AlertEngine) silencedLocked(a *Alert, now time.Time) bool {
	for _, s := range e.silences {
		if s.Rule == a.Rule && (s.Key == "" || s.Key == a.Key) && (s.Until.IsZero() || now.Before(s.Until)) {
			return true
		}
	}
	return false
}

func (e *AlertEngine) expireSilencesLocked(now time.Time) {
	keep := e.silences[:0]
	for _, s := range e.silences {
		if s.Until.IsZero() || now.Before(s.Until) {
			keep = append(keep, s)
		}
	}
	e.silences = keep
}

// a silence "until it clears" on one alert ends when that alert resolves
func (e *AlertEngine) dropSilencesForLocked(key string) {
	keep := e.silences[:0]
	for _, s := range e.silences {
		if !(s.Key == key && s.Until.IsZero()) {
			keep = append(keep, s)
		}
	}
	e.silences = keep
}

func (e *AlertEngine) saveStateLocked() error {
	act := make([]*Alert, 0, len(e.active))
	for _, a := range e.active {
		act = append(act, a)
	}
	sort.Slice(act, func(i, j int) bool { return act[i].Key < act[j].Key })
	return e.st.save("state.json", map[string]any{"active": act, "silences": e.silences})
}

func (e *AlertEngine) saveRulesLocked() error {
	return e.st.save("rules.json", map[string]any{"rules": e.rules})
}

// ---------------------------------------------------------------- sending queue

func (e *AlertEngine) enqueue(n notice) {
	e.pending.Add(1)
	select {
	case e.queue <- n:
	default:
		e.pending.Done()
		e.st.appendHistory([]HistoryEntry{{At: n.At, Kind: "failed", Rule: n.Rule.ID, RuleName: n.Rule.Name, Text: "message dropped: too many waiting to be sent"}})
	}
}

func (e *AlertEngine) worker() {
	for {
		select {
		case <-e.stop:
			return
		case n := <-e.queue:
			func() {
				defer e.pending.Done()
				defer func() {
					if p := recover(); p != nil {
						log.Printf("alerts: sending panicked: %v", p)
					}
				}()
				e.st.appendHistory(e.deliver(n))
			}()
		}
	}
}

// flush waits until every queued message has been handled (tests).
func (e *AlertEngine) flush() { e.pending.Wait() }

// ---------------------------------------------------------------- rule changes (from the API)

func (e *AlertEngine) ruleByID(id string) (*AlertRule, int) {
	for i, r := range e.rules {
		if r.ID == id {
			return r, i
		}
	}
	return nil, -1
}

// dropAlertsLocked forgets a rule's alerts (deleted, disabled, or its check changed).
func (e *AlertEngine) dropAlertsLocked(id string) {
	for k := range e.active {
		if strings.HasPrefix(k, id+"|") {
			delete(e.active, k)
		}
	}
	keep := e.silences[:0]
	for _, s := range e.silences {
		if s.Rule != id {
			keep = append(keep, s)
		}
	}
	e.silences = keep
	delete(e.slow, id)
	delete(e.status, id)
}

// sameCheck: the edit keeps what is checked, so the rule's current alerts stay.
func sameCheck(a, b *AlertRule) bool {
	return a.Cluster == b.Cluster && a.Condition == b.Condition && a.Threshold == b.Threshold && a.Window == b.Window &&
		a.Match == b.Match && a.Kind == b.Kind && a.Workload == b.Workload && a.Labels == b.Labels &&
		strings.Join(a.Namespaces, ",") == strings.Join(b.Namespaces, ",") && a.Owner == b.Owner
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
