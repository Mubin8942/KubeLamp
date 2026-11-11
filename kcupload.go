package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kubeconfigs come from the user: uploaded or pasted in the browser (stored in
// <data>/kubeconfigs, mode 600) or, on this machine, a path to a file. Nothing is
// loaded until the user provides one.

const maxKubeconfigSize = 4 << 20

func (s *Server) kcDir(r *http.Request) string { return filepath.Join(s.userDir(r), "kubeconfigs") }

// isUploaded reports whether p is a kubeconfig KubeLamp stored from an upload.
func (s *Server) isUploaded(r *http.Request, p string) bool {
	rel, err := filepath.Rel(s.kcDir(r), p)
	return err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// credential plugins an uploaded kubeconfig may run when KubeLamp is reachable from
// other machines; anything else would let whoever can open the UI run commands here.
var allowedExecPlugins = map[string]bool{
	"aws": true, "aws-iam-authenticator": true, "kubelogin": true, "kubectl": true, "gke-gcloud-auth-plugin": true,
	"gcloud": true, "doctl": true, "oci": true, "az": true,
}

// checkUploadedKubeconfig parses an uploaded kubeconfig and, unless the server only
// listens on localhost, rejects things that would read or run files on the server.
func checkUploadedKubeconfig(b []byte, local bool) (kcFile, error) {
	var f kcFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("not valid YAML: %v", err)
	}
	if len(f.Contexts) == 0 {
		return f, errors.New("no contexts in this kubeconfig")
	}
	if local {
		return f, nil
	}
	for _, c := range f.Clusters {
		if c.Cluster.CAFile != "" {
			return f, fmt.Errorf("cluster %q points to a CA file (%s); embed it as certificate-authority-data instead", c.Name, c.Cluster.CAFile)
		}
	}
	for _, u := range f.Users {
		x := u.User
		switch {
		case x.ClientCert != "" || x.ClientKey != "":
			return f, fmt.Errorf("user %q points to certificate files; embed them as client-certificate-data / client-key-data instead", u.Name)
		case x.TokenFile != "":
			return f, fmt.Errorf("user %q reads its token from a file (%s); put the token in the kubeconfig instead", u.Name, x.TokenFile)
		case x.Exec != nil:
			cmd := x.Exec.Command
			if strings.ContainsAny(cmd, `/\`) || !allowedExecPlugins[strings.TrimSuffix(cmd, ".exe")] {
				return f, fmt.Errorf("user %q runs %q to log in; only known credential plugins (aws, kubelogin, gke-gcloud-auth-plugin, …) are allowed in uploaded kubeconfigs", u.Name, cmd)
			}
		}
	}
	return f, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// setKubeconfigs reloads the clusters from paths and remembers them.
func (s *Server) setKubeconfigs(r *http.Request, paths []string) {
	dir := s.userDir(r)
	_ = os.MkdirAll(dir, 0o700)
	st := loadSettings(dir)
	st.Kubeconfig = paths
	s.R(r).Load(paths, st.Access)
	saveSettings(dir, st)
}

func (s *Server) currentPaths(r *http.Request) []string {
	reg := s.R(r)
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return append([]string(nil), reg.paths...)
}

// KubeconfigSource is one file the clusters come from.
type KubeconfigSource struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	Uploaded bool     `json:"uploaded"`
	Contexts []string `json:"contexts"`
	Error    string   `json:"error,omitempty"`
}

func (s *Server) kubeconfigSources(r *http.Request) []KubeconfigSource {
	out := []KubeconfigSource{}
	for _, p := range s.currentPaths(r) {
		src := KubeconfigSource{Path: p, Name: p, Uploaded: s.isUploaded(r, p), Contexts: []string{}}
		if src.Uploaded {
			// stored as <name>-<random>.yaml: show the name the user gave
			base := strings.TrimSuffix(filepath.Base(p), ".yaml")
			if i := strings.LastIndex(base, "-"); i > 0 {
				base = base[:i]
			}
			src.Name = base
		}
		if b, err := os.ReadFile(p); err != nil {
			src.Error = err.Error()
		} else {
			var f kcFile
			if err := yaml.Unmarshal(b, &f); err != nil {
				src.Error = err.Error()
			}
			for _, c := range f.Contexts {
				src.Contexts = append(src.Contexts, c.Name)
			}
		}
		out = append(out, src)
	}
	return out
}

func (s *Server) kubeconfigRoutes(mux *http.ServeMux) {
	// upload or paste: {name, yaml}
	mux.HandleFunc("POST /api/kubeconfig/upload", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
			YAML string `json:"yaml"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKubeconfigSize)).Decode(&req); err != nil {
			writeErr(w, 400, errors.New("kubeconfig is too large or unreadable"))
			return
		}
		if strings.TrimSpace(req.YAML) == "" {
			writeErr(w, 400, errors.New("the kubeconfig is empty"))
			return
		}
		// anything goes only for an admin on their own machine; otherwise the kubeconfig
		// must not make the server read its files or run arbitrary commands
		f, err := checkUploadedKubeconfig([]byte(req.YAML), s.localOnly && s.isAdmin(r))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		// warn about contexts an earlier file already defines (the first file wins, like kubectl)
		have := map[string]bool{}
		for _, c := range s.R(r).All() {
			have[c.Info.Name] = true
		}
		var shadowed []string
		for _, c := range f.Contexts {
			if have[c.Name] {
				shadowed = append(shadowed, c.Name)
			}
		}
		name := strings.Trim(unsafeName.ReplaceAllString(strings.TrimSuffix(strings.TrimSuffix(req.Name, ".yaml"), ".yml"), "-"), "-.")
		if name == "" {
			name = "kubeconfig"
		}
		if len(name) > 60 {
			name = name[:60]
		}
		if err := os.MkdirAll(s.kcDir(r), 0o700); err != nil {
			writeErr(w, 500, err)
			return
		}
		p := filepath.Join(s.kcDir(r), name+"-"+randHex(4)+".yaml")
		if err := os.WriteFile(p, []byte(req.YAML), 0o600); err != nil {
			writeErr(w, 500, err)
			return
		}
		if _, err := loadKubeconfig([]string{p}); err != nil {
			os.Remove(p)
			writeErr(w, 400, err)
			return
		}
		s.setKubeconfigs(r, append(s.currentPaths(r), p))
		writeJSON(w, 200, map[string]any{"ok": true, "contexts": len(f.Contexts), "shadowed": shadowed})
	})

	// a kubeconfig file already on the machine KubeLamp runs on: {path}
	mux.HandleFunc("POST /api/kubeconfig/path", func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			writeErr(w, 403, errors.New("only admins can load kubeconfig files from the server's disk; upload or paste yours instead"))
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		paths := splitPaths(req.Path)
		if len(paths) == 0 {
			writeErr(w, 400, errors.New("enter the path to a kubeconfig file"))
			return
		}
		for _, p := range paths {
			if st, err := os.Stat(p); err != nil {
				writeErr(w, 400, errors.New("cannot read "+p+": "+err.Error()))
				return
			} else if st.IsDir() {
				writeErr(w, 400, errors.New(p+" is a folder — point to the config file inside it"))
				return
			}
		}
		if _, err := loadKubeconfig(paths); err != nil {
			writeErr(w, 400, err)
			return
		}
		all := s.currentPaths(r)
		for _, p := range paths {
			if !contains(all, p) {
				all = append(all, p)
			}
		}
		s.setKubeconfigs(r, all)
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// stop using one kubeconfig; uploaded copies are deleted
	mux.HandleFunc("DELETE /api/kubeconfig", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		all := s.currentPaths(r)
		if !contains(all, p) {
			writeErr(w, 404, errors.New("that kubeconfig isn't loaded"))
			return
		}
		keep := []string{}
		for _, x := range all {
			if x != p {
				keep = append(keep, x)
			}
		}
		if s.isUploaded(r, p) {
			_ = os.Remove(p)
		}
		s.setKubeconfigs(r, keep)
		writeJSON(w, 200, map[string]any{"ok": true})
	})
}
