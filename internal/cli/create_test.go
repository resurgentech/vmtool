package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// stubTunnelExe is a tunnel path that is not a test binary; the stub ansible
// never runs it.
const stubTunnelExe = "/opt/vmtool/vmtool"

// createFake is a Manager on which create gets as far as the address wait.
type createFake struct {
	cliFake
	log *createLog
	ip  string
}

type createLog struct {
	mu    sync.Mutex
	ops   []string
	waits []time.Duration
}

func (l *createLog) add(s string) {
	l.mu.Lock()
	l.ops = append(l.ops, s)
	l.mu.Unlock()
}

func (l *createLog) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, o := range l.ops {
		if o == s {
			return true
		}
	}
	return false
}

func (c createFake) ImagePath(n string) (string, error) { return "/pool/" + n, nil }
func (c createFake) CloneImage(_, n, _ string, _ uint64) (string, error) {
	return "/pool/" + n + ".qcow2", nil
}
func (c createFake) ListPools() ([]vmtool.PoolInfo, error) { return nil, nil }
func (c createFake) ResizeVolume(string, uint64) error     { return nil }
func (c createFake) Define(vmtool.VMConfig) error          { c.log.add("Define"); return nil }
func (c createFake) Start(string) error                    { c.log.add("Start"); return nil }
func (c createFake) Destroy(string) error                  { c.log.add("Destroy"); return nil }
func (c createFake) Delete(string, bool) error             { c.log.add("Delete"); return nil }
func (c createFake) WaitForIP(_ string, d time.Duration) (string, error) {
	c.log.add("WaitForIP")
	c.log.mu.Lock()
	c.log.waits = append(c.log.waits, d)
	c.log.mu.Unlock()
	if c.ip == "" {
		return "", errors.New("timed out waiting for IP")
	}
	return c.ip, nil
}
func (c createFake) Info(n string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: n, IP: c.ip, State: vmtool.StateRunning}, nil
}

func installCreateFake(t *testing.T, ip string) *createLog {
	t.Helper()
	log := &createLog{}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith("jetfire", config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return createFake{log: log, ip: ip}, func() {}, nil
		})
	}
	return log
}

type createRun struct {
	out string
	err error
	env *env
}

// runCreate runs `vmtool create jetfire/web u.qcow2 <args>` through the real
// command tree with stub ansible and a stub tunnel path. cfg is extra
// config.yml text.
func runCreate(t *testing.T, cfg string, args ...string) createRun {
	t.Helper()
	testexec.Ansible(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: jetfire\n"+cfg+goodConfig(), "")
	var out bytes.Buffer
	root, e := rootFor(dir, &out, io.Discard)
	e.tunnelExe = stubTunnelExe
	root.SetArgs(append([]string{"create", "jetfire/web", "u.qcow2"}, args...))
	err := root.Execute()
	return createRun{out: out.String(), err: err, env: e}
}

func TestCreateSuccessPrintsCreatedLineAndIP(t *testing.T) {
	installCreateFake(t, "10.0.0.5")
	r := runCreate(t, "", "--inventory", "")
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []string{`VM "jetfire/web" created and started`, "IP: 10.0.0.5"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, "did not finish") {
		t.Errorf("success output has the failure line:\n%s", r.out)
	}
}

func TestCreateNoAddressPrintsExistsLineAndFails(t *testing.T) {
	log := installCreateFake(t, "")
	r := runCreate(t, "", "--inventory", "")
	if r.err == nil || !strings.Contains(r.err.Error(), "got no address") {
		t.Fatalf("err = %v, want the no-address error", r.err)
	}
	if !strings.Contains(r.out, `VM "jetfire/web" exists and is running, but create did not finish:`) {
		t.Errorf("no exists-and-running line:\n%s", r.out)
	}
	if strings.Contains(r.out, "created and started") || strings.Contains(r.out, "IP:") {
		t.Errorf("success lines printed on failure:\n%s", r.out)
	}
	if log.has("Destroy") || log.has("Delete") {
		t.Error("the VM was removed")
	}
}

// A failure at a later stage (here the grow) gets the same line and no
// success line.
func TestCreateLaterStageFailurePrintsExistsLine(t *testing.T) {
	installCreateFake(t, "10.0.0.5")
	t.Setenv("STUB_GROW_FAIL", "1")
	t.Setenv("STUB_GROW_OUT", "growpart: boom")
	r := runCreate(t, "", "--inventory", "", "--disk-size", "20")
	if r.err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(r.out, `VM "jetfire/web" exists and is running, but create did not finish:`) {
		t.Errorf("no exists-and-running line:\n%s", r.out)
	}
	if strings.Contains(r.out, "created and started") || strings.Contains(r.out, "IP:") {
		t.Errorf("success lines printed on failure:\n%s", r.out)
	}
}

func TestCreateWaitSSHFailurePrintsExistsLine(t *testing.T) {
	installCreateFake(t, "10.0.0.5")
	t.Setenv("STUB_WAIT_FAIL", "1")
	// A 1 s wait is one probe: the retry would end past the deadline.
	r := runCreate(t, "ssh_wait_seconds: 1\n", "--inventory", "")
	if r.err == nil || !strings.Contains(r.err.Error(), "did not accept SSH") {
		t.Fatalf("err = %v", r.err)
	}
	if !strings.Contains(r.out, "exists and is running, but create did not finish:") || strings.Contains(r.out, "created and started") {
		t.Errorf("output:\n%s", r.out)
	}
}

func TestCreateNoWaitFlag(t *testing.T) {
	log := installCreateFake(t, "")
	r := runCreate(t, "", "--inventory", "", "--no-wait")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if log.has("WaitForIP") || !log.has("Start") {
		t.Errorf("ops = %v", log.ops)
	}
	if !strings.Contains(r.out, `VM "jetfire/web" created and started`) || strings.Contains(r.out, "IP:") {
		t.Errorf("output:\n%s", r.out)
	}
}

func TestCreateNoWaitWithPlaybookRefused(t *testing.T) {
	log := installCreateFake(t, "10.0.0.5")
	r := runCreate(t, "", "--inventory", "", "--no-wait", "--playbook", "x.yml")
	if r.err == nil || !strings.Contains(r.err.Error(), "--no-wait") || !strings.Contains(r.err.Error(), "--playbook") {
		t.Fatalf("err = %v", r.err)
	}
	if log.has("Define") {
		t.Error("a domain was defined")
	}
	if strings.Contains(r.out, "did not finish") {
		t.Errorf("a refused create says the VM exists:\n%s", r.out)
	}
}

func TestCreateUsesConfigIPWait(t *testing.T) {
	log := installCreateFake(t, "10.0.0.5")
	r := runCreate(t, "ip_wait_seconds: 45\nssh_wait_seconds: 50\n", "--inventory", "")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(log.waits) != 1 || log.waits[0] != 45*time.Second {
		t.Errorf("WaitForIP timeouts = %v, want [45s]", log.waits)
	}
}

func TestServiceGetsConfigWaits(t *testing.T) {
	installCreateFake(t, "")
	e := &env{cfg: &config.Config{IPWaitSeconds: 45, SSHWaitSeconds: 50}, tunnelExe: stubTunnelExe}
	s := e.service("")
	if s.IPTimeout != 45*time.Second || s.SSHTimeout != 50*time.Second {
		t.Errorf("IPTimeout %s SSHTimeout %s", s.IPTimeout, s.SSHTimeout)
	}
}

func TestParseExtraVarsRefusesBadName(t *testing.T) {
	if _, err := parseExtraVars([]string{"foo-bar=1"}); err == nil || !strings.Contains(err.Error(), "foo-bar") {
		t.Fatalf("err = %v", err)
	}
	if got, err := parseExtraVars([]string{"ok_1=a=b"}); err != nil || got["ok_1"] != "a=b" {
		t.Fatalf("good: %v %v", got, err)
	}
}
