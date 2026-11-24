package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HTTP API for alerting. Everyone signed in can read rules, alerts and history (phone
// numbers and email addresses are hidden from non-admins); only admins change anything.
// Channel secrets are write-only: the API says whether they are set, never what they are.

type ruleView struct {
	AlertRule
	SMSCount   int        `json:"smsCount"`
	EmailCount int        `json:"emailCount"`
	Status     ruleStatus `json:"status"`
	Firing     int        `json:"firing"`
	Pending    int        `json:"pending"`
}

type smsView struct {
	SMSConfig
	UsernameSet bool `json:"usernameSet"`
	PasswordSet bool `json:"passwordSet"`
}

type emailView struct {
	EmailConfig
	UsernameSet bool `json:"usernameSet"`
	PasswordSet bool `json:"passwordSet"`
}

func (e *AlertEngine) channelsView() map[string]any {
	e.mu.Lock()
	ch := e.ch
	e.mu.Unlock()
	sms := smsView{SMSConfig: ch.SMS, UsernameSet: ch.SMS.Username != "", PasswordSet: ch.SMS.Password != ""}
	sms.Username, sms.Password = "", ""
	em := emailView{EmailConfig: ch.Email, UsernameSet: ch.Email.Username != "", PasswordSet: ch.Email.Password != ""}
	em.Username, em.Password = "", ""
	return map[string]any{"sms": sms, "email": em, "updated": ch.Updated, "updatedBy": ch.UpdatedBy, "presets": smsPresets}
}

func (s *Server) alertRoutes(mux *http.ServeMux) {
	e := s.alerts
	ready := func(w http.ResponseWriter) bool {
		if e == nil {
			writeErr(w, 503, errors.New("alerting is not available: the secret key in the data folder could not be read or created"))
			return false
		}
		return true
	}
	admin := func(w http.ResponseWriter, r *http.Request) bool {
		if !ready(w) {
			return false
		}
		if !s.isAdmin(r) {
			writeErr(w, 403, errors.New("only admins can change alerting"))
			return false
		}
		return true
	}
	who := func(r *http.Request) string {
		if u := reqUser(r); u != nil {
			return u.Username
		}
		return "local"
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(v); err != nil {
			writeErr(w, 400, fmt.Errorf("bad request: %v", err))
			return false
		}
		return true
	}
	// a rule from the browser: checked, and its cluster must be one the admin has
	readRule := func(w http.ResponseWriter, r *http.Request) (*AlertRule, bool) {
		var rule AlertRule
		if !decode(w, r, &rule) {
			return nil, false
		}
		if err := rule.normalize(); err != nil {
			writeErr(w, 400, err)
			return nil, false
		}
		if s.R(r).Get(rule.Cluster) == nil {
			writeErr(w, 400, fmt.Errorf("cluster %q is not in your kubeconfig", rule.Cluster))
			return nil, false
		}
		rule.Owner, rule.OwnerName = reqUserID(r), who(r)
		return &rule, true
	}

	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		isAdmin := s.isAdmin(r)
		now := e.now()
		e.mu.Lock()
		rules := make([]ruleView, 0, len(e.rules))
		count := map[string][2]int{}
		active := make([]Alert, 0, len(e.active))
		for _, a := range e.active {
			x := *a
			x.Silenced = e.silencedLocked(a, now)
			active = append(active, x)
			c := count[a.Rule]
			if a.State == "firing" {
				c[0]++
			} else {
				c[1]++
			}
			count[a.Rule] = c
		}
		for _, x := range e.rules {
			v := ruleView{AlertRule: *x, SMSCount: len(x.SMS), EmailCount: len(x.Email), Status: e.status[x.ID], Firing: count[x.ID][0], Pending: count[x.ID][1]}
			if !isAdmin {
				v.SMS, v.Email = nil, nil
			}
			rules = append(rules, v)
		}
		sil := append([]Silence{}, e.silences...)
		ch := e.ch
		e.mu.Unlock()
		sev := map[string]int{"critical": 0, "warning": 1, "info": 2}
		sort.Slice(active, func(i, j int) bool {
			a, b := active[i], active[j]
			if (a.State == "firing") != (b.State == "firing") {
				return a.State == "firing"
			}
			if sev[a.Severity] != sev[b.Severity] {
				return sev[a.Severity] < sev[b.Severity]
			}
			return a.Since.After(b.Since)
		})
		sort.Slice(rules, func(i, j int) bool { return strings.ToLower(rules[i].Name) < strings.ToLower(rules[j].Name) })
		zone, _ := now.Zone()
		writeJSON(w, 200, map[string]any{
			"canEdit": isAdmin, "catalog": condTable, "rules": rules, "active": active, "silences": sil, "zone": zone,
			"channels": map[string]bool{"sms": ch.SMS.Enabled && ch.SMS.URL != "", "email": ch.Email.Enabled && ch.Email.Host != ""},
			"defaults": map[string]string{"smsText": defSMSText, "emailSubject": defEmailSubject, "emailBody": defEmailBody},
		})
	})

	mux.HandleFunc("GET /api/alerts/history", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > historyKeep {
			limit = 500
		}
		all := e.st.readHistory(0)
		rule := r.URL.Query().Get("rule")
		out := make([]HistoryEntry, 0, limit)
		for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
			if rule == "" || all[i].Rule == rule {
				out = append(out, all[i])
			}
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("POST /api/alerts/rules", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		rule, ok := readRule(w, r)
		if !ok {
			return
		}
		now := e.now()
		rule.ID, rule.Created, rule.Updated, rule.UpdatedBy = newID(), now, now, rule.OwnerName
		e.mu.Lock()
		if len(e.rules) >= 500 {
			e.mu.Unlock()
			writeErr(w, 400, errors.New("at most 500 rules"))
			return
		}
		e.rules = append(e.rules, rule)
		err := e.saveRulesLocked()
		e.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, rule)
	})

	mux.HandleFunc("PUT /api/alerts/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		rule, ok := readRule(w, r)
		if !ok {
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		old, i := e.ruleByID(r.PathValue("id"))
		if old == nil {
			writeErr(w, 404, errors.New("no such rule"))
			return
		}
		rule.ID, rule.Created, rule.Updated, rule.UpdatedBy = old.ID, old.Created, e.now(), rule.OwnerName
		if !sameCheck(old, rule) || !rule.Enabled {
			e.dropAlertsLocked(old.ID)
		} else {
			for _, a := range e.active { // keep the alerts, with the new name and severity
				if a.Rule == old.ID {
					a.RuleName, a.Severity = rule.Name, rule.Severity
				}
			}
		}
		e.rules[i] = rule
		if err := e.saveRulesLocked(); err != nil {
			writeErr(w, 500, err)
			return
		}
		_ = e.saveStateLocked()
		writeJSON(w, 200, rule)
	})

	mux.HandleFunc("POST /api/alerts/rules/{id}/enabled", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if !decode(w, r, &req) {
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		rule, _ := e.ruleByID(r.PathValue("id"))
		if rule == nil {
			writeErr(w, 404, errors.New("no such rule"))
			return
		}
		rule.Enabled, rule.Updated, rule.UpdatedBy = req.Enabled, e.now(), who(r)
		if !req.Enabled {
			e.dropAlertsLocked(rule.ID)
		}
		if err := e.saveRulesLocked(); err != nil {
			writeErr(w, 500, err)
			return
		}
		_ = e.saveStateLocked()
		writeJSON(w, 200, map[string]bool{"enabled": req.Enabled})
	})

	mux.HandleFunc("DELETE /api/alerts/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		rule, i := e.ruleByID(r.PathValue("id"))
		if rule == nil {
			writeErr(w, 404, errors.New("no such rule"))
			return
		}
		e.rules = append(e.rules[:i], e.rules[i+1:]...)
		e.dropAlertsLocked(rule.ID)
		if err := e.saveRulesLocked(); err != nil {
			writeErr(w, 500, err)
			return
		}
		_ = e.saveStateLocked()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// check: what an (unsaved) rule matches right now, and its messages
	mux.HandleFunc("POST /api/alerts/check", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		rule, ok := readRule(w, r)
		if !ok {
			return
		}
		n := e.checkNow(r.Context(), s.R(r).Get(rule.Cluster), rule, "test")
		out := map[string]any{"matches": n.Alerts, "error": "", "noData": false}
		if n.err != nil {
			var nd errNoData
			out["error"], out["noData"] = n.err.Error(), errors.As(n.err, &nd)
		}
		v := noticeVars(n.notice)
		out["preview"] = map[string]string{"sms": renderTpl(rule.SMSText, v, nil), "subject": renderTpl(rule.EmailSubject, v, nil),
			"body": renderTpl(rule.EmailBody, v, nil), "sample": strconv.FormatBool(n.sample)}
		writeJSON(w, 200, out)
	})

	// test: send the rule's messages now to its recipients, marked [TEST]
	mux.HandleFunc("POST /api/alerts/test", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		rule, ok := readRule(w, r)
		if !ok {
			return
		}
		if len(rule.SMS)+len(rule.Email) == 0 {
			writeErr(w, 400, errors.New("add a phone number or an email address first"))
			return
		}
		if rule.ID == "" {
			rule.ID = "test"
		}
		n := e.checkNow(r.Context(), s.R(r).Get(rule.Cluster), rule, "test")
		hist := e.deliver(n.notice)
		for i := range hist {
			hist[i].Text = "test: " + hist[i].Text
		}
		e.st.appendHistory(hist)
		writeJSON(w, 200, hist)
	})

	mux.HandleFunc("POST /api/alerts/silences", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		var req struct {
			Rule    string `json:"rule"`
			Key     string `json:"key"`
			Minutes int    `json:"minutes"` // 0 = until the alert clears (needs a key)
			Comment string `json:"comment"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Minutes < 0 || req.Minutes > 30*24*60 || (req.Minutes == 0 && req.Key == "") {
			writeErr(w, 400, errors.New("silence for 1 minute to 30 days, or one alert until it clears"))
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if rule, _ := e.ruleByID(req.Rule); rule == nil {
			writeErr(w, 404, errors.New("no such rule"))
			return
		}
		if req.Key != "" && e.active[req.Key] == nil {
			writeErr(w, 404, errors.New("that alert is no longer active"))
			return
		}
		sil := Silence{ID: newID(), Rule: req.Rule, Key: req.Key, By: who(r), Comment: short(req.Comment, 200)}
		if req.Minutes > 0 {
			sil.Until = e.now().Add(time.Duration(req.Minutes) * time.Minute)
		}
		e.silences = append(e.silences, sil)
		if err := e.saveStateLocked(); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, sil)
	})

	mux.HandleFunc("DELETE /api/alerts/silences/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		keep := e.silences[:0]
		for _, x := range e.silences {
			if x.ID != r.PathValue("id") {
				keep = append(keep, x)
			}
		}
		e.silences = keep
		_ = e.saveStateLocked()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/alerts/channels", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		writeJSON(w, 200, e.channelsView())
	})

	// save one channel; empty username/password keep the saved ones
	mux.HandleFunc("PUT /api/alerts/channels/{kind}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		kind := r.PathValue("kind")
		e.mu.Lock()
		ch := e.ch
		e.mu.Unlock()
		seal := func(newV, old string) (string, error) {
			if newV == "" {
				return old, nil
			}
			return e.st.box.seal(newV)
		}
		var err error
		switch kind {
		case "sms":
			var c SMSConfig
			if !decode(w, r, &c) {
				return
			}
			if err := checkSMS(&c); err != nil {
				writeErr(w, 400, err)
				return
			}
			if c.Username, err = seal(c.Username, ch.SMS.Username); err == nil {
				c.Password, err = seal(c.Password, ch.SMS.Password)
			}
			ch.SMS = c
		case "email":
			var c EmailConfig
			if !decode(w, r, &c) {
				return
			}
			if err := checkEmail(&c); err != nil {
				writeErr(w, 400, err)
				return
			}
			if c.Username, err = seal(c.Username, ch.Email.Username); err == nil {
				c.Password, err = seal(c.Password, ch.Email.Password)
			}
			ch.Email = c
		default:
			writeErr(w, 404, errors.New("unknown channel"))
			return
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		ch.Updated, ch.UpdatedBy = e.now(), who(r)
		e.mu.Lock()
		e.ch = ch
		err = e.st.save("channels.json", e.ch)
		e.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, e.channelsView())
	})

	mux.HandleFunc("DELETE /api/alerts/channels/{kind}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		e.mu.Lock()
		switch r.PathValue("kind") {
		case "sms":
			e.ch.SMS = SMSConfig{}
		case "email":
			e.ch.Email = EmailConfig{}
		}
		e.ch.Updated, e.ch.UpdatedBy = e.now(), who(r)
		err := e.st.save("channels.json", e.ch)
		e.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, e.channelsView())
	})

	mux.HandleFunc("POST /api/alerts/channels/{kind}/test", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		var req struct {
			To string `json:"to"`
		}
		if !decode(w, r, &req) {
			return
		}
		e.mu.Lock()
		ch := e.ch
		e.mu.Unlock()
		text := "KubeLamp test message at " + e.now().Format("2006-01-02 15:04") + ". If you got this, alerts can reach you."
		var err error
		to := strings.TrimSpace(req.To)
		switch r.PathValue("kind") {
		case "sms":
			to = strings.NewReplacer(" ", "", "-", "").Replace(to)
			if !phoneRe.MatchString(to) {
				writeErr(w, 400, errors.New("give a phone number to send the test to"))
				return
			}
			err = e.sendSMS(ch.SMS, to, text)
		case "email":
			a, perr := mail.ParseAddress(to)
			if perr != nil {
				writeErr(w, 400, errors.New("give an email address to send the test to"))
				return
			}
			err = e.sendEmail(ch.Email, []string{a.Address}, "KubeLamp test email", text)
		default:
			writeErr(w, 404, errors.New("unknown channel"))
			return
		}
		h := HistoryEntry{At: e.now(), Kind: "sent", Rule: "", RuleName: "Channel test", Text: "test " + r.PathValue("kind") + " sent"}
		if err != nil {
			h.Kind, h.Text = "failed", "test "+r.PathValue("kind")+" failed: "+err.Error()
		}
		e.st.appendHistory([]HistoryEntry{h})
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}

type checkResult struct {
	notice
	err    error
	sample bool
}

// checkNow evaluates a rule once against a cluster (no state is kept) and builds the
// notice its messages would carry; with no matches it uses a sample alert.
func (e *AlertEngine) checkNow(ctx context.Context, c *Cluster, rule *AlertRule, kind string) checkResult {
	now := e.now()
	res := checkResult{notice: notice{Rule: *rule, Kind: kind, At: now}}
	if c == nil {
		res.err = errors.New("unknown cluster")
	} else {
		c.touch()
		ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		hits, err := evalRule(&evalEnv{ctx: ctx, srv: e.srv, c: c, snap: c.Snap(), now: now, restarts: newRestartBook()}, rule)
		res.err = err
		for _, h := range hits {
			res.Alerts = append(res.Alerts, Alert{Key: h.Key, Rule: rule.ID, RuleName: rule.Name, Severity: rule.Severity, Cluster: rule.Cluster,
				Namespace: h.NS, Object: h.Object, Summary: h.Summary, State: "firing", Since: now, FiredAt: now})
		}
		sortAlerts(res.Alerts)
	}
	if len(res.Alerts) == 0 {
		ns := "default"
		if len(rule.Namespaces) > 0 {
			ns = rule.Namespaces[0]
		}
		obj := "Deployment/example"
		if d := condByID[rule.Condition]; d != nil && d.Target == "node" {
			obj, ns = "Node/example-node", ""
		}
		res.sample = true
		res.notice.Alerts = []Alert{{Rule: rule.ID, RuleName: rule.Name, Severity: rule.Severity, Cluster: rule.Cluster, Namespace: ns,
			Object: obj, Summary: "(sample) " + condByID[rule.Condition].Label, State: "firing", Since: now, FiredAt: now}}
	}
	return res
}

func checkSMS(c *SMSConfig) error {
	c.URL = strings.TrimSpace(c.URL)
	if c.Enabled || c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("the gateway URL must start with https:// or http://")
		}
	}
	c.Method = strings.ToUpper(firstNonEmpty(c.Method, "POST"))
	if !contains([]string{"POST", "GET", "PUT"}, c.Method) {
		return errors.New("method must be POST, GET or PUT")
	}
	if c.Auth == "" {
		c.Auth = "basic"
	}
	if !contains([]string{"none", "basic", "wsse"}, c.Auth) {
		return errors.New("authentication must be none, basic or wsse")
	}
	if strings.ContainsAny(c.ContentType, "\r\n") || len(c.Body) > 8000 || len(c.Headers) > 4000 {
		return errors.New("content type, headers or body are not valid")
	}
	for _, line := range strings.Split(c.Headers, "\n") {
		if strings.TrimSpace(line) != "" && !strings.Contains(line, ":") {
			return fmt.Errorf("header %q: write it as Name: value", strings.TrimSpace(line))
		}
	}
	return nil
}

func checkEmail(c *EmailConfig) error {
	c.Host = strings.TrimSpace(c.Host)
	if c.Enabled && c.Host == "" {
		return errors.New("give the mail server's host name")
	}
	if strings.ContainsAny(c.Host, " /\\") || c.Port < 0 || c.Port > 65535 {
		return errors.New("host or port is not valid")
	}
	if c.Security == "" {
		c.Security = "starttls"
	}
	if !contains([]string{"starttls", "tls", "none"}, c.Security) {
		return errors.New("security must be starttls, tls or none")
	}
	if c.From != "" {
		a, err := mail.ParseAddress(c.From)
		if err != nil {
			return errors.New("the From address is not valid")
		}
		c.From = a.Address
	} else if c.Enabled {
		return errors.New("give a From address")
	}
	c.FromName = noCRLF(strings.TrimSpace(c.FromName))
	return nil
}
