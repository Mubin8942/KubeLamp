package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TLS certificate expiry for each cluster (KubeLamp 1.4.1):
//   - the API server certificate: recorded from every TLS handshake KubeLamp makes, and read with
//     a handshake-only probe when the cluster can't be reached (an expired certificate is exactly
//     what makes it unreachable);
//   - the client certificate from the kubeconfig (kubeadm's admin.conf expires after a year);
//   - the cluster CA from the kubeconfig.
// Other control-plane certificates (etcd, front-proxy, kubelet, controller-manager) are not
// visible through the API server.

const (
	certWarnDays = 30
	certCritDays = 7
	certProbeTTL = 6 * time.Hour
	certRetry    = 10 * time.Minute
)

type CertInfo struct {
	Role      string    `json:"role"` // server | client | ca
	Label     string    `json:"label"`
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	DaysLeft  int       `json:"daysLeft"` // negative once expired
	State     string    `json:"state"`    // ok | warn | crit | expired
	SANs      []string  `json:"sans,omitempty"`
	Note      string    `json:"note,omitempty"`
}

type CertReport struct {
	Certs    []CertInfo `json:"certs"`
	Soonest  *CertInfo  `json:"soonest,omitempty"`
	Worst    string     `json:"worst"` // the worst state of all certificates ("" when none are known)
	Insecure bool       `json:"insecure,omitempty"`
	ServerAt time.Time  `json:"serverAt,omitempty"` // when the server certificate was last seen
	Err      string     `json:"err,omitempty"`      // why the server certificate is unknown
}

// certSeen holds the chain the API server presented most recently.
type certSeen struct {
	mu       sync.Mutex
	chain    []*x509.Certificate
	at       time.Time
	probing  bool
	probeAt  time.Time
	probeErr string
}

func (s *certSeen) record(chain []*x509.Certificate) {
	if len(chain) == 0 {
		return
	}
	s.mu.Lock()
	s.chain, s.at, s.probeErr = chain, time.Now(), ""
	s.mu.Unlock()
}

func parsePEMCerts(b []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return out
		}
		if blk.Type == "CERTIFICATE" {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
				out = append(out, c)
			}
		}
	}
}

func certState(notAfter, now time.Time) (days int, state string) {
	d := notAfter.Sub(now)
	days = int(math.Floor(d.Hours() / 24))
	switch {
	case d <= 0:
		return days, "expired"
	case days < certCritDays:
		return days, "crit"
	case days < certWarnDays:
		return days, "warn"
	}
	return days, "ok"
}

var certRank = map[string]int{"": 0, "ok": 1, "warn": 2, "crit": 3, "expired": 4}

func certInfo(role, label string, c *x509.Certificate, now time.Time) CertInfo {
	days, st := certState(c.NotAfter, now)
	ci := CertInfo{Role: role, Label: label, Subject: nameOf(c.Subject.CommonName, c.Subject.Organization, c.Subject.String()),
		Issuer: nameOf(c.Issuer.CommonName, c.Issuer.Organization, c.Issuer.String()), NotBefore: c.NotBefore, NotAfter: c.NotAfter, DaysLeft: days, State: st}
	if role == "server" {
		ci.SANs = append(ci.SANs, c.DNSNames...)
		for _, ip := range c.IPAddresses {
			ci.SANs = append(ci.SANs, ip.String())
		}
		if len(ci.SANs) > 12 {
			ci.SANs = append(ci.SANs[:12], "…")
		}
	}
	if now.Before(c.NotBefore) {
		ci.Note = "not valid yet (check the clock)"
	}
	return ci
}

func nameOf(cn string, org []string, full string) string {
	if cn != "" {
		return cn
	}
	if len(org) > 0 {
		return strings.Join(org, ", ")
	}
	return full
}

// CertReport describes the certificates of this connection. It never blocks: when the server
// certificate is unknown or old, a background handshake probe is started.
func (c *Client) CertReport() *CertReport {
	if c == nil || c.seen == nil {
		return nil
	}
	now := time.Now()
	r := &CertReport{Insecure: c.insecure, Certs: []CertInfo{}}
	c.seen.mu.Lock()
	chain, at, probeErr := c.seen.chain, c.seen.at, c.seen.probeErr
	if (len(chain) == 0 || now.Sub(at) > certProbeTTL) && !c.seen.probing && now.Sub(c.seen.probeAt) > certRetry {
		c.seen.probing, c.seen.probeAt = true, now
		go c.probeServerCert()
	}
	c.seen.mu.Unlock()

	if len(chain) > 0 {
		r.Certs = append(r.Certs, certInfo("server", "API server", chain[0], now))
		r.ServerAt = at
	} else if c.base.Scheme == "https" {
		r.Err = probeErr
		if r.Err == "" {
			r.Err = "not read yet"
		}
	}
	c.auth.mu.Lock()
	cert := c.auth.cert
	exec := c.auth.user.Exec != nil
	c.auth.mu.Unlock()
	if cert != nil && len(cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			ci := certInfo("client", "Client certificate", leaf, now)
			if exec {
				ci.Note = "issued by the kubeconfig's exec plugin"
			}
			r.Certs = append(r.Certs, ci)
		}
	}
	for _, ca := range c.cas {
		r.Certs = append(r.Certs, certInfo("ca", "Cluster CA", ca, now))
	}
	for i := range r.Certs {
		ci := &r.Certs[i]
		if certRank[ci.State] > certRank[r.Worst] {
			r.Worst = ci.State
		}
		if r.Soonest == nil || ci.NotAfter.Before(r.Soonest.NotAfter) {
			cp := *ci
			r.Soonest = &cp
		}
	}
	return r
}

// probeServerCert reads the certificate the API server presents with a TLS handshake only (no
// request, no credentials). Verification is off for this probe on purpose: it must also see an
// expired or otherwise invalid certificate.
func (c *Client) probeServerCert() {
	defer func() {
		c.seen.mu.Lock()
		c.seen.probing = false
		c.seen.mu.Unlock()
	}()
	fail := func(msg string) {
		c.seen.mu.Lock()
		c.seen.probeErr = msg
		c.seen.mu.Unlock()
	}
	if c.base.Scheme != "https" {
		return
	}
	host := c.base.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), "443")
	}
	if c.proxied {
		fail("reached through a proxy: the certificate is read on the next successful connection")
		return
	}
	if u, _ := http.ProxyFromEnvironment(&http.Request{URL: c.base}); u != nil {
		fail("reached through a proxy: the certificate is read on the next successful connection")
		return
	}
	name := c.serverName
	if name == "" {
		name = c.base.Hostname()
	}
	d := &net.Dialer{Timeout: 6 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", host, &tls.Config{InsecureSkipVerify: true, ServerName: name, MinVersion: tls.VersionTLS12}) //nolint:gosec // read-only probe of the presented certificate
	if err != nil {
		fail("could not read the certificate: " + friendlyErr(err))
		return
	}
	chain := conn.ConnectionState().PeerCertificates
	_ = conn.Close()
	c.seen.record(chain)
}
