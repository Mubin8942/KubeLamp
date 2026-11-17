package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Aliases in the kubectl console: a set of common ones built in (kgp → kubectl get pods),
// plus each user's own, saved in their folder (<data>/users/<id>/console.json) so they
// follow them across consoles, restarts and devices. An alias only replaces the first
// word; the result goes through the same checks as a typed command.

var builtinAliases = map[string]string{
	"kg": "kubectl get", "kgp": "kubectl get pods", "kgpa": "kubectl get pods -A", "kgpw": "kubectl get pods -o wide",
	"kgd": "kubectl get deployments", "kgs": "kubectl get services", "kgn": "kubectl get nodes", "kgns": "kubectl get namespaces",
	"kgi": "kubectl get ingress", "kgsts": "kubectl get statefulsets", "kgcm": "kubectl get configmaps", "kgsec": "kubectl get secrets",
	"kgj": "kubectl get jobs", "kgcj": "kubectl get cronjobs", "kge": "kubectl get events --sort-by=.lastTimestamp",
	"kd": "kubectl describe", "kdp": "kubectl describe pod", "kdd": "kubectl describe deployment",
	"kl": "kubectl logs", "klf": "kubectl logs -f", "kex": "kubectl exec -it", "kdel": "kubectl delete",
	"krr": "kubectl rollout restart", "krs": "kubectl rollout status", "ktp": "kubectl top pods", "ktn": "kubectl top nodes",
}

var aliasNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)

const maxAliases = 100

type consoleSettings struct {
	Aliases map[string]string `json:"aliases"`
}

func consoleSettingsPath(dir string) string { return filepath.Join(dir, "console.json") }

func loadConsoleSettings(dir string) consoleSettings {
	var cs consoleSettings
	if b, err := os.ReadFile(consoleSettingsPath(dir)); err == nil {
		_ = json.Unmarshal(b, &cs)
	}
	if cs.Aliases == nil {
		cs.Aliases = map[string]string{}
	}
	return cs
}

func saveConsoleSettings(dir string, cs consoleSettings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cs, "", "  ")
	tmp := consoleSettingsPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, consoleSettingsPath(dir))
}

// parseAliasDef reads "name=value" (value may be quoted) from `alias name='kubectl get pods'`.
func parseAliasDef(def string) (name, value string, err error) {
	words, err := splitLine(def)
	if err != nil {
		return "", "", err
	}
	if len(words) != 1 || !strings.Contains(words[0], "=") {
		return "", "", errors.New("usage: alias name='kubectl get pods'   (quote the command)")
	}
	name, value, _ = strings.Cut(words[0], "=")
	value = strings.TrimSpace(value)
	switch {
	case !aliasNameRe.MatchString(name):
		return "", "", errors.New("alias names: letters, digits, . _ - (start with a letter, at most 32)")
	case name == "kubectl" || name == "k" || name == "alias" || name == "unalias" || name == "help" || name == "clear" || contains(kubectlSubs, name):
		return "", "", fmt.Errorf("%q is already a command", name)
	case value == "":
		return "", "", errors.New("the alias needs a command, e.g. alias kgp='kubectl get pods'")
	case len(value) > 300:
		return "", "", errors.New("the alias is too long")
	}
	first := strings.Fields(value)[0]
	if first != "kubectl" && first != "k" && !contains(kubectlSubs, first) {
		return "", "", errors.New("an alias must run kubectl, e.g. alias kgp='kubectl get pods'")
	}
	return name, value, nil
}

// expandAlias replaces the first word if it is an alias (the user's own win over the
// built-in ones). Only once: aliases don't expand inside aliases.
func expandAlias(line string, own map[string]string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	i := strings.IndexAny(trimmed, " \t|")
	first, rest := trimmed, ""
	if i >= 0 {
		first, rest = trimmed[:i], trimmed[i:]
	}
	if v, ok := own[first]; ok {
		return v + rest, true
	}
	if v, ok := builtinAliases[first]; ok {
		return v + rest, true
	}
	return line, false
}

// aliasCommand handles `alias`, `alias name=…`, `unalias name`. ok=false: not an alias command.
func (c *console) aliasCommand(line string) (out string, ok bool) {
	t := strings.TrimSpace(line)
	cmd, rest, _ := strings.Cut(t, " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "alias":
		if rest == "" {
			return c.listAliases(), true
		}
		name, value, err := parseAliasDef(rest)
		if err != nil {
			return "\x1b[31m" + err.Error() + "\x1b[0m\r\n", true
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, exists := c.aliases[name]; !exists && len(c.aliases) >= maxAliases {
			return fmt.Sprintf("\x1b[31mat most %d aliases\x1b[0m\r\n", maxAliases), true
		}
		c.aliases[name] = value
		if err := saveConsoleSettings(c.s.userDir(c.r), consoleSettings{Aliases: c.aliases}); err != nil {
			return "\x1b[31mcannot save the alias: " + err.Error() + "\x1b[0m\r\n", true
		}
		return fmt.Sprintf("\x1b[2malias %s = %s (saved)\x1b[0m\r\n", name, value), true
	case "unalias":
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, exists := c.aliases[rest]; !exists {
			if _, b := builtinAliases[rest]; b {
				return "\x1b[31m" + rest + " is built in; you can redefine it with alias " + rest + "='…'\x1b[0m\r\n", true
			}
			return "\x1b[31mno alias " + rest + "\x1b[0m\r\n", true
		}
		delete(c.aliases, rest)
		if err := saveConsoleSettings(c.s.userDir(c.r), consoleSettings{Aliases: c.aliases}); err != nil {
			return "\x1b[31mcannot save: " + err.Error() + "\x1b[0m\r\n", true
		}
		return "\x1b[2mremoved " + rest + "\x1b[0m\r\n", true
	}
	return "", false
}

func (c *console) listAliases() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	row := func(n, v string) { fmt.Fprintf(&b, "  \x1b[38;5;75m%-8s\x1b[0m %s\r\n", n, v) }
	if len(c.aliases) > 0 {
		b.WriteString("\x1b[1mYour aliases\x1b[0m\r\n")
		for _, n := range sortedKeys(c.aliases) {
			row(n, c.aliases[n])
		}
	}
	b.WriteString("\x1b[1mBuilt in\x1b[0m  \x1b[2m(k = kubectl)\x1b[0m\r\n")
	for _, n := range sortedKeys(builtinAliases) {
		if _, mine := c.aliases[n]; !mine {
			row(n, builtinAliases[n])
		}
	}
	b.WriteString("\x1b[2mAdd: alias kgpw='kubectl get pods -o wide'   Remove: unalias kgpw\x1b[0m\r\n")
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
