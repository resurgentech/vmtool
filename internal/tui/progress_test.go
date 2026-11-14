package tui

import (
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
)

// Upload progress retitles the running create entry; other events do not.
func TestUploadProgressRetitlesLog(t *testing.T) {
	m := initialModel(nil, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	idx := m.addLog("creating veritech01/web...")
	ev := app.ProgressEvent{Stage: "upload", Status: "progress", Sent: 3 << 20, Total: 12 << 20}
	title := uploadTitle("veritech01/web", ev)
	if title != "creating veritech01/web: uploading image 25% (3 / 12 MiB)" {
		t.Fatalf("title %q", title)
	}
	nm, _ := m.Update(progressMsg{logIdx: idx, title: title})
	m = nm.(model)
	if m.log[idx].title != title {
		t.Fatalf("log title %q", m.log[idx].title)
	}
	if uploadTitle("x", app.ProgressEvent{Stage: "clone", Status: "done"}) != "" {
		t.Error("non-upload event titled")
	}
	if !strings.Contains(uploadTitle("x", app.ProgressEvent{Stage: "upload", Status: "hash"}), "hashing") {
		t.Error("hash event not shown")
	}
	// A finished entry is not retitled by a late progress message.
	m.log[idx].status = logStatusDone
	nm, _ = m.Update(progressMsg{logIdx: idx, title: "late"})
	if nm.(model).log[idx].title == "late" {
		t.Error("finished entry retitled")
	}
	// No program: sending is a no-op, not a panic.
	var s *msgSender
	s.Send(progressMsg{})
}

// The service the TUI builds finds local images through the local Manager.
func TestSvcLocalImages(t *testing.T) {
	m := initialModel(imgFake{}, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	p, err := m.svc().LocalImages("u.qcow2")
	if err != nil || p != "/pool/u.qcow2" {
		t.Fatalf("LocalImages = %q, %v", p, err)
	}
}

type imgFake struct{ app.Manager }

func (imgFake) ImagePath(n string) (string, error) { return "/pool/" + n, nil }
