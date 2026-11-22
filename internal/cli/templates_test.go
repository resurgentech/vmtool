package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// tplHost is a thin-pool host: it lists a fixed table and records every
// template call. Nothing is ever removed: it is a fake.
type tplHost struct {
	app.Manager
	mu    *sync.Mutex
	calls *[]string
	tpls  []app.TemplateInfo
	err   error
}

func (h tplHost) rec(s string) {
	h.mu.Lock()
	*h.calls = append(*h.calls, s)
	h.mu.Unlock()
}

func (h tplHost) ListTemplates() ([]app.TemplateInfo, error) {
	h.rec("list")
	return h.tpls, h.err
}
func (h tplHost) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	h.rec("prepare " + image + " " + pool)
	return app.TemplateInfo{Template: vmtool.Template{Name: image, Image: image, Pool: "thin-test", LV: "vmtpl-aaaaaaaaaaaa", Origin: "image", SizeBytes: 10 << 30}}, h.err
}
func (h tplHost) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	h.rec("delete " + name + " " + pool)
	lv := "vmtpl-aaaaaaaaaaaa"
	if strings.HasPrefix(name, "vmtpl-") {
		lv = name
	}
	return app.TemplateInfo{Template: vmtool.Template{Name: "img1", Pool: "thin-test", LV: lv, Clones: 2}}, h.err
}

func installTemplateHosts(t *testing.T, mgrs map[string]app.Manager) {
	t.Helper()
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		var configured []string
		for n := range e.cfg.Hosts {
			configured = append(configured, n)
		}
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, configured, func() (app.Manager, func(), error) {
			return mgrs[e.cfg.Name], func() {}, nil
		}).WithRemote(func(host, token string) (app.Manager, func(), error) {
			return mgrs[host], func() {}, nil
		})
	}
}

func tmpl(lv, name, origin, sha string, stale bool, clones int) app.TemplateInfo {
	t := vmtool.Template{Name: name, LV: lv, Pool: "thin-test", Origin: origin, SHA256: sha, SizeBytes: 10 << 30, UsedBytes: 1 << 30, Clones: clones,
		Created: time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)}
	if origin == vmtool.TemplateOriginImage {
		t.Image = name
	}
	return app.TemplateInfo{Template: t, Stale: stale}
}

func templatesFixture(t *testing.T) (dir string, calls *[]string) {
	t.Helper()
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir = writeWorkdir(t, twoHostCfg("  veritech02: { address: v2.labtwo, ssh_user: ubuntu }\n"), "")
	calls = &[]string{}
	mu := &sync.Mutex{}
	sha := strings.Repeat("ab", 32)
	installTemplateHosts(t, map[string]app.Manager{
		"jetfire": listMgr{}, // local libvirt: no thin pools
		"veritech01": tplHost{mu: mu, calls: calls, tpls: []app.TemplateInfo{
			tmpl("vmtpl-abababababab", "img1", "image", sha, false, 2),
			tmpl("vmtpl-cdcdcdcdcdcd", "img1", "image", strings.Repeat("cd", 32), true, 0),
			tmpl("vmtpl-000000000001", "proj", "saved", "", false, 1),
		}},
		"veritech02": tplHost{mu: mu, calls: calls, err: errors.New("daemon down")},
	})
	return dir, calls
}

func TestTemplatesListAllHosts(t *testing.T) {
	dir, _ := templatesFixture(t)
	r := runGate(t, dir, map[string]bool{"vmtool templates list": true}, "templates", "list")
	if r.err != nil {
		t.Fatalf("list: %v (a failed host must not fail the listing)", r.err)
	}
	want := `NAME ORIGIN IMAGE SHA256 POOL SIZE USED CLONES STALE CREATED EXPORT
veritech01/img1 image img1 abababababab thin-test 10G 1G 2 no 2026-11-18 10:30 -
veritech01/img1 image img1 cdcdcdcdcdcd thin-test 10G 1G 0 yes 2026-11-18 10:30 -
veritech01/proj saved - - thin-test 10G 1G 1 no 2026-11-18 10:30 -
veritech02 ERROR: list_templates: daemon down
`
	if got := norm(r.out); got != want {
		t.Errorf("list output:\n%s\nwant:\n%s", got, want)
	}
}

func TestTemplatesListStaleAndHost(t *testing.T) {
	dir, _ := templatesFixture(t)
	keep := map[string]bool{"vmtool templates list": true}
	r := runGate(t, dir, keep, "templates", "list", "--stale")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if strings.Contains(r.out, "abababababab") || strings.Contains(r.out, "proj") || !strings.Contains(r.out, "cdcdcdcdcdcd") {
		t.Errorf("--stale output:\n%s", r.out)
	}
	r = runGate(t, dir, keep, "templates", "list", "--host", "veritech01", "--stale")
	if r.err != nil || strings.Contains(r.out, "veritech02") || strings.Count(r.out, "\n") != 2 {
		t.Errorf("--host veritech01 --stale: %v\n%s", r.err, r.out)
	}
	// one host that fails is the command's error
	r = runGate(t, dir, keep, "templates", "list", "--host", "veritech02")
	if r.err == nil || !strings.Contains(r.err.Error(), "daemon down") {
		t.Errorf("--host veritech02: %v", r.err)
	}
	r = runGate(t, dir, keep, "templates", "list", "--host", "nosuch")
	if !errors.Is(r.err, hosts.ErrUnknownHost) {
		t.Errorf("--host nosuch: %v", r.err)
	}
	// the own name has no daemon and no thin pools: no rows, no error
	r = runGate(t, dir, keep, "templates", "list", "--host", "jetfire")
	if r.err != nil || strings.Count(r.out, "\n") != 1 {
		t.Errorf("--host jetfire: %v\n%s", r.err, r.out)
	}
}

func TestTemplatesPrepare(t *testing.T) {
	dir, calls := templatesFixture(t)
	keep := map[string]bool{"vmtool templates prepare": true}
	r := runGate(t, dir, keep, "templates", "prepare", "veritech01", "img1", "--pool", "thin-test")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := strings.Join(*calls, "|"); got != "prepare img1 thin-test" {
		t.Errorf("calls %q", got)
	}
	if !strings.Contains(r.out, "vmtpl-aaaaaaaaaaaa") || !strings.Contains(r.out, "veritech01") {
		t.Errorf("output %q", r.out)
	}
	// no --pool: the host's default is the host's to resolve
	*calls = nil
	if r := runGate(t, dir, keep, "templates", "prepare", "veritech01", "img1"); r.err != nil || strings.Join(*calls, "|") != "prepare img1 " {
		t.Errorf("no pool: %v %q", r.err, *calls)
	}
	// argument parsing: host and image are both required; the host must be known
	for _, args := range [][]string{
		{"templates", "prepare"},
		{"templates", "prepare", "veritech01"},
		{"templates", "prepare", "veritech01", "img1", "extra"},
		{"templates", "prepare", "veritech01/img1"},
	} {
		*calls = nil
		if r := runGate(t, dir, keep, args...); r.err == nil || len(*calls) != 0 {
			t.Errorf("%v: err %v calls %q", args, r.err, *calls)
		}
	}
	if r := runGate(t, dir, keep, "templates", "prepare", "nosuch", "img1"); !errors.Is(r.err, hosts.ErrUnknownHost) {
		t.Errorf("unknown host: %v", r.err)
	}
}

func TestTemplatesDelete(t *testing.T) {
	dir, calls := templatesFixture(t)
	keep := map[string]bool{"vmtool templates delete": true}
	r := runGate(t, dir, keep, "templates", "delete", "veritech01/img1")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := strings.Join(*calls, "|"); got != "delete img1 " {
		t.Errorf("calls %q", got)
	}
	if !strings.Contains(r.out, "vmtpl-aaaaaaaaaaaa") || !strings.Contains(r.out, "2 clones") {
		t.Errorf("output %q", r.out)
	}
	*calls = nil
	if r := runGate(t, dir, keep, "templates", "delete", "veritech01/vmtpl-cdcdcdcdcdcd", "--pool", "thin-test"); r.err != nil || strings.Join(*calls, "|") != "delete vmtpl-cdcdcdcdcdcd thin-test" {
		t.Errorf("by LV and pool: %v %q", r.err, *calls)
	}
	for _, args := range [][]string{
		{"templates", "delete"},
		{"templates", "delete", "img1"},            // a bare name: the host is required
		{"templates", "delete", "veritech01/"},     // no name
		{"templates", "delete", "/img1"},           // no host
		{"templates", "delete", "veritech01/a/b"},  // a path
		{"templates", "delete", "veritech01/../x"}, // dots
		{"templates", "delete", "a", "b"},
	} {
		*calls = nil
		if r := runGate(t, dir, keep, args...); r.err == nil || len(*calls) != 0 {
			t.Errorf("%v: err %v calls %q", args, r.err, *calls)
		}
	}
	if r := runGate(t, dir, keep, "templates", "delete", "nosuch/img1"); !errors.Is(r.err, hosts.ErrUnknownHost) {
		t.Errorf("unknown host: %v", r.err)
	}
}

func TestTemplatesPrune(t *testing.T) {
	dir, calls := templatesFixture(t)
	keep := map[string]bool{"vmtool templates prune": true}
	r := runGate(t, dir, keep, "templates", "prune", "veritech01")
	if r.err != nil {
		t.Fatal(r.err)
	}
	// the one stale template, by LV name and pool; never the current or the saved
	if got := strings.Join(*calls, "|"); got != "list|delete vmtpl-cdcdcdcdcdcd thin-test" {
		t.Errorf("calls %q", got)
	}
	if !strings.Contains(r.out, "vmtpl-cdcdcdcdcdcd") || !strings.Contains(r.out, "1 stale") {
		t.Errorf("output %q", r.out)
	}
	for _, args := range [][]string{{"templates", "prune"}, {"templates", "prune", "a", "b"}} {
		if r := runGate(t, dir, keep, args...); r.err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// nothing stale: nothing removed, and it says so
	*calls = nil
	if r := runGate(t, dir, keep, "templates", "prune", "jetfire"); r.err != nil || !strings.Contains(r.out, "No stale") {
		t.Errorf("jetfire: %v %q", r.err, r.out)
	}
}

func TestWriteTemplateTable(t *testing.T) {
	var b bytes.Buffer
	rows := []app.HostTemplates{{Host: "hv1", Templates: []app.TemplateInfo{{Template: vmtool.Template{Name: "img1", Image: "img1", Origin: "image", SHA256: strings.Repeat("a", 64), Pool: "thin-test", SizeBytes: 5 << 30}}}}}
	if err := writeTemplateTable(&b, rows, false); err != nil {
		t.Fatal(err)
	}
	// an unknown creation time is "-"
	want := "NAME ORIGIN IMAGE SHA256 POOL SIZE USED CLONES STALE CREATED EXPORT\nhv1/img1 image img1 aaaaaaaaaaaa thin-test 5G 0 0 no - -\n"
	if got := norm(b.String()); got != want {
		t.Errorf("table:\n%q\nwant\n%q", got, want)
	}
	_ = fmt.Sprint()
}

func (h tplHost) ListPools() ([]vmtool.PoolInfo, error) {
	return []vmtool.PoolInfo{{Name: "thin-test", Path: "/dev/vg-test", Active: true, Type: vmtool.PoolTypeThin}}, nil
}

// A template with an export volume shows its size, whether the digest is known
// and when the daemon removes it.
func TestWriteTemplateTableShowsTheExport(t *testing.T) {
	created := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	mk := func(name, sha string) app.TemplateInfo {
		return app.TemplateInfo{Template: vmtool.Template{Name: name, Origin: "saved", Pool: "thin-test", SizeBytes: 20 << 30},
			Export: &app.ExportVolume{ThinExport: vmtool.ThinExport{Bytes: 11 << 30, SHA256: sha, Created: created}}}
	}
	var b bytes.Buffer
	rows := []app.HostTemplates{{Host: "hv1", Templates: []app.TemplateInfo{mk("a", ""), mk("b", strings.Repeat("a", 64))}}}
	if err := writeTemplateTable(&b, rows, false); err != nil {
		t.Fatal(err)
	}
	got := norm(b.String())
	for _, want := range []string{
		"hv1/a saved - - thin-test 20G 0 0 no - 11G, sha256 not yet known, expires 2026-11-21 10:30",
		"hv1/b saved - - thin-test 20G 0 0 no - 11G, sha256 known, expires 2026-11-21 10:30",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
}
