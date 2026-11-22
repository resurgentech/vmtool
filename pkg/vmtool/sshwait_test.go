package vmtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/testexec"
)

// probeClock advances only when WaitSSH sleeps.
type probeClock struct {
	t      time.Time
	sleeps int
}

func (f *probeClock) clk() SSHClock {
	return SSHClock{
		Now:   func() time.Time { return f.t },
		Sleep: func(d time.Duration) { f.t = f.t.Add(d); f.sleeps++ },
	}
}

var probeTunnel = Tunnel{Exe: "/opt/vmtool/vmtool", Token: "tok"}

// stubProbeSSH installs `ssh` and `sshpass` stubs. ssh logs its argv and
// SSHPASS; it exits 255 for the first failN calls, then 0.
func stubProbeSSH(t *testing.T, failN int, out string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "ssh.log")
	count := filepath.Join(dir, "count")
	ssh := "#!/bin/sh\necho \"ssh $* [SSHPASS=$SSHPASS]\" >> '" + log + "'\n" +
		"echo x >> '" + count + "'\n" +
		"n=$(wc -l < '" + count + "')\n" +
		"echo '" + out + "'\n" +
		"if [ \"$n\" -le " + string(rune('0'+failN)) + " ]; then exit 255; fi\nexit 0\n"
	pass := "#!/bin/sh\necho \"sshpass $*\" >> '" + log + "'\nshift 2\nexec ssh \"$@\"\n"
	testexec.Stub(t, map[string]string{"ssh": ssh, "sshpass": pass})
	return log
}

func TestWaitSSHRetriesThenSucceeds(t *testing.T) {
	log := stubProbeSSH(t, 3, "ssh: connect refused")
	fc := &probeClock{t: time.Unix(1000, 0)}
	out, err := WaitSSH(probeTunnel, "jetfire", "web", Auth{User: "u", KeyPath: "/k/id"}, 30*time.Second, fc.clk())
	if err != nil {
		t.Fatalf("err = %v, out %q", err, out)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 || fc.sleeps != 3 {
		t.Fatalf("attempts %d sleeps %d, want 4 and 3: %q", len(lines), fc.sleeps, lines)
	}
	l := lines[0]
	for _, want := range []string{
		"ProxyCommand=/opt/vmtool/vmtool tunnel jetfire/web",
		"ConnectTimeout=2", "BatchMode=yes", "StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null", "IdentitiesOnly=yes", "-i /k/id", "-l u",
		" jetfire/web true",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("argv lacks %q: %s", want, l)
		}
	}
}

func TestWaitSSHPasswordGoesThroughSSHPassEnv(t *testing.T) {
	log := stubProbeSSH(t, 0, "")
	fc := &probeClock{t: time.Unix(1000, 0)}
	if _, err := WaitSSH(probeTunnel, "jetfire", "web", Auth{User: "u", Password: "s3cret"}, 30*time.Second, fc.clk()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	s := string(b)
	if !strings.Contains(s, "sshpass -e ssh") {
		t.Errorf("not through sshpass -e: %s", s)
	}
	argv := strings.Split(s, "\n")
	for _, l := range argv {
		if i := strings.Index(l, " [SSHPASS="); i >= 0 {
			l = l[:i]
		}
		if strings.Contains(l, "s3cret") {
			t.Errorf("password in argv: %s", l)
		}
	}
	if !strings.Contains(s, "[SSHPASS=s3cret]") {
		t.Errorf("SSHPASS env not set: %s", s)
	}
	if strings.Contains(s, "BatchMode=yes") || !strings.Contains(s, "NumberOfPasswordPrompts=1") {
		t.Errorf("password login options wrong: %s", s)
	}
}

func TestWaitSSHTimeoutKeepsLastOutput(t *testing.T) {
	stubProbeSSH(t, 9, "boom: sshd not answering")
	fc := &probeClock{t: time.Unix(1000, 0)}
	out, err := WaitSSH(probeTunnel, "jetfire", "web", Auth{User: "u", KeyPath: "/k"}, 3*time.Second, fc.clk())
	if err == nil || !strings.Contains(out, "sshd not answering") {
		t.Fatalf("err %v out %q", err, out)
	}
	if fc.sleeps != 2 {
		t.Errorf("sleeps = %d, want 2 (3 s of 1 s retries)", fc.sleeps)
	}
}

func TestWaitSSHStartsNoAnsible(t *testing.T) {
	stubProbeSSH(t, 0, "")
	alog := filepath.Join(t.TempDir(), "ansible.log")
	dir := os.Getenv(testexec.StubsEnv)
	if err := os.WriteFile(filepath.Join(dir, "ansible"), []byte("#!/bin/sh\necho x >> '"+alog+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fc := &probeClock{t: time.Unix(1000, 0)}
	if _, err := WaitSSH(probeTunnel, "jetfire", "web", Auth{User: "u", KeyPath: "/k"}, time.Minute, fc.clk()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(alog); err == nil {
		t.Error("ansible was started")
	}
}

func TestWaitSSHRefusesBadTunnelAndName(t *testing.T) {
	stubProbeSSH(t, 0, "")
	fc := &probeClock{t: time.Unix(1000, 0)}
	if _, err := WaitSSH(Tunnel{}, "jetfire", "web", Auth{User: "u", Password: "p"}, time.Minute, fc.clk()); err == nil {
		t.Error("no tunnel exe: want error")
	}
	if _, err := WaitSSH(probeTunnel, "jet fire", "web", Auth{User: "u", Password: "p"}, time.Minute, fc.clk()); err == nil {
		t.Error("bad host: want error")
	}
}
