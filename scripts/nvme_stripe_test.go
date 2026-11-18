package scripts_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// L5: structural tests of ansible/playbooks/tasks/nvme_stripe.yml and its
// include in setup_host.yml. Nothing here runs ansible, mdadm or lvm.

const (
	nvmeStripePath = "../ansible/playbooks/tasks/nvme_stripe.yml"
	setupHostPath  = "../ansible/playbooks/setup_host.yml"
	nvmeGuard      = "vmtool_nvme_drives | length > 0"
)

func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

func hasWhen(whens []string, sub string) bool {
	for _, w := range whens {
		if strings.Contains(norm(w), sub) {
			return true
		}
	}
	return false
}

// nvmeTasks parses nvme_stripe.yml (a plain task list) and flattens blocks.
func nvmeTasks(t *testing.T) []botTask {
	t.Helper()
	b, err := os.ReadFile(nvmeStripePath)
	if err != nil {
		t.Fatal(err)
	}
	var list []any
	if err := yaml.Unmarshal(b, &list); err != nil {
		t.Fatalf("parse %s: %v", nvmeStripePath, err)
	}
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
				for _, k := range []string{"rescue", "always"} {
					if r, ok := m[k].([]any); ok {
						walk(r, w)
					}
				}
				continue
			}
			out = append(out, botTask{index: len(out), name: name, body: m, whens: w})
		}
	}
	walk(list, nil)
	if len(out) == 0 {
		t.Fatal("no tasks in nvme_stripe.yml")
	}
	return out
}

// text is everything a task says: module arguments, name and conditions.
func (bt botTask) text() string {
	b, _ := yaml.Marshal(bt.body)
	return string(b)
}

// codeLines is the file without comment lines, so that header prose does not
// trip the token scans.
func nvmeCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(nvmeStripePath)
	if err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

func firstIndex(tasks []botTask, pred func(botTask) bool) int {
	for _, tk := range tasks {
		if pred(tk) {
			return tk.index
		}
	}
	return -1
}

func TestNvmeStripeIncludeIsOptIn(t *testing.T) {
	b, err := os.ReadFile(setupHostPath)
	if err != nil {
		t.Fatal(err)
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatal(err)
	}
	vars, _ := plays[0]["vars"].(map[string]any)
	if d, ok := vars["vmtool_nvme_drives"].([]any); !ok || len(d) != 0 {
		t.Errorf("vmtool_nvme_drives must default to an empty list, got %#v", vars["vmtool_nvme_drives"])
	}
	for k, want := range map[string]string{"vmtool_nvme_vg": "vmtool-nvme", "vmtool_nvme_md": "vmtool-nvme", "vmtool_nvme_pool": "nvme"} {
		if vars[k] != want {
			t.Errorf("%s default = %#v, want %q", k, vars[k], want)
		}
	}
	tasks, _ := plays[0]["tasks"].([]any)
	var found int
	for _, it := range tasks {
		m, _ := it.(map[string]any)
		if m["ansible.builtin.include_tasks"] != "tasks/nvme_stripe.yml" {
			continue
		}
		found++
		if !hasWhen(whenList(m["when"]), nvmeGuard) {
			t.Errorf("include of nvme_stripe.yml lacks when: %s (has %v)", nvmeGuard, m["when"])
		}
	}
	if found != 1 {
		t.Fatalf("want one include of tasks/nvme_stripe.yml, got %d", found)
	}
}

// Belt and braces: every task of the include carries the guard, not only the
// ones that change the system.
func TestNvmeStripeEveryTaskGuarded(t *testing.T) {
	for _, tk := range nvmeTasks(t) {
		if !hasWhen(tk.whens, nvmeGuard) {
			t.Errorf("task %q lacks when: %s", tk.name, nvmeGuard)
		}
	}
}

// The create commands are the only writing storage commands, and these
// destructive tools and flags appear nowhere in the file's code.
func TestNvmeStripeOnlyCreatesNeverDestroys(t *testing.T) {
	code := nvmeCode(t)
	for _, line := range strings.Split(code, "\n") {
		if strings.Contains(line, "wipefs") && !strings.Contains(line, "--no-act") {
			t.Errorf("wipefs without --no-act: %q", line)
		}
	}
	for _, re := range []string{
		`lvremove`, `vgremove`, `pvremove`, `lvcreate`, `--zero-superblock`, `--stop\b`,
		`\bdd\b`, `mkfs`, `sgdisk`, `\bparted\b`, `--force`, `pool-build`,
	} {
		if regexp.MustCompile(re).MatchString(code) {
			t.Errorf("file must not contain %s", re)
		}
	}
	creates := map[string]bool{}
	for _, tk := range nvmeTasks(t) {
		txt := tk.text()
		if tk.body["ansible.builtin.command"] == nil {
			continue
		}
		for _, tool := range []string{"mdadm", "pvcreate", "vgcreate"} {
			if !regexp.MustCompile(`(?m)^\s*(- )?\[?'?` + tool + `\b|'` + tool + `'|argv: \[` + tool + `,`).MatchString(txt) {
				continue
			}
			if tool == "mdadm" && strings.Contains(txt, "--detail") {
				if strings.Contains(txt, "--create") {
					t.Errorf("task %q mixes --detail and --create", tk.name)
				}
				continue
			}
			creates[tool] = true
			if !hasWhen(tk.whens, "vmtool_nvme_create | bool") {
				t.Errorf("writing task %q lacks the vmtool_nvme_create guard", tk.name)
			}
		}
	}
	for _, tool := range []string{"mdadm", "pvcreate", "vgcreate"} {
		if !creates[tool] {
			t.Errorf("no guarded %s create task found", tool)
		}
	}
	// The exact array command.
	for _, w := range []string{"'--level=0'", "'--raid-devices=' ~ (vmtool_nvme_drives | length)", "'--metadata=1.2'", "'--name=' ~ vmtool_nvme_md", "'/dev/md/' ~ vmtool_nvme_md"} {
		if !strings.Contains(norm(code), w) {
			t.Errorf("mdadm --create lacks %s", w)
		}
	}
}

// All preflight refusals come before the first writing task.
func TestNvmeStripePreflightBeforeAnyWrite(t *testing.T) {
	tasks := nvmeTasks(t)
	create := firstIndex(tasks, func(b botTask) bool { return strings.Contains(b.text(), "--create") })
	if create < 0 {
		t.Fatal("no mdadm --create task")
	}
	install := firstIndex(tasks, func(b botTask) bool { return b.body["ansible.builtin.apt"] != nil })
	if install < 0 || install > create {
		t.Errorf("package install (%d) must come before the create (%d)", install, create)
	}
	apt, _ := tasks[install].body["ansible.builtin.apt"].(map[string]any)
	if s := norm(strings.Join(anyStrings(apt["name"]), " ")); s != "mdadm lvm2" {
		t.Errorf("packages = %q, want mdadm lvm2", s)
	}
	markers := []string{
		"vmtool_family != 'debian'", "length < 2", "/dev/disk/by-id/", "-part", "unique",
		"without the VG", "partition", "signature", "held by another device", "mounted", "swap",
		"not a block device",
	}
	for _, m := range markers {
		idx := firstIndex(tasks, func(b botTask) bool {
			return b.body["ansible.builtin.fail"] != nil && strings.Contains(norm(b.text()+strings.Join(b.whens, " ")), m)
		})
		if idx < 0 {
			t.Errorf("no fail task mentions %q", m)
		} else if idx > create {
			t.Errorf("the fail for %q (task %d) comes after the create (%d)", m, idx, create)
		}
	}
	// The ARRAY-without-VG refusal says nothing was changed.
	for _, tk := range tasks {
		if tk.body["ansible.builtin.fail"] != nil && !strings.Contains(tk.text(), "nothing was changed") && !strings.Contains(tk.text(), "Nothing was changed") {
			t.Errorf("fail task %q must say nothing was changed", tk.name)
		}
	}
}

func anyStrings(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Adopt first: the VG is read with a read-only command, the create flag is
// false when it exists, and every drive probe is skipped when it is false.
func TestNvmeStripeAdoptsExistingVG(t *testing.T) {
	tasks := nvmeTasks(t)
	vgs := firstIndex(tasks, func(b botTask) bool {
		return strings.Contains(b.text(), "- vgs\n") && strings.Contains(b.text(), "- --noheadings\n") && b.body["changed_when"] == false
	})
	if vgs < 0 {
		t.Fatal("no read-only vgs probe (changed_when: false)")
	}
	flag := firstIndex(tasks, func(b botTask) bool {
		sf, ok := b.body["ansible.builtin.set_fact"].(map[string]any)
		if !ok {
			return false
		}
		s, _ := sf["vmtool_nvme_create"].(string)
		return strings.Contains(s, "vmtool_nvme_vg_info.rc != 0") && strings.Contains(s, "not vmtool_nvme_md_before.stat.exists")
	})
	if flag < vgs {
		t.Errorf("vmtool_nvme_create must be set after the vgs probe (vgs %d, flag %d)", vgs, flag)
	}
	probes := 0
	for _, tk := range tasks {
		txt := tk.text()
		for _, p := range []string{"lsblk", "--no-act", "blkid", "/holders", "findmnt", "swapon", "isblk", "vmtool_nvme_stat"} {
			if strings.Contains(txt, p) {
				probes++
				if !hasWhen(tk.whens, "vmtool_nvme_create | bool") {
					t.Errorf("drive probe %q (%s) is not skipped when the VG is adopted", tk.name, p)
				}
				break
			}
		}
	}
	if probes < 10 {
		t.Errorf("expected the blank-check probes and refusals, found %d", probes)
	}
	for _, p := range []string{"lsblk", "wipefs", "blkid", "/holders", "findmnt", "swapon"} {
		if !strings.Contains(nvmeCode(t), p) {
			t.Errorf("blank check lacks %s", p)
		}
	}
	// Probes are read-only.
	for _, tk := range tasks {
		if tk.body["ansible.builtin.command"] == nil {
			continue
		}
		txt := tk.text()
		for _, p := range []string{"lsblk", "--no-act", "blkid", "/holders", "findmnt", "swapon", "realpath", "- vgs\n", "pool-info", "pool-dumpxml"} {
			if strings.Contains(txt, p) && tk.body["changed_when"] != false {
				t.Errorf("probe %q must set changed_when: false", tk.name)
			}
		}
	}
}

func TestNvmeStripePersistence(t *testing.T) {
	tasks := nvmeTasks(t)
	var conf, upd = -1, -1
	for _, tk := range tasks {
		if li, ok := tk.body["ansible.builtin.lineinfile"].(map[string]any); ok {
			conf = tk.index
			if li["path"] != "/etc/mdadm/mdadm.conf" {
				t.Errorf("lineinfile path = %v", li["path"])
			}
			if re, _ := li["regexp"].(string); !strings.Contains(re, "UUID=") {
				t.Errorf("lineinfile regexp must key on the UUID, got %q", re)
			}
			if tk.body["register"] != "vmtool_nvme_conf" {
				t.Errorf("lineinfile must register vmtool_nvme_conf")
			}
		}
		if strings.Contains(tk.text(), "update-initramfs -u") {
			upd = tk.index
			if !hasWhen(tk.whens, "vmtool_nvme_conf is changed") {
				t.Errorf("update-initramfs must run only when mdadm.conf changed, whens %v", tk.whens)
			}
		}
	}
	if conf < 0 || upd < 0 || upd < conf {
		t.Fatalf("want lineinfile (%d) then update-initramfs (%d)", conf, upd)
	}
	if !strings.Contains(norm(nvmeCode(t)), "[mdadm, --detail, --brief,") {
		t.Error("the ARRAY line must come from mdadm --detail --brief")
	}
}

func TestNvmeStripePool(t *testing.T) {
	tasks := nvmeTasks(t)
	define := firstIndex(tasks, func(b botTask) bool { return strings.Contains(b.text(), "pool-define") })
	if define < 0 {
		t.Fatal("no pool-define task")
	}
	d := tasks[define]
	if !hasWhen(d.whens, "vmtool_nvme_pool_info.rc != 0") {
		t.Errorf("define must run only when the pool is absent: %v", d.whens)
	}
	xml := d.text()
	for _, w := range []string{"<pool type='logical'>", "<name>{{ vmtool_nvme_pool }}</name>", "<name>{{ vmtool_nvme_vg }}</name>", "<format type='lvm2'/>", "<path>/dev/{{ vmtool_nvme_vg }}</path>"} {
		if !strings.Contains(xml, w) {
			t.Errorf("pool XML lacks %s", w)
		}
	}
	// A pool of another type or source VG is refused, after the read and before the define.
	fail := firstIndex(tasks, func(b botTask) bool {
		return b.body["ansible.builtin.fail"] != nil && strings.Contains(b.text(), "_type != 'logical' or _src != vmtool_nvme_vg") &&
			hasWhen(b.whens, "vmtool_nvme_pool_info.rc == 0")
	})
	dump := firstIndex(tasks, func(b botTask) bool {
		return strings.Contains(b.text(), "pool-dumpxml") && hasWhen(b.whens, "vmtool_nvme_pool_info.rc == 0")
	})
	if fail < 0 || dump < 0 || !(dump < fail && fail < define) {
		t.Errorf("want dumpxml (%d) < mismatch fail (%d) < define (%d)", dump, fail, define)
	}
	ft := tasks[fail].text()
	if !strings.Contains(ft, "<source>") || !strings.Contains(ft, "<pool") {
		t.Errorf("the mismatch check must read the pool type and <source><name>")
	}
	auto := firstIndex(tasks, func(b botTask) bool {
		return strings.Contains(b.text(), "pool-autostart") && hasWhen(b.whens, "Autostart:\\s+no") && hasWhen(b.whens, "vmtool_nvme_pool_info.rc != 0")
	})
	start := firstIndex(tasks, func(b botTask) bool {
		return strings.Contains(b.text(), "pool-start") && hasWhen(b.whens, "State:\\s+inactive") && hasWhen(b.whens, "vmtool_nvme_pool_info.rc != 0")
	})
	if auto < define || start < define {
		t.Errorf("autostart (%d) and start (%d) tasks, guarded for idempotence, must follow define (%d)", auto, start, define)
	}
}

func TestNvmeStripeSummaryNotesForeignVolumes(t *testing.T) {
	tasks := nvmeTasks(t)
	last := tasks[len(tasks)-1]
	if last.body["ansible.builtin.debug"] == nil {
		t.Fatalf("last task %q is not a debug summary", last.name)
	}
	txt := last.text()
	for _, w := range []string{"vmtool_nvme_vg", "vmtool_nvme_pool", "vmtool did not create", "test leftovers"} {
		if !strings.Contains(txt, w) {
			t.Errorf("summary lacks %q", w)
		}
	}
}

func TestNvmeStripeWarnsDestructive(t *testing.T) {
	phr := []string{"destructive by nature", "only with the drives named", "refuses non-blank drives"}
	for _, f := range []string{nvmeStripePath, setupHostPath, "../README.md", "prereqs.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(string(b), "#", "")), " "))
		s = strings.ReplaceAll(s, "*", "")
		for _, p := range phr {
			if !strings.Contains(s, p) {
				t.Errorf("%s lacks %q", f, p)
			}
		}
		if !strings.Contains(string(b), "just prereqs veritech02 -e") {
			t.Errorf("%s lacks the just prereqs command", f)
		}
		if !strings.Contains(string(b), "vmtool_nvme_drives") {
			t.Errorf("%s does not name vmtool_nvme_drives", f)
		}
	}
}

// ansible-playbook --syntax-check of the include, only when asked for (C19:
// the gate never starts a real ansible). Set VMTOOL_TEST_ANSIBLE_SYNTAX=1 by
// hand; it contacts no host.
func TestNvmeStripeSyntaxCheck(t *testing.T) {
	if os.Getenv("VMTOOL_TEST_ANSIBLE_SYNTAX") != "1" {
		t.Skip("set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to run ansible-playbook --syntax-check")
	}
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook not on PATH")
	}
	abs, err := filepath.Abs(nvmeStripePath)
	if err != nil {
		t.Fatal(err)
	}
	pb := filepath.Join(t.TempDir(), "wrap.yml")
	body := "---\n- hosts: localhost\n  gather_facts: false\n  vars:\n    vmtool_nvme_drives: []\n    vmtool_nvme_vg: vg-test\n    vmtool_nvme_md: vg-test\n    vmtool_nvme_pool: nvme\n    vmtool_family: debian\n  tasks:\n    - ansible.builtin.import_tasks: " + abs + "\n"
	if err := os.WriteFile(pb, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--syntax-check", "-i", "localhost,", pb).CombinedOutput()
	if err != nil {
		t.Fatalf("syntax-check: %v\n%s", err, out)
	}
}

// yamlStrings returns every string scalar in a decoded YAML value.
func yamlStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, yamlStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, yamlStrings(e)...)
		}
		return out
	}
	return nil
}

// ansibleYAMLStrings decodes every *.yml under ../ansible (all documents of
// each) and returns its string scalars, as Jinja receives them, per file.
func ansibleYAMLStrings(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	err := filepath.WalkDir("../ansible", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yml") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		for {
			var doc any
			if err := dec.Decode(&doc); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			out[p] = append(out[p], yamlStrings(doc)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no *.yml under ../ansible")
	}
	return out
}

// L8: the escaping class that broke the first real run. A YAML-decoded string
// that calls a regex_ filter must not hold two consecutive backslashes (a
// decoded '\\1' or '\\s' is what failed on ansible-core 2.21.5), and
// nvme_stripe.yml uses no back-reference at all, because how one must be
// escaped differs between Ansible versions.
func TestNvmeStripeRegexShape(t *testing.T) {
	all := ansibleYAMLStrings(t)
	n := 0
	for p, strs := range all {
		for _, s := range strs {
			if !strings.Contains(s, "regex_") {
				continue
			}
			n++
			if strings.Contains(s, `\\`) {
				t.Errorf("%s: a regex_ expression reaches Jinja with two consecutive backslashes: %s", p, s)
			}
		}
	}
	if n == 0 {
		t.Fatal("no regex_ filter found under ansible/; the test no longer looks at anything")
	}
	backref := regexp.MustCompile(`\\[0-9]|\\g<`)
	strs := all[nvmeStripePath]
	if len(strs) == 0 {
		t.Fatal("nvme_stripe.yml not decoded")
	}
	for _, s := range strs {
		if backref.MatchString(s) {
			t.Errorf("nvme_stripe.yml: a string holds a regex back-reference: %s", s)
		}
	}
}

// nvmeTaskNamed returns the task of nvme_stripe.yml with that exact name.
func nvmeTaskNamed(t *testing.T, name string) botTask {
	t.Helper()
	for _, tk := range nvmeTasks(t) {
		if tk.name == name {
			return tk
		}
	}
	t.Fatalf("nvme_stripe.yml has no task %q", name)
	return botTask{}
}

func taskVar(t *testing.T, tk botTask, name string) string {
	t.Helper()
	vars, _ := tk.body["vars"].(map[string]any)
	s, _ := vars[name].(string)
	if s == "" {
		t.Fatalf("task %q has no vars.%s", tk.name, name)
	}
	return s
}

// ansibleDebugVar evaluates vars[name] with `ansible localhost -c local -m
// debug` (no host, no become, read-only) and returns the rendered value.
func ansibleDebugVar(t *testing.T, name string, vars map[string]any) (any, bool) {
	t.Helper()
	b, err := json.Marshal(vars)
	if err != nil {
		t.Fatal(err)
	}
	vf := filepath.Join(t.TempDir(), "v.json")
	if err := os.WriteFile(vf, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ansible", "localhost", "-i", "localhost,", "-c", "local",
		"-m", "ansible.builtin.debug", "-a", "var="+name, "-e", "@"+vf)
	cmd.Env = ansibleEnv(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Errorf("evaluate %s: %v\n%s%s", name, err, out, stderr.String())
		return nil, false
	}
	i := bytes.Index(out, []byte("=> "))
	var res map[string]any
	if i < 0 || json.Unmarshal(out[i+3:], &res) != nil {
		t.Errorf("evaluate %s: unexpected output\n%s%s", name, out, stderr.String())
		return nil, false
	}
	v, ok := res[name]
	if !ok {
		t.Errorf("evaluate %s: not in output\n%s", name, out)
		return nil, false
	}
	return v, true
}

func wantEval(t *testing.T, label, name string, vars map[string]any, want any) {
	t.Helper()
	got, ok := ansibleDebugVar(t, name, vars)
	if ok && got != want {
		t.Errorf("%s: %s = %#v, want %#v", label, name, got, want)
	}
}

const (
	nvmeConfTask = "Record the array in /etc/mdadm/mdadm.conf"
	nvmePoolTask = "Refuse a {{ vmtool_nvme_pool }} pool that is something else"
	sampleUUID   = "0a1b2c3d:4e5f6a7b:8c9d0e1f:2a3b4c5d"
)

func samplePoolXML(typ, source string) string {
	return "<pool type='" + typ + "'>\n  <name>nvme</name>\n  <uuid>00000000-0000-0000-0000-000000000000</uuid>\n" +
		"  <capacity unit='bytes'>0</capacity>\n  <source>\n" + source + "  </source>\n" +
		"  <target>\n    <path>/dev/vg-test</path>\n  </target>\n</pool>\n"
}

// L8: the very expressions of nvme_stripe.yml, evaluated by the installed
// Ansible on fixed sample input. Opt-in (the gate never starts a real
// ansible); localhost over the local connection, nothing is changed.
func TestNvmeStripeExpressionsEvaluate(t *testing.T) {
	if os.Getenv("VMTOOL_TEST_ANSIBLE_SYNTAX") != "1" {
		t.Skip("set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to evaluate the expressions with ansible")
	}
	if _, err := exec.LookPath("ansible"); err != nil {
		t.Skip("ansible not on PATH")
	}

	conf := nvmeTaskNamed(t, nvmeConfTask)
	uuid := taskVar(t, conf, "_uuid")
	li, _ := conf.body["ansible.builtin.lineinfile"].(map[string]any)
	re, _ := li["regexp"].(string)
	if re == "" {
		t.Fatal("mdadm.conf task has no regexp")
	}
	line := "ARRAY /dev/md/vg-test metadata=1.2 name=PLACEHOLDER-HOST:vg-test UUID=" + sampleUUID
	withUUID := map[string]any{"_uuid": uuid, "_regexp": re, "vmtool_nvme_detail": map[string]any{"stdout": line}}
	wantEval(t, "ARRAY line", "_uuid", withUUID, sampleUUID)
	wantEval(t, "ARRAY line", "_regexp", withUUID, `^ARRAY\s.*UUID=`+sampleUUID)
	noUUID := map[string]any{"_uuid": uuid, "vmtool_nvme_detail": map[string]any{"stdout": "ARRAY /dev/md/vg-test metadata=1.2 name=PLACEHOLDER-HOST:vg-test"}}
	wantEval(t, "no UUID", "_uuid", noUUID, "")

	pool := nvmeTaskNamed(t, nvmePoolTask)
	typ, src := taskVar(t, pool, "_type"), taskVar(t, pool, "_src")
	whens := whenList(pool.body["when"])
	if len(whens) != 3 || norm(whens[2]) != "_type != 'logical' or _src != vmtool_nvme_vg" {
		t.Fatalf("pool refusal's third when item = %v", whens)
	}
	refuse := "{{ " + whens[2] + " }}"
	for _, c := range []struct {
		label, xml, typ, src string
		refuse               bool
	}{
		{"logical over vg-test", samplePoolXML("logical", "    <device path='/dev/md/vg-test'/>\n    <name>vg-test</name>\n    <format type='lvm2'/>\n"), "logical", "vg-test", false},
		{"dir pool", samplePoolXML("dir", ""), "dir", "", true},
		{"logical over other-vg", samplePoolXML("logical", "    <device path='/dev/md/other-vg'/>\n    <name>other-vg</name>\n    <format type='lvm2'/>\n"), "logical", "other-vg", true},
	} {
		vars := map[string]any{
			"_type": typ, "_src": src, "_refuse": refuse, "vmtool_nvme_vg": "vg-test",
			"vmtool_nvme_pool_xml": map[string]any{"stdout": c.xml},
		}
		wantEval(t, c.label, "_type", vars, c.typ)
		wantEval(t, c.label, "_src", vars, c.src)
		wantEval(t, c.label, "_refuse", vars, c.refuse)
	}
}
