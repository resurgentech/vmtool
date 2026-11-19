package scripts_test

import (
	"os"
	"os/exec"
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
