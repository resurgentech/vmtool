package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// tplSource is a repSource whose reader also lists templates.
type tplSource struct {
	*repSource
	tpls []vmtool.Template
	err  error
}

func (s *tplSource) Open() (InventoryReader, func(), error) { return s, func() {}, nil }

func (s *tplSource) Templates() ([]vmtool.Template, error) { return s.tpls, s.err }

func startReporterOver(t *testing.T, src InventorySource) *repEnv {
	t.Helper()
	e := &repEnv{store: newRepStore(), src: &repSource{}, clock: newRepClock(), log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	e.rep = NewInventoryReporter(InventoryReporterConfig{
		Host: "hv1", Version: "v-test", Store: e.store, Source: src,
		Now: e.clock.Now, After: e.clock.After,
		Log: slog.New(slog.NewTextHandler(lockedBuf{e.logMu, e.log}, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.rep.Start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return e
}

func newTplSource(tpls []vmtool.Template, err error) *tplSource {
	s := &tplSource{repSource: &repSource{}, tpls: tpls, err: err}
	s.set(vmtool.VMInfo{Name: "vm1", State: "running", UUID: repUUID1})
	return s
}

// The report carries the host's templates with only what the daemon has: an
// image template names its source image; a saved one does not; a creation
// time the daemon does not know is left out.
func TestReporterSendsTemplates(t *testing.T) {
	created := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	src := newTplSource([]vmtool.Template{
		{Name: "img1", Image: "img1", SHA256: repSHA, Origin: vmtool.TemplateOriginImage, Pool: "pool1", LV: "vmtpl-aaaaaaaaaaaa", SizeBytes: 20 << 30, Created: created},
		{Name: "golden", Origin: vmtool.TemplateOriginSaved, Pool: "pool1", LV: "vmtpl-bbbbbbbbbbbb", SizeBytes: 10 << 30},
	}, nil)
	e := startReporterOver(t, src)
	e.waitSend(t)
	rep := e.store.last()
	if rep.Templates == nil || len(*rep.Templates) != 2 {
		t.Fatalf("templates: %+v", rep.Templates)
	}
	i, s := (*rep.Templates)[0], (*rep.Templates)[1]
	if i.Name != "img1" || i.Kind != storeapi.ReportTemplateKindImage || i.Pool != "pool1" || i.SizeBytes != 20<<30 ||
		i.SourceImage == nil || *i.SourceImage != "img1" || i.Created == nil || !i.Created.Equal(created) {
		t.Errorf("image template: %+v", i)
	}
	if s.Name != "golden" || s.Kind != storeapi.ReportTemplateKindSaved || s.SourceImage != nil || s.Created != nil || s.SizeBytes != 10<<30 {
		t.Errorf("saved template: %+v", s)
	}
	if len(rep.Vms) != 1 {
		t.Errorf("vms: %+v", rep.Vms)
	}
}

// A host with no templates sends an empty list, not an absent one: "none" is
// told apart from "not reported".
func TestReporterSendsAnEmptyTemplateList(t *testing.T) {
	e := startReporterOver(t, newTplSource(nil, nil))
	e.waitSend(t)
	if tp := e.store.last().Templates; tp == nil || len(*tp) != 0 {
		t.Fatalf("templates: %+v", tp)
	}
}

// A reader that cannot list templates (an older source) reports none, and
// neither does one whose listing failed: the VMs still go out, and the
// templates key is absent so the store keeps what it had.
func TestReporterOmitsTemplatesItCannotList(t *testing.T) {
	e := startReporter(t) // repSource has no Templates
	e.waitSend(t)
	if tp := e.store.last().Templates; tp != nil {
		t.Errorf("a reader with no template listing sent %+v", tp)
	}

	e = startReporterOver(t, newTplSource(nil, errors.New("helper did not run")))
	e.waitSend(t)
	rep := e.store.last()
	if rep.Templates != nil || len(rep.Vms) != 1 {
		t.Errorf("a failed listing: templates %+v, vms %+v", rep.Templates, rep.Vms)
	}
	if !strings.Contains(e.logText(), "templates not reported") {
		t.Errorf("no log line for the failed listing:\n%s", e.logText())
	}
}
