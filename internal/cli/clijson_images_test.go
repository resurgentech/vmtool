package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// --json for the template, image, pool and creds commands. The cases
// join jsonCases (the contract walk in clicontract_test.go); the focused
// tests pin each shape.

func init() {
	jsonCases["vmtool templates list"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := templatesFixture(t)
		return dir, []string{"templates", "list"}
	}}
	jsonCases["vmtool templates prepare"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := templatesFixture(t)
		return dir, []string{"templates", "prepare", "veritech01", "img1"}
	}}
	jsonCases["vmtool templates delete"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := templatesFixture(t)
		return dir, []string{"templates", "delete", "veritech01/img1"}
	}}
	jsonCases["vmtool templates prune"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := templatesFixture(t)
		return dir, []string{"templates", "prune", "veritech01"}
	}}
	jsonCases["vmtool templates save"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := templatesFixture(t)
		return dir, []string{"templates", "save", "veritech01/vm1", "proj2"}
	}}
	jsonCases["vmtool templates export"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := xferFixture(t, nil, "")
		return dir, []string{"templates", "export", "veritech01/proj"}
	}}
	jsonCases["vmtool images list"] = jsonCase1("images", "list")
	jsonCases["vmtool images delete"] = jsonCase1("images", "delete", "base.qcow2")
	jsonCases["vmtool pool list"] = jsonCase1("pool", "list")
	jsonCases["vmtool pool create"] = jsonCase1("pool", "create", "fast", "/srv/fast")
	jsonCases["vmtool images push"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		s := newCLIStore(t)
		dir := storeWorkdir(t, s, storeTok)
		return dir, []string{"images", "push", placeholderImageFile(t)}
	}}
	jsonCases["vmtool images pull"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		s := newCLIStore(t)
		dir := storeWorkdir(t, s, storeTok)
		s.images["base.qcow2"], s.updated["base.qcow2"] = []byte("PLACEHOLDER"), time.Now()
		return dir, []string{"images", "pull", "base.qcow2", "--to", filepath.Join(t.TempDir(), "out")}
	}}
	jsonCases["vmtool creds fetch"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		c := newCredCLI(t)
		c.s.vmRecord("ws/web", credUUID, credPass, credKey)
		return c.dir, []string{"creds", "fetch", "ws/web"}
	}}
	jsonCases["vmtool creds forget"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		c := newCredCLI(t)
		c.s.vmRecord("ws/web", credUUID, credPass, credKey)
		if _, _, err := c.run("creds", "fetch", "ws/web"); err != nil {
			t.Fatal(err)
		}
		return c.dir, []string{"creds", "forget", "ws/web"}
	}}
}

// placeholderImageFile is a small placeholder image file to push.
func placeholderImageFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(f, []byte("PLACEHOLDER image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func asList(t *testing.T, o map[string]any, key string) []any {
	t.Helper()
	l, ok := o[key].([]any)
	if !ok {
		t.Fatalf("%q is not a list: %v", key, o)
	}
	return l
}

// templates list: templates under a key, each with its host and the API's
// TemplateInfo spelling; an unreachable host is a {host, error} row under
// errors, and the exit status stays 0.
func TestJSONTemplatesList(t *testing.T) {
	dir, _ := templatesFixture(t)
	r, o := runJSON(t, dir, "vmtool templates list", "templates", "list")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	ts := asList(t, o, "templates")
	if len(ts) != 3 {
		t.Fatalf("templates: %v", ts)
	}
	first := ts[0].(map[string]any)
	if first["host"] != "veritech01" || first["name"] != "img1" || first["origin"] != "image" || first["image"] != "img1" ||
		first["lv"] != "vmtpl-abababababab" || first["pool"] != "thin-test" || first["size_bytes"] != float64(10<<30) ||
		first["clones"] != float64(2) || first["stale"] != false || first["created"] != "2026-11-18T10:30:00Z" {
		t.Errorf("first template %v", first)
	}
	if ts[1].(map[string]any)["stale"] != true {
		t.Errorf("second template %v", ts[1])
	}
	errs := asList(t, o, "errors")
	if len(errs) != 1 {
		t.Fatalf("errors: %v", errs)
	}
	row := errs[0].(map[string]any)
	if row["host"] != "veritech02" || !strings.Contains(row["error"].(string), "daemon down") || len(row) != 2 {
		t.Errorf("error row %v", row)
	}
	if _, ok := o["network_homes"].([]any); !ok {
		t.Errorf("network_homes missing: %v", o)
	}
	if strings.Contains(r.out, "ERROR:") || strings.Contains(r.out, "NAME") {
		t.Errorf("table text on stdout:\n%s", r.out)
	}
}

func TestJSONTemplatesListStaleAndHome(t *testing.T) {
	dir, _ := templatesFixture(t)
	_, o := runJSON(t, dir, "vmtool templates list", "templates", "list", "--stale")
	ts := asList(t, o, "templates")
	if len(ts) != 1 || ts[0].(map[string]any)["lv"] != "vmtpl-cdcdcdcdcdcd" {
		t.Errorf("--stale: %v", ts)
	}
	// a network home is its own key, not a template and not an error
	t.Chdir(t.TempDir())
	dir2 := writeWorkdir(t, homeConfig(), "")
	mu, calls := &sync.Mutex{}, &[]string{}
	installHomeTemplateHosts(t, map[string]app.Manager{
		"jetfire": listMgr{},
		"hv1":     tplHost{mu: mu, calls: calls, tpls: []app.TemplateInfo{tmpl("vmtpl-abababababab", "img1", "image", strings.Repeat("ab", 32), false, 2)}},
		"home1":   tplHost{mu: mu, calls: calls, err: errors.New("home1 unreachable")},
	})
	_, o = runJSON(t, dir2, "vmtool templates list", "templates", "list")
	homes := asList(t, o, "network_homes")
	if len(homes) != 1 || homes[0] != "home1" {
		t.Errorf("network_homes %v", homes)
	}
	if len(asList(t, o, "errors")) != 0 || len(asList(t, o, "templates")) != 1 {
		t.Errorf("a network home is no error: %v", o)
	}
}

// A template with an export carries it in the API's spelling.
func TestJSONTemplatesListExport(t *testing.T) {
	dir, _ := templatesFixture(t)
	created := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	tp := tmpl("vmtpl-000000000001", "proj", "saved", "", false, 0)
	tp.Export = &app.ExportVolume{ThinExport: vmtool.ThinExport{Bytes: 11 << 30, SHA256: strings.Repeat("ef", 32), Created: created}}
	installTemplateHosts(t, map[string]app.Manager{
		"jetfire":    listMgr{},
		"veritech01": tplHost{mu: &sync.Mutex{}, calls: &[]string{}, tpls: []app.TemplateInfo{tp}},
		"veritech02": listMgr{},
	})
	_, o := runJSON(t, dir, "vmtool templates list", "templates", "list")
	ts := asList(t, o, "templates")
	if len(ts) != 1 {
		t.Fatalf("templates %v", ts)
	}
	x, _ := ts[0].(map[string]any)["export"].(map[string]any)
	if x["bytes"] != float64(11<<30) || x["sha256"] != strings.Repeat("ef", 32) || x["expires"] != created.Add(vmtool.ExportTTL).Format(time.RFC3339) {
		t.Errorf("export %v", x)
	}
}

// nostaleHosts is a veritech01 with only a current template.
func nostaleHosts() map[string]app.Manager {
	return map[string]app.Manager{
		"jetfire": listMgr{},
		"veritech01": tplHost{mu: &sync.Mutex{}, calls: &[]string{},
			tpls: []app.TemplateInfo{tmpl("vmtpl-abababababab", "img1", "image", strings.Repeat("ab", 32), false, 2)}},
		"veritech02": listMgr{},
	}
}

func TestJSONTemplateCommandsShapes(t *testing.T) {
	dir, _ := templatesFixture(t)
	r, o := runJSON(t, dir, "vmtool templates prepare", "templates", "prepare", "veritech01", "img1")
	if r.code != 0 || o["name"] != "img1" || o["lv"] != "vmtpl-aaaaaaaaaaaa" || o["pool"] != "thin-test" || o["origin"] != "image" {
		t.Errorf("prepare: exit %d %v\n%s", r.code, o, r.errOut)
	}
	if strings.Contains(r.out, "is ready") {
		t.Errorf("prepare: human text on stdout: %s", r.out)
	}
	if !strings.Contains(r.errOut, "Template veritech01/img1 is ready") {
		t.Errorf("prepare: the human line is not on stderr: %q", r.errOut)
	}

	r, o = runJSON(t, dir, "vmtool templates delete", "templates", "delete", "veritech01/img1")
	if r.code != 0 || o["name"] != "img1" || o["lv"] != "vmtpl-aaaaaaaaaaaa" || o["clones"] != float64(2) {
		t.Errorf("delete: exit %d %v", r.code, o)
	}
	if strings.Contains(r.out, "removed") {
		t.Errorf("delete: human text on stdout: %s", r.out)
	}

	r, o = runJSON(t, dir, "vmtool templates save", "templates", "save", "veritech01/extras", "proj2")
	if r.code != 0 || o["name"] != "proj2" || o["origin"] != "saved" || o["lv"] != "vmtpl-000000000009" {
		t.Errorf("save: exit %d %v", r.code, o)
	}
	disks, _ := o["extra_disks"].([]any)
	if len(disks) != 2 || disks[0] != "vdb" {
		t.Errorf("save: extra_disks %v", o["extra_disks"])
	}
	if strings.Contains(r.out, "warning") || strings.Contains(r.out, requirementsLine) ||
		!strings.Contains(r.errOut, "warning: extra disks") || !strings.Contains(r.errOut, requirementsLine) {
		t.Errorf("save: notes belong on stderr:\nout %q\nerr %q", r.out, r.errOut)
	}
}

func TestJSONTemplatesPrune(t *testing.T) {
	dir, _ := templatesFixture(t)
	r, o := runJSON(t, dir, "vmtool templates prune", "templates", "prune", "veritech01")
	if r.code != 0 || o["host"] != "veritech01" {
		t.Fatalf("prune: exit %d %v\n%s", r.code, o, r.errOut)
	}
	removed := asList(t, o, "removed")
	if len(removed) != 1 || removed[0].(map[string]any)["lv"] != "vmtpl-cdcdcdcdcdcd" {
		t.Errorf("removed %v", removed)
	}
	if strings.Contains(r.out, "Removed 1") {
		t.Errorf("human text on stdout: %s", r.out)
	}
	// nothing stale: an empty list, not null and not absent
	dir2, _ := templatesFixture(t)
	installTemplateHosts(t, nostaleHosts())
	_, o = runJSON(t, dir2, "vmtool templates prune", "templates", "prune", "veritech01")
	if l, ok := o["removed"].([]any); !ok || len(l) != 0 {
		t.Errorf("removed %v", o["removed"])
	}
}

func TestJSONTemplatesExport(t *testing.T) {
	dir, _ := xferFixture(t, nil, "")
	r, o := runJSON(t, dir, "vmtool templates export", "templates", "export", "veritech01/proj", "--pool", "thin-test")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if o["host"] != "veritech01" || o["name"] != "proj" || o["sha256"] != strings.Repeat("ab", 32) || o["size"] != float64(3<<20) {
		t.Errorf("export: %v", o)
	}
	if _, ok := o["store"]; ok {
		t.Errorf("no --push, but a store row: %v", o)
	}
	if !strings.Contains(r.errOut, "exporting") {
		t.Errorf("progress is not on stderr: %q", r.errOut)
	}
	if strings.Contains(r.out, "exporting") || strings.Contains(r.out, "kept 24 hours") {
		t.Errorf("human text on stdout: %s", r.out)
	}
}

func TestJSONTemplatesExportPush(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 500))
	dir, _, _ := pushFixture(t, blob)
	r, o := runJSON(t, dir, "vmtool templates export", "templates", "export", "veritech01/proj", "--push")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	st, _ := o["store"].(map[string]any)
	if st["name"] != "proj" || st["sha256"] != sumHex(blob) || st["size"] != float64(len(blob)) {
		t.Errorf("store row %v (object %v)", st, o)
	}
	for _, secret := range []string{storeTok, daemonTok} {
		if strings.Contains(r.out, secret) || strings.Contains(r.errOut, secret) {
			t.Errorf("a token is in the output")
		}
	}
	if !strings.Contains(r.errOut, "pushing") || strings.Contains(r.out, "pushing") {
		t.Errorf("progress placement: out %q err %q", r.out, r.errOut)
	}
}

func TestJSONImagesAndPools(t *testing.T) {
	dir := jsonDir(t, "")
	installFakeHosts(t) // cliFake lists one pool; the succeeding fake lists none
	r, o := runJSON(t, dir, "vmtool images list", "images", "list")
	pools := asList(t, o, "pools")
	if r.code != 0 || len(pools) != 1 {
		t.Fatalf("images list: exit %d %v", r.code, o)
	}
	p := pools[0].(map[string]any)
	imgs, _ := p["images"].([]any)
	if p["name"] != "default" || len(imgs) != 1 || imgs[0] != "jetfire.qcow2" {
		t.Errorf("pool row %v", p)
	}

	r, o = runJSON(t, dir, "vmtool images delete", "images", "delete", "base.qcow2", "--pool", "fast")
	if r.code != 0 || o["deleted"] != "base.qcow2" || o["pool"] != "fast" || strings.Contains(r.out, "deleted from") {
		t.Errorf("images delete: %v / %q", o, r.out)
	}
	_, o = runJSON(t, dir, "vmtool images delete", "images", "delete", "base.qcow2")
	if o["deleted"] != "base.qcow2" || o["pool"] != "" {
		t.Errorf("images delete, default pool: %v", o)
	}

	r, o = runJSON(t, dir, "vmtool pool list", "pool", "list")
	pl := asList(t, o, "pools")
	if r.code != 0 || len(pl) != 1 {
		t.Fatalf("pool list: %v", o)
	}
	row := pl[0].(map[string]any)
	if row["name"] != "jetfire-pool" || row["path"] != "/p" || row["active"] != true {
		t.Errorf("pool row %v", row)
	}
	if _, ok := row["size_bytes"]; ok {
		t.Errorf("an unknown size is absent, not 0: %v", row)
	}
	if strings.Contains(r.out, "NAME") {
		t.Errorf("table on stdout: %s", r.out)
	}

	r, o = runJSON(t, dir, "vmtool pool create", "pool", "create", "fast", "/srv/fast")
	if r.code != 0 || len(o) != 2 || o["name"] != "fast" || o["path"] != "/srv/fast" || strings.Contains(r.out, "created at") {
		t.Errorf("pool create: %v / %q", o, r.out)
	}
}

func TestJSONImagesListStore(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	s.images["a.qcow2"], s.updated["a.qcow2"] = []byte("aaaa"), time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	r, o := runJSON(t, dir, "vmtool images list", "images", "list", "--store")
	imgs := asList(t, o, "images")
	if r.code != 0 || len(imgs) != 1 {
		t.Fatalf("exit %d %v", r.code, o)
	}
	i := imgs[0].(map[string]any)
	if i["name"] != "a.qcow2" || i["size"] != float64(4) || i["sha256"] != sumHex([]byte("aaaa")) || i["updated"] != "2026-11-17T09:30:00Z" {
		t.Errorf("image %v", i)
	}
}

func TestJSONImagesPushPull(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	file := placeholderImageFile(t)
	sum := sumHex([]byte("PLACEHOLDER image bytes"))
	r, o := runJSON(t, dir, "vmtool images push", "images", "push", file)
	if r.code != 0 || o["name"] != "base.qcow2" || o["sha256"] != sum || o["size"] != float64(23) {
		t.Fatalf("push: exit %d %v\n%s", r.code, o, r.errOut)
	}
	if !strings.Contains(r.errOut, "pushing") || strings.Contains(r.out, "pushing") || strings.Contains(r.out, "is in the store") {
		t.Errorf("push: progress placement: out %q err %q", r.out, r.errOut)
	}

	to := filepath.Join(t.TempDir(), "out.qcow2")
	r, o = runJSON(t, dir, "vmtool images pull", "images", "pull", "base.qcow2", "--to", to)
	if r.code != 0 || o["name"] != "base.qcow2" || o["path"] != to || o["sha256"] != sum || o["size"] != float64(23) {
		t.Fatalf("pull: exit %d %v\n%s", r.code, o, r.errOut)
	}
	if strings.Contains(r.out, "written to") || !strings.Contains(r.errOut, "pulling") {
		t.Errorf("pull: progress placement: out %q err %q", r.out, r.errOut)
	}
}

func TestJSONImagesPullFromAHost(t *testing.T) {
	data := []byte("PLACEHOLDER the host's image")
	imgDir := t.TempDir()
	dir, _ := xferFixture(t, data, "image_dir: "+imgDir+"\n")
	r, o := runJSON(t, dir, "vmtool images pull", "images", "pull", "veritech01/proj")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if o["host"] != "veritech01" || o["name"] != "proj" || o["path"] != filepath.Join(imgDir, "proj") ||
		o["sha256"] != sumHex(data) || o["size"] != float64(len(data)) || o["unchanged"] != false {
		t.Errorf("host pull: %v", o)
	}
	// again: the same bytes are there already
	_, o = runJSON(t, dir, "vmtool images pull", "images", "pull", "veritech01/proj")
	if o["unchanged"] != true {
		t.Errorf("second pull: %v", o)
	}
}

// creds fetch and forget print small objects. creds fetch never prints a
// password or key (it prints only a confirmation line today): neither the
// object nor stderr carries the store record's password, its keys, or the
// store's token.
func TestJSONCredsNeverPrintSecrets(t *testing.T) {
	c := newCredCLI(t)
	c.s.vmRecord("ws/web", credUUID, credPass, credKey)
	r, o := runJSON(t, c.dir, "vmtool creds fetch", "creds", "fetch", "ws/web")
	if r.code != 0 || o["fetched"] != "ws/web" || len(o) != 1 {
		t.Fatalf("fetch: exit %d %v\n%s", r.code, o, r.errOut)
	}
	noSecrets(t, "creds fetch --json stdout", r.out)
	noSecrets(t, "creds fetch --json stderr", r.errOut)
	for _, w := range []string{"PLACEHOLDER-pub", credUUID, "password", "private_key", "username"} {
		if strings.Contains(r.out, w) || strings.Contains(r.errOut, w) {
			t.Errorf("creds fetch --json carries %q: %q / %q", w, r.out, r.errOut)
		}
	}
	if b, err := os.ReadFile(filepath.Join(credDir(), "id_ed25519")); err != nil || string(b) != credKey {
		t.Errorf("the cache was not written: %v", err)
	}

	r, o = runJSON(t, c.dir, "vmtool creds forget", "creds", "forget", "ws/web")
	if r.code != 0 || o["forgotten"] != "ws/web" || len(o) != 1 {
		t.Fatalf("forget: exit %d %v\n%s", r.code, o, r.errOut)
	}
	noSecrets(t, "creds forget --json", r.out+r.errOut)
	if _, err := os.Stat(credDir()); !os.IsNotExist(err) {
		t.Error("cache kept")
	}
	// the human line is unchanged without --json
	if r := runContract(t, c.dir, "vmtool creds fetch", "creds", "fetch", "ws/web"); r.out != "Credentials for ws/web fetched into the cache\n" {
		t.Errorf("human line %q", r.out)
	}
}

// A failure under --json is the error object on stderr and nothing on stdout.
func TestJSONStoreGroupFailure(t *testing.T) {
	c := newCredCLI(t) // the store has no record
	r := runContract(t, c.dir, "vmtool creds fetch", "--json", "creds", "fetch", "ws/web")
	if r.code != 1 || r.out != "" || !strings.Contains(r.errOut, `"not_found"`) {
		t.Errorf("exit %d, out %q, err %q", r.code, r.out, r.errOut)
	}
}
