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
	if vars["vmtool_nvme_thin"] != false {
		t.Errorf("vmtool_nvme_thin must default to false, got %#v", vars["vmtool_nvme_thin"])
	}
	for k, want := range map[string]string{"vmtool_nvme_thin_pool": "nvme-thin", "vmtool_nvme_thin_size": "70%FREE", "vmtool_nvme_thin_chunk": "512k", "vmtool_nvme_thin_metadata": "8G"} {
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
		`vgremove`, `pvremove`, `lvreduce`, `vgreduce`, `lvextend`, `lvconvert`, `lvrename`, `--zero-superblock`, `--stop\b`,
		`\bdd\b`, `mkfs`, `sgdisk`, `\bparted\b`, `--force`, `pool-build`,
	} {
		if regexp.MustCompile(re).MatchString(code) {
			t.Errorf("file must not contain %s", re)
		}
	}
	// T1/D2: lvcreate only as `lvcreate --type thin-pool` or as the one
	// scratch volume over the free space; lvchange only to set zeroing off, to
	// attach the profile or to set discards to passdown. (lvremove and
	// blkdiscard have their own test, TestNvmeScratchOnlyScratchIsDiscarded.)
	norc := norm(code)
	nCreate := strings.Count(norc, "lvcreate")
	nThin := len(regexp.MustCompile(`'lvcreate', '--type', 'thin-pool'`).FindAllString(norc, -1))
	nScratch := len(regexp.MustCompile(`lvcreate, -l, 100%FREE, -n, vmtool-discard-scratch,`).FindAllString(norc, -1))
	if nThin != 1 || nScratch != 1 || nCreate != nThin+nScratch {
		t.Errorf("every lvcreate must be `lvcreate --type thin-pool` (1) or the scratch form (1): %d lvcreate, %d thin-pool, %d scratch", nCreate, nThin, nScratch)
	}
	nChange := strings.Count(norc, "lvchange")
	nOK := len(regexp.MustCompile(`lvchange, (--zero, "n"|--metadataprofile, vmtool-thin|--discards, passdown)`).FindAllString(norc, -1))
	if nChange != nOK || nChange != 3 {
		t.Errorf("lvchange only with --zero n, --metadataprofile vmtool-thin or --discards passdown: %d lvchange, %d allowed (want 3)", nChange, nOK)
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
		if tk.body["ansible.builtin.fail"] == nil {
			continue
		}
		// D2: the one fail that follows a failed discard leaves the scratch
		// volume behind on purpose and says so instead.
		if strings.Contains(tk.text(), "vmtool_nvme_scratch_discard") {
			if !strings.Contains(tk.text(), "was left in place") {
				t.Errorf("fail task %q must say the scratch volume was left in place", tk.name)
			}
			continue
		}
		if !strings.Contains(tk.text(), "nothing was changed") && !strings.Contains(tk.text(), "Nothing was changed") {
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
		if li, ok := tk.body["ansible.builtin.lineinfile"].(map[string]any); ok && li["path"] != "/etc/lvm/lvm.conf" {
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
	body := "---\n- hosts: localhost\n  gather_facts: false\n  vars:\n    vmtool_nvme_drives: []\n    vmtool_nvme_vg: vg-test\n    vmtool_nvme_md: vg-test\n    vmtool_nvme_pool: nvme\n    vmtool_nvme_thin: false\n    vmtool_nvme_thin_pool: nvme-thin\n    vmtool_nvme_thin_size: 70%FREE\n    vmtool_nvme_thin_chunk: 512k\n    vmtool_nvme_thin_metadata: 8G\n    vmtool_family: debian\n  tasks:\n    - ansible.builtin.import_tasks: " + abs + "\n"
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

// ---------------------------------------------------------------------- T1
// The thin pool in the array's VG: shape tests. Nothing here runs ansible
// (the expression test below is opt-in) or any lvm command.

const (
	thinGuard   = "vmtool_nvme_thin | bool"
	thinPrefix  = "Thin pool: "
	thinProfile = "../ansible/files/vmtool-thin.profile"
)

func thinTasks(t *testing.T) []botTask {
	t.Helper()
	var out []botTask
	for _, tk := range nvmeTasks(t) {
		if strings.HasPrefix(tk.name, thinPrefix) {
			out = append(out, tk)
		}
	}
	if len(out) < 8 {
		t.Fatalf("want the thin-pool tasks (names starting %q), found %d", thinPrefix, len(out))
	}
	return out
}

func thinTask(t *testing.T, sub string) botTask {
	t.Helper()
	for _, tk := range thinTasks(t) {
		if strings.Contains(tk.name, sub) {
			return tk
		}
	}
	t.Fatalf("no thin-pool task named like %q", sub)
	return botTask{}
}

// Every thin task carries the stripe guard and the opt-in switch.
func TestNvmeThinEveryTaskGuarded(t *testing.T) {
	// A task that names a thin variable or a thin-pool command is a thin task,
	// whatever it is called; only the final summary (which reads the thin
	// result through default([])) is exempt.
	tasks := nvmeTasks(t)
	for _, tk := range tasks[:len(tasks)-1] {
		txt := tk.text()
		if (strings.Contains(txt, "vmtool_nvme_thin") || strings.Contains(txt, "thin-pool") || strings.Contains(txt, "lvchange")) &&
			!strings.HasPrefix(tk.name, thinPrefix) {
			t.Errorf("task %q is a thin-pool task but is not named %q, so the thin guards are not checked on it", tk.name, thinPrefix)
		}
	}
	for _, tk := range thinTasks(t) {
		if !hasWhen(tk.whens, nvmeGuard) {
			t.Errorf("task %q lacks when: %s", tk.name, nvmeGuard)
		}
		if !hasWhen(tk.whens, thinGuard) {
			t.Errorf("task %q lacks when: %s", tk.name, thinGuard)
		}
	}
}

// argvText is the command task's argv as one line: a templated string as it is
// written, a list joined by spaces.
func argvText(tk botTask) string {
	cmd, _ := tk.body["ansible.builtin.command"].(map[string]any)
	switch a := cmd["argv"].(type) {
	case string:
		return a
	case []any:
		return strings.Join(anyStrings(a), " ")
	}
	return ""
}

func TestNvmeThinPackageProfileAndMonitor(t *testing.T) {
	apt, _ := thinTask(t, "thin-provisioning-tools").body["ansible.builtin.apt"].(map[string]any)
	if s := norm(strings.Join(anyStrings(apt["name"]), " ")); s != "thin-provisioning-tools" && apt["name"] != "thin-provisioning-tools" {
		t.Errorf("thin packages = %v, want thin-provisioning-tools", apt["name"])
	}
	cp, _ := thinTask(t, "LVM profile").body["ansible.builtin.copy"].(map[string]any)
	if cp["dest"] != "/etc/lvm/profile/vmtool-thin.profile" || cp["mode"] != "0644" {
		t.Errorf("profile copy dest/mode = %v / %v", cp["dest"], cp["mode"])
	}
	if src, _ := cp["src"].(string); !strings.Contains(src, "files/vmtool-thin.profile") {
		t.Errorf("profile copy src = %v", cp["src"])
	}
	sd, _ := thinTask(t, "lvm2-monitor").body["ansible.builtin.systemd"].(map[string]any)
	if sd["name"] != "lvm2-monitor" || sd["enabled"] != true {
		t.Errorf("lvm2-monitor task = %v", sd)
	}
	b, err := os.ReadFile(thinProfile)
	if err != nil {
		t.Fatal(err)
	}
	var body []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			body = append(body, l)
		}
	}
	got := strings.Join(strings.Fields(strings.Join(body, "\n")), " ")
	want := "activation { thin_pool_autoextend_threshold = 80 thin_pool_autoextend_percent = 10 }"
	if got != want {
		t.Errorf("profile = %q, want %q", got, want)
	}
}

func TestNvmeThinCreate(t *testing.T) {
	tasks := nvmeTasks(t)
	probe := thinTask(t, "probe")
	txt := norm(probe.text())
	for _, w := range []string{"lvs", "--reportformat", "json", "lv_name,lv_attr,lv_tags", "vmtool_nvme_vg", "vmtool_nvme_thin_pool"} {
		if !strings.Contains(txt, w) {
			t.Errorf("probe lacks %q: %s", w, txt)
		}
	}
	if probe.body["changed_when"] != false || probe.body["check_mode"] != false {
		t.Errorf("probe must be read-only (changed_when/check_mode false)")
	}
	cr := thinTask(t, "create the pool")
	code := norm(argvText(cr))
	for _, w := range []string{
		"'lvcreate', '--type', 'thin-pool'", "'-n', vmtool_nvme_thin_pool",
		"'--chunksize', vmtool_nvme_thin_chunk", "'--poolmetadatasize', vmtool_nvme_thin_metadata",
		"'--zero', 'n'", "'--errorwhenfull', 'n'", "'--monitor', 'y'",
		"'--metadataprofile', 'vmtool-thin'", "'--addtag', 'vmtool_thinpool'", "vmtool_nvme_vg",
		"'--discards', 'passdown'", "'%' in", "'-l'", "'-L'", "vmtool_nvme_thin_size",
	} {
		if !strings.Contains(code, w) {
			t.Errorf("lvcreate task lacks %s: %s", w, code)
		}
	}
	if strings.Contains(code, "--force") || strings.Contains(code, "'-f'") {
		t.Errorf("lvcreate must not force")
	}
	if !hasWhen(cr.whens, "not (vmtool_nvme_thin_present | bool)") {
		t.Errorf("create must run only when the pool is absent: %v", cr.whens)
	}
	prof := firstIndex(tasks, func(b botTask) bool { return strings.Contains(b.name, "LVM profile") })
	if !(prof < probe.index && probe.index < cr.index) {
		t.Errorf("want profile (%d) < probe (%d) < create (%d)", prof, probe.index, cr.index)
	}
}

func TestNvmeThinAdoptAndRefuse(t *testing.T) {
	refuse := thinTask(t, "refuse")
	if refuse.body["ansible.builtin.fail"] == nil {
		t.Fatalf("refuse task is not a fail")
	}
	rt := strings.ToLower(norm(refuse.text()))
	for _, w := range []string{"vmtool_nvme_thin_pool", "vmtool_nvme_vg", "nothing was changed", "thin pool", "vmtool_thinpool"} {
		if !strings.Contains(rt, w) {
			t.Errorf("refusal lacks %q", w)
		}
	}
	if !hasWhen(refuse.whens, "vmtool_nvme_thin_present | bool") || !hasWhen(refuse.whens, "not (vmtool_nvme_thin_adoptable | bool)") {
		t.Errorf("refusal must fire when present and not adoptable: %v", refuse.whens)
	}
	cr := thinTask(t, "create the pool")
	if !(refuse.index < cr.index) {
		t.Errorf("refusal (%d) must come before create (%d)", refuse.index, cr.index)
	}
	zero := thinTask(t, "zeroing")
	prof := thinTask(t, "attach the profile")
	for _, tk := range []botTask{zero, prof} {
		if !hasWhen(tk.whens, "vmtool_nvme_thin_adoptable | bool") {
			t.Errorf("%q must run only on an adopted pool: %v", tk.name, tk.whens)
		}
		if tk.index < refuse.index {
			t.Errorf("%q comes before the refusal", tk.name)
		}
	}
	if !hasWhen(zero.whens, "vmtool_nvme_thin_zeroing | bool") || !strings.Contains(argvText(zero), "lvchange --zero n ") {
		t.Errorf("zeroing task wrong: %v / %s", zero.whens, argvText(zero))
	}
	if !hasWhen(prof.whens, "vmtool_nvme_thin_profile_now != 'vmtool-thin'") || !strings.Contains(argvText(prof), "lvchange --metadataprofile vmtool-thin ") {
		t.Errorf("profile task wrong: %v / %s", prof.whens, argvText(prof))
	}
	// The probe reads what the changed-only-when-different decisions need.
	if tx := norm(thinTask(t, "probe").text()); !strings.Contains(tx, "zero") || !strings.Contains(tx, "lv_profile") {
		t.Errorf("probe must read zero and lv_profile too: %s", tx)
	}
}

func TestNvmeThinSummaryShowsPool(t *testing.T) {
	tasks := nvmeTasks(t)
	read := thinTask(t, "usage")
	tx := norm(read.text())
	for _, w := range []string{"lvs", "lv_size", "data_percent", "metadata_percent", "vmtool_nvme_thin_pool"} {
		if !strings.Contains(tx, w) {
			t.Errorf("summary read lacks %q", w)
		}
	}
	last := tasks[len(tasks)-1]
	if read.index > last.index || !strings.Contains(last.text(), "vmtool_nvme_thin_final") {
		t.Errorf("the final summary must show the thin read (%d, last %d)", read.index, last.index)
	}
}

// T1: the thin-pool expressions of nvme_stripe.yml, evaluated by the installed
// Ansible on sample `lvs --reportformat json` output. Opt-in; localhost only.
func TestNvmeThinExpressionsEvaluate(t *testing.T) {
	if os.Getenv("VMTOOL_TEST_ANSIBLE_SYNTAX") != "1" {
		t.Skip("set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to evaluate the expressions with ansible")
	}
	if _, err := exec.LookPath("ansible"); err != nil {
		t.Skip("ansible not on PATH")
	}
	rp := thinTask(t, "read the probe")
	sf, _ := rp.body["ansible.builtin.set_fact"].(map[string]any)
	lv := taskVar(t, rp, "_lv")
	report := func(name, attr, tags, zero, profile string, discards ...string) string {
		d := ""
		if len(discards) > 0 {
			d = discards[0]
		}
		return `{"report":[{"lv":[{"lv_name":"` + name + `","lv_attr":"` + attr + `","lv_tags":"` + tags + `","zero":"` + zero + `","lv_profile":"` + profile + `","discards":"` + d + `"}]}]}`
	}
	// D2: the discards column, from the same expression.
	discardsExpr, _ := sf["vmtool_nvme_thin_discards"].(string)
	if discardsExpr == "" {
		t.Fatal("set_fact lacks vmtool_nvme_thin_discards")
	}
	for label, c := range map[string]struct{ stdout, want string }{
		"discards passdown":   {report("thin-test", "twi-aotz--", "vmtool_thinpool", "", "", "passdown"), "passdown"},
		"discards ignore":     {report("thin-test", "twi-aotz--", "vmtool_thinpool", "", "", "ignore"), "ignore"},
		"discards nopassdown": {report("thin-test", "twi-aotz--", "vmtool_thinpool", "", "", "nopassdown"), "nopassdown"},
		"discards absent":     {"", ""},
	} {
		rc := 0
		if c.stdout == "" {
			rc = 5
		}
		vars := map[string]any{"_lv": lv, "vmtool_nvme_thin_discards": discardsExpr, "vmtool_nvme_thin_probe": map[string]any{"rc": rc, "stdout": c.stdout}}
		wantEval(t, label, "vmtool_nvme_thin_discards", vars, c.want)
	}
	for _, c := range []struct {
		label                  string
		rc                     int
		stdout                 string
		present, adopt, zeroed bool
		profile                string
	}{
		{"absent", 5, "", false, false, false, ""},
		{"absent with empty report", 5, `{"report":[{"lv":[]}]}`, false, false, false, ""},
		{"tagged thin pool, zeroing on", 0, report("thin-test", "twi-a-tz--", "vmtool_thinpool", "zero", ""), true, true, true, ""},
		{"tagged thin pool, adopted fully", 0, report("thin-test", "twi-aotz--", "other,vmtool_thinpool", "", "vmtool-thin"), true, true, false, "vmtool-thin"},
		{"untagged thin pool", 0, report("thin-test", "twi-a-tz--", "", "", ""), true, false, false, ""},
		{"tag of another name", 0, report("thin-test", "twi-a-tz--", "vmtool_thinpool_x", "", ""), true, false, false, ""},
		{"tagged but a plain LV", 0, report("thin-test", "-wi-a-----", "vmtool_thinpool", "", ""), true, false, false, ""},
	} {
		vars := map[string]any{"_lv": lv, "vmtool_nvme_thin_probe": map[string]any{"rc": c.rc, "stdout": c.stdout}}
		for _, k := range []string{"vmtool_nvme_thin_present", "vmtool_nvme_thin_adoptable", "vmtool_nvme_thin_zeroing", "vmtool_nvme_thin_profile_now"} {
			e, _ := sf[k].(string)
			if e == "" {
				t.Fatalf("set_fact lacks %s", k)
			}
			vars[k] = e
		}
		wantEval(t, c.label, "vmtool_nvme_thin_present", vars, c.present)
		wantEval(t, c.label, "vmtool_nvme_thin_adoptable", vars, c.adopt)
		wantEval(t, c.label, "vmtool_nvme_thin_zeroing", vars, c.zeroed)
		wantEval(t, c.label, "vmtool_nvme_thin_profile_now", vars, c.profile)
	}

	// -l for a percentage, -L for a size, from the very expression in the argv.
	code := norm(argvText(thinTask(t, "create the pool")))
	m := regexp.MustCompile(`\['-l' if [^\]]*\]`).FindString(code)
	if m == "" {
		t.Fatalf("no size-flag expression in: %s", code)
	}
	for size, want := range map[string]string{"70%FREE": "-l 70%FREE", "100%VG": "-l 100%VG", "2T": "-L 2T", "500G": "-L 500G"} {
		got, ok := ansibleDebugVar(t, "_size_args", map[string]any{"_size_args": "{{ " + m + " | join(' ') }}", "vmtool_nvme_thin_size": size})
		if ok && got != want {
			t.Errorf("size %s: args %#v, want %q", size, got, want)
		}
	}
}

// ---------------------------------------------------------------------- D2
// Discards: issue_discards for thick volumes, and a one-time discard of the
// free space before a thin pool is made. Shape tests only; nothing here runs
// ansible (the regexp and expression checks are Go or opt-in) or any lvm tool.

const (
	lvmConfPath  = "/etc/lvm/lvm.conf"
	scratchLV    = "vmtool-discard-scratch"
	scratchGuard = "not (vmtool_nvme_thin_present | bool)"

	scratchLook    = "Thin pool: look for a leftover scratch volume"
	scratchRead    = "Thin pool: read the leftover scratch volume"
	scratchNotOurs = "Thin pool: fail on a scratch volume that vmtool did not make"
	scratchMake    = "Thin pool: create the discard scratch volume"
	scratchDiscard = "Thin pool: discard the scratch volume"
	scratchStop    = "Thin pool: stop when the scratch discard failed"
	scratchRemove  = "Thin pool: remove the scratch volume"
	poolCreate     = "Thin pool: create the pool"
)

func TestNvmeIssueDiscardsInLvmConf(t *testing.T) {
	tasks := nvmeTasks(t)
	var edit botTask
	n := 0
	for _, tk := range tasks {
		if li, ok := tk.body["ansible.builtin.lineinfile"].(map[string]any); ok && li["path"] == lvmConfPath {
			edit = tk
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one lineinfile on %s, got %d", lvmConfPath, n)
	}
	li, _ := edit.body["ansible.builtin.lineinfile"].(map[string]any)
	if line, _ := li["line"].(string); strings.TrimSpace(line) != "issue_discards = 1" {
		t.Errorf("line = %q, want issue_discards = 1", li["line"])
	}
	if re, _ := li["regexp"].(string); !strings.Contains(re, "issue_discards") {
		t.Errorf("regexp must key on issue_discards: %q", li["regexp"])
	}
	if ia, _ := li["insertafter"].(string); !strings.Contains(ia, "devices") {
		t.Errorf("a missing line must go after `devices {`: insertafter = %q", li["insertafter"])
	}
	if li["create"] == true {
		t.Errorf("lvm.conf must not be created")
	}
	if !hasWhen(edit.whens, nvmeGuard) || hasWhen(edit.whens, thinGuard) {
		t.Errorf("the lvm.conf edit runs with the stripe, not only with the thin pool: %v", edit.whens)
	}
	// After the volume group exists (preflight is over), before the pool.
	vg := firstIndex(tasks, func(b botTask) bool { return b.name == "Create the volume group" })
	if vg < 0 || edit.index < vg {
		t.Errorf("the lvm.conf edit (%d) must follow the preflight and the VG create (%d)", edit.index, vg)
	}
	// The check: lvmconfig prints issue_discards=1 or the play fails.
	read := firstIndex(tasks, func(b botTask) bool { return strings.Contains(argvText(b), "lvmconfig devices/issue_discards") })
	if read < 0 {
		t.Fatal("no `lvmconfig devices/issue_discards` task")
	}
	rt := tasks[read]
	if rt.body["changed_when"] != false || rt.body["check_mode"] != false || rt.body["failed_when"] != false {
		t.Errorf("the lvmconfig read must be read-only and leave the verdict to the fail task")
	}
	fail := firstIndex(tasks, func(b botTask) bool {
		return b.body["ansible.builtin.fail"] != nil && strings.Contains(b.text(), "issue_discards=1")
	})
	if fail < 0 {
		t.Fatal("no fail task on issue_discards=1")
	}
	ft := tasks[fail]
	if !hasWhen(ft.whens, "not ansible_check_mode") || !hasWhen(ft.whens, "issue_discards=1") || !hasWhen(ft.whens, nvmeGuard) {
		t.Errorf("the check fails the play unless issue_discards=1, not under --check: %v", ft.whens)
	}
	if !(edit.index < read && read < fail) {
		t.Errorf("want edit (%d) < lvmconfig read (%d) < fail (%d)", edit.index, read, fail)
	}
	pool := firstIndex(tasks, func(b botTask) bool { return strings.Contains(b.text(), "pool-info") })
	if fail > pool {
		t.Errorf("the check (%d) must come before the libvirt pool (%d)", fail, pool)
	}
}

// The line is found whether commented, 0 or already 1 (so a file that is right
// is left alone and no second line is added), and nothing else matches.
func TestNvmeIssueDiscardsRegexp(t *testing.T) {
	var re string
	for _, tk := range nvmeTasks(t) {
		if li, ok := tk.body["ansible.builtin.lineinfile"].(map[string]any); ok && li["path"] == lvmConfPath {
			re, _ = li["regexp"].(string)
		}
	}
	if re == "" {
		t.Fatal("no regexp on the lvm.conf edit")
	}
	rx, err := regexp.Compile(re)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"\tissue_discards = 0", "\t# issue_discards = 0", "    issue_discards=0", "issue_discards = 1", "\tissue_discards = 1"} {
		if !rx.MatchString(l) {
			t.Errorf("regexp %q must match %q", re, l)
		}
	}
	for _, l := range []string{"\t# Configuration option devices/issue_discards.", "\tissue_discards_x = 1", "\tthin_pool_discards = 1", "devices {", ""} {
		if rx.MatchString(l) {
			t.Errorf("regexp %q must not match %q", re, l)
		}
	}
}

// scratchTasks are the tasks that mention the scratch volume.
func scratchTasks(t *testing.T) []botTask {
	t.Helper()
	var out []botTask
	for _, tk := range nvmeTasks(t) {
		if strings.Contains(tk.text(), "scratch") {
			out = append(out, tk)
		}
	}
	if len(out) != 7 {
		t.Fatalf("want 7 scratch tasks, found %d", len(out))
	}
	return out
}

// None of the scratch tasks can run when the pool exists.
func TestNvmeScratchGuardedLikeThePoolCreate(t *testing.T) {
	for _, tk := range scratchTasks(t) {
		for _, g := range []string{nvmeGuard, thinGuard, scratchGuard} {
			if !hasWhen(tk.whens, g) {
				t.Errorf("scratch task %q lacks when: %s", tk.name, g)
			}
		}
		if !strings.HasPrefix(tk.name, thinPrefix) {
			t.Errorf("scratch task %q must be named %q...", tk.name, thinPrefix)
		}
	}
	if cr := nvmeTaskNamed(t, poolCreate); !hasWhen(cr.whens, scratchGuard) {
		t.Errorf("the pool create lost its guard")
	}
}

// The scratch volume is the only thing discarded or removed, never forced.
func TestNvmeScratchOnlyScratchIsDiscarded(t *testing.T) {
	var discards, removes int
	for _, tk := range nvmeTasks(t) {
		txt := tk.text()
		argv := argvText(tk)
		isCmd := tk.body["ansible.builtin.command"] != nil
		if strings.Contains(txt, "blkdiscard") {
			discards++
			if !isCmd || norm(argv) != "blkdiscard /dev/{{ vmtool_nvme_vg }}/"+scratchLV {
				t.Errorf("blkdiscard only as `blkdiscard /dev/{{ vmtool_nvme_vg }}/%s`: task %q has %q", scratchLV, tk.name, argv)
			}
		}
		if strings.Contains(txt, "lvremove") {
			if isCmd {
				removes++
				if norm(argv) != "lvremove -y {{ vmtool_nvme_vg }}/"+scratchLV {
					t.Errorf("lvremove only as `lvremove -y {{ vmtool_nvme_vg }}/%s`: task %q has %q", scratchLV, tk.name, argv)
				}
			} else if tk.body["ansible.builtin.fail"] == nil {
				t.Errorf("task %q mentions lvremove and is neither the removal nor a fail message", tk.name)
			}
		}
		for _, f := range strings.Fields(argv) {
			if (f == "-f" || f == "--force") && (strings.Contains(txt, "blkdiscard") || strings.Contains(txt, "lvremove")) {
				t.Errorf("task %q forces: %q", tk.name, argv)
			}
		}
	}
	if discards != 1 || removes != 1 {
		t.Errorf("want one blkdiscard task and one lvremove task, got %d and %d", discards, removes)
	}
	if n := strings.Count(nvmeCode(t), "blkdiscard"); n != 1 {
		t.Errorf("blkdiscard appears %d times in the code, want 1 (no message names it)", n)
	}
}

func TestNvmeScratchSequenceAndFailure(t *testing.T) {
	look := nvmeTaskNamed(t, scratchLook)
	read := nvmeTaskNamed(t, scratchRead)
	notOurs := nvmeTaskNamed(t, scratchNotOurs)
	mk := nvmeTaskNamed(t, scratchMake)
	disc := nvmeTaskNamed(t, scratchDiscard)
	stop := nvmeTaskNamed(t, scratchStop)
	rm := nvmeTaskNamed(t, scratchRemove)
	pool := nvmeTaskNamed(t, poolCreate)
	probe := thinTask(t, "read the probe")
	refuse := thinTask(t, "refuse")
	if !(probe.index < refuse.index && refuse.index < look.index && look.index < read.index && read.index < notOurs.index &&
		notOurs.index < mk.index && mk.index < disc.index && disc.index < stop.index && stop.index < rm.index && rm.index < pool.index) {
		t.Errorf("order: probe %d refuse %d look %d read %d not-ours %d create %d discard %d stop %d remove %d pool %d",
			probe.index, refuse.index, look.index, read.index, notOurs.index, mk.index, disc.index, stop.index, rm.index, pool.index)
	}
	// The leftover probe is read-only; the tag decides whether it is ours.
	lt := norm(argvText(look))
	if !strings.HasPrefix(lt, "lvs ") || !strings.Contains(lt, "lv_tags") || !strings.HasSuffix(lt, "{{ vmtool_nvme_vg }}/"+scratchLV) {
		t.Errorf("leftover probe argv = %q", lt)
	}
	if look.body["changed_when"] != false || look.body["check_mode"] != false || look.body["failed_when"] != false {
		t.Errorf("the leftover probe must be read-only")
	}
	sf, _ := read.body["ansible.builtin.set_fact"].(map[string]any)
	if ours, _ := sf["vmtool_nvme_scratch_ours"].(string); !strings.Contains(ours, "'vmtool_scratch' in") {
		t.Errorf("the tag decides whether the leftover is ours: %v", sf)
	}
	if notOurs.body["ansible.builtin.fail"] == nil || !hasWhen(notOurs.whens, "vmtool_nvme_scratch_present | bool") || !hasWhen(notOurs.whens, "not (vmtool_nvme_scratch_ours | bool)") {
		t.Errorf("a leftover without the tag must fail the play before anything touches it: %v", notOurs.whens)
	}
	// Created as the step says, only when there is no leftover to reuse.
	mc := norm(argvText(mk))
	if !strings.HasPrefix(mc, "lvcreate -l 100%FREE -n "+scratchLV+" ") || !strings.Contains(mc, "--addtag vmtool_scratch") || !strings.HasSuffix(mc, "{{ vmtool_nvme_vg }}") {
		t.Errorf("scratch lvcreate argv = %q", mc)
	}
	if !hasWhen(mk.whens, "not (vmtool_nvme_scratch_present | bool)") {
		t.Errorf("a leftover is reused, not made again: %v", mk.whens)
	}
	// The discard's failure is read, not raised, so the message is ours.
	if disc.body["register"] != "vmtool_nvme_scratch_discard" || disc.body["failed_when"] != false {
		t.Errorf("the discard must register vmtool_nvme_scratch_discard and set failed_when: false")
	}
	if stop.body["ansible.builtin.fail"] == nil || !hasWhen(stop.whens, "vmtool_nvme_scratch_discard.rc | default(0) != 0") {
		t.Errorf("a failed discard must stop the play: %v", stop.whens)
	}
	st := norm(stop.text())
	for _, w := range []string{scratchLV, "was left in place", "holds no data of value", "lvremove {{ vmtool_nvme_vg }}/" + scratchLV, "was not created"} {
		if !strings.Contains(st, w) {
			t.Errorf("the failed-discard message lacks %q", w)
		}
	}
	// lvremove runs only after a discard that succeeded.
	if !hasWhen(rm.whens, "vmtool_nvme_scratch_discard.rc | default(1) == 0") {
		t.Errorf("lvremove must be gated on a successful discard: %v", rm.whens)
	}
}

func TestNvmeThinDiscardsPassdown(t *testing.T) {
	if !strings.Contains(norm(thinTask(t, "probe").text()), "discards") {
		t.Errorf("the probe must read the discards column")
	}
	rp := thinTask(t, "read the probe")
	if sf, _ := rp.body["ansible.builtin.set_fact"].(map[string]any); sf["vmtool_nvme_thin_discards"] == nil {
		t.Errorf("the probe's set_fact lacks vmtool_nvme_thin_discards")
	}
	set := nvmeTaskNamed(t, "Thin pool: pass discards down on an adopted pool")
	stop := nvmeTaskNamed(t, "Thin pool: stop when LVM would not set discards")
	refuse := thinTask(t, "refuse")
	if norm(argvText(set)) != "lvchange --discards passdown {{ vmtool_nvme_vg }}/{{ vmtool_nvme_thin_pool }}" {
		t.Errorf("set argv = %q", argvText(set))
	}
	for _, w := range []string{"vmtool_nvme_thin_adoptable | bool", "vmtool_nvme_thin_discards != 'passdown'"} {
		if !hasWhen(set.whens, w) {
			t.Errorf("setting discards must be guarded by %q: %v", w, set.whens)
		}
	}
	if set.body["failed_when"] != false || set.body["register"] != "vmtool_nvme_thin_discards_change" {
		t.Errorf("the change must register vmtool_nvme_thin_discards_change with failed_when: false")
	}
	if stop.body["ansible.builtin.fail"] == nil || !hasWhen(stop.whens, "vmtool_nvme_thin_discards_change.rc | default(0) != 0") {
		t.Errorf("a refused change must stop the play: %v", stop.whens)
	}
	st := strings.ToLower(norm(stop.text()))
	for _, w := range []string{"ignore", "inactive", "nothing was changed"} {
		if !strings.Contains(st, w) {
			t.Errorf("the refusal message lacks %q", w)
		}
	}
	if !(refuse.index < set.index && set.index < stop.index) {
		t.Errorf("want refuse (%d) < set (%d) < stop (%d)", refuse.index, set.index, stop.index)
	}
}
