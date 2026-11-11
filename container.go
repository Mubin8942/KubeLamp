package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Container and in-cluster support (KubeLamp 1.4): settings from environment variables,
// a stable access token, and a context built from the pod's service account.

// envName maps a flag name to its environment variable: -max-download → KUBELAMP_MAX_DOWNLOAD.
func envName(flagName string) string {
	return "KUBELAMP_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnv sets every flag that has a KUBELAMP_* environment variable. Command-line flags
// are parsed afterwards, so they still win.
func applyEnv(fs *flag.FlagSet, getenv func(string) string) error {
	var errs []string
	fs.VisitAll(func(f *flag.Flag) {
		v := getenv(envName(f.Name))
		if v == "" {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			errs = append(errs, fmt.Sprintf("%s=%q: %v", envName(f.Name), v, err))
		}
	})
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// inClusterContext builds a context from the pod's service account, like client-go's
// InClusterConfig. The token is read from its file on every request, so Kubernetes' token
// rotation is picked up without a restart.
func inClusterContext(name string) (*resolvedContext, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running inside Kubernetes (KUBERNETES_SERVICE_HOST is not set)")
	}
	dir := os.Getenv("KUBELAMP_SA_DIR")
	if dir == "" {
		dir = serviceAccountDir
	}
	tok, ca := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
	for _, f := range []string{tok, ca} {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("service account file missing (automountServiceAccountToken off?): %w", err)
		}
	}
	if name == "" {
		name = "in-cluster"
	}
	server := "https://" + net.JoinHostPort(host, port)
	rc := &resolvedContext{ContextInfo: ContextInfo{Name: name, Cluster: name, User: "serviceaccount", Server: server,
		Current: true, AuthType: "service account", Source: "in-cluster"}}
	rc.cluster = kcCluster{Server: server, CAFile: ca}
	rc.user = kcUser{TokenFile: tok}
	return rc, nil
}

// existingPaths keeps the kubeconfig files that exist.
func existingPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// runHealthcheck asks the local KubeLamp for /healthz (distroless images have no curl).
func runHealthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad address:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}
