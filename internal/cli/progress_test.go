package cli

import (
	"bytes"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
)

func TestUploadProgressLine(t *testing.T) {
	var b bytes.Buffer
	if uploadProgress(&b, app.ProgressEvent{Stage: "clone", Status: "done", Detail: "cloned"}) {
		t.Fatal("non-upload event handled")
	}
	uploadProgress(&b, app.ProgressEvent{Stage: "upload", Status: "progress", Sent: 1 << 20, Total: 4 << 20})
	uploadProgress(&b, app.ProgressEvent{Stage: "upload", Status: "progress", Sent: 4 << 20, Total: 4 << 20})
	want := "\ruploading:  25% (1 / 4 MiB)\ruploading: 100% (4 / 4 MiB)\n"
	if b.String() != want {
		t.Fatalf("stderr %q\nwant %q", b.String(), want)
	}
	// "done" still goes to the normal (stdout) path.
	if uploadProgress(&b, app.ProgressEvent{Stage: "upload", Status: "done", Detail: "uploaded"}) {
		t.Error("done event swallowed")
	}
}
