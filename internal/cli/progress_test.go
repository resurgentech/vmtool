package cli

import (
	"bytes"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
)

func TestUploadProgressLine(t *testing.T) {
	var b bytes.Buffer
	if imageTransferProgress(&b, app.ProgressEvent{Stage: "clone", Status: "done", Detail: "cloned"}) {
		t.Fatal("non-upload event handled")
	}
	imageTransferProgress(&b, app.ProgressEvent{Stage: "upload", Status: "progress", Sent: 1 << 20, Total: 4 << 20})
	imageTransferProgress(&b, app.ProgressEvent{Stage: "upload", Status: "progress", Sent: 4 << 20, Total: 4 << 20})
	want := "\ruploading:  25% (1 / 4 MiB)\ruploading: 100% (4 / 4 MiB)\n"
	if b.String() != want {
		t.Fatalf("stderr %q\nwant %q", b.String(), want)
	}
	// "done" still goes to the normal (stdout) path.
	if imageTransferProgress(&b, app.ProgressEvent{Stage: "upload", Status: "done", Detail: "uploaded"}) {
		t.Error("done event swallowed")
	}
}

// The progress line names what is happening: the host fetching from the
// store, this machine relaying it, or this machine uploading its own copy.
func TestTransferProgressWordsByStage(t *testing.T) {
	for stage, word := range map[string]string{"fetch": "fetching", "relay": "relaying", "upload": "uploading"} {
		var b bytes.Buffer
		if !imageTransferProgress(&b, app.ProgressEvent{Stage: stage, Status: "progress", Sent: 1 << 20, Total: 4 << 20}) {
			t.Fatalf("%s: progress not handled", stage)
		}
		if want := "\r" + word + ":  25% (1 / 4 MiB)"; b.String() != want {
			t.Errorf("%s: %q, want %q", stage, b.String(), want)
		}
		if imageTransferProgress(&b, app.ProgressEvent{Stage: stage, Status: "done", Detail: "x"}) {
			t.Errorf("%s: done event swallowed", stage)
		}
	}
}

// The timings line names each transfer stage that ran under its own name.
func TestTimingsLineNamesTransferStages(t *testing.T) {
	evs := []app.ProgressEvent{
		{Stage: "fetch", Status: "start", ElapsedMS: 0},
		{Stage: "fetch", Status: "done", ElapsedMS: 8300},
		{Stage: "relay", Status: "start", ElapsedMS: 8300},
		{Stage: "relay", Status: "error", ElapsedMS: 9300},
		{Stage: "upload", Status: "hash", ElapsedMS: 9300},
		{Stage: "upload", Status: "done", ElapsedMS: 11300},
	}
	want := "timings: fetch 8.3s, relay 1.0s, upload 2.0s, total 11.3s"
	if got := timingsLine(evs); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}
