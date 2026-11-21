package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/internal/vmnotes"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// N12: a VM's origin and comment live in the store only. Create writes them
// to the store after define; `vmtool comment` writes the store without the
// host; info reads them from the store. The store is an in-process loopback
// TLS double.

var notesNow = time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)

const notesSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// notesStore answers whoami, the notes route and the inventory read, and
// records the notes writes.
type notesStore struct {
	ts *httptest.Server

	mu         sync.Mutex
	puts       []string // "PUT path" in order
	bodies     []storeapi.VMNotesInput
	whoamiDown bool // whoami fails: the command is in local mode
	notesCode  int  // the notes route answers this instead of 200
	readDown   bool // the inventory read fails
	reads      int
	inv        storeapi.Inventory
}

func newNotesStore(t *testing.T) *notesStore {
	t.Helper()
	f := &notesStore{inv: storeapi.Inventory{Hosts: []storeapi.InventoryHost{}, Vms: []storeapi.InventoryVM{}, Machines: []storeapi.MachineSummary{}}}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.Header.Get("Authorization") != "Bearer "+stTok {
			out(401, map[string]string{"error": "unauthorized"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/whoami":
			if f.whoamiDown {
				out(503, map[string]string{"error": "down"})
				return
			}
			out(200, map[string]string{"name": "jared"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/notes"):
			var in storeapi.VMNotesInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.puts = append(f.puts, "PUT "+r.URL.Path)
			f.bodies = append(f.bodies, in)
			if f.notesCode != 0 {
				out(f.notesCode, map[string]string{"error": "refused"})
				return
			}
			parts := strings.Split(r.URL.Path, "/") // /v1/inventory/{host}/{name}/notes
			n := storeapi.VMNotes{Host: parts[3], Name: parts[4], Uuid: notesStr(in.Uuid), Origin: in.Origin, Comment: notesStr(in.Comment)}
			if n.Uuid == "" {
				n.Uuid = stUUID
			}
			out(200, n)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/inventory":
			f.reads++
			if f.readDown {
				out(503, map[string]string{"error": "down"})
				return
			}
			out(200, f.inv)
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func notesStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (f *notesStore) session(t *testing.T, notes *[]string) *StoreSession {
	t.Helper()
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return OpenStoreSession(f.ts.URL, func() (string, error) { return stTok, nil },
		[]storeclient.Option{storeclient.WithRootCAs(pool)}, func(l string) {
			if notes != nil {
				*notes = append(*notes, l)
			}
		})
}

func (f *notesStore) writes() ([]string, []storeapi.VMNotesInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.puts...), append([]storeapi.VMNotesInput(nil), f.bodies...)
}

// thinTplVMs is a host that caches images (its manifest gives the digest) and has
// thin pools: it lists its templates.
type thinTplVMs struct {
	shaVMs
	templates []TemplateInfo
	listed    int
}

func (f *thinTplVMs) ListTemplates() ([]TemplateInfo, error) { f.listed++; return f.templates, nil }
func (f *thinTplVMs) PrepareTemplate(string, string) (TemplateInfo, error) {
	return TemplateInfo{}, fmt.Errorf("not here")
}
func (f *thinTplVMs) DeleteTemplate(string, string) (TemplateInfo, error) {
	return TemplateInfo{}, fmt.Errorf("not here")
}

func notesCreate(t *testing.T, st *notesStore, m Manager, in CreateVMInput, notes *[]string) error {
	t.Helper()
	t.Chdir(t.TempDir())
	in.Host, in.Name, in.Image = "hv1", "vm1", "img1"
	if !in.Noclone && in.OnProgress == nil {
		in.NoWait = true
	}
	svc := &Service{Now: func() time.Time { return notesNow }}
	if st != nil {
		svc.Store = st.session(t, notes)
	}
	_, err := svc.CreateVM(context.Background(), m, in)
	return err
}

func ptr(s string) *string { return &s }

// Create writes the VM's origin (and any comment) to the store, keyed by the
// UUID the host gave the domain, once, after define; the domain definition
// carries none of it.
func TestCreateWritesOriginToStore(t *testing.T) {
	tpl := TemplateInfo{Template: vmtool.Template{Name: "img1", Image: "img1", SHA256: notesSHA,
		Origin: vmtool.TemplateOriginImage, Pool: "thin-test", LV: "vmtpl-aaaaaaaaaaaa"}}
	for _, c := range []struct {
		name      string
		clonePath string
		in        CreateVMInput
		templates []TemplateInfo
		want      storeapi.VMOrigin
	}{
		{"thin template", "/dev/vg-thin/vm1", CreateVMInput{Pool: "thin-test"}, []TemplateInfo{tpl},
			storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Template: ptr("img1"), Clone: ptr("thin"), Pool: ptr("thin-test")}},
		{"thin, explicit disk cache (pools not listed before the clone)", "/dev/vg-thin/vm1", CreateVMInput{Pool: "thin-test", DiskCache: "none"}, []TemplateInfo{tpl},
			storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Template: ptr("img1"), Clone: ptr("thin"), Pool: ptr("thin-test")}},
		{"thick LV", "/dev/vg-test/vm1", CreateVMInput{Pool: "nvme"}, nil,
			storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Clone: ptr("block"), Pool: ptr("nvme")}},
		{"qcow2 file", "", CreateVMInput{Pool: "vmtool"}, nil,
			storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Clone: ptr("file"), Pool: ptr("vmtool")}},
		{"file, host default pool found by its path", "", CreateVMInput{}, nil,
			storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Clone: ptr("file"), Pool: ptr("vmtool")}},
		{"noclone", "", CreateVMInput{Noclone: true, NoWait: true}, nil,
			storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Clone: ptr("none")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newNotesStore(t)
			m := &thinTplVMs{shaVMs: shaVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools, clonePath: c.clonePath, uuid: stUUID}}, sha: notesSHA},
				templates: c.templates}
			var lines []string
			if err := notesCreate(t, st, m, c.in, &lines); err != nil {
				t.Fatal(err)
			}
			puts, bodies := st.writes()
			if len(puts) != 1 || puts[0] != "PUT /v1/inventory/hv1/vm1/notes" {
				t.Fatalf("notes writes %q", puts)
			}
			b := bodies[0]
			want := c.want
			want.Created = ptr(notesNow.Format(time.RFC3339))
			if b.Uuid == nil || *b.Uuid != stUUID || b.Comment != nil || b.Origin == nil {
				t.Fatalf("body %+v", b)
			}
			got, _ := json.Marshal(b.Origin)
			exp, _ := json.Marshal(&want)
			if string(got) != string(exp) {
				t.Errorf("origin %s, want %s", got, exp)
			}
			if len(lines) != 0 {
				t.Errorf("lines %q", lines)
			}
		})
	}
}

// The image's SHA-256 is recorded when the create knew it: the host's
// manifest after the sync.
type shaVMs struct {
	uefiVMs
	sha string
}

func (f *shaVMs) ImageManifest(string) (RemoteImage, error) {
	return RemoteImage{SHA256: f.sha, Size: 5}, nil
}
func (f *shaVMs) UploadImage(context.Context, string, io.Reader, int64, string, func(int64)) error {
	return fmt.Errorf("no upload expected")
}

func TestCreateWritesImageSHAAndComment(t *testing.T) {
	st := newNotesStore(t)
	m := &shaVMs{uefiVMs: uefiVMs{createVMs: createVMs{uuid: stUUID}}, sha: notesSHA}
	if err := notesCreate(t, st, m, CreateVMInput{Comment: "placeholder comment"}, nil); err != nil {
		t.Fatal(err)
	}
	_, bodies := st.writes()
	if len(bodies) != 1 {
		t.Fatalf("writes %+v", bodies)
	}
	b := bodies[0]
	if b.Origin == nil || notesStr(b.Origin.ImageSha256) != notesSHA || b.Comment == nil || *b.Comment != "placeholder comment" {
		t.Errorf("body %+v", b)
	}
}

// The notes are written also when the guest gets no address: the VM exists.
func TestCreateWritesNotesWhenWaitFails(t *testing.T) {
	st := newNotesStore(t)
	m := &uefiVMs{createVMs: createVMs{uuid: stUUID}}
	err := notesCreate(t, st, m, CreateVMInput{OnProgress: func(ProgressEvent) {}}, nil)
	if err == nil {
		t.Fatal("create succeeded; the fake gives no address")
	}
	if puts, _ := st.writes(); len(puts) != 1 {
		t.Fatalf("writes %q", puts)
	}
}

// No store in use: create works and records no origin, with no extra line;
// with a configured store that does not answer, only its one local-mode line.
func TestCreateLocalModeRecordsNothing(t *testing.T) {
	var evs []ProgressEvent
	m := &uefiVMs{createVMs: createVMs{uuid: stUUID}}
	if err := notesCreate(t, nil, m, CreateVMInput{NoWait: true, OnProgress: func(e ProgressEvent) { evs = append(evs, e) }}, nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Status == "notice" {
			t.Errorf("a notice with no store: %+v", e)
		}
	}

	st := newNotesStore(t)
	st.whoamiDown = true
	var lines []string
	m = &uefiVMs{createVMs: createVMs{uuid: stUUID}}
	if err := notesCreate(t, st, m, CreateVMInput{}, &lines); err != nil {
		t.Fatal(err)
	}
	if puts, _ := st.writes(); len(puts) != 0 {
		t.Errorf("local mode wrote notes: %q", puts)
	}
	if len(lines) > 1 || (len(lines) == 1 && !strings.Contains(lines[0], usingLocal)) {
		t.Errorf("lines %q, want at most the local-mode line", lines)
	}
}

// noCallMgr fails the test on any Manager call (its embedded nil interface
// panics).
type noCallMgr struct{ Manager }

// No store in use: --comment is refused before anything is created, saying
// comments are kept in the store and no store is in use (REST 503).
func TestCreateCommentNeedsStore(t *testing.T) {
	down := newNotesStore(t)
	down.whoamiDown = true
	for name, st := range map[string]*notesStore{"no store configured": nil, "store not answering": down} {
		t.Run(name, func(t *testing.T) {
			var lines []string
			err := notesCreate(t, st, noCallMgr{}, CreateVMInput{Comment: "placeholder comment"}, &lines)
			ae := asAppErr(t, err)
			if ae.Kind != KindUnavailable || !strings.Contains(err.Error(), "comments are kept in the store") ||
				!strings.Contains(err.Error(), "no store is in use") {
				t.Fatalf("err %v", err)
			}
			if st != nil {
				if puts, _ := st.writes(); len(puts) != 0 {
					t.Errorf("writes %q", puts)
				}
			}
		})
	}
}

// A bad comment is refused before anything is made, naming the field.
func TestCreateRefusesBadComment(t *testing.T) {
	for _, c := range []string{strings.Repeat("a", 513), "two\nlines", "tab\there", "bell\a"} {
		err := notesCreate(t, nil, noCallMgr{}, CreateVMInput{Comment: c}, nil)
		ae := asAppErr(t, err)
		if ae.Kind != KindInvalid || !strings.Contains(err.Error(), "comment") {
			t.Errorf("%.12q: err %v", c, err)
		}
	}
}

// The store fails after define (store mode at the start): the create
// succeeds, and one line says the notes were not saved and how to set the
// comment later.
func TestCreateStoreFailsAfterDefine(t *testing.T) {
	for _, code := range []int{503, 409} {
		st := newNotesStore(t)
		st.notesCode = code
		var lines []string
		var evs []ProgressEvent
		m := &uefiVMs{createVMs: createVMs{uuid: stUUID}}
		err := notesCreate(t, st, m, CreateVMInput{Comment: "placeholder comment", NoWait: true,
			OnProgress: func(e ProgressEvent) { evs = append(evs, e) }}, &lines)
		if err != nil {
			t.Fatalf("%d: create failed: %v", code, err)
		}
		if m.defined.Name != "vm1" {
			t.Fatalf("%d: not defined", code)
		}
		if len(lines) != 1 {
			t.Fatalf("%d: lines %q", code, lines)
		}
		l := lines[0]
		for _, want := range []string{"hv1/vm1", "origin", "comment", "not saved", "vmtool comment hv1/vm1"} {
			if !strings.Contains(l, want) {
				t.Errorf("%d: line %q lacks %q", code, l, want)
			}
		}
		if strings.Contains(l, stTok) {
			t.Errorf("line carries the token")
		}
		seen := false
		for _, e := range evs {
			seen = seen || (e.Status == "notice" && e.Detail == l)
		}
		if !seen {
			t.Errorf("%d: the line is not on the progress stream: %+v", code, evs)
		}
	}
}

// `vmtool comment` writes the store only: no Manager, no host.
func TestSetCommentWritesStore(t *testing.T) {
	st := newNotesStore(t)
	svc := &Service{Store: st.session(t, nil)}
	ref := VMRef{Host: "hv1", Name: "vm1"}
	n, err := svc.SetComment(context.Background(), ref, "placeholder comment")
	if err != nil || n.Comment != "placeholder comment" || n.UUID != stUUID {
		t.Fatalf("set: %+v %v", n, err)
	}
	if n, err = svc.SetComment(context.Background(), ref, ""); err != nil || n.Comment != "" {
		t.Fatalf("clear: %+v %v", n, err)
	}
	puts, bodies := st.writes()
	if len(puts) != 2 || puts[0] != "PUT /v1/inventory/hv1/vm1/notes" {
		t.Fatalf("writes %q", puts)
	}
	for i, want := range []string{"placeholder comment", ""} {
		b := bodies[i]
		if b.Comment == nil || *b.Comment != want || b.Uuid != nil || b.Origin != nil {
			t.Errorf("body %d %+v", i, b)
		}
	}
}

func TestSetCommentErrors(t *testing.T) {
	ref := VMRef{Host: "hv1", Name: "vm1"}
	// A bad comment: invalid, the store is not called.
	st := newNotesStore(t)
	_, err := (&Service{Store: st.session(t, nil)}).SetComment(context.Background(), ref, strings.Repeat("é", 257))
	if asAppErr(t, err).Kind != KindInvalid || !strings.Contains(err.Error(), "comment") {
		t.Errorf("bad comment: %v", err)
	}
	if puts, _ := st.writes(); len(puts) != 0 {
		t.Errorf("bad comment reached the store")
	}
	// No store in use: unavailable, saying why.
	down := newNotesStore(t)
	down.whoamiDown = true
	for _, svc := range []*Service{{}, {Store: down.session(t, nil)}} {
		_, err := svc.SetComment(context.Background(), ref, "x")
		if asAppErr(t, err).Kind != KindUnavailable || !strings.Contains(err.Error(), "comments are kept in the store") ||
			!strings.Contains(err.Error(), "no store is in use") {
			t.Errorf("local mode: %v", err)
		}
	}
	// The store has no row: not found; the row is gone: conflict.
	for code, kind := range map[int]Kind{404: KindNotFound, 409: KindConflict, 503: KindUnavailable} {
		st := newNotesStore(t)
		st.notesCode = code
		_, err := (&Service{Store: st.session(t, nil)}).SetComment(context.Background(), ref, "x")
		if asAppErr(t, err).Kind != kind || !strings.Contains(err.Error(), "hv1/vm1") {
			t.Errorf("%d: %v, want kind %v", code, err, kind)
		}
	}
}

// info's notes come from the store in store mode only: one read, nothing on
// a store failure or in local mode.
func TestNotesForInfo(t *testing.T) {
	ref := VMRef{Host: "hv1", Name: "vm1"}
	st := newNotesStore(t)
	st.inv.Vms = []storeapi.InventoryVM{
		{Host: "hv1", Name: "vm1", Uuid: stUUID, Comment: "placeholder comment",
			Origin: &storeapi.VMOrigin{Image: ptr("img1"), ImageSha256: ptr(notesSHA), Template: ptr("img1"), Clone: ptr("thin"),
				Pool: ptr("thin-test"), Created: ptr("2026-11-21T04:05:06Z")}},
		{Host: "hv1", Name: "vm2", Uuid: "00000000-0000-4000-8000-000000000002"},
	}
	var lines []string
	svc := &Service{Store: st.session(t, &lines)}
	n := svc.Notes(context.Background(), ref, stUUID)
	want := vmnotes.Origin{Image: "img1", ImageSHA256: notesSHA, Template: "img1", CloneKind: "thin", Pool: "thin-test", Created: notesNow}
	if n == nil || n.Origin == nil || *n.Origin != want || n.Comment != "placeholder comment" {
		t.Fatalf("notes %+v", n)
	}
	if st.reads != 1 {
		t.Errorf("reads %d, want 1", st.reads)
	}
	// A VM with none: read, nothing to show.
	if n := svc.Notes(context.Background(), VMRef{Host: "hv1", Name: "vm2"}, ""); n == nil || n.Origin != nil || n.Comment != "" {
		t.Errorf("vm2 %+v", n)
	}
	// Another UUID of that name is another VM.
	if n := svc.Notes(context.Background(), ref, "00000000-0000-4000-8000-000000000009"); n == nil || n.Origin != nil || n.Comment != "" {
		t.Errorf("other uuid %+v", n)
	}
	// Local mode and a failing store: nil, and no line.
	if n := (&Service{}).Notes(context.Background(), ref, stUUID); n != nil {
		t.Errorf("local mode %+v", n)
	}
	st.readDown = true
	if n := svc.Notes(context.Background(), ref, stUUID); n != nil {
		t.Errorf("store down %+v", n)
	}
	if len(lines) != 0 {
		t.Errorf("lines %q", lines)
	}
	// All of them at once, for the TUI.
	st.readDown = false
	all := svc.AllNotes(context.Background())
	if a, ok := all[ref]; !ok || a.Comment != "placeholder comment" || a.UUID != stUUID {
		t.Errorf("all %+v", all)
	}
	if (&Service{}).AllNotes(context.Background()) != nil {
		t.Error("local mode AllNotes not nil")
	}
}
