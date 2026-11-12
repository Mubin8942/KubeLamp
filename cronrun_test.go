package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManualJobName(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 43, 7, 0, time.FixedZone("BDT", 6*3600))
	if got := manualJobName("info360-scheduler-job", at); got != "info360-scheduler-job-manual-20261112-034307" {
		t.Errorf("got %s (time must be UTC)", got)
	}
	long := strings.Repeat("a", 40) + "-" + strings.Repeat("b", 11) // 52 chars
	got := manualJobName(long, at)
	dns := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	if len(got) > maxJobName || !strings.HasSuffix(got, "-manual-20261112-034307") || !dns.MatchString(got) || strings.Contains(got, "--") {
		t.Errorf("long name: %q (%d)", got, len(got))
	}
}

func TestJobFromCronJob(t *testing.T) {
	var cj cronJobObj
	_ = json.Unmarshal([]byte(`{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"nightly","namespace":"ops","uid":"u-1"},
	  "spec":{"schedule":"* * * * *","jobTemplate":{"metadata":{"labels":{"app":"nightly"},"annotations":{"team":"ops"}},
	  "spec":{"backoffLimit":2,"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"busybox","args":["echo","hi"]}]}}}}}}`), &cj)
	b, err := jobFromCronJob(cj, "nightly-manual-x")
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		APIVersion, Kind string
		Metadata         struct {
			Name, Namespace string
			Labels          map[string]string
			Annotations     map[string]string
			OwnerReferences []struct {
				Kind, Name, UID string
				Controller      bool
			}
		}
		Spec struct {
			BackoffLimit int `json:"backoffLimit"`
			Template     struct {
				Spec struct {
					Containers []struct{ Image string }
				}
			}
		}
	}
	_ = json.Unmarshal(b, &job)
	m := job.Metadata
	if job.Kind != "Job" || job.APIVersion != "batch/v1" || m.Name != "nightly-manual-x" || m.Namespace != "ops" || m.Labels["app"] != "nightly" ||
		m.Annotations["team"] != "ops" || m.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" {
		t.Fatalf("metadata: %+v", m)
	}
	if len(m.OwnerReferences) != 1 || m.OwnerReferences[0].Kind != "CronJob" || m.OwnerReferences[0].UID != "u-1" || !m.OwnerReferences[0].Controller {
		t.Fatalf("owner: %+v", m.OwnerReferences)
	}
	if job.Spec.BackoffLimit != 2 || job.Spec.Template.Spec.Containers[0].Image != "busybox" {
		t.Fatalf("spec not copied: %s", b)
	}
	if _, err := jobFromCronJob(cronJobObj{}, "x"); err == nil {
		t.Error("empty template accepted")
	}
}

// fake API: a CronJob "nightly" in "ops"; POST /jobs is recorded (or forbidden)
type cronAPI struct {
	mu        sync.Mutex
	created   []map[string]any
	forbidden bool
}

func (f *cronAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/v1":
		json.NewEncoder(w).Encode(map[string]any{"groupVersion": "v1", "resources": []any{}})
	case p == "/apis":
		json.NewEncoder(w).Encode(map[string]any{"groups": []any{map[string]any{"name": "batch", "preferredVersion": map[string]any{"groupVersion": "batch/v1", "version": "v1"}}}})
	case p == "/apis/batch/v1":
		json.NewEncoder(w).Encode(map[string]any{"groupVersion": "batch/v1", "resources": []any{
			map[string]any{"name": "cronjobs", "kind": "CronJob", "namespaced": true, "verbs": []string{"get", "list", "create"}},
			map[string]any{"name": "jobs", "kind": "Job", "namespaced": true, "verbs": []string{"get", "list", "create"}}}})
	case p == "/apis/batch/v1/namespaces/ops/cronjobs/nightly" && r.Method == "GET":
		io.WriteString(w, `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"nightly","namespace":"ops","uid":"u-1"},
		  "spec":{"jobTemplate":{"metadata":{"labels":{"app":"nightly"}},"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"busybox"}]}}}}}}`)
	case p == "/apis/batch/v1/namespaces/ops/jobs" && r.Method == "POST":
		if f.forbidden {
			w.WriteHeader(403)
			io.WriteString(w, `{"kind":"Status","code":403,"message":"jobs.batch is forbidden: User \"dev\" cannot create resource \"jobs\""}`)
			return
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.created = append(f.created, m)
		f.mu.Unlock()
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(m)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"kind":"Status","code":404,"message":"not found"}`)
	}
}

func TestRunCronJobHTTP(t *testing.T) {
	api := &cronAPI{}
	k8s := httptest.NewServer(api)
	defer k8s.Close()
	dir := t.TempDir()
	kc := filepath.Join(dir, "kc.yaml")
	os.WriteFile(kc, []byte("clusters: [{name: c, cluster: {server: "+k8s.URL+"}}]\nusers: [{name: u, user: {token: t}}]\ncontexts: [{name: k, context: {cluster: c, user: u}}]\n"), 0o600)
	reg := &Registry{}
	reg.Load([]string{kc}, nil)
	defer func() {
		for _, c := range reg.All() {
			c.Close()
		}
	}()
	srv := httptest.NewServer((&Server{reg: reg, dataDir: dir, localOnly: true}).routes())
	defer srv.Close()
	post := func(base string, body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+"/api/action", strings.NewReader(string(b)))
		req.Host = "127.0.0.1"
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	run := map[string]any{"ctx": "k", "group": "batch", "resource": "cronjobs", "ns": "ops", "name": "nightly", "action": "trigger"}

	code, m := post(srv.URL, run)
	job, _ := m["job"].(string)
	if code != 200 || !regexp.MustCompile(`^nightly-manual-\d{8}-\d{6}$`).MatchString(job) {
		t.Fatalf("run: %d %v", code, m)
	}
	if len(api.created) != 1 || api.created[0]["metadata"].(map[string]any)["name"] != job {
		t.Fatalf("created: %v", api.created)
	}
	// only CronJobs
	bad := map[string]any{"ctx": "k", "group": "batch", "resource": "jobs", "ns": "ops", "name": "nightly", "action": "trigger"}
	if code, _ := post(srv.URL, bad); code != 400 {
		t.Errorf("trigger on a Job: %d", code)
	}
	// missing CronJob
	miss := map[string]any{"ctx": "k", "group": "batch", "resource": "cronjobs", "ns": "ops", "name": "nope", "action": "trigger"}
	if code, _ := post(srv.URL, miss); code != 404 {
		t.Errorf("missing CronJob: %d", code)
	}
	// RBAC says no: the API's 403 reaches the user
	api.forbidden = true
	if code, m := post(srv.URL, run); code != 403 || !strings.Contains(m["error"].(string), "forbidden") {
		t.Errorf("forbidden: %d %v", code, m)
	}
	// read-only KubeLamp never creates anything
	api.forbidden = false
	ro := httptest.NewServer((&Server{reg: reg, dataDir: dir, localOnly: true, readOnly: true}).routes())
	defer ro.Close()
	if code, _ := post(ro.URL, run); code != 403 || len(api.created) != 1 {
		t.Errorf("read-only: %d, created %d", code, len(api.created))
	}
}
