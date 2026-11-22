package hosts

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

const thinHelper = "/usr/local/libexec/vmtool-thin"

// One thin pool nvme-thin (60 GiB, 25 % data, 1.5 % metadata) in vg-test
// holding vm1 (20 GiB) and vm1-2 (10 GiB).
const oneThinPoolReport = `{"report":[{"lv":[` +
	`{"lv_name":"nvme-thin","vg_name":"vg-test","lv_attr":"twi-aotz--","lv_size":"64424509440","pool_lv":"","origin":"","data_percent":"25.00","metadata_percent":"1.50","lv_tags":"vmtool_thinpool","lv_time":""},` +
	`{"lv_name":"vm1","vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"21474836480","pool_lv":"nvme-thin","origin":"","data_percent":"5.00","metadata_percent":"","lv_tags":"vmtool_vol","lv_time":""},` +
	`{"lv_name":"vm1-2","vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"10737418240","pool_lv":"nvme-thin","origin":"","data_percent":"","metadata_percent":"","lv_tags":"vmtool_vol","lv_time":""}` +
	`]}]}`

// thinSudo stubs sudo: it logs its argv (one call per line) and prints
// $STUB_OUT, exiting $STUB_RC with $STUB_ERR on stderr.
func thinSudo(t *testing.T) (log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "sudo.log")
	testexec.Stub(t, map[string]string{"sudo": `#!/bin/sh
echo "$*" >> '` + log + `'
[ -n "$STUB_OUT" ] && printf '%s' "$STUB_OUT"
[ -n "$STUB_ERR" ] && printf '%s\n' "$STUB_ERR" >&2
exit ${STUB_RC:-0}
`})
	return log
}

func sudoCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// libvirtPools is a fake inner Manager: it lists fixed pools and fails every
// other method (the embedded interface is nil), except List, which proves
// passthrough.
type libvirtPools struct {
	app.Manager
	pools []vmtool.PoolInfo
	err   error
}

func (f *libvirtPools) ListPools() ([]vmtool.PoolInfo, error) { return f.pools, f.err }
func (f *libvirtPools) List() ([]vmtool.VMInfo, error)        { return []vmtool.VMInfo{{Name: "vm1"}}, nil }

func logger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == "time" {
			return slog.Attr{}
		}
		return a
	}}))
}

func newThin(inner app.Manager, buf *bytes.Buffer) ThinPools {
	return ThinPools{Manager: inner, Thin: &vmtool.Thin{Helper: thinHelper}, Log: logger(buf)}
}

func TestThinPoolsListedAfterLibvirtPools(t *testing.T) {
	log := thinSudo(t)
	t.Setenv("STUB_OUT", oneThinPoolReport)
	var buf bytes.Buffer
	m := newThin(&libvirtPools{pools: []vmtool.PoolInfo{{Name: "vmtool", Path: "/srv/vmtool", Active: true, Type: "dir"}}}, &buf)
	pools, err := m.ListPools()
	if err != nil || len(pools) != 2 {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	if pools[0].Name != "vmtool" {
		t.Errorf("libvirt pool must come first: %+v", pools)
	}
	p := pools[1]
	if p.Name != "nvme-thin" || p.Type != "thin" || p.Path != "/dev/vg-test" || !p.Active {
		t.Errorf("thin pool = %+v", p)
	}
	if p.SizeBytes == nil || *p.SizeBytes != 64424509440 ||
		p.UsedBytes == nil || *p.UsedBytes != 16106127360 ||
		p.VirtualBytes == nil || *p.VirtualBytes != 32212254720 ||
		p.DataPercent == nil || *p.DataPercent != 25 ||
		p.MetaPercent == nil || *p.MetaPercent != 1.5 {
		t.Errorf("thin numbers = %+v", p)
	}
	if got := sudoCalls(t, log); len(got) != 1 || got[0] != "-n "+thinHelper+" report" {
		t.Errorf("sudo calls = %q", got)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log: %s", buf.String())
	}
}

func TestThinPoolNameClashLeftOutAndLogged(t *testing.T) {
	thinSudo(t)
	t.Setenv("STUB_OUT", oneThinPoolReport)
	var buf bytes.Buffer
	m := newThin(&libvirtPools{pools: []vmtool.PoolInfo{{Name: "nvme-thin", Path: "/srv/x", Active: true, Type: "dir"}}}, &buf)
	pools, err := m.ListPools()
	if err != nil || len(pools) != 1 || pools[0].Type != "dir" {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "nvme-thin") {
		t.Errorf("want one log line naming nvme-thin, got %q", buf.String())
	}
}

func TestThinHelperFailureListsLibvirtPoolsOnly(t *testing.T) {
	thinSudo(t)
	t.Setenv("STUB_RC", "1")
	t.Setenv("STUB_ERR", "sudo: a password is required")
	var buf bytes.Buffer
	m := newThin(&libvirtPools{pools: []vmtool.PoolInfo{{Name: "vmtool", Active: true}}}, &buf)
	pools, err := m.ListPools()
	if err != nil || len(pools) != 1 || pools[0].Name != "vmtool" {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "password is required") {
		t.Errorf("want one log line with the reason, got %q", buf.String())
	}
}

func TestThinNoPoolsIsQuiet(t *testing.T) {
	thinSudo(t)
	t.Setenv("STUB_OUT", `{"report":[{"lv":[]}]}`)
	var buf bytes.Buffer
	m := newThin(&libvirtPools{pools: []vmtool.PoolInfo{{Name: "vmtool"}}}, &buf)
	pools, err := m.ListPools()
	if err != nil || len(pools) != 1 || buf.Len() != 0 {
		t.Fatalf("ListPools = %+v, %v, log %q", pools, err, buf.String())
	}
}

// The inner Manager's own failure is returned, and the helper is not asked.
func TestThinInnerErrorPropagates(t *testing.T) {
	log := thinSudo(t)
	var buf bytes.Buffer
	m := newThin(&libvirtPools{err: os.ErrPermission}, &buf)
	if _, err := m.ListPools(); err == nil {
		t.Fatal("want the inner error")
	}
	if got := sudoCalls(t, log); len(got) != 0 {
		t.Errorf("helper ran: %q", got)
	}
}

func TestThinPassesOtherMethodsThrough(t *testing.T) {
	var buf bytes.Buffer
	m := newThin(&libvirtPools{}, &buf)
	vms, err := m.List()
	if err != nil || len(vms) != 1 || vms[0].Name != "vm1" {
		t.Fatalf("List = %+v, %v", vms, err)
	}
}

// Through the resolver: a hypervisor given a helper lists thin pools; a
// workstation (local libvirt) lists none and never runs the helper.
func TestResolveThinOnlyOnHypervisor(t *testing.T) {
	log := thinSudo(t)
	t.Setenv("STUB_OUT", oneThinPoolReport)
	local := func() (app.Manager, func(), error) {
		return &libvirtPools{pools: []vmtool.PoolInfo{{Name: "vmtool", Active: true}}}, func() {}, nil
	}
	th := &vmtool.Thin{Helper: thinHelper}

	hv := NewWith("hv1", config.RoleHypervisor, nil, local).WithThin(th)
	m, done, err := hv.Resolve("hv1")
	if err != nil {
		t.Fatal(err)
	}
	pools, err := m.ListPools()
	done()
	if err != nil || len(pools) != 2 || pools[1].Name != "nvme-thin" {
		t.Fatalf("hypervisor pools = %+v, %v", pools, err)
	}

	ws := NewWith("box", config.RoleWorkstation, nil, local).WithThin(th)
	m, done, err = ws.Resolve("box")
	if err != nil {
		t.Fatal(err)
	}
	before := len(sudoCalls(t, log))
	pools, err = m.ListPools()
	done()
	if err != nil || len(pools) != 1 || pools[0].Name != "vmtool" {
		t.Fatalf("workstation pools = %+v, %v", pools, err)
	}
	if after := len(sudoCalls(t, log)); after != before {
		t.Errorf("workstation ran the helper")
	}

	// no helper configured: no wrapper, hypervisor or not
	m, done, err = NewWith("hv1", config.RoleHypervisor, nil, local).Resolve("hv1")
	if err != nil {
		t.Fatal(err)
	}
	pools, _ = m.ListPools()
	done()
	if len(pools) != 1 {
		t.Errorf("pools without a helper = %+v", pools)
	}
}

// New gives a hypervisor-role resolver the default helper client and a
// workstation none.
func TestNewWiresThinByRole(t *testing.T) {
	if r := New(&config.Config{Name: "hv1", Role: config.RoleHypervisor}); r.thin == nil {
		t.Error("hypervisor resolver has no thin client")
	}
	if r := New(&config.Config{Name: "box", Role: config.RoleWorkstation}); r.thin != nil {
		t.Error("workstation resolver has a thin client")
	}
	if r := New(nil); r.thin != nil {
		t.Error("default resolver has a thin client")
	}
}
