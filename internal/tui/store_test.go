package tui

import (
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
)

// The TUI's workflow layer carries the run's store session, so credential
// and image work in the TUI uses the mode the CLI decided at start.
func TestSvcCarriesStoreSession(t *testing.T) {
	m := initialModel(nil, "hv1", "pb", "inv.yml")
	if m.svc().Store != nil {
		t.Fatal("a session appeared from nowhere")
	}
	sess := app.OpenStoreSession("https://PLACEHOLDER-HOST:8095", func() (string, error) { return "PLACEHOLDER-not-a-secret-1", nil }, nil, nil)
	m.store = sess
	if m.svc().Store != sess {
		t.Fatal("svc() dropped the store session")
	}
}
