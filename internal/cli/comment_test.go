package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/vmnotes"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// N12: origin and comment live in the store only. `vmtool comment` writes the
// store without the host; create writes the origin after define; info shows
// them from the store in store mode only.

const notesUUID = "00000000-0000-4000-8000-000000000001"

// vmtool info shows origin and comment when the store was read, "-" for a VM
// that has none, and neither line when the store was not read.
func TestWriteVMInfoNotes(t *testing.T) {
	sha := strings.Repeat("a", 64)
	info := &vmtool.VMInfo{Name: "vm1"}
	var b bytes.Buffer
	writeVMInfo(&b, info, &app.VMNotes{Comment: "placeholder comment", Origin: &vmnotes.Origin{
		Image: "img1", ImageSHA256: sha, Template: "img1", CloneKind: vmnotes.CloneKindThin, Pool: "nvme-thin",
		Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}})
	for _, want := range []string{"Origin:    img1", "thin", "nvme-thin", sha, "template img1", "2026-11-21T04:05:06Z",
		"Comment:   placeholder comment\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	writeVMInfo(&b, info, &app.VMNotes{})
	for _, want := range []string{"Origin:    -\n", "Comment:   -\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	writeVMInfo(&b, info, nil)
	if strings.Contains(b.String(), "Origin") || strings.Contains(b.String(), "Comment") {
		t.Errorf("notes shown without the store:\n%s", b.String())
	}
}

// notesFake is a host whose VM has a UUID; it records Define and every
// Manager call it is asked for.
type notesFake struct {
	createFake
	rec *notesLog
}

type notesLog struct {
	mu      sync.Mutex
	calls   []string
	defined int
}

func (l *notesLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (c notesFake) Info(n string) (*vmtool.VMInfo, error) {
	c.rec.add("Info")
	return &vmtool.VMInfo{Name: n, State: vmtool.StateRunning, UUID: notesUUID}, nil
}
func (c notesFake) Define(cfg vmtool.VMConfig) error {
	c.rec.add("Define")
	c.rec.mu.Lock()
	c.rec.defined++
	c.rec.mu.Unlock()
	return c.createFake.Define(cfg)
}

// notesCLI runs the CLI against a host fake; with a store (s non-nil) the
// config names it, else no store is configured. resolved counts the times the
// host's Manager was opened.
func notesCLI(t *testing.T, s *cliStore, args ...string) (*notesLog, int, string, string, error) {
	t.Helper()
	rec := &notesLog{}
	resolved := 0
	var dir string
	if s != nil {
		dir = storeWorkdir(t, s, storeTok)
	} else {
		t.Setenv(config.TokenEnv, gateToken)
		t.Chdir(t.TempDir())
		dir = writeWorkdir(t, "name: ws\n"+goodConfig(), "")
	}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			resolved++
			return notesFake{createFake: createFake{log: &createLog{}}, rec: rec}, func() {}, nil
		})
	}
	var out, errb bytes.Buffer
	root, e := rootFor(dir, &out, &errb)
	e.tunnelExe = stubTunnelExe
	root.SetArgs(args)
	err := root.Execute()
	return rec, resolved, out.String(), errb.String(), err
}

// `vmtool comment` sets and clears the comment in the store; the host is not
// opened (it may sleep).
func TestCommentCommandWritesStore(t *testing.T) {
	s := newCLIStore(t)
	rec, resolved, out, _, err := notesCLI(t, s, "comment", "ws/web", "placeholder comment")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 0 || len(rec.calls) != 0 {
		t.Errorf("the host was asked: resolved %d calls %q", resolved, rec.calls)
	}
	if !strings.Contains(out, "ws/web") {
		t.Errorf("output %q", out)
	}
	_, _, _, _, err = notesCLI(t, s, "comment", "ws/web", "--clear")
	if err != nil {
		t.Fatal(err)
	}
	w := s.notesWrites()
	if len(w) != 2 || w[0].path != "/v1/inventory/ws/web/notes" || w[0].body["comment"] != "placeholder comment" ||
		w[1].body["comment"] != "" || w[0].body["origin"] != nil || w[0].body["uuid"] != nil {
		t.Fatalf("store writes %+v", w)
	}
}

// No store in use: `vmtool comment` fails, saying comments are kept in the
// store, before any Manager call.
func TestCommentCommandNeedsStore(t *testing.T) {
	rec, resolved, _, _, err := notesCLI(t, nil, "comment", "ws/web", "placeholder comment")
	if err == nil || !strings.Contains(err.Error(), "comments are kept in the store") || !strings.Contains(err.Error(), "no store is in use") {
		t.Fatalf("err %v", err)
	}
	if resolved != 0 || len(rec.calls) != 0 {
		t.Errorf("the host was asked: resolved %d calls %q", resolved, rec.calls)
	}
}

func TestCommentCommandRefusals(t *testing.T) {
	for name, args := range map[string][]string{
		"no text":        {"comment", "ws/web"},
		"text and clear": {"comment", "ws/web", "x", "--clear"},
		"newline":        {"comment", "ws/web", "a\nb"},
		"too long":       {"comment", "ws/web", strings.Repeat("a", 513)},
	} {
		s := newCLIStore(t)
		_, _, _, _, err := notesCLI(t, s, args...)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if w := s.notesWrites(); len(w) != 0 {
			t.Errorf("%s: reached the store: %+v", name, w)
		}
		if name != "no text" && name != "text and clear" && !strings.Contains(err.Error(), "comment") {
			t.Errorf("%s: error %v does not name comment", name, err)
		}
	}
}

// create --comment writes origin and comment to the store after define.
func TestCreateCommentFlagWritesStore(t *testing.T) {
	s := newCLIStore(t)
	rec, _, _, errOut, err := notesCLI(t, s, "create", "ws/web", "u.qcow2", "--comment", "placeholder comment", "--no-wait", "--inventory", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if rec.defined != 1 {
		t.Fatalf("defined %d", rec.defined)
	}
	w := s.notesWrites()
	if len(w) != 1 || w[0].path != "/v1/inventory/ws/web/notes" || w[0].body["uuid"] != notesUUID || w[0].body["comment"] != "placeholder comment" {
		t.Fatalf("store writes %+v", w)
	}
	o, _ := w[0].body["origin"].(map[string]any)
	if o["image"] != "u.qcow2" || o["clone"] != "file" {
		t.Errorf("origin %+v", o)
	}
}

// No store in use: create --comment is refused before anything is created;
// create without one works and records nothing.
func TestCreateCommentFlagNeedsStore(t *testing.T) {
	rec, _, _, _, err := notesCLI(t, nil, "create", "ws/web", "u.qcow2", "--comment", "placeholder comment", "--no-wait", "--inventory", "")
	if err == nil || !strings.Contains(err.Error(), "comments are kept in the store") || len(rec.calls) != 0 {
		t.Fatalf("err %v calls %q", err, rec.calls)
	}
	rec, _, _, errOut, err := notesCLI(t, nil, "create", "ws/web", "u.qcow2", "--no-wait", "--inventory", "")
	if err != nil || rec.defined != 1 {
		t.Fatalf("plain create: %v defined %d", err, rec.defined)
	}
	if strings.Contains(errOut, "origin") || strings.Contains(errOut, "store") {
		t.Errorf("a line in local mode: %q", errOut)
	}
}

// info shows origin and comment from the store in store mode; with no store,
// no such lines.
func TestInfoShowsNotesFromStore(t *testing.T) {
	s := newCLIStore(t)
	s.inv = map[string]any{"hosts": []any{}, "machines": []any{}, "vms": []any{
		map[string]any{"host": "ws", "name": "web", "uuid": notesUUID, "state": "running", "vcpus": 2, "memory": 4, "ip": "",
			"autostart": false, "firmware": "efi", "disks": []any{}, "comment": "placeholder comment",
			"origin":     map[string]any{"image": "img1", "clone": "thin", "pool": "nvme-thin", "created": "2026-11-21T04:05:06Z"},
			"first_seen": "2026-11-21T00:00:00Z", "last_seen": "2026-11-21T00:00:00Z", "has_credentials": false},
	}}
	_, _, out, _, err := notesCLI(t, s, "info", "ws/web")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"UUID:      " + notesUUID, "Origin:    img1, clone kind thin, pool nvme-thin", "Comment:   placeholder comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}
	_, _, out, errOut, err := notesCLI(t, nil, "info", "ws/web")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Origin") || strings.Contains(out, "Comment") || errOut != "" {
		t.Errorf("local mode info:\n%s\n%s", out, errOut)
	}
}
