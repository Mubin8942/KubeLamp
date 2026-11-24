package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sending: SMS through an HTTP gateway whose request is fully configurable (URL, method,
// headers, body template, auth), and email over SMTP. Secrets are stored encrypted and
// never leave the server.

type SMSConfig struct {
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url"`
	Method      string `json:"method"`      // POST | GET | PUT
	ContentType string `json:"contentType"` // application/json | application/x-www-form-urlencoded | text/plain
	Auth        string `json:"auth"`        // none | basic | wsse
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	ServiceID   string `json:"serviceId"`
	FA          string `json:"fa"`
	PresentID   string `json:"presentId"`
	LinkID      string `json:"linkId"`
	Sender      string `json:"sender"`
	// WSSE with a fixed digest (as some gateways are set up): used when WSSEDigest is set,
	// else a fresh nonce and digest are made from the password for every message
	WSSENonce   string `json:"wsseNonce"`
	WSSECreated string `json:"wsseCreated"`
	WSSEDigest  string `json:"wsseDigest,omitempty"`  // encrypted, never returned
	ClearDigest bool   `json:"clearDigest,omitempty"` // request only: forget the fixed digest
	Headers     string `json:"headers"`               // one "Name: value" per line, placeholders allowed
	Body        string `json:"body"`
	InsecureTLS bool   `json:"insecureTLS"`
}

type EmailConfig struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"` // starttls | tls | none
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	From        string `json:"from"`
	FromName    string `json:"fromName"`
	InsecureTLS bool   `json:"insecureTLS"`
}

type Channels struct {
	SMS       SMSConfig   `json:"sms"`
	Email     EmailConfig `json:"email"`
	Updated   time.Time   `json:"updated"`
	UpdatedBy string      `json:"updatedBy"`
}

// SMS request presets: a starting point the admin can change on the Channels tab.
var smsPresets = []map[string]string{
	{"id": "sdp", "label": "Huawei SDP REST (WSSE headers)", "method": "POST", "contentType": "application/json; charset=UTF-8", "auth": "wsse",
		"headers": "Accept: application/json\nX-RequestHeader: request ServiceId=\"{{service_id}}\",FA=\"{{fa}}\",PresentId=\"{{present_id}}\",LinkId=\"{{link_id}}\"",
		"body":    `{"outboundSMSMessageRequest":{"address":["{{to}}"],"senderAddress":"{{sender}}","outboundSMSTextMessage":{"message":"{{message}}"}}}`},
	{"id": "oneapi", "label": "OneAPI JSON (Basic auth)", "method": "POST", "contentType": "application/json", "auth": "basic",
		"headers": "Accept: application/json",
		"body": `{"outboundSMSMessageRequest":{"address":["tel:{{to}}"],"senderAddress":"tel:{{sender}}","senderName":"{{sender}}",` +
			`"outboundSMSTextMessage":{"message":"{{message}}"},"clientCorrelator":"{{correlator}}"}}`},
	{"id": "form", "label": "Form POST", "method": "POST", "contentType": "application/x-www-form-urlencoded", "auth": "none", "headers": "",
		"body": "username={{username}}&password={{password}}&to={{to}}&from={{sender}}&text={{message}}"},
}

// ---------------------------------------------------------------- message text

// renderTpl replaces {{name}} placeholders; esc escapes each value (for JSON bodies etc).
func renderTpl(tpl string, vars map[string]string, esc func(string) string) string {
	var b strings.Builder
	for {
		i := strings.Index(tpl, "{{")
		if i < 0 {
			b.WriteString(tpl)
			break
		}
		j := strings.Index(tpl[i:], "}}")
		if j < 0 {
			b.WriteString(tpl)
			break
		}
		b.WriteString(tpl[:i])
		name := strings.ToLower(strings.TrimSpace(tpl[i+2 : i+j]))
		if v, ok := vars[name]; ok {
			if esc != nil {
				v = esc(v)
			}
			b.WriteString(v)
		} else {
			b.WriteString(tpl[i : i+j+2]) // unknown: left as typed
		}
		tpl = tpl[i+j+2:]
	}
	return b.String()
}

var statusWord = map[string]string{"firing": "FIRING", "reminder": "STILL FIRING", "resolved": "RESOLVED", "test": "TEST"}

// noticeVars are the placeholders a rule's messages can use.
func noticeVars(n notice) map[string]string {
	as := n.Alerts
	v := map[string]string{
		"status": statusWord[n.Kind], "severity": strings.ToUpper(n.Rule.Severity), "rule": n.Rule.Name,
		"description": n.Rule.Description, "cluster": n.Rule.Cluster, "count": strconv.Itoa(len(as)),
		"time": n.At.Format("2006-01-02 15:04"), "condition": "",
	}
	if d := condByID[n.Rule.Condition]; d != nil {
		v["condition"] = d.Label
	}
	var nss, lines, sums []string
	for _, a := range as {
		if a.Namespace != "" && !contains(nss, a.Namespace) {
			nss = append(nss, a.Namespace)
		}
		where := a.Object
		if a.Namespace != "" {
			where = a.Namespace + "/" + a.Object
		}
		since := ""
		if !a.FiredAt.IsZero() && n.Kind != "test" {
			since = " (since " + a.FiredAt.Format("15:04") + ")"
		}
		lines = append(lines, "- "+where+": "+a.Summary+since)
		sums = append(sums, a.Object+" "+a.Summary)
	}
	v["namespace"] = strings.Join(nss, ", ")
	v["list"] = strings.Join(lines, "\n")
	if len(as) > 0 {
		v["object"], v["summary"] = as[0].Object, as[0].Summary
		v["since"] = as[0].FiredAt.Format("2006-01-02 15:04")
	}
	if len(as) > 1 {
		v["object"] = fmt.Sprintf("%d objects", len(as))
		s := strings.Join(sums, "; ")
		if len(s) > 300 {
			s = s[:299] + "…"
		}
		v["summary"] = fmt.Sprintf("%d alerts: %s", len(as), s)
	}
	return v
}

// ---------------------------------------------------------------- delivery

// send delivers one notice to the rule's phone numbers and email addresses.
func (e *AlertEngine) send(n notice) []HistoryEntry {
	e.mu.Lock()
	ch := e.ch
	e.mu.Unlock()
	vars := noticeVars(n)
	entry := func(kind, text string) HistoryEntry {
		h := HistoryEntry{At: e.now(), Kind: kind, Rule: n.Rule.ID, RuleName: n.Rule.Name, Severity: n.Rule.Severity, Cluster: n.Rule.Cluster, Text: text}
		if len(n.Alerts) == 1 {
			h.Namespace, h.Object = n.Alerts[0].Namespace, n.Alerts[0].Object
		}
		return h
	}
	what := map[string]string{"firing": "alert", "reminder": "reminder", "resolved": "resolved message", "test": "test message"}[n.Kind]
	var out []HistoryEntry
	if len(n.Rule.SMS) > 0 {
		text := renderTpl(n.Rule.SMSText, vars, nil)
		if n.Kind == "test" {
			text = "[TEST] " + text
		}
		var failed []string
		for _, to := range n.Rule.SMS {
			if err := e.sendSMS(ch.SMS, to, text); err != nil {
				failed = append(failed, to+": "+err.Error())
			}
		}
		if len(failed) == 0 {
			out = append(out, entry("sent", fmt.Sprintf("SMS %s to %d number(s)", what, len(n.Rule.SMS))))
		} else {
			out = append(out, entry("failed", fmt.Sprintf("SMS %s: %d of %d failed: %s", what, len(failed), len(n.Rule.SMS), short(strings.Join(failed, "; "), 400))))
		}
	}
	if len(n.Rule.Email) > 0 {
		subj := renderTpl(n.Rule.EmailSubject, vars, nil)
		body := renderTpl(n.Rule.EmailBody, vars, nil)
		if n.Kind == "test" {
			subj = "[TEST] " + subj
		}
		if err := e.sendEmail(ch.Email, n.Rule.Email, subj, body); err != nil {
			out = append(out, entry("failed", fmt.Sprintf("email %s to %d address(es) failed: %s", what, len(n.Rule.Email), short(err.Error(), 400))))
		} else {
			out = append(out, entry("sent", fmt.Sprintf("email %s to %d address(es)", what, len(n.Rule.Email))))
		}
	}
	return out
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func noCRLF(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// buildSMSRequest makes the gateway request for one number. It also returns the secret
// values in it, to be masked when the request is logged.
func (e *AlertEngine) buildSMSRequest(ctx context.Context, cfg SMSConfig, to, text string) (*http.Request, []string, error) {
	if !cfg.Enabled || cfg.URL == "" {
		return nil, nil, errors.New("SMS is not set up (Tools → Alerting → Channels)")
	}
	user, err := e.st.box.open(cfg.Username)
	if err != nil {
		return nil, nil, err
	}
	pass, err := e.st.box.open(cfg.Password)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	nonceS := hex.EncodeToString(nonce)
	digest := sha256.Sum256([]byte(nonceS + created + pass))
	digestS := base64.StdEncoding.EncodeToString(digest[:])
	if cfg.WSSEDigest != "" { // the fixed values the gateway was set up with
		if digestS, err = e.st.box.open(cfg.WSSEDigest); err != nil {
			return nil, nil, err
		}
		nonceS, created = cfg.WSSENonce, cfg.WSSECreated
	}
	corr := make([]byte, 8)
	_, _ = rand.Read(corr)
	vars := map[string]string{
		"to": to, "to_plain": strings.TrimPrefix(to, "+"), "message": text, "sender": cfg.Sender, "username": user, "password": pass,
		"service_id": cfg.ServiceID, "fa": cfg.FA, "present_id": cfg.PresentID, "link_id": cfg.LinkID,
		"correlator": hex.EncodeToString(corr), "wsse_nonce": nonceS, "wsse_created": created,
		"wsse_digest": digestS,
	}
	var esc func(string) string
	switch {
	case strings.Contains(cfg.ContentType, "json"):
		esc = jsonEsc
	case strings.Contains(cfg.ContentType, "form"):
		esc = url.QueryEscape
	}
	method := strings.ToUpper(firstNonEmpty(cfg.Method, "POST"))
	target := renderTpl(cfg.URL, vars, url.QueryEscape)
	var body io.Reader
	if method != "GET" {
		body = strings.NewReader(renderTpl(cfg.Body, vars, esc))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, err
	}
	if body != nil && cfg.ContentType != "" {
		req.Header.Set("Content-Type", cfg.ContentType)
	}
	switch cfg.Auth {
	case "basic":
		req.SetBasicAuth(user, pass)
	case "wsse":
		req.Header.Set("Authorization", `WSSE realm="SDP",profile="UsernameToken"`) // add type="Appkey" in Extra headers if a gateway wants it
		req.Header.Set("X-WSSE", fmt.Sprintf(`UsernameToken Username="%s", PasswordDigest="%s", Nonce="%s", Created="%s"`,
			noCRLF(user), digestS, noCRLF(nonceS), noCRLF(created)))
	}
	for _, line := range strings.Split(cfg.Headers, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if k = strings.TrimSpace(k); !ok || k == "" {
			continue
		}
		req.Header.Set(k, noCRLF(strings.TrimSpace(renderTpl(v, vars, nil))))
	}
	return req, []string{pass, digestS}, nil
}

func (e *AlertEngine) sendSMS(cfg SMSConfig, to, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, secrets, err := e.buildSMSRequest(ctx, cfg, to, text)
	if err != nil {
		log.Printf("alerts: SMS to %s not sent: %v", to, err)
		return err
	}
	logSMSRequest(req, secrets)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // admin's explicit choice for an internal gateway
	}
	start := time.Now()
	resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		log.Printf("alerts: SMS to %s: gateway not reachable after %s: %v", to, time.Since(start).Round(time.Millisecond), err)
		return fmt.Errorf("gateway not reachable: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var hs []string
	for _, k := range []string{"Content-Type", "Content-Length", "Server", "Date"} {
		if v := resp.Header.Get(k); v != "" {
			hs = append(hs, k+": "+v)
		}
	}
	log.Printf("alerts: SMS to %s: gateway answered %s in %s [%s]\n  response body: %s", to, resp.Status, time.Since(start).Round(time.Millisecond),
		strings.Join(hs, "; "), firstNonEmpty(strings.TrimSpace(string(b)), "(empty)"))
	if resp.StatusCode/100 != 2 {
		why := strings.TrimSpace(string(b))
		if why == "" {
			why = "(the gateway sent no error text; the request is in the server log)"
		}
		return fmt.Errorf("gateway answered %s: %s", resp.Status, short(why, 300))
	}
	return nil
}

// logSMSRequest writes the request to the server log the way curl -v shows it, with the
// password and digest masked.
func logSMSRequest(req *http.Request, secrets []string) {
	mask := func(s string) string {
		for _, x := range secrets {
			if len(x) >= 3 {
				s = strings.ReplaceAll(s, x, maskSecret(x))
			}
		}
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "alerts: SMS request\n  > %s %s", req.Method, mask(req.URL.String()))
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range req.Header[k] {
			if k == "Authorization" && strings.HasPrefix(v, "Basic ") {
				v = "Basic ****"
			}
			fmt.Fprintf(&b, "\n  > %s: %s", k, mask(v))
		}
	}
	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			body, _ := io.ReadAll(io.LimitReader(rc, 8192))
			rc.Close()
			fmt.Fprintf(&b, "\n  > body: %s", mask(string(body)))
		}
	}
	log.Print(b.String())
}

// maskSecret keeps the first 4 characters and the length: enough to compare with the
// value in a working script, not enough to reuse it.
func maskSecret(s string) string {
	if len(s) <= 6 {
		return "****"
	}
	return fmt.Sprintf("%s…(%d chars)", s[:4], len(s))
}

func (e *AlertEngine) sendEmail(cfg EmailConfig, to []string, subject, body string) error {
	err := e.sendEmailOnce(cfg, to, subject, body)
	if err != nil {
		log.Printf("alerts: email %q to %s via %s:%d (%s) failed: %v", subject, strings.Join(to, ", "), cfg.Host, cfg.Port, cfg.Security, err)
	} else {
		log.Printf("alerts: email %q sent to %s via %s", subject, strings.Join(to, ", "), cfg.Host)
	}
	return err
}

func (e *AlertEngine) sendEmailOnce(cfg EmailConfig, to []string, subject, body string) error {
	if !cfg.Enabled || cfg.Host == "" || cfg.From == "" {
		return errors.New("email is not set up (Tools → Alerting → Channels)")
	}
	user, err := e.st.box.open(cfg.Username)
	if err != nil {
		return err
	}
	pass, err := e.st.box.open(cfg.Password)
	if err != nil {
		return err
	}
	from := mail.Address{Name: cfg.FromName, Address: cfg.From}
	var msg bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&msg, "%s: %s\r\n", k, noCRLF(v)) }
	hdr("From", from.String())
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", noCRLF(subject)))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	mid := make([]byte, 12)
	_, _ = rand.Read(mid)
	hdr("Message-ID", "<"+hex.EncodeToString(mid)+"@kubelamp>")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	msg.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&msg)
	_, _ = qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	_ = qp.Close()

	port := cfg.Port
	if port == 0 {
		port = map[string]int{"tls": 465, "none": 25}[cfg.Security]
		if port == 0 {
			port = 587
		}
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	tcfg := &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: cfg.InsecureTLS} //nolint:gosec // admin's choice for an internal relay
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if cfg.Security == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tcfg)
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail server not reachable: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.Security == "starttls" || cfg.Security == "" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS: choose TLS or none")
		}
		if err := c.StartTLS(tcfg); err != nil {
			return fmt.Errorf("STARTTLS: %v", err)
		}
	}
	if user != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("the mail server does not accept a login (no AUTH): leave the username empty")
		}
		if err := c.Auth(loginAuth(user, pass, cfg.Host)); err != nil {
			return fmt.Errorf("login refused: %v", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("sender refused: %v", err)
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("recipient %s refused: %v", t, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message refused: %v", err)
	}
	return c.Quit()
}

// loginAuth is PLAIN auth that also works over a connection the admin chose to leave
// unencrypted (security "none" to an internal relay); smtp.PlainAuth refuses that.
type plainAuth struct{ user, pass, host string }

func loginAuth(user, pass, host string) smtp.Auth { return &plainAuth{user, pass, host} }

func (a *plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}
