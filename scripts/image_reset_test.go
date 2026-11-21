package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// T14: structural tests of ansible/playbooks/image_reset.yml, the optional
// reset playbook for template authors. Nothing here runs ansible, and nothing
// may: the playbook shuts the machine it is run on down.

const (
	imageResetPath = "../ansible/playbooks/image_reset.yml"
	packerResetHCL = "../packer_images/qemu_images/ubuntu2604_btrfs/template.pkr.hcl"
)

// packerGrubSed extracts the GRUB_CMDLINE_LINUX_DEFAULT sed line of the
// Packer template's final provisioner and undoes the HCL string quoting
// (\\ to \, \" to "), minus the leading "sudo ".
func packerGrubSed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(packerResetHCL)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "sudo sed -i '/^GRUB_CMDLINE_LINUX_DEFAULT=") {
			continue
		}
		line = strings.TrimSuffix(strings.TrimPrefix(line, `"`), `",`)
		line = strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(line)
		return strings.TrimPrefix(line, "sudo ")
	}
	t.Fatalf("no GRUB_CMDLINE_LINUX_DEFAULT sed in %s", packerResetHCL)
	return ""
}

// packerResetCommands returns the commands of a template's final provisioner
// (from "cloud-init status --wait" to "cloud-init clean"), HCL quoting undone,
// "sudo " dropped (the playbook uses become), comment lines skipped.
func packerResetCommands(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !in {
			if strings.HasPrefix(line, `"sudo cloud-init status --wait`) {
				in = true
			} else {
				continue
			}
		}
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		line = strings.TrimSuffix(strings.TrimPrefix(line, `"`), `",`)
		line = strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(line)
		out = append(out, strings.ReplaceAll(line, "sudo ", ""))
		if strings.Contains(line, "cloud-init clean") {
			return out
		}
	}
	t.Fatalf("no final provisioner in %s", path)
	return nil
}

// TestImageResetMatchesEveryTemplate: the playbook's commands (all tasks but
// the shutdown) are the template provisioner's, line by line, in every
// packer_images template.
func TestImageResetMatchesEveryTemplate(t *testing.T) {
	tasks := imageResetTasks(t)
	var got []string
	for _, tk := range tasks[:len(tasks)-1] {
		got = append(got, cmdText(tk))
	}
	files, err := filepath.Glob("../packer_images/qemu_images/*/template.pkr.hcl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, f := range files {
		want := packerResetCommands(t, f)
		if len(got) != len(want) {
			t.Errorf("%s: playbook has %d commands, template %d", f, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s command %d:\n playbook %s\n template %s", f, i, got[i], want[i])
			}
		}
	}
}

// imageResetTasks returns the tasks of the playbook's single play.
func imageResetTasks(t *testing.T) []botTask {
	t.Helper()
	b, err := os.ReadFile(imageResetPath)
	if err != nil {
		t.Fatal(err)
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatalf("parse %s: %v", imageResetPath, err)
	}
	if len(plays) != 1 {
		t.Fatalf("want one play, got %d", len(plays))
	}
	if plays[0]["become"] != true {
		t.Errorf("the play must use become: true (the commands are root's, without sudo)")
	}
	list, _ := plays[0]["tasks"].([]any)
	var out []botTask
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		out = append(out, botTask{index: len(out), name: name, body: m, whens: whenList(m["when"])})
	}
	return out
}

// cmdText is the command line a shell or command task runs.
func cmdText(bt botTask) string {
	for _, k := range []string{"ansible.builtin.shell", "shell", "ansible.builtin.command", "command"} {
		if s, ok := bt.body[k].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func TestImageResetCommandsInOrder(t *testing.T) {
	tasks := imageResetTasks(t)
	want := []string{
		"cloud-init status --wait || true",
		packerGrubSed(t),
		"update-grub",
		"if ! test -e /boot/efi/EFI/BOOT/BOOTX64.EFI; then grub-install --target=x86_64-efi --efi-directory=/boot/efi --removable --no-nvram; fi",
		"systemctl mask tmp.mount",
		"apt-get clean",
		"rm -f /root/.bash_history",
		"sed -i '/^127[.]0[.]1[.]1[[:space:]]/d' /etc/hosts",
		"cloud-init clean --machine-id --logs",
	}
	if len(tasks) != len(want)+1 {
		t.Fatalf("want %d tasks (the commands, then the shutdown), got %d", len(want)+1, len(tasks))
	}
	for i, w := range want {
		if got := cmdText(tasks[i]); got != w {
			t.Errorf("task %d (%q):\n got  %s\n want %s", i, tasks[i].name, got, w)
		}
	}
}

// The mask task sits after the removable loader and before the apt cache,
// is named for what it does, and is idempotent (not changed_when: true).
func TestImageResetMaskTmpMountPlace(t *testing.T) {
	tasks := imageResetTasks(t)
	idx := func(sub string) int {
		for i, tk := range tasks {
			if strings.Contains(cmdText(tk), sub) {
				return i
			}
		}
		return -1
	}
	m, l, a := idx("systemctl mask tmp.mount"), idx("grub-install"), idx("apt-get clean")
	if m < 0 || l < 0 || a < 0 || !(l < m && m == l+1 && m+1 == a) {
		t.Fatalf("mask task at %d, loader at %d, apt clean at %d: want loader, mask, apt clean in a row", m, l, a)
	}
	if tasks[m].name != "Keep /tmp on disk (mask tmp.mount)" {
		t.Errorf("mask task name = %q", tasks[m].name)
	}
	if strings.Contains(cmdText(tasks[m]), "--now") {
		t.Error("mask must not use --now (it would unmount /tmp)")
	}
	if tasks[m].body["changed_when"] == true {
		t.Error("mask task must not be changed_when: true; derive it from the output")
	}
}

func TestImageResetShutdownLastAndAsync(t *testing.T) {
	tasks := imageResetTasks(t)
	if len(tasks) == 0 {
		t.Fatal("no tasks")
	}
	last := tasks[len(tasks)-1]
	c := cmdText(last)
	if !strings.Contains(c, "poweroff") && !strings.Contains(c, "shutdown") {
		t.Fatalf("last task is not a shutdown: %q", c)
	}
	for _, tk := range tasks[:len(tasks)-1] {
		if s := cmdText(tk); strings.Contains(s, "poweroff") || strings.Contains(s, "shutdown") {
			t.Errorf("task %q shuts down before the last task", tk.name)
		}
	}
	if n, _ := last.body["async"].(int); n <= 0 {
		t.Errorf("shutdown async = %v, want > 0", last.body["async"])
	}
	if p, ok := last.body["poll"].(int); !ok || p != 0 {
		t.Errorf("shutdown poll = %v, want 0 (do not wait for the connection)", last.body["poll"])
	}
}

func TestImageResetNoSetupBot(t *testing.T) {
	b, err := os.ReadFile(imageResetPath)
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`setup_bot`).Match(b) {
		t.Error("image_reset.yml must not refer to setup_bot anywhere")
	}
}

func TestImageResetHeader(t *testing.T) {
	b, err := os.ReadFile(imageResetPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, sub := range []string{
		"vmtool playbook",
		"templates save",
		"create --playbook",
		"powers",
	} {
		if !strings.Contains(s, sub) {
			t.Errorf("header comment lacks %q", sub)
		}
	}
}

// ansible-playbook --syntax-check, only when asked for. The gate never starts
// a real ansible; set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to run it by hand. It
// contacts no host and runs no task.
func TestImageResetSyntaxCheck(t *testing.T) {
	if os.Getenv("VMTOOL_TEST_ANSIBLE_SYNTAX") != "1" {
		t.Skip("set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to run ansible-playbook --syntax-check")
	}
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook not on PATH")
	}
	out, err := exec.Command(bin, "--syntax-check", "-i", "localhost,", imageResetPath).CombinedOutput()
	if err != nil {
		t.Fatalf("syntax-check: %v\n%s", err, out)
	}
}
