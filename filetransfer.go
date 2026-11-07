package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kubelamp/internal/ws"
)

// Tools → Pod File Transfer: browse, download and upload files inside running containers
// through the pod exec API (no kubectl cp, no agent in the pod, nothing written to the pod
// except the uploaded file itself).
//
// Safety rules that shape the code below:
//   - downloads stream straight from `cat`/`tar` to the browser; nothing is staged in the pod
//   - every download is size-checked first and again while streaming (hard stop at the limit)
//   - uploads check the target volume (writable? enough free space? container layer?) first,
//     write to a hidden part file, verify size/sha256 and only then rename into place
//   - transfers are throttled and limited to 2 at a time, one per pod

const (
	ftDefMaxDown  = 256 << 20
	ftDefMaxUp    = 100 << 20
	ftCeilDown    = 2 << 30
	ftDefRate     = 20 << 20
	ftListMax     = 1500
	ftSpaceMargin = 64 << 20
)

func (s *Server) ftLimits() (down, up, rate int64) {
	down, up, rate = s.ftMaxDown, s.ftMaxUp, s.ftRate
	if down <= 0 {
		down = ftDefMaxDown
	}
	if down > ftCeilDown {
		down = ftCeilDown
	}
	if up <= 0 {
		up = ftDefMaxUp
	}
	if rate <= 0 {
		rate = ftDefRate
	}
	return
}

// ftExecSem caps the short helper execs (list/stat/preflight); ftSem caps whole transfers.
var (
	ftExecSem = make(chan struct{}, 4)
	ftSem     = make(chan struct{}, 2)
	ftBusy    sync.Map
)

func ftAcquire(key string) (func(), error) {
	if _, loaded := ftBusy.LoadOrStore(key, struct{}{}); loaded {
		return nil, errors.New("another transfer with this pod is still running; wait for it to finish")
	}
	select {
	case ftSem <- struct{}{}:
	default:
		ftBusy.Delete(key)
		return nil, errors.New("two transfers are already running; wait for one to finish")
	}
	return func() { <-ftSem; ftBusy.Delete(key) }, nil
}

// ---------------------------------------------------------------- pod-side scripts
//
// Plain POSIX sh (works with busybox/dash/bash). Paths and sizes always arrive as
// positional arguments, never spliced into the script text. The first comment line is a
// marker (the mock cluster uses it to recognise these scripts).

const ftListScript = `# kl-ft list
d=$1
echo "@@id|$(id 2>/dev/null)"
[ -d "$d" ] || { echo "@@err|not a directory (or it does not exist)"; exit 0; }
{ [ -r "$d" ] && [ -x "$d" ]; } || { echo "@@err|permission denied for the container user"; exit 0; }
cd "$d" || { echo "@@err|cannot enter the directory"; exit 0; }
n=0
ls -1A 2>/dev/null | while IFS= read -r f; do
  n=$((n+1)); [ "$n" -gt 1500 ] && { echo "@@more"; break; }
  if [ -L "./$f" ]; then k=l; elif [ -d "./$f" ]; then k=d; elif [ -f "./$f" ]; then k=f; else k=o; fi
  if [ -d "./$f" ]; then fl=d; elif [ -f "./$f" ]; then fl=f; else fl=o; fi
  m=$(stat -c '%s|%Y|%a|%U' "./$f" 2>/dev/null) || m='|||'
  echo "@@e|$k|$fl|$m|$f"
done
`

const ftStatScript = `# kl-ft stat
p=$1; want=$2
t=; for x in gzip tar nice head tail stat; do command -v $x >/dev/null 2>&1 && t="$t $x"; done
echo "@@tools|$t"
if [ -L "$p" ]; then k=l; elif [ -d "$p" ]; then k=d; elif [ -f "$p" ]; then k=f; elif [ -e "$p" ]; then k=o; else echo "@@k|missing"; exit 0; fi
if [ -d "$p" ]; then f=d; elif [ -f "$p" ]; then f=f; else f=o; fi
if [ -r "$p" ]; then r=1; else r=0; fi
s=
if [ "$f" = f ]; then s=$(stat -L -c %s "$p" 2>/dev/null) || s=$(wc -c < "$p" 2>/dev/null); fi
echo "@@k|$k|$f|$r|$(echo $s)"
if [ "$f" = d ] && [ "$want" = du ]; then echo "@@du|$(du -sk "$p" 2>/dev/null | cut -f1)"; fi
`

// download scripts: $N lowers the CPU priority of the reader when nice exists.
const (
	ftCatScript  = "# kl-ft cat\nN=; command -v nice >/dev/null 2>&1 && N='nice -n 10'; exec $N head -c \"$1\" < \"$2\"\n"
	ftTailScript = "# kl-ft tail\nN=; command -v nice >/dev/null 2>&1 && N='nice -n 10'; exec $N tail -c \"$1\" < \"$2\"\n"
	ftTarScript  = "# kl-ft tar\nN=; command -v nice >/dev/null 2>&1 && N='nice -n 10'\nd=${1%/*}; b=${1##*/}; [ -z \"$d\" ] && d=/\ncd \"$d\" || exit 2\nif [ \"$2\" = gz ]; then $N tar cf - \"$b\" | $N gzip -c; else exec $N tar cf - \"$b\"; fi\n"
)

const ftPreScript = `# kl-ft pre
d=$1; n=$2
[ -d "$d" ] || { echo "@@nodir|1"; exit 0; }
if [ -w "$d" ] && [ -x "$d" ]; then echo "@@w|1"; else echo "@@w|0"; fi
if [ -e "$d/$n" ] || [ -L "$d/$n" ]; then echo "@@exists|1"; else echo "@@exists|0"; fi
echo "@@df|$(df -P -k "$d" 2>/dev/null | tail -n 1)"
`

const ftWriteScript = `# kl-ft write
head -c "$2" > "$1" || { echo "@@werr|1"; exit 3; }
echo "@@size|$(wc -c < "$1" 2>/dev/null | tr -d ' ')"
if command -v sha256sum >/dev/null 2>&1; then echo "@@sha|$(sha256sum < "$1" 2>/dev/null | cut -d' ' -f1)"; fi
`

const ftCommitScript = `# kl-ft commit
if [ "$1" = keep ]; then mv -f "$2" "$3"; else rm -f "$2"; fi
`

// ---------------------------------------------------------------- exec helpers

func ftErr(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	if strings.Contains(m, "executable file not found") || strings.Contains(m, `"sh"`) {
		return errors.New("this container has no shell (distroless image): pick another container, or another pod that mounts the same volume")
	}
	return errors.New(friendlyErr(err))
}

func (c *Cluster) ftRun(ctx context.Context, ns, pod, ctr, script string, timeout time.Duration, args ...string) (string, error) {
	cmd := append([]string{"sh", "-c", script, "kl"}, args...)
	out, err := c.execWith(ctx, ftExecSem, ns, pod, ctr, cmd, timeout)
	return out, ftErr(err)
}

func ftFacts(out string) map[string][]string {
	m := map[string][]string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if !strings.HasPrefix(ln, "@@") {
			continue
		}
		f := strings.Split(ln[2:], "|")
		m[f[0]] = append(m[f[0]], strings.Join(f[1:], "|"))
	}
	return m
}

type streamRes struct {
	Exit   int
	Stderr string
}

// execStream runs a command and copies its stdout to w as it arrives (and stdin from r),
// without buffering the data. Uses the same WebSocket exec endpoint as the terminal.
func (c *Cluster) execStream(ctx context.Context, ns, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer) (streamRes, error) {
	var res streamRes
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "command": cmd}
	if stdin != nil {
		q.Set("stdin", "true")
	}
	if container != "" {
		q.Set("container", container)
	}
	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	rwc, proto, err := c.client.upgrade(dctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/exec", q,
		[]string{"v5.channel.k8s.io", "v4.channel.k8s.io", "channel.k8s.io"})
	dcancel()
	if err != nil {
		return res, ftErr(err)
	}
	conn := ws.NewClient(rwc)
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()

	if stdin != nil {
		go func() {
			buf := make([]byte, 1+32<<10)
			for {
				n, err := stdin.Read(buf[1:])
				if n > 0 {
					buf[0] = 0
					if conn.WriteMessage(ws.OpBinary, buf[:n+1]) != nil {
						return
					}
				}
				if err != nil {
					if proto == "v5.channel.k8s.io" {
						_ = conn.WriteMessage(ws.OpBinary, []byte{255, 0}) // close stdin
					}
					return
				}
			}
		}()
	}
	var errb bytes.Buffer
	for {
		op, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			return res, errors.New("the connection to the pod closed before the command finished")
		}
		if op != ws.OpBinary || len(msg) < 1 {
			continue
		}
		switch msg[0] {
		case 1:
			if len(msg) > 1 {
				if _, err := stdout.Write(msg[1:]); err != nil {
					return res, err
				}
			}
		case 2:
			if errb.Len() < 4096 {
				errb.Write(msg[1:])
			}
		case 3:
			res.Stderr = strings.TrimSpace(errb.String())
			var st struct {
				Status  string `json:"status"`
				Message string `json:"message"`
				Details struct {
					Causes []struct{ Reason, Message string } `json:"causes"`
				} `json:"details"`
			}
			_ = json.Unmarshal(msg[1:], &st)
			if st.Status == "Failure" {
				for _, cs := range st.Details.Causes {
					if cs.Reason == "ExitCode" {
						res.Exit, _ = strconv.Atoi(cs.Message)
						return res, nil
					}
				}
				return res, ftErr(errors.New(st.Message))
			}
			return res, nil
		}
	}
}

// ---------------------------------------------------------------- throttling

type ftPacer struct {
	rate  int64
	start time.Time
	n     int64
}

func newPacer(rate int64) *ftPacer { return &ftPacer{rate: rate, start: time.Now()} }

func (p *ftPacer) wait(n int) {
	p.n += int64(n)
	if d := time.Duration(float64(p.n)/float64(p.rate)*float64(time.Second)) - time.Since(p.start); d > 0 {
		time.Sleep(d)
	}
}

var errFtTooBig = errors.New("transfer limit reached")

// ftOut is the download sink: starts the HTTP response on the first byte (so an early
// failure can still be reported as JSON), enforces the byte cap and the speed limit.
type ftOut struct {
	w       http.ResponseWriter
	begin   func()
	started bool
	n, max  int64
	pace    *ftPacer
}

func (o *ftOut) Write(p []byte) (int, error) {
	if o.n+int64(len(p)) > o.max {
		return 0, errFtTooBig
	}
	if !o.started {
		o.started = true
		o.begin()
	}
	n, err := o.w.Write(p)
	o.n += int64(n)
	o.pace.wait(n)
	return n, err
}

type ftIn struct {
	r    io.Reader
	n    int64
	pace *ftPacer
	h    hash.Hash
}

func (i *ftIn) Read(p []byte) (int, error) {
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	n, err := i.r.Read(p)
	if n > 0 {
		i.n += int64(n)
		i.h.Write(p[:n])
		i.pace.wait(n)
	}
	return n, err
}

// ---------------------------------------------------------------- mounts

type FtMount struct {
	Path     string `json:"path"`
	Volume   string `json:"volume"`
	Kind     string `json:"kind"` // pvc | emptyDir | configMap | secret | hostPath | nfs | other
	Claim    string `json:"claim,omitempty"`
	SubPath  string `json:"subPath,omitempty"`
	ReadOnly bool   `json:"readOnly"`
	Detail   string `json:"detail,omitempty"`
}

func volKind(v Volume) (kind, detail, claim string) {
	switch {
	case v.PVC != nil:
		return "pvc", v.PVC.ClaimName, v.PVC.ClaimName
	case v.EmptyDir != nil:
		if v.EmptyDir.Medium == "Memory" {
			return "emptyDir", "memory-backed (counts against the pod's memory)", ""
		}
		return "emptyDir", "node disk (counts against ephemeral storage)", ""
	case v.ConfigMap != nil:
		return "configMap", "", ""
	case v.Secret != nil:
		return "secret", "", ""
	case v.Projected != nil:
		return "projected", "", ""
	case v.DownwardAPI != nil:
		return "downwardAPI", "", ""
	case v.Ephemeral != nil:
		return "pvc", "generic ephemeral volume", ""
	case v.HostPath != nil:
		return "hostPath", v.HostPath.Path, ""
	case v.NFS != nil:
		return "nfs", v.NFS.Server + ":" + v.NFS.Path, ""
	case v.CephFS != nil:
		return "cephfs", "", ""
	}
	return "other", "", ""
}

func ftVolumeReadOnly(kind string) bool {
	switch kind {
	case "configMap", "secret", "projected", "downwardAPI":
		return true
	}
	return false
}

func containerMounts(p *Pod, ct *Container) []FtMount {
	vols := map[string]Volume{}
	for _, v := range p.Spec.Volumes {
		vols[v.Name] = v
	}
	var out []FtMount
	for _, vm := range ct.VolumeMounts {
		v := vols[vm.Name]
		k, d, cl := volKind(v)
		out = append(out, FtMount{Path: path.Clean(vm.MountPath), Volume: vm.Name, Kind: k, Claim: cl, SubPath: vm.SubPath,
			ReadOnly: vm.ReadOnly || ftVolumeReadOnly(k), Detail: d})
	}
	return out
}

func pathUnder(p, base string) bool {
	return base == "/" || p == base || strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/")
}

// mountFor returns the longest mount path containing p, or nil when p is on the
// container's own writable layer.
func mountFor(ms []FtMount, p string) *FtMount {
	var best *FtMount
	for i := range ms {
		if pathUnder(p, ms[i].Path) && (best == nil || len(ms[i].Path) > len(best.Path)) {
			best = &ms[i]
		}
	}
	return best
}

type FtAlt struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Path      string `json:"path"`
	Same      bool   `json:"samePod"`
}

func podContainer(p *Pod, name string) *Container {
	for i := range p.Spec.Containers {
		if p.Spec.Containers[i].Name == name {
			return &p.Spec.Containers[i]
		}
	}
	return nil
}

func containerRunning(p *Pod, name string) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.State.Running != nil
		}
	}
	return false
}

// ftAlternatives lists other containers (typically an ncdu/maintenance pod) that mount the
// same PVC read-write and can reach the same files, with the path translated for them.
func ftAlternatives(pods []Pod, pod, container, p string) (*FtMount, []FtAlt) {
	var src *Pod
	for i := range pods {
		if pods[i].Metadata.Name == pod {
			src = &pods[i]
		}
	}
	if src == nil {
		return nil, nil
	}
	ct := podContainer(src, container)
	if ct == nil {
		return nil, nil
	}
	ms := containerMounts(src, ct)
	m := mountFor(ms, p)
	if m == nil || m.Kind != "pvc" || m.Claim == "" {
		return m, nil
	}
	claimPath := path.Clean("/" + path.Join(m.SubPath, strings.TrimPrefix(p, m.Path)))
	var out []FtAlt
	for i := range pods {
		p2 := &pods[i]
		if p2.Metadata.DeletionTimestamp != nil || p2.Status.Phase != "Running" || p2.Metadata.Namespace != src.Metadata.Namespace {
			continue
		}
		for j := range p2.Spec.Containers {
			c2 := &p2.Spec.Containers[j]
			if (p2.Metadata.Name == pod && c2.Name == container) || !containerRunning(p2, c2.Name) {
				continue
			}
			for _, m2 := range containerMounts(p2, c2) {
				if m2.Kind != "pvc" || m2.Claim != m.Claim || m2.ReadOnly {
					continue
				}
				base := "/"
				if m2.SubPath != "" {
					base = path.Clean("/" + m2.SubPath)
				}
				if !pathUnder(claimPath, base) {
					continue
				}
				rel := strings.TrimPrefix(strings.TrimPrefix(claimPath, strings.TrimSuffix(base, "/")), "/")
				out = append(out, FtAlt{p2.Metadata.Name, c2.Name, path.Join(m2.Path, rel), p2.Metadata.Name == pod})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Same && out[j].Same })
	return m, out
}

// ---------------------------------------------------------------- routes

func (s *Server) denyExecTool(w http.ResponseWriter) bool {
	if s.readOnly {
		writeErr(w, 403, errors.New("KubeLamp is running in read-only mode (-read-only): this tool runs commands in pods, so it is disabled"))
		return true
	}
	return false
}

func cleanAbs(p string) (string, error) {
	if p == "" || p[0] != '/' || strings.ContainsRune(p, 0) || len(p) > 4096 {
		return "", errors.New("the path must be absolute (start with /)")
	}
	return path.Clean(p), nil
}

type ftCtx struct {
	c         *Cluster
	pod       *Pod
	ns        string
	container string
	all       []Pod
}

// ftTarget resolves ?ctx&ns&pod&container to a running container.
func (s *Server) ftTarget(w http.ResponseWriter, r *http.Request) (*ftCtx, bool) {
	c, _, ok := s.cluster(w, r)
	if !ok {
		return nil, false
	}
	q := r.URL.Query()
	ns, name, ctr := q.Get("ns"), q.Get("pod"), q.Get("container")
	if ns == "" || name == "" {
		writeErr(w, 400, errors.New("namespace and pod are required"))
		return nil, false
	}
	pods, err := c.FreshPods(r.Context(), ns)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return nil, false
	}
	var pod *Pod
	for i := range pods {
		if pods[i].Metadata.Name == name {
			pod = &pods[i]
		}
	}
	if pod == nil {
		writeErr(w, 404, fmt.Errorf("pod %s/%s not found", ns, name))
		return nil, false
	}
	if ctr == "" && len(pod.Spec.Containers) > 0 {
		ctr = pod.Spec.Containers[0].Name
	}
	if podContainer(pod, ctr) == nil {
		writeErr(w, 404, fmt.Errorf("container %q not found in this pod", ctr))
		return nil, false
	}
	if pod.Status.Phase != "Running" || !containerRunning(pod, ctr) {
		writeErr(w, 409, fmt.Errorf("container %q is not running", ctr))
		return nil, false
	}
	return &ftCtx{c, pod, ns, ctr, pods}, true
}

func (t *ftCtx) key() string { return t.ns + "/" + t.pod.Metadata.Name }

type ftEntry struct {
	Name  string `json:"n"`
	Kind  string `json:"k"` // d dir, f file, l symlink, o other
	Dir   bool   `json:"dir"`
	File  bool   `json:"file"`
	Size  int64  `json:"s"`
	MTime int64  `json:"m"`
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"u,omitempty"`
}

func parseFtList(out string) (entries []ftEntry, more bool, errMsg string) {
	entries = []ftEntry{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		switch {
		case strings.HasPrefix(ln, "@@err|"):
			errMsg = ln[6:]
		case ln == "@@more":
			more = true
		case strings.HasPrefix(ln, "@@e|"):
			f := strings.SplitN(ln[4:], "|", 7)
			if len(f) < 7 || f[6] == "" {
				continue
			}
			e := ftEntry{Name: f[6], Kind: f[0], Dir: f[1] == "d", File: f[1] == "f", Mode: f[4], Owner: f[5]}
			e.Size, _ = strconv.ParseInt(f[2], 10, 64)
			e.MTime, _ = strconv.ParseInt(f[3], 10, 64)
			entries = append(entries, e)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return
}

type ftStat struct {
	Missing  bool
	Kind     string
	Dir      bool
	File     bool
	Readable bool
	Size     int64
	DuKB     int64
	HasDu    bool
	Tools    map[string]bool
}

func parseFtStat(out string) ftStat {
	f := ftFacts(out)
	st := ftStat{Tools: map[string]bool{}}
	if v := f["tools"]; len(v) > 0 {
		for _, t := range strings.Fields(v[0]) {
			st.Tools[t] = true
		}
	}
	if v := f["k"]; len(v) > 0 {
		p := strings.Split(v[0], "|")
		if p[0] == "missing" || len(p) < 4 {
			st.Missing = true
		} else {
			st.Kind, st.Dir, st.File, st.Readable = p[0], p[1] == "d", p[1] == "f", p[2] == "1"
			if len(p) > 3 {
				st.Size, _ = strconv.ParseInt(strings.TrimSpace(p[3]), 10, 64)
			}
		}
	}
	if v := f["du"]; len(v) > 0 {
		if n, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64); err == nil {
			st.DuKB, st.HasDu = n, true
		}
	}
	return st
}

func (s *Server) ftStat(ctx context.Context, t *ftCtx, p string, du bool) (ftStat, error) {
	want := "no"
	if du {
		want = "du"
	}
	to := 25 * time.Second
	out, err := t.c.ftRun(ctx, t.ns, t.pod.Metadata.Name, t.container, ftStatScript, to, p, want)
	if err != nil {
		return ftStat{}, err
	}
	return parseFtStat(out), nil
}

func humanMB(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (s *Server) ftRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools/files/info", func(w http.ResponseWriter, r *http.Request) {
		down, up, rate := s.ftLimits()
		writeJSON(w, 200, map[string]any{"maxDownload": down, "maxUpload": up, "rate": rate, "ceiling": int64(ftCeilDown), "readOnly": s.readOnly})
	})

	// containers of a pod with their mounts (so the UI can label every folder)
	mux.HandleFunc("GET /api/tools/files/pod", func(w http.ResponseWriter, r *http.Request) {
		if s.denyExecTool(w) {
			return
		}
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		pods, err := c.FreshPods(r.Context(), q.Get("ns"))
		if err != nil {
			writeErr(w, apiStatus(err), err)
			return
		}
		for i := range pods {
			p := &pods[i]
			if p.Metadata.Name != q.Get("pod") {
				continue
			}
			type cv struct {
				Name    string    `json:"name"`
				Running bool      `json:"running"`
				Mounts  []FtMount `json:"mounts"`
			}
			cs := []cv{}
			for j := range p.Spec.Containers {
				ct := &p.Spec.Containers[j]
				ms := containerMounts(p, ct)
				if ms == nil {
					ms = []FtMount{}
				}
				cs = append(cs, cv{ct.Name, containerRunning(p, ct.Name), ms})
			}
			writeJSON(w, 200, map[string]any{"pod": p.Metadata.Name, "phase": p.Status.Phase, "containers": cs})
			return
		}
		writeErr(w, 404, errors.New("pod not found"))
	})

	mux.HandleFunc("GET /api/tools/files/list", func(w http.ResponseWriter, r *http.Request) {
		if s.denyExecTool(w) {
			return
		}
		t, ok := s.ftTarget(w, r)
		if !ok {
			return
		}
		p, err := cleanAbs(r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		out, err := t.c.ftRun(r.Context(), t.ns, t.pod.Metadata.Name, t.container, ftListScript, 30*time.Second, p)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		entries, more, msg := parseFtList(out)
		user := ""
		if v := ftFacts(out)["id"]; len(v) > 0 {
			user = strings.TrimSpace(v[0])
		}
		if msg != "" {
			writeJSON(w, 200, map[string]any{"path": p, "error": msg, "entries": entries, "user": user})
			return
		}
		writeJSON(w, 200, map[string]any{"path": p, "entries": entries, "more": more, "max": ftListMax, "user": user})
	})

	// size check for the UI before it starts a download
	mux.HandleFunc("GET /api/tools/files/stat", func(w http.ResponseWriter, r *http.Request) {
		if s.denyExecTool(w) {
			return
		}
		t, ok := s.ftTarget(w, r)
		if !ok {
			return
		}
		p, err := cleanAbs(r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		st, err := s.ftStat(r.Context(), t, p, true)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		down, _, _ := s.ftLimits()
		resp := map[string]any{"path": p, "missing": st.Missing, "limit": down, "gzip": st.Tools["gzip"], "tar": st.Tools["tar"], "tail": st.Tools["tail"]}
		if !st.Missing {
			size := st.Size
			if st.Dir {
				size = st.DuKB << 10
			}
			resp["dir"], resp["file"], resp["readable"], resp["size"], resp["sizeKnown"] = st.Dir, st.File, st.Readable, size, st.File || st.HasDu
			ok, why := true, ""
			switch {
			case !st.Readable:
				ok, why = false, "the container user cannot read this"
			case !st.File && !st.Dir:
				ok, why = false, "not a regular file or folder"
			case st.Dir && !st.Tools["tar"]:
				ok, why = false, "folders need tar in the container"
			case st.Dir && !st.HasDu:
				ok, why = false, "could not measure the folder size"
			case size > down:
				ok, why = false, fmt.Sprintf("%s is over the %s download limit", humanMB(size), humanMB(down))
			}
			resp["ok"], resp["why"] = ok, why
		}
		writeJSON(w, 200, resp)
	})

	mux.HandleFunc("GET /api/tools/files/view", s.ftView)
	mux.HandleFunc("GET /api/tools/files/download", s.ftDownload)
	mux.HandleFunc("POST /api/tools/files/upload", s.ftUpload)

	// other containers that mount the same volume (e.g. an ncdu pod), with the path translated
	mux.HandleFunc("GET /api/tools/files/targets", func(w http.ResponseWriter, r *http.Request) {
		if s.denyExecTool(w) {
			return
		}
		c, _, ok := s.cluster(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		p, err := cleanAbs(q.Get("path"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		pods, err := c.FreshPods(r.Context(), q.Get("ns"))
		if err != nil {
			writeErr(w, apiStatus(err), err)
			return
		}
		m, alts := ftAlternatives(pods, q.Get("pod"), q.Get("container"), p)
		if alts == nil {
			alts = []FtAlt{}
		}
		writeJSON(w, 200, map[string]any{"mount": m, "alternatives": alts})
	})
}

func ftDispName(name string) string {
	return "attachment; filename=\"" + strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name) + "\"; filename*=UTF-8''" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

func (s *Server) ftDownload(w http.ResponseWriter, r *http.Request) {
	if s.denyExecTool(w) {
		return
	}
	t, ok := s.ftTarget(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p, err := cleanAbs(q.Get("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	down, _, rate := s.ftLimits()
	release, err := ftAcquire(t.key())
	if err != nil {
		writeErr(w, 429, err)
		return
	}
	defer release()

	st, err := s.ftStat(r.Context(), t, p, true)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	switch {
	case st.Missing:
		writeErr(w, 404, errors.New("no such file or folder"))
		return
	case !st.Readable:
		writeErr(w, 403, errors.New("the container user cannot read this"))
		return
	case !st.File && !st.Dir:
		writeErr(w, 400, errors.New("only regular files and folders can be downloaded"))
		return
	}
	name := path.Base(p)
	var script string
	var args []string
	var max int64 = down
	var length int64 = -1
	ctype := "application/octet-stream"
	pod := t.pod.Metadata.Name
	mode := "file"
	if st.Dir {
		if p == "/" {
			writeErr(w, 400, errors.New("downloading the whole filesystem is not allowed; pick a folder"))
			return
		}
		if !st.Tools["tar"] {
			writeErr(w, 501, errors.New("folders need tar in the container"))
			return
		}
		if !st.HasDu || st.DuKB<<10 > down {
			writeErr(w, 413, fmt.Errorf("the folder is %s, over the %s download limit", humanMB(st.DuKB<<10), humanMB(down)))
			return
		}
		gz := q.Get("gz") == "1" && st.Tools["gzip"]
		script, args, mode = ftTarScript, []string{p, "tar"}, "folder"
		name += ".tar"
		ctype = "application/x-tar"
		if gz {
			args[1] = "gz"
			name += ".gz"
			ctype = "application/gzip"
		}
		max = down + down/10 + 1<<20 // tar headers/padding
	} else {
		n := st.Size
		if tl := q.Get("tail"); tl != "" {
			mb, _ := strconv.Atoi(tl)
			if mb < 1 || !st.Tools["tail"] {
				writeErr(w, 400, errors.New("bad tail size, or tail is missing in the container"))
				return
			}
			if lim := int64(mb) << 20; lim < n {
				n = lim
			}
			script, mode = ftTailScript, "tail"
			name += ".last" + strconv.Itoa(mb) + "MiB"
		} else {
			script = ftCatScript
		}
		if n > down {
			writeErr(w, 413, fmt.Errorf("the file is %s, over the %s download limit; download the last part instead", humanMB(n), humanMB(down)))
			return
		}
		args = []string{strconv.FormatInt(n, 10), p}
		length, max = n, n
	}
	out := &ftOut{w: w, max: max, pace: newPacer(rate)}
	out.begin = func() {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", ftDispName(name))
		w.Header().Set("Cache-Control", "no-store")
		if length >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		}
		w.WriteHeader(200)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	cmd := append([]string{"sh", "-c", script, "kl"}, args...)
	res, err := t.c.execStream(ctx, t.ns, pod, t.container, cmd, nil, out)
	log.Printf("file transfer: download %s %s:%s (%s) %d bytes in %s err=%v", mode, t.key(), p, t.container, out.n, time.Since(start).Round(time.Millisecond), err)
	if err != nil {
		if !out.started {
			code := 502
			if errors.Is(err, errFtTooBig) {
				code = 413
			}
			writeErr(w, code, err)
			return
		}
		panic(http.ErrAbortHandler) // already streaming: cut the connection so the browser reports a failed download
	}
	if res.Exit != 0 && !out.started {
		msg := res.Stderr
		if msg == "" {
			msg = "the command in the pod failed (exit " + strconv.Itoa(res.Exit) + ")"
		}
		writeErr(w, 502, errors.New(msg))
		return
	}
	if length >= 0 && out.n != length {
		panic(http.ErrAbortHandler) // the file changed (rotated) while it was read
	}
	if !out.started { // empty file
		out.begin()
	}
}

func (s *Server) ftUpload(w http.ResponseWriter, r *http.Request) {
	if s.denyWrite(w) {
		return
	}
	t, ok := s.ftTarget(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	dir, err := cleanAbs(q.Get("dir"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	name := q.Get("name")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00\n") || len(name) > 255 {
		writeErr(w, 400, errors.New("invalid file name"))
		return
	}
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil || size < 0 || r.ContentLength != size {
		writeErr(w, 400, errors.New("size must match the request body length"))
		return
	}
	_, up, rate := s.ftLimits()
	if size > up {
		writeJSON(w, 413, map[string]any{"error": fmt.Sprintf("%s is over the %s upload limit", humanMB(size), humanMB(up)), "code": "toobig"})
		return
	}
	fail := func(code int, c, msg string, extra map[string]any) {
		m := map[string]any{"error": msg, "code": c}
		for k, v := range extra {
			m[k] = v
		}
		writeJSON(w, code, m)
	}
	ct := podContainer(t.pod, t.container)
	mount := mountFor(containerMounts(t.pod, ct), path.Join(dir, name))
	alts := func() map[string]any {
		_, a := ftAlternatives(t.all, t.pod.Metadata.Name, t.container, dir)
		if a == nil {
			a = []FtAlt{}
		}
		return map[string]any{"alternatives": a}
	}
	switch {
	case mount == nil && q.Get("allowLayer") != "1":
		fail(409, "layer", "that folder is not on a volume: the file would live in the container's own filesystem, count against ephemeral storage and disappear on restart", alts())
		return
	case mount != nil && mount.ReadOnly:
		fail(403, "readonly", fmt.Sprintf("%s is mounted read-only (%s)", mount.Path, mount.Kind), alts())
		return
	}
	release, err := ftAcquire(t.key())
	if err != nil {
		fail(429, "busy", err.Error(), nil)
		return
	}
	defer release()
	pod := t.pod.Metadata.Name

	out, err := t.c.ftRun(r.Context(), t.ns, pod, t.container, ftPreScript, 25*time.Second, dir, name)
	if err != nil {
		fail(502, "exec", err.Error(), nil)
		return
	}
	f := ftFacts(out)
	switch {
	case len(f["nodir"]) > 0:
		fail(404, "nodir", "the target folder does not exist in the container", nil)
		return
	case len(f["w"]) == 0 || f["w"][0] != "1":
		fail(403, "notwritable", "the container user cannot write to that folder (read-only or wrong owner). Another pod that mounts the same volume may be able to", alts())
		return
	case len(f["exists"]) > 0 && f["exists"][0] == "1" && q.Get("overwrite") != "1":
		fail(409, "exists", "a file with this name already exists", nil)
		return
	}
	if len(f["df"]) > 0 {
		if fl := strings.Fields(f["df"][0]); len(fl) >= 6 {
			if kb, err := strconv.ParseInt(fl[len(fl)-3], 10, 64); err == nil && kb<<10 < size+ftSpaceMargin {
				fail(507, "nospace", fmt.Sprintf("not enough free space on that volume: %s free, %s needed plus a %s safety margin", humanMB(kb<<10), humanMB(size), humanMB(ftSpaceMargin)), alts())
				return
			}
		}
	}

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	part := path.Join(dir, "."+name+".kl-part-"+hex.EncodeToString(rnd[:]))
	target := path.Join(dir, name)
	in := &ftIn{r: io.LimitReader(r.Body, size), pace: newPacer(rate), h: sha256.New()}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	var obuf bytes.Buffer
	start := time.Now()
	cmd := []string{"sh", "-c", ftWriteScript, "kl", part, strconv.FormatInt(size, 10)}
	res, werr := t.c.execStream(ctx, t.ns, pod, t.container, cmd, in, &obuf)
	discard := func() {
		dctx, dc := context.WithTimeout(context.Background(), 20*time.Second)
		defer dc()
		_, _ = t.c.ftRun(dctx, t.ns, pod, t.container, ftCommitScript, 20*time.Second, "drop", part, target)
	}
	if werr == nil && res.Exit != 0 {
		msg := res.Stderr
		if msg == "" {
			msg = "the write failed (disk full or no permission)"
		}
		werr = errors.New(msg)
	}
	if werr == nil && in.n != size {
		werr = fmt.Errorf("the upload was cut short (%d of %d bytes received)", in.n, size)
	}
	var got int64 = -1
	sum, hasSum := "", false
	if werr == nil {
		wf := ftFacts(obuf.String())
		if v := wf["size"]; len(v) > 0 {
			got, _ = strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
		}
		if v := wf["sha"]; len(v) > 0 && v[0] != "" {
			sum, hasSum = v[0], true
		}
		switch {
		case got != size:
			werr = fmt.Errorf("size mismatch after writing: %d bytes in the pod, expected %d", got, size)
		case hasSum && sum != hex.EncodeToString(in.h.Sum(nil)):
			werr = errors.New("checksum mismatch after writing; the file was discarded")
		}
	}
	if werr != nil {
		discard()
		log.Printf("file transfer: upload FAILED %s:%s (%s): %v", t.key(), target, t.container, werr)
		fail(502, "write", werr.Error(), nil)
		return
	}
	cout, cerr := t.c.ftRun(ctx, t.ns, pod, t.container, ftCommitScript, 20*time.Second, "keep", part, target)
	_ = cout
	if cerr != nil {
		discard()
		fail(502, "commit", "could not move the file into place: "+cerr.Error(), nil)
		return
	}
	verified := "size"
	if hasSum {
		verified = "sha256"
	}
	where := "container filesystem"
	if mount != nil {
		where = mount.Kind
		if mount.Claim != "" {
			where = "pvc " + mount.Claim
		}
	}
	log.Printf("file transfer: upload %s:%s (%s) %d bytes in %s, verified by %s", t.key(), target, t.container, size, time.Since(start).Round(time.Millisecond), verified)
	writeJSON(w, 200, map[string]any{"ok": true, "path": target, "size": size, "sha256": sum, "verified": verified, "volume": where})
}

const (
	ftViewMax = 512 << 10
	ftViewDef = 64 << 10
)

// hexDump renders the first bytes of a binary file like `xxd`.
func hexDump(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	var sb strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		fmt.Fprintf(&sb, "%08x  ", off)
		for i := off; i < off+16; i++ {
			if i < end {
				fmt.Fprintf(&sb, "%02x ", b[i])
			} else {
				sb.WriteString("   ")
			}
			if i == off+7 {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(" |")
		for _, c := range b[off:end] {
			if c >= 32 && c < 127 {
				sb.WriteByte(c)
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}

// ftView reads the start (or end) of a file for the File Explorer: at most 512 KiB, as text or,
// for binary files, a hex preview. Only regular files are read (never FIFOs or devices, which
// could block), and the container's own user permissions apply.
func (s *Server) ftView(w http.ResponseWriter, r *http.Request) {
	if s.denyExecTool(w) {
		return
	}
	t, ok := s.ftTarget(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p, err := cleanAbs(q.Get("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	n, _ := strconv.Atoi(q.Get("bytes"))
	if n <= 0 {
		n = ftViewDef
	}
	if n > ftViewMax {
		n = ftViewMax
	}
	mode := "head"
	if q.Get("mode") == "tail" {
		mode = "tail"
	}
	st, err := s.ftStat(r.Context(), t, p, false)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	switch {
	case st.Missing:
		writeErr(w, 404, errors.New("no such file"))
		return
	case st.Dir:
		writeErr(w, 400, errors.New("this is a folder"))
		return
	case !st.File:
		writeErr(w, 400, errors.New("only regular files can be read (not devices, sockets or pipes)"))
		return
	case !st.Readable:
		writeErr(w, 403, errors.New("permission denied: the container's user cannot read this file"))
		return
	case mode == "tail" && !st.Tools["tail"], !st.Tools["head"]:
		writeErr(w, 501, errors.New("head/tail are not available in this container"))
		return
	}
	script := ftCatScript
	if mode == "tail" {
		script = ftTailScript
	}
	out, err := t.c.ftRun(r.Context(), t.ns, t.pod.Metadata.Name, t.container, script, 30*time.Second, strconv.Itoa(n), p)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	b := []byte(out)
	resp := map[string]any{"path": p, "mode": mode, "bytes": n, "shown": len(b), "size": st.Size, "binary": false}
	// size 0 with content happens for /proc and /sys files: trust what we read
	trunc := len(b) >= n
	if st.Size > int64(len(b)) {
		trunc = true
	}
	resp["truncated"] = trunc
	if bytes.IndexByte(b, 0) >= 0 {
		resp["binary"], resp["hex"], resp["text"] = true, hexDump(b), ""
	} else {
		resp["text"] = strings.ToValidUTF8(string(b), "\uFFFD")
	}
	log.Printf("file explorer: read %s:%s (%s) %s %d bytes", t.key(), p, t.container, mode, len(b))
	writeJSON(w, 200, resp)
}
