package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogLine is one line from one container. Short JSON keys keep the stream small.
type LogLine struct {
	Pod       string `json:"p"`
	Container string `json:"c"`
	TS        string `json:"t,omitempty"`
	Line      string `json:"l"`
	ts        time.Time
}

type logOpts struct {
	ns, container string
	tail          int
	sinceSec      int
	previous      bool
}

const maxLineLen = 16 << 10

// streamContainerLogs reads one container's log, calling emit per line until EOF,
// error, or ctx cancellation.
func streamContainerLogs(ctx context.Context, cl *Client, o logOpts, pod, container string,
	follow bool, tail int, since *time.Time, emit func(LogLine)) error {
	q := url.Values{"container": {container}, "timestamps": {"true"}}
	if follow {
		q.Set("follow", "true")
	}
	if o.previous {
		q.Set("previous", "true")
	}
	if since != nil {
		q.Set("sinceTime", since.UTC().Format(time.RFC3339))
	} else {
		if tail >= 0 {
			q.Set("tailLines", strconv.Itoa(tail))
		}
		if o.sinceSec > 0 {
			q.Set("sinceSeconds", strconv.Itoa(o.sinceSec))
		}
	}
	resp, err := cl.do(ctx, "/api/v1/namespaces/"+url.PathEscape(o.ns)+"/pods/"+url.PathEscape(pod)+"/log", q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			ll := LogLine{Pod: pod, Container: container}
			if sp := strings.IndexByte(line, ' '); sp > 0 && sp < 40 {
				if t, perr := time.Parse(time.RFC3339Nano, line[:sp]); perr == nil {
					ll.ts, ll.TS, line = t, line[:sp], line[sp+1:]
				}
			}
			if len(line) > maxLineLen {
				line = line[:maxLineLen] + " …[truncated]"
			}
			ll.Line = line
			emit(ll)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

type podRef struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Node       string   `json:"node"`
	Containers []string `json:"containers"`
	Restarts   int      `json:"restarts"`
}

// resolveLogTarget turns a workload (or a raw selector) into a label selector string.
func resolveLogTarget(ctx context.Context, c *Cluster, q url.Values) (ns, selector string, err error) {
	ns = q.Get("ns")
	if ns == "" {
		return "", "", errors.New("namespace is required")
	}
	if pod := strings.TrimSpace(q.Get("pod")); pod != "" {
		return ns, "metadata.name=" + pod, nil // single pod (field selector)
	}
	if raw := strings.TrimSpace(q.Get("selector")); raw != "" {
		return ns, raw, nil
	}
	kind, name := q.Get("kind"), q.Get("name")
	if kind == "" {
		kind = "Deployment"
	}
	if name == "" {
		return "", "", errors.New("pick a deployment or enter a label selector")
	}
	if s := c.Snap(); s != nil {
		if w := findWorkload(s, kind, ns, name); w != nil {
			return ns, w.sel.String(), nil
		}
	}
	// not in snapshot yet (just created?) — ask the API directly
	path := map[string]string{"Deployment": "deployments", "StatefulSet": "statefulsets", "DaemonSet": "daemonsets"}[kind]
	if path == "" {
		return "", "", fmt.Errorf("unsupported kind %q", kind)
	}
	var obj struct {
		Spec struct {
			Selector LabelSelector `json:"selector"`
		} `json:"spec"`
	}
	if err := c.client.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(ns)+"/"+path+"/"+url.PathEscape(name), nil, &obj); err != nil {
		return "", "", err
	}
	return ns, obj.Spec.Selector.String(), nil
}

func listPodsBySelector(ctx context.Context, cl *Client, ns, sel string) ([]Pod, error) {
	q := url.Values{"labelSelector": {sel}}
	if f, ok := strings.CutPrefix(sel, "metadata.name="); ok {
		q = url.Values{"fieldSelector": {"metadata.name=" + f}}
	}
	pods, err := list[Pod](ctx, cl, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", q)
	if err != nil {
		return nil, err
	}
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Metadata.CreationTimestamp.Before(pods[j].Metadata.CreationTimestamp)
	})
	return pods, nil
}

func podRefs(pods []Pod, onlyContainer string) []podRef {
	out := make([]podRef, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		r := podRef{Name: p.Metadata.Name, Status: podStatus(p), Node: p.Spec.NodeName, Restarts: podRestarts(p)}
		for _, c := range p.Spec.Containers {
			if onlyContainer == "" || onlyContainer == c.Name {
				r.Containers = append(r.Containers, c.Name)
			}
		}
		out = append(out, r)
	}
	return out
}

func parseLogOpts(q url.Values, ns string) logOpts {
	o := logOpts{ns: ns, container: q.Get("container"), tail: 200, previous: q.Get("previous") == "1"}
	if v, err := strconv.Atoi(q.Get("tail")); err == nil {
		o.tail = v
	}
	if o.tail > 20000 {
		o.tail = 20000
	}
	if v, err := strconv.Atoi(q.Get("since")); err == nil && v > 0 {
		o.sinceSec = v
	}
	return o
}

// collectInitial fetches the tail of every container concurrently and returns the
// lines merged in timestamp order, plus the newest timestamp seen per stream.
func collectInitial(ctx context.Context, cl *Client, o logOpts, pods []podRef) ([]LogLine, map[string]time.Time, []string) {
	var (
		mu    sync.Mutex
		lines []LogLine
		last  = map[string]time.Time{}
		warns []string
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 16) // be gentle with the API server
	)
	for _, p := range pods {
		for _, ct := range p.Containers {
			wg.Add(1)
			go func(pod, ct string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				var local []LogLine
				err := streamContainerLogs(ctx, cl, o, pod, ct, false, o.tail, nil, func(l LogLine) { local = append(local, l) })
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					warns = append(warns, pod+"/"+ct+": "+friendlyErr(err))
				}
				lines = append(lines, local...)
				if n := len(local); n > 0 {
					last[pod+"/"+ct] = local[n-1].ts
				}
			}(p.Name, ct)
		}
	}
	wg.Wait()
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].ts.Before(lines[j].ts) })
	return lines, last, warns
}

type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

func (s *sseWriter) send(event string, v any) error {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

func (s *sseWriter) ping() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.WriteString(s.w, ": ping\n\n"); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// handleLogs streams merged logs of every pod matching a deployment's selector.
// Events: "meta" (selector + pods), "lines" (batched), "warn", "done".
func (srv *Server) handleLogs(w http.ResponseWriter, r *http.Request, c *Cluster) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	ns, sel, err := resolveLogTarget(ctx, c, q)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	o := parseLogOpts(q, ns)
	follow := q.Get("follow") == "1"
	pods, err := listPodsBySelector(ctx, c.client, ns, sel)
	if err != nil {
		writeErr(w, 502, errors.New(friendlyErr(err)))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sw := &sseWriter{w: w, f: fl}
	refs := podRefs(pods, o.container)
	if sw.send("meta", map[string]any{"selector": sel, "namespace": ns, "pods": refs}) != nil {
		return
	}

	initial, last, warns := collectInitial(ctx, c.client, o, refs)
	for _, wmsg := range warns {
		sw.send("warn", map[string]string{"message": wmsg})
	}
	for i := 0; i < len(initial); i += 500 {
		if sw.send("lines", initial[i:min(i+500, len(initial))]) != nil {
			return
		}
	}
	if !follow || o.previous {
		sw.send("done", map[string]int{"lines": len(initial)})
		return
	}

	// ---- live phase
	lines := make(chan LogLine, 4096)
	var mu sync.Mutex
	running := map[string]bool{}
	ended := map[string]bool{}
	lastSeen := map[string]time.Time{}
	for k, t := range last {
		lastSeen[k] = t
	}
	known := map[string]bool{}
	for _, p := range refs {
		known[p.Name] = true
	}
	startStream := func(pod, ct string, since *time.Time, fresh bool) {
		key := pod + "/" + ct
		mu.Lock()
		if running[key] {
			mu.Unlock()
			return
		}
		running[key] = true
		if t, ok := lastSeen[key]; ok && !fresh && t.After(time.Time{}) {
			since = &t // resume exactly after the last line we delivered
		}
		mu.Unlock()
		go func() {
			defer func() { mu.Lock(); delete(running, key); mu.Unlock() }()
			tail := 0
			if fresh {
				tail = o.tail // a pod that appeared during the session: show its start
			}
			var after time.Time
			if since != nil {
				after = *since
			}
			err := streamContainerLogs(ctx, c.client, o, pod, ct, true, tail, since, func(l LogLine) {
				if !after.IsZero() && !l.ts.After(after) {
					return // sinceTime has 1 s resolution: skip lines we already sent
				}
				mu.Lock()
				lastSeen[key] = l.ts
				delete(ended, key)
				mu.Unlock()
				select {
				case lines <- l:
				case <-ctx.Done():
				}
			})
			mu.Lock()
			already := ended[key]
			ended[key] = true
			mu.Unlock()
			if ctx.Err() == nil && !already {
				msg := "stream ended (container stopped or pod deleted)"
				if err != nil {
					msg = friendlyErr(err)
				}
				sw.send("ended", map[string]string{"pod": pod, "container": ct, "message": msg})
			}
		}()
	}
	for _, p := range refs {
		for _, ct := range p.Containers {
			if t, ok := last[p.Name+"/"+ct]; ok {
				t := t
				startStream(p.Name, ct, &t, false)
			} else {
				now := time.Now().Add(-2 * time.Second)
				startStream(p.Name, ct, &now, false)
			}
		}
	}

	rescan := time.NewTicker(5 * time.Second)
	defer rescan.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	batch := make([]LogLine, 0, 256)
	flushT := time.NewTicker(150 * time.Millisecond)
	defer flushT.Stop()
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		err := sw.send("lines", batch)
		batch = batch[:0]
		return err == nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-lines:
			batch = append(batch, l)
			if len(batch) >= 500 && !flush() {
				return
			}
		case <-flushT.C:
			if !flush() {
				return
			}
		case <-heartbeat.C:
			if sw.ping() != nil {
				return
			}
		case <-rescan.C:
			// pick up pods created by rollouts / scaling while we watch
			lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			cur, err := listPodsBySelector(lctx, c.client, ns, sel)
			cancel()
			if err != nil {
				continue
			}
			refs := podRefs(cur, o.container)
			changed := len(refs) != len(known)
			for _, p := range refs {
				fresh := !known[p.Name]
				if fresh {
					changed = true
					known[p.Name] = true
				}
				for _, ct := range p.Containers {
					if fresh {
						startStream(p.Name, ct, nil, true)
					} else if p.Status == "Running" {
						// restart streams that ended (container restarted, or wasn't running yet)
						now := time.Now().Add(-6 * time.Second)
						startStream(p.Name, ct, &now, false)
					}
				}
			}
			if changed {
				cur := map[string]bool{}
				for _, p := range refs {
					cur[p.Name] = true
				}
				known = cur
				sw.send("meta", map[string]any{"selector": sel, "namespace": ns, "pods": refs})
			}
		}
	}
}

// handleLogDownload returns a plain-text merged log for the same query.
func (srv *Server) handleLogDownload(w http.ResponseWriter, r *http.Request, c *Cluster) {
	ctx := r.Context()
	q := r.URL.Query()
	ns, sel, err := resolveLogTarget(ctx, c, q)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	o := parseLogOpts(q, ns)
	pods, err := listPodsBySelector(ctx, c.client, ns, sel)
	if err != nil {
		writeErr(w, 502, errors.New(friendlyErr(err)))
		return
	}
	lines, _, warns := collectInitial(ctx, c.client, o, podRefs(pods, o.container))
	name := q.Get("name")
	if name == "" {
		name = "selector"
	}
	fname := fmt.Sprintf("%s-%s-%s.log", ns, name, time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	bw := bufio.NewWriterSize(w, 64<<10)
	fmt.Fprintf(bw, "# %s  namespace=%s  selector=%s  pods=%d\n", c.Info.Name, ns, sel, len(pods))
	for _, wmsg := range warns {
		fmt.Fprintf(bw, "# warning: %s\n", wmsg)
	}
	for _, l := range lines {
		fmt.Fprintf(bw, "%s [%s/%s] %s\n", l.TS, l.Pod, l.Container, l.Line)
	}
	bw.Flush()
}
