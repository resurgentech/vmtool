package cli

import (
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
)

// Golden lines: the closing timings text is a contract (scripts read it), so
// one success and one failure are pinned byte for byte, including the
// formatting of a half-tenth (250 ms prints 0.2s, 350 ms 0.3s).
func TestTimingsLineGolden(t *testing.T) {
	ok := []app.ProgressEvent{
		{Stage: "clone", Status: "start", ElapsedMS: 0},
		{Stage: "clone", Status: "done", ElapsedMS: 250},
		{Stage: "grow_disk", Status: "done", ElapsedMS: 250},
		{Stage: "start", Status: "start", ElapsedMS: 250},
		{Stage: "start", Status: "done", ElapsedMS: 600},
		{Stage: "wait_ip", Status: "start", ElapsedMS: 600},
		{Stage: "wait_ip", Status: "done", ElapsedMS: 2600},
	}
	if got, want := timingsLine(ok), "timings: clone 0.2s, start 0.3s, wait_ip 2.0s, total 2.6s"; got != want {
		t.Errorf("success\ngot  %q\nwant %q", got, want)
	}
	failed := []app.ProgressEvent{
		{Stage: "clone", Status: "start", ElapsedMS: 0},
		{Stage: "clone", Status: "done", ElapsedMS: 300},
		{Stage: "wait_ssh", Status: "start", ElapsedMS: 300},
		{Stage: "wait_ssh", Status: "error", ElapsedMS: 90300},
	}
	if got, want := timingsLine(failed), "timings: clone 0.3s, wait_ssh 90.0s, total 90.3s"; got != want {
		t.Errorf("failure\ngot  %q\nwant %q", got, want)
	}
}
