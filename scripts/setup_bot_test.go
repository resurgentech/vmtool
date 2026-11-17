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

// excludesVM reports whether a `when` condition keeps a task off VMs.
func excludesVM(w string) bool {
	s := strings.Join(strings.Fields(w), " ")
	return strings.Contains(s, "vmtool_kind != 'vm'") || strings.Contains(s, `vmtool_kind != "vm"`)
}

func onlyForVM(whens []string) bool {
	for _, w := range whens {
		s := strings.Join(strings.Fields(w), " ")
		if strings.Contains(s, "vmtool_kind == 'vm'") || strings.Contains(s, `vmtool_kind == "vm"`) {
			return true
		}
	}
	return false
}

func runsForVM(whens []string) bool {
	for _, w := range whens {
		if excludesVM(w) {
			return false
		}
	}
	return true
}

// C17: sshd keeps the first value it reads and reads sshd_config.d/*.conf in
// lexical order, so the VM drop-in must sort before cloud-init's
// 50-cloud-init.conf (which says PasswordAuthentication yes for ssh_pwauth).
func TestSetupBotVMDropInSortsFirst(t *testing.T) {
	var dests []string
	for _, task := range setupBotTasks(t) {
		cp, ok := task.body["copy"].(map[string]any)
		if !ok || !onlyForVM(task.whens) || !runsForVM(task.whens) {
			continue
		}
		content, _ := cp["content"].(string)
		if !regexp.MustCompile(`(?im)^\s*PasswordAuthentication no\s*$`).MatchString(content) {
			continue
		}
		dest, _ := cp["dest"].(string)
		dests = append(dests, dest)
	}
	if len(dests) != 1 {
		t.Fatalf("want one VM sshd drop-in that sets PasswordAuthentication no, got %q", dests)
	}
	base := dests[0][strings.LastIndex(dests[0], "/")+1:]
	if !strings.HasPrefix(dests[0], "/etc/ssh/sshd_config.d/") || !(base < "50-cloud-init.conf") {
		t.Errorf("VM drop-in %q must be in /etc/ssh/sshd_config.d/ and sort before 50-cloud-init.conf", dests[0])
	}
}

// C17: the old VM drop-in, which sorted after cloud-init's, is removed so a
// re-run fixes VMs set up before.
func TestSetupBotRemovesOldVMDropIn(t *testing.T) {
	for _, task := range setupBotTasks(t) {
		f, ok := task.body["file"].(map[string]any)
		if ok && f["path"] == "/etc/ssh/sshd_config.d/99-disable-password-auth.conf" && f["state"] == "absent" && runsForVM(task.whens) {
			return
		}
	}
	t.Fatal("no task removes /etc/ssh/sshd_config.d/99-disable-password-auth.conf on VMs")
}

// C17: on a VM the effective sshd policy is checked (password login off),
// before packer, the way back in, is removed.
func TestSetupBotChecksVMPasswordLoginBeforeRemovingPacker(t *testing.T) {
	tasks := setupBotTasks(t)
	removePacker := -1
	for _, task := range tasks {
		if task.name == "Remove packer user" {
			removePacker = task.index
		}
	}
	if removePacker < 0 {
		t.Fatal(`no task "Remove packer user"`)
	}
	check := -1
	for _, task := range tasks {
		a, ok := task.body["assert"].(map[string]any)
		if !ok || !runsForVM(task.whens) {
			continue
		}
		that, _ := a["that"].([]any)
		var pw, kbd bool
		for _, c := range that {
			s, _ := c.(string)
			pw = pw || strings.Contains(strings.ToLower(s), "passwordauthentication no")
			kbd = kbd || strings.Contains(strings.ToLower(s), "kbdinteractiveauthentication no")
		}
		if pw && kbd {
			check = task.index
			break
		}
	}
	if check < 0 {
		t.Fatal("no assert of passwordauthentication no and kbdinteractiveauthentication no runs for vmtool_kind == 'vm'")
	}
	if check > removePacker {
		t.Errorf("the VM password-login check (task %d) must come before Remove packer user (task %d)", check, removePacker)
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
