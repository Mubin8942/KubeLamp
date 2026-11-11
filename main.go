// KubeLamp — a lightweight, multi-cluster Kubernetes dashboard with terminals.
// Single binary, standard library + a YAML parser. MIT licensed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "1.5.0"

func main() {
	kubeconfig := flag.String("kubeconfig", "", "optional: path(s) to kubeconfig files to load at start; separate several with "+string(os.PathListSeparator)+". Without it KubeLamp asks for a kubeconfig in the browser")
	addr := flag.String("addr", "127.0.0.1:7080", "listen address; use 0.0.0.0:7080 to share on the network")
	adminUser := flag.String("admin-user", "", "name of the admin created on first start (default admin); ignored once users exist")
	adminPwd := flag.String("admin-password", "", "password of the admin created on first start (default admin, which must be changed at first login); prefer the KUBELAMP_ADMIN_PASSWORD variable")
	token := flag.String("token", "", "removed: KubeLamp now has user accounts (ignored)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser on start")
	readOnly := flag.Bool("read-only", false, "disable every write: edit, delete, scale, restart, exec and the shell")
	allowShell := flag.Bool("allow-shell", false, "allow the local shell even when listening beyond localhost (dangerous)")
	maxDown := flag.Int("max-download", 256, "largest file or folder (MiB) the file transfer tool may download from a pod (max 2048)")
	maxUp := flag.Int("max-upload", 100, "largest file (MiB) the file transfer tool may upload into a pod")
	xferRate := flag.Int("transfer-rate", 20, "file transfer speed limit in MiB/s per transfer, so large copies don't load the API server or the node")
	dataFlag := flag.String("data", "", "folder for users, their kubeconfigs and settings (default: your user config folder; /data in the Docker image)")
	tokenFile := flag.String("token-file", "", "removed: KubeLamp now has user accounts (ignored)")
	inCluster := flag.String("in-cluster", "false", "also connect to the Kubernetes cluster KubeLamp runs in, with its pod's service account: false (default), true, or auto (only when running in a pod)")
	clusterName := flag.String("cluster-name", "in-cluster", "name shown for the in-cluster connection")
	healthcheck := flag.Bool("healthcheck", false, "check that the KubeLamp listening on -addr answers /healthz, then exit (for Docker HEALTHCHECK)")
	interval := flag.Duration("interval", 15*time.Second, "refresh interval for clusters you are viewing (min 5s); history keeps 1 hour")
	// macOS passes -psn_… to apps started from Finder on older versions; ignore it
	var args []string
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-psn_") {
			args = append(args, a)
		}
	}
	// every flag can also come from a KUBELAMP_* environment variable (Docker, Kubernetes)
	if err := applyEnv(flag.CommandLine, os.Getenv); err != nil {
		log.Fatalf("environment: %v", err)
	}
	_ = flag.CommandLine.Parse(args)
	if *healthcheck {
		os.Exit(runHealthcheck(*addr))
	}
	if *interval < 5*time.Second {
		*interval = 5 * time.Second
	}
	activeInterval = *interval
	historyPoints = int(time.Hour / *interval)

	dataDir := *dataFlag
	if dataDir == "" {
		cfgDir, _ := os.UserConfigDir()
		dataDir = filepath.Join(cfgDir, "KubeLamp")
		migrateLegacyData(filepath.Join(cfgDir, "KubeDeck"), dataDir)
	}

	// kubeconfigs come from each user, in the browser. -kubeconfig is only a starting
	// point for the admin. Nothing is picked up from ~/.kube or $KUBECONFIG.
	paths := splitPaths(*kubeconfig)

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("bad -addr: %v", err)
	}
	local := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if *token != "" || *tokenFile != "" {
		log.Printf("warning: -token / -token-file (KUBELAMP_TOKEN, KUBELAMP_TOKEN_FILE) are no longer used: sign in with a user account")
	}

	// in-cluster: use the pod's service account (Deployment of KubeLamp inside the cluster)
	var ic *resolvedContext
	if *inCluster != "false" {
		var err error
		if ic, err = inClusterContext(*clusterName); err != nil {
			if *inCluster == "true" {
				log.Fatalf("in-cluster: %v", err)
			}
			ic = nil
		}
	}

	// Already running? Same version: just open it (double-clicking twice is common).
	// Older version (e.g. after an upgrade): ask it to quit and take over the port.
	if local {
		if v, ok := existingInstance(*addr); ok {
			if v == version || !replaceOlder(*addr) {
				if !*noBrowser {
					_ = openBrowser("http://" + *addr + "/")
				}
				return
			}
			log.Printf("replaced running KubeLamp %s with %s", v, version)
		}
	}
	// macOS .app: hand off to a background process so the launcher can exit.
	if detachFromLauncher(args) {
		return
	}
	fixPath()

	users, err := openUserStore(dataDir, *adminUser, *adminPwd, ic, paths)
	if err != nil {
		log.Fatalf("users: %v", err)
	}
	srv := &Server{reg: &Registry{}, users: users, dataDir: dataDir, localOnly: local, readOnly: *readOnly, allowShell: *allowShell,
		ftMaxDown: int64(*maxDown) << 20, ftMaxUp: int64(*maxUp) << 20, ftRate: int64(*xferRate) << 20}
	srv.quit = func() {
		log.Println("quit requested from the UI")
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v", *addr, err)
	}
	shown := strings.Replace(ln.Addr().String(), "0.0.0.0", "localhost", 1)
	shown = strings.Replace(shown, "[::]", "localhost", 1)
	u := "http://" + shown + "/"
	mode := "read-write"
	if *readOnly {
		mode = "read-only"
	}
	fmt.Printf("KubeLamp %s\n  users:      %s\n  open:       %s  (sign in; each user adds their own kubeconfigs)\n  (%s; press Ctrl+C to stop)\n",
		version, filepath.Join(dataDir, "users.json"), u, mode)
	if !*noBrowser {
		go func() { time.Sleep(300 * time.Millisecond); _ = openBrowser(u) }()
	}
	hs := &http.Server{Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.Serve(ln))
}

func existingInstance(addr string) (string, bool) {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil || v.Version == "" {
		return "", false
	}
	return v.Version, true
}

// replaceOlder asks a running KubeLamp to quit and waits for the port to free up.
func replaceOlder(addr string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/quit", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-KubeLamp-Replace", version)
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		if ln, err := net.Listen("tcp", addr); err == nil {
			ln.Close()
			return true
		}
	}
	return false
}

// migrateLegacyData copies settings from the pre-rename "KubeDeck" folder the
// first time KubeLamp starts, so saved kubeconfig paths and per-cluster access
// settings survive the rename. The old folder is left untouched.
func migrateLegacyData(oldDir, newDir string) {
	if _, err := os.Stat(newDir); err == nil {
		return
	}
	entries, err := os.ReadDir(oldDir)
	if err != nil {
		return
	}
	if os.MkdirAll(newDir, 0o700) != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(oldDir, e.Name())); err == nil {
			_ = os.WriteFile(filepath.Join(newDir, e.Name()), b, 0o600)
		}
	}
}
