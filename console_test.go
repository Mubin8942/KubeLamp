package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitLine(t *testing.T) {
	for in, want := range map[string][]string{
		`get pods -n kube-system`:              {"get", "pods", "-n", "kube-system"},
		`get pods -l 'app in (a, b)'`:          {"get", "pods", "-l", "app in (a, b)"},
		`exec -it web -- sh -c "echo $HOME"`:   {"exec", "-it", "web", "--", "sh", "-c", "echo $HOME"},
		`get po|grep api`:                      {"get", "po", "|", "grep", "api"},
		`annotate x a=b\ c`:                    {"annotate", "x", "a=b c"},
		`get pods; rm -rf / && echo $(whoami)`: {"get", "pods;", "rm", "-rf", "/", "&&", "echo", "$(whoami)"},
	} {
		got, err := splitLine(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitLine(%q) = %q %v", in, got, err)
		}
	}
	if _, err := splitLine(`get "pods`); err == nil {
		t.Error("unclosed quote accepted")
	}
}

func TestParseConsole(t *testing.T) {
	args, g, b, err := parseConsole("k get pods -A | grep -i API")
	if err != nil || b != "" || !reflect.DeepEqual(args, []string{"get", "pods", "-A"}) || g == nil || !g.keep("default  my-api-1  Running") || g.keep("NAME READY") {
		t.Fatalf("grep: %v %v %v", args, g, err)
	}
	if args, _, _, _ := parseConsole("get nodes"); !reflect.DeepEqual(args, []string{"get", "nodes"}) {
		t.Error("bare subcommand must work without kubectl")
	}
	if _, _, b, _ := parseConsole("help"); b != "help" {
		t.Error("help")
	}
	// a | after -- belongs to the container command
	if args, g, _, err := parseConsole(`exec web -- sh -c "ps | grep x"`); err != nil || g != nil || args[len(args)-1] != "ps | grep x" {
		t.Errorf("quoted pipe: %v %v", args, err)
	}
	for _, bad := range []string{"ls -la", "cat /data/users.json", "bash", "sh -c id", "get pods | sh", "get pods | grep", "get pods | grep a | grep b", "| grep x", "get pods | grep a b"} {
		if _, _, _, err := parseConsole(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, g, _, _ := parseConsole("get pods | grep -v Running"); g == nil || g.keep("x Running") || !g.keep("x Pending") {
		t.Error("grep -v")
	}
}

func TestCheckKubectlArgs(t *testing.T) {
	blocked := []string{
		"cp web:/etc/passwd /tmp/x", "cp /data/users.json web:/tmp", "plugin list", "proxy", "port-forward svc/x 8080:80",
		"edit deploy/x", "kustomize .", "alpha debug x",
		"apply -f /data/users.json", "apply -f=/etc/passwd", "apply -f/etc/passwd", "create --filename x.yaml", "apply --filename=x",
		"apply -k ./dir", "apply --kustomize=dir", "diff -f x",
		"create secret generic s --from-file=/data/users/abc/kubeconfigs/k.yaml", "create configmap c --from-env-file=/etc/environment",
		"set env deploy/x --env-file=.env", "patch deploy x --patch-file=/data/sessions.json",
		"get pods --kubeconfig=/data/users/other/kubeconfigs/k.yaml", "get pods --kubeconfig /x", "get pods --context other",
		"get pods --server=http://169.254.169.254", "get pods -s http://10.0.0.1", "get pods -shttp://x", "get pods --cluster=x",
		"get pods --certificate-authority=/etc/x", "get pods --client-key=/data/x", "get pods --cache-dir=/data",
		"get pods --log-file=/data/users.json", "get pods --log_dir=/tmp", "get pods --profile-output=/data/x",
		"get pods -o go-template-file=/data/users.json", "get pods --output=jsonpath-file=/etc/passwd", "get pods -ogo-template-file=/x",
		"get pods --token=abc", "get pods --username=a", "cluster-info dump --output-directory=/data",
		"config use-context other", "config set-credentials x --token=y", "config view-raw-x", "apply edit-last-applied deploy/x",
	}
	for _, line := range blocked {
		if err := checkKubectlArgs(strings.Fields(line), false); err == nil {
			t.Errorf("allowed: kubectl %s", line)
		}
	}
	allowed := []string{
		"get pods", "get pods -A -o wide", "get pods -o jsonpath={.items[*].metadata.name}", "get pods -o go-template={{.kind}}",
		"logs -f deploy/web", "logs web -f --tail 100", "logs --follow web", "describe node n1", "delete pod web", "scale deploy/x --replicas 2",
		"exec -it web -- sh", "exec web -- cat /etc/passwd -f -k -s", "rollout restart deploy/x", "top pods", "auth can-i get pods",
		"create secret generic s --from-literal=a=b", "patch deploy x -p {}", "get pods -l app=x -w", "config view", "config get-contexts",
		"debug -it web --image=busybox --profile=general", "run tmp --image=busybox -it --rm -- sh", "apply -f -", "get --raw /healthz",
	}
	for _, line := range allowed {
		if err := checkKubectlArgs(strings.Fields(line), false); err != nil {
			t.Errorf("blocked: kubectl %s: %v", line, err)
		}
	}
	// read-only KubeLamp: only reading verbs
	for _, line := range []string{"delete pod x", "scale deploy/x --replicas 0", "exec -it web -- sh", "apply -f -", "run x --image=y", "rollout restart deploy/x", "create ns x", "label pod x a=b"} {
		if err := checkKubectlArgs(strings.Fields(line), true); err == nil {
			t.Errorf("read-only allowed: %s", line)
		}
	}
	for _, line := range []string{"get pods", "describe pod x", "logs -f x", "top nodes", "events", "auth can-i list pods", "api-resources"} {
		if err := checkKubectlArgs(strings.Fields(line), true); err != nil {
			t.Errorf("read-only blocked: %s: %v", line, err)
		}
	}
}

func TestKubectlArgv(t *testing.T) {
	got := kubectlArgv([]string{"exec", "-it", "web", "--", "sh", "-n", "x"}, "prod", "payments", false)
	want := []string{"exec", "-it", "web", "--context", "prod", "--namespace", "payments", "--", "sh", "-n", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exec: %q", got)
	}
	if got := kubectlArgv([]string{"get", "pods", "-n", "other"}, "prod", "payments", false); indexOf(got, "--namespace") >= 0 {
		t.Errorf("-n on the line must win: %q", got)
	}
	if got := kubectlArgv([]string{"get", "pods", "-A"}, "prod", "payments", false); indexOf(got, "--namespace") >= 0 {
		t.Errorf("-A must win: %q", got)
	}
	if got := kubectlArgv([]string{"get", "pods"}, "in-cluster", "", true); indexOf(got, "--context") >= 0 {
		t.Errorf("in-cluster has no context flag: %q", got)
	}
}

func TestCompleteLine(t *testing.T) {
	if l, _ := completeLine("get dep"); l != "get deployments " {
		t.Errorf("%q", l)
	}
	if l, hits := completeLine("get p"); len(hits) < 2 || l != "get p" {
		t.Errorf("%q %v", l, hits)
	}
}

func TestRedactArgs(t *testing.T) {
	s := redactArgs([]string{"create", "secret", "generic", "x", "--from-literal=password=hunter2", "--token=abc"})
	if strings.Contains(s, "hunter2") || strings.Contains(s, "abc") {
		t.Error(s)
	}
}
