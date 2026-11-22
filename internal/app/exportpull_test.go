package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// `templates export` and `images pull host/<name>`: the workflows. The host is
// a fake; no daemon, libvirt or network is touched.

// xferHost is a host that exports templates and serves image blobs.
type xferHost struct {
	Manager
	// export
	events    []ExportEvent
	exported  ExportedImage
	exportErr error
	exports   []string
	hashes    []bool
	// blob
	blob     []byte
	declared int64  // Content-Length the host says; -1 = same as blob
	digest   string // digest the host says; "" = the blob's
	openErr  error
	opens    int
	onOpen   func() // runs inside OpenImageBlob (a race injected by a test)
	failMid  bool
	// trailer: the digest is not in the header; it follows the body
	useTrailer bool
	trailer    string // what the trailer says; "" = the blob's digest
	noTrailer  bool   // the host declared a trailer and sent none
}

func (h *xferHost) ExportTemplate(_ context.Context, name, pool string, hash bool, on func(ExportEvent)) (ExportedImage, error) {
	h.exports = append(h.exports, name+" "+pool)
	h.hashes = append(h.hashes, hash)
	for _, e := range h.events {
		on(e)
	}
	return h.exported, h.exportErr
}

type failingReader struct {
	r io.Reader
	n int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection reset")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

func (h *xferHost) OpenImageBlob(_ context.Context, name string) (io.ReadCloser, ImageBlobInfo, error) {
	h.opens++
	if h.onOpen != nil {
		h.onOpen()
	}
	if h.openErr != nil {
		return nil, ImageBlobInfo{}, h.openErr
	}
	info := ImageBlobInfo{Size: int64(len(h.blob)), SHA256: sumHex(h.blob)}
	if h.declared != 0 {
		info.Size = h.declared
	}
	if h.digest != "" {
		info.SHA256 = h.digest
	}
	if h.useTrailer {
		v := h.trailer
		if v == "" && !h.noTrailer {
			v = sumHex(h.blob)
		}
		info.SHA256 = ""
		info.Trailer = func() string { return v }
	}
	var r io.Reader = bytes.NewReader(h.blob)
	if h.failMid {
		r = &failingReader{r: r, n: len(h.blob) / 2}
	}
	return io.NopCloser(r), info, nil
}

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestExportTemplateMapsEventsToProgress(t *testing.T) {
	h := &xferHost{
		events:   []ExportEvent{{Stage: "export", Received: 0, Total: 100}, {Stage: "export", Received: 50, Total: 100}, {Stage: "hash", Received: 60, Total: 60}},
		exported: ExportedImage{Name: "proj", SHA256: strings.Repeat("a", 64), Size: 60, Path: "/var/lib/vmtool/images/proj"},
	}
	var got []ProgressEvent
	img, err := (&Service{}).ExportTemplate(context.Background(), h, ExportTemplateInput{Host: "hv1", Name: "proj", Pool: "thin-test",
		OnProgress: func(ev ProgressEvent) { got = append(got, ev) }})
	if err != nil {
		t.Fatal(err)
	}
	if img.Name != "proj" || img.Size != 60 || strings.Join(h.exports, "|") != "proj thin-test" {
		t.Errorf("image %+v exports %v", img, h.exports)
	}
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.Stage+"/"+e.Status)
	}
	if got[0].Status != "start" || !strings.Contains(got[0].Detail, "hv1/proj") ||
		got[len(got)-1].Status != "done" || !strings.Contains(got[len(got)-1].Detail, "proj") {
		t.Errorf("events %v: %+v", kinds, got)
	}
	var prog []ProgressEvent
	for _, e := range got {
		if e.Status == "progress" {
			prog = append(prog, e)
		}
	}
	if len(prog) != 3 || prog[1].Sent != 50 || prog[1].Total != 100 || prog[0].Stage != "export" || prog[2].Detail != "hash" {
		t.Errorf("progress %+v", prog)
	}
}

func TestExportTemplateRefusals(t *testing.T) {
	ctx := context.Background()
	// a name that is not a plain name never reaches the host
	h := &xferHost{}
	_, err := (&Service{}).ExportTemplate(ctx, h, ExportTemplateInput{Host: "hv1", Name: "../x"})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindInvalid || len(h.exports) != 0 {
		t.Errorf("bad name: %v, exports %v", err, h.exports)
	}
	// a host without thin pools (no exporter): invalid, said plainly
	_, err = (&Service{}).ExportTemplate(ctx, struct{ Manager }{}, ExportTemplateInput{Host: "hv1", Name: "proj"})
	if !AsError(err, &ae) || ae.Kind != KindInvalid || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("no exporter: %v", err)
	}
	// the host's refusal is the error, with its kind
	h = &xferHost{exportErr: &Error{Kind: KindConflict, Err: errors.New("image proj already exists")}}
	var evs []ProgressEvent
	_, err = (&Service{}).ExportTemplate(ctx, h, ExportTemplateInput{Host: "hv1", Name: "proj", OnProgress: func(e ProgressEvent) { evs = append(evs, e) }})
	if !AsError(err, &ae) || ae.Kind != KindConflict || ae.Op != "export_template" {
		t.Errorf("conflict: %v", err)
	}
	if n := len(evs); n == 0 || evs[n-1].Status != "error" {
		t.Errorf("no error event: %+v", evs)
	}
}

func pullDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPullHostImageWritesTheFile(t *testing.T) {
	dir := pullDir(t)
	data := bytes.Repeat([]byte("PLACEHOLDER image "), 5000)
	h := &xferHost{blob: data}
	var prog []ProgressEvent
	got, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir,
		OnProgress: func(e ProgressEvent) { prog = append(prog, e) }})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "proj")
	if got.Path != want || got.Size != int64(len(data)) || got.SHA256 != sumHex(data) || got.Unchanged {
		t.Errorf("result %+v", got)
	}
	if b, _ := os.ReadFile(want); !bytes.Equal(b, data) {
		t.Error("file differs")
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Errorf("directory %v: a temporary file was left", names)
	}
	if len(prog) < 2 || prog[0].Status != "start" || prog[len(prog)-1].Status != "done" {
		t.Errorf("progress %+v", prog)
	}
}

// A file of that name already there: the same SHA-256 is "nothing to do";
// a different one is refused and is never touched, and nothing is downloaded
// into the way.
func TestPullHostImageNeverOverwritesADifferentFile(t *testing.T) {
	dir := pullDir(t)
	data := []byte("PLACEHOLDER the host's image")
	dest := filepath.Join(dir, "proj")

	// same content: unchanged, and the host's bytes are not even opened twice
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h := &xferHost{blob: data}
	got, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
	if err != nil || !got.Unchanged || got.Path != dest {
		t.Fatalf("same file: %+v, %v", got, err)
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Errorf("directory %v", names)
	}

	// different content: refused, untouched
	other := []byte("PLACEHOLDER something else the user keeps")
	if err := os.WriteFile(dest, other, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindConflict || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--to") {
		t.Errorf("different file: %v", err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, other) {
		t.Error("the existing file was changed")
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Errorf("directory %v", names)
	}

	// a symlink of that name is a different file too (never followed or replaced)
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(dest, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "linked", Dir: dir}); err == nil {
		t.Error("a symlink was replaced")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink gone: %v", err)
	}
}

// The file that appears between the check and the rename is a different file:
// the rename is a link that fails instead of overwriting.
func TestPullHostImageRaceKeepsTheNewFile(t *testing.T) {
	dir := pullDir(t)
	dest := filepath.Join(dir, "proj")
	theirs := []byte("PLACEHOLDER appeared during the download")
	h := &xferHost{blob: []byte("PLACEHOLDER the host's image"), onOpen: func() {
		if err := os.WriteFile(dest, theirs, 0o644); err != nil {
			t.Error(err)
		}
	}}
	_, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindConflict {
		t.Errorf("race: %v", err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, theirs) {
		t.Errorf("the file that appeared was overwritten: %q", b)
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Errorf("directory %v: a temporary file was left", names)
	}
}

// A size or digest that does not match, a stream that breaks, and a host that
// says nothing of its size or digest all leave nothing in the directory.
func TestPullHostImageMismatchLeavesNothing(t *testing.T) {
	data := []byte("PLACEHOLDER the host's image bytes")
	for name, h := range map[string]*xferHost{
		"wrong digest":      {blob: data, digest: strings.Repeat("0", 64)},
		"short":             {blob: data, declared: int64(len(data)) + 10},
		"long":              {blob: data, declared: int64(len(data)) - 5},
		"broken stream":     {blob: data, failMid: true},
		"no digest":         {blob: data, digest: "x"},
		"open refused":      {openErr: &Error{Kind: KindNotFound, Err: fmt.Errorf("no such image: %w", vmtool.ErrNotFound)}},
		"foreign refused":   {openErr: &Error{Kind: KindConflict, Err: errors.New("will not serve it")}},
		"empty declared":    {blob: data, declared: -1},
		"host without blob": nil,
	} {
		dir := pullDir(t)
		var m Manager = h
		if h == nil {
			m = struct{ Manager }{}
		}
		_, err := (&Service{}).PullHostImage(context.Background(), m, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if names := dirEntries(t, dir); len(names) != 0 {
			t.Errorf("%s: left %v", name, names)
		}
	}
	// the mismatch is reported as one, and the not-found stays not found
	h := &xferHost{blob: data, digest: strings.Repeat("0", 64)}
	_, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: pullDir(t)})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindBadGateway || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("digest mismatch: %v", err)
	}
	h = &xferHost{openErr: &Error{Kind: KindNotFound, Err: vmtool.ErrNotFound}}
	_, err = (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: pullDir(t)})
	if !AsError(err, &ae) || ae.Kind != KindNotFound {
		t.Errorf("not found: %v", err)
	}
}

func TestPullHostImageRefusesBadInput(t *testing.T) {
	dir := pullDir(t)
	h := &xferHost{blob: []byte("x")}
	for _, name := range []string{"", "../x", "a/b", ".hidden"} {
		_, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: name, Dir: dir})
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindInvalid {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj"}); err == nil {
		t.Error("no directory")
	}
	if _, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a directory that does not exist")
	}
	if h.opens != 0 {
		t.Errorf("the host was asked %d times for input that is refused first", h.opens)
	}
}

// The digest follows the last byte (a template export whose digest the host
// did not know when the send began): the pull compares at the end.
func TestPullHostImageTrailerDigest(t *testing.T) {
	data := bytes.Repeat([]byte("PLACEHOLDER trailer image "), 4000)
	dir := pullDir(t)
	h := &xferHost{blob: data, useTrailer: true}
	got, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != sumHex(data) || got.Size != int64(len(data)) || got.Unchanged {
		t.Errorf("result %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "proj")); !bytes.Equal(b, data) {
		t.Error("file differs")
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Errorf("directory %v", names)
	}
	// The same file already there: nothing to do (the stream is read to compare).
	again, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
	if err != nil || !again.Unchanged || again.SHA256 != sumHex(data) {
		t.Errorf("same file again: %+v %v", again, err)
	}
	// A different file of the same size is never replaced.
	other := bytes.Repeat([]byte("x"), len(data))
	if err := os.WriteFile(filepath.Join(dir, "proj"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindConflict {
		t.Errorf("different file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "proj")); !bytes.Equal(b, other) {
		t.Error("the existing file was replaced")
	}
}

// A trailer that does not match the bytes, no trailer, or a malformed one keeps
// nothing, and a truncated body is refused before the trailer is looked at.
func TestPullHostImageTrailerMismatchOrMissingKeepsNothing(t *testing.T) {
	data := bytes.Repeat([]byte("PLACEHOLDER trailer image "), 4000)
	for name, tc := range map[string]struct {
		h    *xferHost
		want string
	}{
		"mismatch":  {&xferHost{blob: data, useTrailer: true, trailer: strings.Repeat("0", 64)}, "nothing was kept"},
		"missing":   {&xferHost{blob: data, useTrailer: true, noTrailer: true}, "after the last byte"},
		"malformed": {&xferHost{blob: data, useTrailer: true, trailer: "zz"}, "after the last byte"},
		"short":     {&xferHost{blob: data, useTrailer: true, declared: int64(len(data)) + 10}, "got"},
		"long":      {&xferHost{blob: data, useTrailer: true, declared: int64(len(data)) - 5}, "more than"},
	} {
		dir := pullDir(t)
		_, err := (&Service{}).PullHostImage(context.Background(), tc.h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: dir})
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindBadGateway || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a bad gateway containing %q", name, err, tc.want)
		}
		if names := dirEntries(t, dir); len(names) != 0 {
			t.Errorf("%s: left %v", name, names)
		}
	}
}

// A host that sends neither a header digest nor a trailer is the old error.
func TestPullHostImageNeitherDigestNorTrailer(t *testing.T) {
	h := &xferHost{blob: []byte("PLACEHOLDER"), digest: "x"}
	_, err := (&Service{}).PullHostImage(context.Background(), h, PullHostImageInput{Host: "hv1", Name: "proj", Dir: pullDir(t)})
	if err == nil || !strings.Contains(err.Error(), "did not send a SHA-256 digest") {
		t.Errorf("%v", err)
	}
}
