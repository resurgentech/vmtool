package scripts_test

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const setupBotPath = "../ansible/playbooks/setup_bot.yml"

// botTask is one task of setup_bot.yml, flattened out of its blocks, with the
// `when` conditions of every enclosing block (outermost first) and its own.
type botTask struct {
	index int // position in document order
	name  string
	body  map[string]any
	whens []string
}

// setupBotTasks parses setup_bot.yml and returns every task of its single
// play in document order, block and rescue members included.
func setupBotTasks(t *testing.T) []botTask {
	t.Helper()
	b, err := os.ReadFile(setupBotPath)
	if err != nil {
		t.Fatal(err)
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatalf("parse %s: %v", setupBotPath, err)
	}
	if len(plays) != 1 {
		t.Fatalf("want one play, got %d", len(plays))
	}
	tasks, _ := plays[0]["tasks"].([]any)
	var out []botTask
	var walk func(list []any, whens []string)
	walk = func(list []any, whens []string) {
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			w := append(append([]string{}, whens...), whenList(m["when"])...)
			name, _ := m["name"].(string)
			if blk, ok := m["block"].([]any); ok {
				walk(blk, w)
				if r, ok := m["rescue"].([]any); ok {
					walk(r, w)
				}
				continue
			}
			out = append(out, botTask{index: len(out), name: name, body: m, whens: w})
		}
	}
	walk(tasks, nil)
	return out
}

func whenList(v any) []string {
	switch w := v.(type) {
	case string:
		return []string{w}
	case []any:
		var s []string
		for _, x := range w {
			if str, ok := x.(string); ok {
				s = append(s, str)
			}
		}
		return s
	}
	return nil
}

// C16: the reconnect as bot keeps the inventory's ssh args (they carry vmtool's
// tunnel ProxyCommand) and only adds IdentitiesOnly.
func TestSetupBotReconnectKeepsSSHArgs(t *testing.T) {
	var found bool
	for _, task := range setupBotTasks(t) {
		sf, ok := task.body["set_fact"].(map[string]any)
		if !ok || sf["ansible_user"] != "bot" {
			continue
		}
		found = true
		args, _ := sf["ansible_ssh_common_args"].(string)
		if !regexp.MustCompile(`\{\{[^}]*\bansible_ssh_common_args\b[^}]*\}\}`).MatchString(args) {
			t.Errorf("%q: ansible_ssh_common_args must read the existing value inside {{ }}, got %q", task.name, args)
		}
		if !strings.Contains(args, "IdentitiesOnly=yes") {
			t.Errorf("%q: ansible_ssh_common_args lacks IdentitiesOnly=yes: %q", task.name, args)
		}
	}
	if !found {
		t.Fatal("no set_fact sets ansible_user: bot")
	}
}

// ansible-playbook --syntax-check, only when asked for. The gate must never
// start a real ansible (C19); set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to run it by
// hand. It contacts no host.
func TestSetupBotSyntaxCheck(t *testing.T) {
	if os.Getenv("VMTOOL_TEST_ANSIBLE_SYNTAX") != "1" {
		t.Skip("set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to run ansible-playbook --syntax-check")
	}
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook not on PATH")
	}
	out, err := exec.Command(bin, "--syntax-check", "-i", "localhost,", setupBotPath).CombinedOutput()
	if err != nil {
		t.Fatalf("syntax-check: %v\n%s", err, out)
	}
}
