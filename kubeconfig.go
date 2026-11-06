package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------- kubeconfig file model

type kcFile struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string    `yaml:"name"`
		Cluster kcCluster `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User kcUser `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string    `yaml:"name"`
		Context kcContext `yaml:"context"`
	} `yaml:"contexts"`
}

type kcCluster struct {
	Server        string `yaml:"server"`
	CAData        string `yaml:"certificate-authority-data"`
	CAFile        string `yaml:"certificate-authority"`
	Insecure      bool   `yaml:"insecure-skip-tls-verify"`
	TLSServerName string `yaml:"tls-server-name"`
	ProxyURL      string `yaml:"proxy-url"`
	dir           string
}

type kcUser struct {
	ClientCertData string `yaml:"client-certificate-data"`
	ClientCert     string `yaml:"client-certificate"`
	ClientKeyData  string `yaml:"client-key-data"`
	ClientKey      string `yaml:"client-key"`
	Token          string `yaml:"token"`
	TokenFile      string `yaml:"tokenFile"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	Exec           *struct {
		Command    string   `yaml:"command"`
		Args       []string `yaml:"args"`
		APIVersion string   `yaml:"apiVersion"`
		Env        []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
	} `yaml:"exec"`
	AuthProvider *struct {
		Name   string            `yaml:"name"`
		Config map[string]string `yaml:"config"`
	} `yaml:"auth-provider"`
	dir string
}

type kcContext struct {
	Cluster   string `yaml:"cluster"`
	User      string `yaml:"user"`
	Namespace string `yaml:"namespace"`
}

// ContextInfo is what the UI sees about a kubeconfig context.
type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	User      string `json:"user"`
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
	Current   bool   `json:"current"`
	AuthType  string `json:"authType"`
	Source    string `json:"source"`
	Error     string `json:"error,omitempty"`
}

type resolvedContext struct {
	ContextInfo
	cluster kcCluster
	user    kcUser
}

// expandPath handles ~ and environment variables.
func expandPath(p string) string {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'`))
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	return os.ExpandEnv(p)
}

// splitPaths accepts the KUBECONFIG list syntax (':' on unix, ';' on windows)
// and also newlines/commas, which are friendlier in a text box.
func splitPaths(s string) []string {
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	f := func(r rune) bool {
		return r == '\n' || r == ',' || r == ';' || (sep == ":" && r == ':')
	}
	var out []string
	for _, p := range strings.FieldsFunc(s, f) {
		if p = expandPath(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func defaultKubeconfig() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

// loadKubeconfig merges files using kubectl's rule: the first file to define a name wins.
func loadKubeconfig(paths []string) ([]resolvedContext, error) {
	clusters := map[string]kcCluster{}
	users := map[string]kcUser{}
	type ctxEntry struct {
		name, src string
		ctx       kcContext
	}
	var ctxs []ctxEntry
	seenCtx := map[string]bool{}
	current := ""
	var errs []string
	loaded := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var f kcFile
		if err := yaml.Unmarshal(b, &f); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		loaded++
		dir := filepath.Dir(p)
		if current == "" {
			current = f.CurrentContext
		}
		for _, c := range f.Clusters {
			if _, ok := clusters[c.Name]; !ok {
				c.Cluster.dir = dir
				clusters[c.Name] = c.Cluster
			}
		}
		for _, u := range f.Users {
			if _, ok := users[u.Name]; !ok {
				u.User.dir = dir
				users[u.Name] = u.User
			}
		}
		for _, c := range f.Contexts {
			if !seenCtx[c.Name] {
				seenCtx[c.Name] = true
				ctxs = append(ctxs, ctxEntry{c.Name, p, c.Context})
			}
		}
	}
	if loaded == 0 {
		if len(errs) == 0 {
			errs = append(errs, "no kubeconfig path given")
		}
		return nil, errors.New(strings.Join(errs, "; "))
	}
	out := make([]resolvedContext, 0, len(ctxs))
	for _, c := range ctxs {
		rc := resolvedContext{ContextInfo: ContextInfo{
			Name: c.name, Cluster: c.ctx.Cluster, User: c.ctx.User,
			Namespace: c.ctx.Namespace, Current: c.name == current, Source: c.src,
		}}
		cl, ok := clusters[c.ctx.Cluster]
		if !ok {
			rc.Error = fmt.Sprintf("cluster %q not found in kubeconfig", c.ctx.Cluster)
		}
		u, uok := users[c.ctx.User]
		if !uok && c.ctx.User != "" {
			rc.Error = fmt.Sprintf("user %q not found in kubeconfig", c.ctx.User)
		}
		rc.cluster, rc.user = cl, u
		rc.Server = cl.Server
		rc.AuthType = authType(u)
		out = append(out, rc)
	}
	return out, nil
}

func authType(u kcUser) string {
	switch {
	case u.Exec != nil:
		return "exec: " + filepath.Base(u.Exec.Command)
	case u.ClientCertData != "" || u.ClientCert != "":
		return "client certificate"
	case u.Token != "" || u.TokenFile != "":
		return "token"
	case u.AuthProvider != nil:
		return "auth-provider: " + u.AuthProvider.Name
	case u.Username != "":
		return "basic"
	}
	return "none"
}

func resolveFile(dir, p string) string {
	p = expandPath(p)
	if p != "" && !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}

func dataOrFile(data, file, dir string) ([]byte, error) {
	if data != "" {
		return base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	}
	if file != "" {
		return os.ReadFile(resolveFile(dir, file))
	}
	return nil, nil
}

// ---------------------------------------------------------------- authenticated client

type Client struct {
	Info ContextInfo
	base *url.URL
	http *http.Client // for normal requests (with timeout via context)
	auth *authState
}

type authState struct {
	mu        sync.Mutex
	user      kcUser
	server    string
	cert      *tls.Certificate
	token     string
	expiresAt time.Time // for exec credentials
}

func newClient(rc resolvedContext) (*Client, error) {
	if rc.Error != "" {
		return nil, errors.New(rc.Error)
	}
	if rc.cluster.Server == "" {
		return nil, errors.New("cluster has no server address")
	}
	base, err := url.Parse(strings.TrimRight(rc.cluster.Server, "/"))
	if err != nil {
		return nil, fmt.Errorf("bad server URL: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: rc.cluster.TLSServerName}
	if rc.cluster.Insecure {
		tlsCfg.InsecureSkipVerify = true
	} else if ca, err := dataOrFile(rc.cluster.CAData, rc.cluster.CAFile, rc.cluster.dir); err != nil {
		return nil, fmt.Errorf("certificate authority: %w", err)
	} else if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("certificate authority data is not valid PEM")
		}
		tlsCfg.RootCAs = pool
	}

	as := &authState{user: rc.user, server: rc.cluster.Server}
	if cd, err := dataOrFile(rc.user.ClientCertData, rc.user.ClientCert, rc.user.dir); err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	} else if len(cd) > 0 {
		kd, err := dataOrFile(rc.user.ClientKeyData, rc.user.ClientKey, rc.user.dir)
		if err != nil {
			return nil, fmt.Errorf("client key: %w", err)
		}
		cert, err := tls.X509KeyPair(cd, kd)
		if err != nil {
			return nil, fmt.Errorf("client certificate/key: %w", err)
		}
		as.cert = &cert
	}
	tlsCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		as.mu.Lock()
		defer as.mu.Unlock()
		if as.cert == nil {
			return &tls.Certificate{}, nil
		}
		return as.cert, nil
	}

	proxy := http.ProxyFromEnvironment
	if rc.cluster.ProxyURL != "" {
		pu, err := url.Parse(rc.cluster.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("proxy-url: %w", err)
		}
		proxy = http.ProxyURL(pu)
	}
	tr := &http.Transport{
		Proxy:                 proxy,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Client{Info: rc.ContextInfo, base: base, http: &http.Client{Transport: tr}, auth: as}, nil
}

// execCredential runs an exec plugin (aws eks get-token, kubelogin, gke-gcloud-auth-plugin…).
func (a *authState) execCredential(ctx context.Context) error {
	e := a.user.Exec
	apiVersion := e.APIVersion
	if apiVersion == "" {
		apiVersion = "client.authentication.k8s.io/v1beta1"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Command, e.Args...)
	cmd.Env = os.Environ()
	for _, kv := range e.Env {
		cmd.Env = append(cmd.Env, kv.Name+"="+kv.Value)
	}
	info, _ := json.Marshal(map[string]any{
		"apiVersion": apiVersion, "kind": "ExecCredential",
		"spec": map[string]any{"interactive": false},
	})
	cmd.Env = append(cmd.Env, "KUBERNETES_EXEC_INFO="+string(info))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return fmt.Errorf("auth plugin %q failed: %v %s", e.Command, err, msg)
	}
	var cred struct {
		Status struct {
			Token                 string    `json:"token"`
			ClientCertificateData string    `json:"clientCertificateData"`
			ClientKeyData         string    `json:"clientKeyData"`
			ExpirationTimestamp   time.Time `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &cred); err != nil {
		return fmt.Errorf("auth plugin %q returned invalid JSON: %v", e.Command, err)
	}
	a.token = cred.Status.Token
	if cred.Status.ClientCertificateData != "" {
		c, err := tls.X509KeyPair([]byte(cred.Status.ClientCertificateData), []byte(cred.Status.ClientKeyData))
		if err != nil {
			return fmt.Errorf("auth plugin certificate: %w", err)
		}
		a.cert = &c
	}
	a.expiresAt = cred.Status.ExpirationTimestamp
	if a.expiresAt.IsZero() {
		a.expiresAt = time.Now().Add(10 * time.Minute)
	}
	return nil
}

func (a *authState) apply(ctx context.Context, req *http.Request, force bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.user
	switch {
	case u.Exec != nil:
		if force || a.token == "" && a.cert == nil || time.Until(a.expiresAt) < time.Minute {
			if err := a.execCredential(ctx); err != nil {
				return err
			}
		}
	case u.TokenFile != "":
		b, err := os.ReadFile(resolveFile(u.dir, u.TokenFile))
		if err != nil {
			return fmt.Errorf("tokenFile: %w", err)
		}
		a.token = strings.TrimSpace(string(b))
	case u.Token != "":
		a.token = u.Token
	case u.AuthProvider != nil:
		a.token = u.AuthProvider.Config["id-token"]
		if a.token == "" {
			a.token = u.AuthProvider.Config["access-token"]
		}
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	} else if u.Username != "" {
		req.SetBasicAuth(u.Username, u.Password)
	}
	return nil
}

// APIError carries the Kubernetes Status message.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%d %s", e.Code, http.StatusText(e.Code))
}

func (c *Client) do(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	return c.request(ctx, http.MethodGet, path, q, nil, "", "application/json")
}

func (c *Client) url(path string, q url.Values) string {
	u := *c.base
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawQuery = q.Encode()
	return u.String()
}

// request performs an authenticated API call. Non-2xx answers become *APIError
// carrying the Kubernetes Status message.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body []byte, ctype, accept string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		req.Header.Set("User-Agent", "kubedeck/"+version)
		if err := c.auth.apply(ctx, req, attempt > 0); err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.auth.user.Exec != nil {
			resp.Body.Close() // token expired early: refresh once
			continue
		}
		if resp.StatusCode >= 300 {
			defer resp.Body.Close()
			var st struct {
				Message string `json:"message"`
			}
			b := make([]byte, 64<<10)
			n, _ := ioReadFull(resp.Body, b)
			_ = json.Unmarshal(b[:n], &st)
			if st.Message == "" {
				st.Message = strings.TrimSpace(string(b[:min(n, 300)]))
			}
			return nil, &APIError{resp.StatusCode, st.Message}
		}
		return resp, nil
	}
	return nil, errors.New("unauthorized")
}

// send performs a write and returns the response body (the updated object).
func (c *Client) send(ctx context.Context, method, path string, q url.Values, body []byte, ctype string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.request(ctx, method, path, q, body, ctype, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// upgrade opens a WebSocket to the API server (pod exec). HTTP/1.1 is forced
// because WebSocket upgrades don't work over HTTP/2.
func (c *Client) upgrade(ctx context.Context, path string, q url.Values, protocols []string) (io.ReadWriteCloser, string, error) {
	tr := c.http.Transport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig != nil {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path, q), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	req.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ", "))
	req.Header.Set("User-Agent", "kubedeck/"+version)
	if err := c.auth.apply(ctx, req, false); err != nil {
		return nil, "", err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &st)
		if st.Message == "" {
			st.Message = strings.TrimSpace(string(b))
		}
		return nil, "", &APIError{resp.StatusCode, st.Message}
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, "", errors.New("connection does not support upgrades")
	}
	return rwc, resp.Header.Get("Sec-WebSocket-Protocol"), nil
}

// get decodes a single JSON object.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.do(ctx, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// list fetches every page of a collection, keeping memory bounded per page.
func list[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("limit", "500")
	var all []T
	for page := 0; page < 200; page++ {
		var res struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []T `json:"items"`
		}
		if err := c.get(ctx, path, q, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if res.Metadata.Continue == "" {
			break
		}
		q.Set("continue", res.Metadata.Continue)
	}
	return all, nil
}
