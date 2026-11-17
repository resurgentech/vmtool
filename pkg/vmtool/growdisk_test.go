package vmtool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// growEnv is a sandbox for growDiskCommand: stub tools on PATH that log their
// argv, and a temp dir standing in for /sys/class/block.
type growEnv struct {
	dir string
}

func newGrowEnv(t *testing.T, source, fstype, pkname, part, partNum string) *growEnv {
	t.Helper()
	dir := t.TempDir()
	e := &growEnv{dir: dir}
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("source", source+"\n", 0o644)
	write("fstype", fstype+"\n", 0o644)
	write("pkname", pkname+"\n", 0o644)
	write("gp_rc", "0\n", 0o644)
	write("gp_out", "CHANGED: partition resized\n", 0o644)

	stub("findmnt", `case "$*" in *SOURCE*) cat "`+dir+`/source";; *FSTYPE*) cat "`+dir+`/fstype";; esac`)
	stub("lsblk", `echo "lsblk $*" >> "`+dir+`/log"; cat "`+dir+`/pkname"`)
	stub("growpart", `echo "growpart $*" >> "`+dir+`/log"; cat "`+dir+`/gp_out"; exit "$(cat "`+dir+`/gp_rc")"`)
	stub("resize2fs", `echo "resize2fs $*" >> "`+dir+`/log"`)
	stub("btrfs", `echo "btrfs $*" >> "`+dir+`/log"`)

	if part != "" {
		sys := filepath.Join(dir, "sys", part)
		if err := os.MkdirAll(sys, 0o755); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("sys", part, "partition"), partNum+"\n", 0o644)
	} else if err := os.MkdirAll(filepath.Join(dir, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *growEnv) growpart(rc int, out string) {
	os.WriteFile(filepath.Join(e.dir, "gp_rc"), []byte(strconv.Itoa(rc)+"\n"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "gp_out"), []byte(out), 0o644)
}

// run executes growDiskCommand under /bin/sh with only the stubs and
// /usr/bin:/bin on PATH. It returns combined output, the exit error, and the
// stub call log lines.
func (e *growEnv) run(t *testing.T) (string, error, []string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", growDiskCommand)
	cmd.Env = []string{
		"PATH=" + filepath.Join(e.dir, "bin") + ":/usr/bin:/bin",
		"VMTOOL_SYSBLOCK=" + filepath.Join(e.dir, "sys"),
	}
	out, err := cmd.CombinedOutput()
	logb, _ := os.ReadFile(filepath.Join(e.dir, "log"))
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(logb)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return string(out), err, lines
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestGrowDiskCommandFindsRootPartition(t *testing.T) {
	cases := []struct {
		name, source, fstype, disk, part, num string
		wantGrow, wantFS                      string
	}{
		{"vda2 ext4", "/dev/vda2", "ext4", "vda", "vda2", "2",
			"growpart /dev/vda 2", "resize2fs /dev/vda2"},
		{"vda1 ext4", "/dev/vda1", "ext4", "vda", "vda1", "1",
			"growpart /dev/vda 1", "resize2fs /dev/vda1"},
		{"btrfs subvol suffix", "/dev/vda2[/@]", "btrfs", "vda", "vda2", "2",
			"growpart /dev/vda 2", "btrfs filesystem resize max /"},
		{"btrfs plain", "/dev/vdb3", "btrfs", "vdb", "vdb3", "3",
			"growpart /dev/vdb 3", "btrfs filesystem resize max /"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newGrowEnv(t, c.source, c.fstype, c.disk, c.part, c.num)
			out, err, log := e.run(t)
			if err != nil {
				t.Fatalf("want success, got %v\n%s", err, out)
			}
			if !hasLine(log, c.wantGrow) {
				t.Errorf("missing %q in log %q", c.wantGrow, log)
			}
			if !hasLine(log, c.wantFS) {
				t.Errorf("missing %q in log %q", c.wantFS, log)
			}
			if c.fstype == "btrfs" {
				for _, l := range log {
					if strings.HasPrefix(l, "resize2fs") {
						t.Errorf("resize2fs ran on btrfs: %q", log)
					}
				}
			}
		})
	}
}

func TestGrowDiskCommandNochangeIsSuccess(t *testing.T) {
	e := newGrowEnv(t, "/dev/vda1", "ext4", "vda", "vda1", "1")
	e.growpart(1, "NOCHANGE: partition 1 is size 100. it cannot be grown\n")
	out, err, log := e.run(t)
	if err != nil {
		t.Fatalf("NOCHANGE must be success, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOCHANGE") {
		t.Errorf("growpart output not passed through: %q", out)
	}
	if !hasLine(log, "resize2fs /dev/vda1") {
		t.Errorf("fs step must still run after NOCHANGE: %q", log)
	}
}

func TestGrowDiskCommandGrowpartErrorFails(t *testing.T) {
	e := newGrowEnv(t, "/dev/vda2", "ext4", "vda", "vda2", "2")
	e.growpart(2, "FAILED: sfdisk exploded\n")
	out, err, log := e.run(t)
	if err == nil {
		t.Fatalf("growpart exit 2 must fail the grow\n%s", out)
	}
	if !strings.Contains(out, "FAILED: sfdisk exploded") {
		t.Errorf("growpart output not passed through: %q", out)
	}
	for _, l := range log {
		if strings.HasPrefix(l, "resize2fs") || strings.HasPrefix(l, "btrfs") {
			t.Errorf("fs step ran after growpart failure: %q", log)
		}
	}
}

func TestGrowDiskCommandExit1WithoutNochangeFails(t *testing.T) {
	e := newGrowEnv(t, "/dev/vda2", "ext4", "vda", "vda2", "2")
	e.growpart(1, "FAILED: something else\n")
	if out, err, _ := e.run(t); err == nil {
		t.Fatalf("exit 1 without NOCHANGE must fail\n%s", out)
	}
}

func TestGrowDiskCommandUnresolvableRootFailsClearly(t *testing.T) {
	// LVM: /dev/mapper/... has no sysfs partition entry.
	e := newGrowEnv(t, "/dev/mapper/vg-root", "ext4", "sda", "", "")
	out, err, log := e.run(t)
	if err == nil {
		t.Fatalf("want failure\n%s", out)
	}
	if !strings.Contains(out, "cannot find root partition") {
		t.Errorf("want clear message, got %q", out)
	}
	if hasLine(log, "growpart /dev/sda 0") || len(log) > 1 {
		for _, l := range log {
			if strings.HasPrefix(l, "growpart") {
				t.Errorf("must not guess and run growpart: %q", log)
			}
		}
	}

	// Partition known but no PKNAME (whole-disk filesystem).
	e = newGrowEnv(t, "/dev/vda2", "ext4", "", "vda2", "2")
	out, err, _ = e.run(t)
	if err == nil || !strings.Contains(out, "cannot find root partition") {
		t.Errorf("empty PKNAME must fail clearly: err=%v out=%q", err, out)
	}
}
