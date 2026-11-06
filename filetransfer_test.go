package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runSh(t *testing.T, script string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "kl"}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	_ = cmd.Run()
	return out.String()
}

func TestFtListAndStatScripts(t *testing.T) {
	d := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(d, "sub dir"), 0o755))
	must(os.WriteFile(filepath.Join(d, "b.txt"), []byte("12345"), 0o644))
	must(os.WriteFile(filepath.Join(d, ".hidden"), nil, 0o600))
	must(os.WriteFile(filepath.Join(d, "we|ird"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(d, "-dash"), []byte("x"), 0o644))
	must(os.Symlink("sub dir", filepath.Join(d, "link")))

	entries, more, msg := parseFtList(runSh(t, ftListScript, nil, d))
	if msg != "" || more {
		t.Fatalf("msg=%q more=%v", msg, more)
	}
	names := []string{}
	byName := map[string]ftEntry{}
	for _, e := range entries {
		names = append(names, e.Name)
		byName[e.Name] = e
	}
	if len(entries) != 6 {
		t.Fatalf("entries: %v", names)
	}
	if !entries[0].Dir || !entries[1].Dir { // directories first (the link to a dir counts)
		t.Errorf("dirs not first: %v", names)
	}
	if e := byName["link"]; e.Kind != "l" || !e.Dir {
		t.Errorf("symlink to a dir: %+v", e)
	}
	if e := byName["b.txt"]; e.Size != 5 || !e.File || e.MTime == 0 || e.Mode != "644" {
		t.Errorf("b.txt: %+v", e)
	}
	if _, ok := byName["we|ird"]; !ok {
		t.Errorf("name containing a pipe lost: %v", names)
	}
	if _, ok := byName["-dash"]; !ok {
		t.Errorf("name starting with a dash lost: %v", names)
	}
	if _, _, msg := parseFtList(runSh(t, ftListScript, nil, filepath.Join(d, "nope"))); msg == "" {
		t.Error("missing dir should report an error")
	}

	st := parseFtStat(runSh(t, ftStatScript, nil, filepath.Join(d, "b.txt"), "no"))
	if st.Missing || !st.File || st.Size != 5 || !st.Readable || !st.Tools["head"] {
		t.Errorf("stat file: %+v", st)
	}
	st = parseFtStat(runSh(t, ftStatScript, nil, d, "du"))
	if !st.Dir || !st.HasDu || !st.Tools["tar"] {
		t.Errorf("stat dir: %+v", st)
	}
	if st = parseFtStat(runSh(t, ftStatScript, nil, filepath.Join(d, "gone"), "no")); !st.Missing {
		t.Errorf("stat missing: %+v", st)
	}
}

func TestFtTransferScripts(t *testing.T) {
	d := t.TempDir()
	data := []byte(strings.Repeat("0123456789", 1000))
	f := filepath.Join(d, "f.log")
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// exact-length reads: the file may grow while we download, so we cap at the size we measured
	if got := runSh(t, ftCatScript, nil, "1000", f); got != string(data[:1000]) {
		t.Errorf("cat: %d bytes", len(got))
	}
	if got := runSh(t, ftTailScript, nil, "20", f); got != string(data[len(data)-20:]) {
		t.Errorf("tail: %q", got)
	}
	// folder as tar (and gz)
	sub := filepath.Join(d, "pkg")
	_ = os.MkdirAll(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "a"), []byte("A"), 0o644)
	if got := runSh(t, ftTarScript, nil, sub, "tar"); !strings.Contains(got, "pkg/a") {
		t.Errorf("tar output has no pkg/a")
	}
	if got := runSh(t, ftTarScript, nil, sub, "gz"); len(got) < 2 || got[0] != 0x1f || got[1] != 0x8b {
		t.Errorf("gz output is not gzip")
	}

	// upload: preflight, write part file, commit, replace
	pre := ftFacts(runSh(t, ftPreScript, nil, d, "new.bin"))
	if pre["w"][0] != "1" || pre["exists"][0] != "0" || len(pre["df"]) == 0 {
		t.Errorf("preflight: %v", pre)
	}
	if pre = ftFacts(runSh(t, ftPreScript, nil, d, "f.log")); pre["exists"][0] != "1" {
		t.Errorf("exists not detected: %v", pre)
	}
	part := filepath.Join(d, ".new.bin.kl-part-1")
	w := ftFacts(runSh(t, ftWriteScript, []byte("hello world and more"), part, "11"))
	if w["size"][0] != "11" || len(w["sha"]) == 0 {
		t.Errorf("write facts: %v", w) // only the first 11 bytes are taken
	}
	if b, _ := os.ReadFile(part); string(b) != "hello world" {
		t.Errorf("part content %q", b)
	}
	runSh(t, ftCommitScript, nil, "keep", part, filepath.Join(d, "new.bin"))
	if b, _ := os.ReadFile(filepath.Join(d, "new.bin")); string(b) != "hello world" {
		t.Errorf("commit: %q", b)
	}
	if _, err := os.Stat(part); err == nil {
		t.Error("part file still there after commit")
	}
	_ = os.WriteFile(part, []byte("x"), 0o644)
	runSh(t, ftCommitScript, nil, "drop", part, filepath.Join(d, "new.bin"))
	if _, err := os.Stat(part); err == nil {
		t.Error("discard did not remove the part file")
	}
	if b, _ := os.ReadFile(filepath.Join(d, "new.bin")); string(b) != "hello world" {
		t.Error("discard touched the target")
	}
}

func ftPod(name string, mounts map[string][]VolumeMount, vols []Volume) Pod {
	var p Pod
	p.Metadata.Name, p.Metadata.Namespace = name, "ns"
	p.Status.Phase = "Running"
	p.Spec.Volumes = vols
	for cn, vm := range mounts {
		p.Spec.Containers = append(p.Spec.Containers, Container{Name: cn, VolumeMounts: vm})
		var cs ContainerStatus
		cs.Name = cn
		cs.State.Running = &struct {
			StartedAt time.Time `json:"startedAt"`
		}{}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, cs)
	}
	return p
}

func pvcVol(name, claim string) Volume {
	v := Volume{Name: name}
	v.PVC = &struct {
		ClaimName string `json:"claimName"`
	}{claim}
	return v
}

func TestFtMountsAndAlternatives(t *testing.T) {
	cm := Volume{Name: "cfg", ConfigMap: &struct{}{}}
	ed := Volume{Name: "tmp", EmptyDir: &struct {
		Medium string `json:"medium,omitempty"`
	}{}}
	app := ftPod("app-1", map[string][]VolumeMount{"php": {
		{Name: "data", MountPath: "/var/www/storage/app", SubPath: "public"},
		{Name: "cfg", MountPath: "/etc/app"},
		{Name: "tmp", MountPath: "/tmp"},
	}}, []Volume{pvcVol("data", "uploads"), cm, ed})
	ncdu := ftPod("ncdu", map[string][]VolumeMount{"ncdu": {{Name: "u", MountPath: "/data"}}}, []Volume{pvcVol("u", "uploads")})
	other := ftPod("other", map[string][]VolumeMount{"x": {{Name: "u", MountPath: "/mnt/x", SubPath: "private"}}}, []Volume{pvcVol("u", "uploads")})
	ro := ftPod("ro", map[string][]VolumeMount{"x": {{Name: "u", MountPath: "/ro", ReadOnly: true}}}, []Volume{pvcVol("u", "uploads")})
	unrelated := ftPod("unrelated", map[string][]VolumeMount{"x": {{Name: "u", MountPath: "/d"}}}, []Volume{pvcVol("u", "somethingelse")})
	pods := []Pod{app, ncdu, other, ro, unrelated}

	ms := containerMounts(&app, &app.Spec.Containers[0])
	if m := mountFor(ms, "/var/www/storage/app/images/a.png"); m == nil || m.Kind != "pvc" || m.Claim != "uploads" || m.SubPath != "public" {
		t.Errorf("pvc mount: %+v", m)
	}
	if m := mountFor(ms, "/etc/app/x.conf"); m == nil || !m.ReadOnly || m.Kind != "configMap" {
		t.Errorf("configMap should be read-only: %+v", m)
	}
	if m := mountFor(ms, "/tmp/x"); m == nil || m.Kind != "emptyDir" || m.ReadOnly {
		t.Errorf("emptyDir: %+v", m)
	}
	if m := mountFor(ms, "/var/www/storage/logs"); m != nil {
		t.Errorf("/var/www/storage/logs is on the container layer, got %+v", m)
	}
	if m := mountFor(ms, "/etc/application"); m != nil {
		t.Errorf("prefix match must respect path boundaries, got %+v", m)
	}

	// /var/www/storage/app/images → claim path /public/images → ncdu sees /data/public/images;
	// "other" only mounts the private/ subpath, "ro" is read-only, "unrelated" is another claim
	_, alts := ftAlternatives(pods, "app-1", "php", "/var/www/storage/app/images")
	if len(alts) != 1 || alts[0].Pod != "ncdu" || alts[0].Path != "/data/public/images" {
		t.Fatalf("alternatives: %+v", alts)
	}
	// the claim root itself
	_, alts = ftAlternatives(pods, "ncdu", "ncdu", "/data/private/docs")
	// app-1 mounts only public/, ro is read-only, unrelated is another claim: only "other" qualifies
	if len(alts) != 1 || alts[0].Pod != "other" || alts[0].Path != "/mnt/x/docs" {
		t.Fatalf("from ncdu (subPath translation): %+v", alts)
	}
	if _, alts := ftAlternatives(pods, "app-1", "php", "/tmp"); len(alts) != 0 {
		t.Errorf("emptyDir is not shared: %+v", alts)
	}
}

func TestCleanAbs(t *testing.T) {
	for in, want := range map[string]string{"/a/b/../c": "/a/c", "/": "/", "/a//b/": "/a/b"} {
		if got, err := cleanAbs(in); err != nil || got != want {
			t.Errorf("cleanAbs(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "rel/path", "../x", "/a\x00b"} {
		if _, err := cleanAbs(bad); err == nil {
			t.Errorf("cleanAbs(%q) should fail", bad)
		}
	}
}
