package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
)

// `vmtool templates export host/<name>` and `vmtool images pull host/<name>`.
// The hosts are fakes (no daemon, no libvirt, no network).

// xferCLIHost is a thin host that exports templates and serves blobs.
type xferCLIHost struct {
	tplHost
	blob      []byte
	exportErr error
}

func (h xferCLIHost) ExportTemplate(_ context.Context, name, pool string, hash bool, on func(app.ExportEvent)) (app.ExportedImage, error) {
	h.rec("export " + name + " " + pool)
	on(app.ExportEvent{Stage: "export", Received: 1 << 20, Total: 4 << 20})
	if h.exportErr != nil {
		return app.ExportedImage{}, h.exportErr
	}
	return app.ExportedImage{Name: "proj", SHA256: strings.Repeat("ab", 32), Size: 3 << 20}, nil
}

func (h xferCLIHost) OpenImageBlob(_ context.Context, name string) (io.ReadCloser, app.ImageBlobInfo, error) {
	h.rec("open " + name)
	return io.NopCloser(bytes.NewReader(h.blob)), app.ImageBlobInfo{Size: int64(len(h.blob)), SHA256: sumHex(h.blob)}, nil
}

func xferFixture(t *testing.T, blob []byte, extraCfg string) (dir string, calls *[]string) {
	t.Helper()
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir = writeWorkdir(t, twoHostCfg(extraCfg), "")
	calls = &[]string{}
	mu := &sync.Mutex{}
	installTemplateHosts(t, map[string]app.Manager{
		"jetfire":    listMgr{},
		"veritech01": xferCLIHost{tplHost: tplHost{mu: mu, calls: calls}, blob: blob},
	})
	return dir, calls
}

func TestTemplatesExportPrintsProgressAndTheImage(t *testing.T) {
	dir, calls := xferFixture(t, nil, "")
	keep := map[string]bool{"vmtool templates export": true}
	r := runGate(t, dir, keep, "templates", "export", "veritech01/proj", "--pool", "thin-test")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := strings.Join(*calls, "|"); got != "export proj thin-test" {
		t.Errorf("calls %q", got)
	}
	if !strings.Contains(r.errOut, "exporting") {
		t.Errorf("no progress on stderr: %q", r.errOut)
	}
	if !strings.Contains(r.out, "veritech01/proj") || !strings.Contains(r.out, strings.Repeat("ab", 32)) || !strings.Contains(r.out, "3145728") {
		t.Errorf("image line %q", r.out)
	}
	if !strings.Contains(r.out, "NVMe array of veritech01") || !strings.Contains(r.out, "kept 24 hours") || strings.Contains(r.out, "image cache") {
		t.Errorf("the line must say the export is on the array and how long it is kept: %q", r.out)
	}
	if !strings.Contains(r.out, "vmtool images pull veritech01/proj") {
		t.Errorf("no hint at how to pull: %q", r.out)
	}
	// no pool: none sent
	*calls = nil
	if r := runGate(t, dir, keep, "templates", "export", "veritech01/proj"); r.err != nil || strings.Join(*calls, "|") != "export proj " {
		t.Errorf("no pool: %v %q", r.err, *calls)
	}
}

func TestTemplatesExportRefusals(t *testing.T) {
	dir, calls := xferFixture(t, nil, "")
	keep := map[string]bool{"vmtool templates export": true}
	for _, arg := range []string{"proj", "veritech01/", "/proj", "veritech01/a/b", "veritech01/../x"} {
		if r := runGate(t, dir, keep, "templates", "export", arg); r.err == nil {
			t.Errorf("%q accepted", arg)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("calls %q", *calls)
	}
	// a host that answers with a refusal: its message is the command's error
	dir2, _ := xferFixture(t, nil, "")
	installTemplateHosts(t, map[string]app.Manager{
		"veritech01": xferCLIHost{tplHost: tplHost{mu: &sync.Mutex{}, calls: &[]string{}},
			exportErr: &app.Error{Kind: app.KindConflict, Err: errors.New("image proj already exists on veritech01")}},
	})
	r := runGate(t, dir2, keep, "templates", "export", "veritech01/proj")
	if r.err == nil || !strings.Contains(r.err.Error(), "already exists") {
		t.Errorf("conflict: %v", r.err)
	}
	// a host with no thin pools or daemon (local libvirt): refused, said plainly
	r = runGate(t, dir, keep, "templates", "export", "jetfire/proj")
	if r.err == nil {
		t.Error("local libvirt host exported")
	}
}

// images pull host/<name> is a host pull (image names have no '/'), written to
// --to or image_dir, in local mode: no store, none probed.
func TestImagesPullFromAHost(t *testing.T) {
	data := []byte("PLACEHOLDER the host's image")
	imgDir := t.TempDir()
	dir, calls := xferFixture(t, data, "image_dir: "+imgDir+"\n")
	r := runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(filepath.Join(imgDir, "proj")); !bytes.Equal(got, data) {
		t.Fatalf("image_dir copy %q", got)
	}
	if got := strings.Join(*calls, "|"); got != "open proj" {
		t.Errorf("calls %q", got)
	}
	if !strings.Contains(r.out, filepath.Join(imgDir, "proj")) || !strings.Contains(r.out, sumHex(data)) {
		t.Errorf("output %q", r.out)
	}
	if strings.Contains(r.errOut, "store") {
		t.Errorf("the store was mentioned: %q", r.errOut)
	}

	// again: the same file is there: nothing to do, said so
	r = runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj")
	if r.err != nil || !strings.Contains(r.out, "already") {
		t.Errorf("again: %v %q", r.err, r.out)
	}

	// a different file of that name is never replaced: there is no --force
	if err := os.WriteFile(filepath.Join(imgDir, "proj"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj")
	if r.err == nil || !strings.Contains(r.err.Error(), "already exists") {
		t.Errorf("different file: %v", r.err)
	}
	if got, _ := os.ReadFile(filepath.Join(imgDir, "proj")); string(got) != "mine" {
		t.Errorf("the file was replaced: %q", got)
	}
	if r := runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj", "--force"); r.err == nil {
		t.Error("--force exists on a host pull")
	}

	// --to a directory
	to := t.TempDir()
	if r := runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj", "--to", to); r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(filepath.Join(to, "proj")); !bytes.Equal(got, data) {
		t.Errorf("--to copy %q", got)
	}
	// --to that is not a directory
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj", "--to", file); r.err == nil {
		t.Error("--to a file accepted for a host pull")
	}
}

// Without image_dir the default is the current directory, and a malformed
// host/name is refused before any host is asked.
func TestImagesPullFromAHostDefaultsAndRefusals(t *testing.T) {
	data := []byte("PLACEHOLDER the host's image")
	dir, calls := xferFixture(t, data, "")
	cwd, _ := os.Getwd()
	if r := runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj"); r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(filepath.Join(cwd, "proj")); !bytes.Equal(got, data) {
		t.Errorf("./proj %q", got)
	}
	*calls = nil
	for _, arg := range []string{"veritech01/", "/proj", "veritech01/a/b", "veritech01/.x", "nosuch/proj"} {
		if r := runGate(t, dir, pullKeep, "images", "pull", arg); r.err == nil {
			t.Errorf("%q accepted", arg)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("calls %q", *calls)
	}
}

// A host pull never touches the store, even when one is configured and down:
// no probe line, no refusal; and a bare name is still the store's pull.
func TestImagesPullFromAHostIgnoresTheStore(t *testing.T) {
	s := newCLIStore(t)
	s.down.Store(true)
	data := []byte("PLACEHOLDER the host's image")
	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, storeTok)
	t.Chdir(t.TempDir())
	imgDir := t.TempDir()
	dir := writeWorkdir(t, twoHostCfg("image_dir: "+imgDir+"\n"+s.cfg()), "")
	installTemplateHosts(t, map[string]app.Manager{
		"veritech01": xferCLIHost{tplHost: tplHost{mu: &sync.Mutex{}, calls: &[]string{}}, blob: data},
	})
	r := runGate(t, dir, pullKeep, "images", "pull", "veritech01/proj")
	if r.err != nil || strings.Contains(r.errOut, "unreachable") {
		t.Errorf("host pull with the store down: %v %q", r.err, r.errOut)
	}
	if got, _ := os.ReadFile(filepath.Join(imgDir, "proj")); !bytes.Equal(got, data) {
		t.Errorf("copy %q", got)
	}
	// a bare name is the store's: with the store down it is refused as before
	r = runGate(t, dir, pullKeep, "images", "pull", "base.qcow2", "--to", filepath.Join(t.TempDir(), "o"))
	if r.err == nil || !strings.Contains(r.err.Error(), "no store in use") {
		t.Errorf("bare name with the store down: %v", r.err)
	}
}

func TestImagesPullHelpSaysWhichFormIsWhich(t *testing.T) {
	var out, errb bytes.Buffer
	root, _ := rootFor(t.TempDir(), &out, &errb)
	root.SetArgs([]string{"images", "pull", "--help"})
	_ = root.Execute()
	h := out.String()
	for _, want := range []string{"<host>/<name>", "bare name", "store", "--to"} {
		if !strings.Contains(h, want) {
			t.Errorf("help lacks %q:\n%s", want, h)
		}
	}
}
