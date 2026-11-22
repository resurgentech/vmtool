package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// tmplHost is a host with thin pools: createVMs that caches images (storeVMs)
// and manages templates, recording the order of every call into calls.
type tmplHost struct {
	storeVMs
	calls  []string
	list   []TemplateInfo
	delErr error
}

func (f *tmplHost) ListTemplates() ([]TemplateInfo, error) {
	f.calls = append(f.calls, "ListTemplates")
	return f.list, nil
}

func (f *tmplHost) PrepareTemplate(image, pool string) (TemplateInfo, error) {
	f.calls = append(f.calls, fmt.Sprintf("PrepareTemplate %s %s", image, pool))
	return TemplateInfo{Template: vmtool.Template{Name: image, Image: image, Pool: "thin-test", LV: "vmtpl-aaaaaaaaaaaa", Origin: vmtool.TemplateOriginImage}}, nil
}

func (f *tmplHost) DeleteTemplate(name, pool string) (TemplateInfo, error) {
	f.calls = append(f.calls, fmt.Sprintf("DeleteTemplate %s %s", name, pool))
	if f.delErr != nil {
		return TemplateInfo{}, f.delErr
	}
	return TemplateInfo{Template: vmtool.Template{Name: name, LV: name, Pool: pool}}, nil
}

func tplSvc(t *testing.T, found bool) *Service {
	t.Helper()
	t.Chdir(t.TempDir())
	local := filepath.Join(t.TempDir(), "img1")
	if err := os.WriteFile(local, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &Service{ImageCachePath: filepath.Join(t.TempDir(), vmtool.ImageManifestFile)}
	if found {
		svc.Images = ImageSourceFunc(func(string) (string, error) { return local, nil })
	}
	return svc
}

// PrepareTemplate syncs the image to the host (an upload here: the host has
// no copy) and only then asks the host to build.
func TestPrepareTemplateSyncsThenBuilds(t *testing.T) {
	svc := tplSvc(t, true)
	m := &tmplHost{}
	m.pools = thinTestPools
	got, err := svc.PrepareTemplate(context.Background(), m, PrepareTemplateInput{Host: "hv1", Image: "img1", Pool: "thin-test"})
	if err != nil {
		t.Fatal(err)
	}
	if m.uploads != 1 {
		t.Errorf("uploads = %d, want 1 (the sync create does)", m.uploads)
	}
	if strings.Join(m.calls, "|") != "PrepareTemplate img1 thin-test" || got.LV != "vmtpl-aaaaaaaaaaaa" {
		t.Errorf("calls %q, got %+v", m.calls, got)
	}

	// up to date on the host: no upload, still builds
	m = &tmplHost{}
	svc2 := tplSvc(t, false)
	m.remote = RemoteImage{SHA256: "ab", Size: 9}
	if _, err := svc2.PrepareTemplate(context.Background(), m, PrepareTemplateInput{Host: "hv1", Image: "img1"}); err != nil || m.uploads != 0 || len(m.calls) != 1 {
		t.Errorf("cached only: %v uploads=%d calls=%q", err, m.uploads, m.calls)
	}

	// a foreign volume of that name: conflict, and the host is never asked
	m = &tmplHost{}
	m.remote = RemoteImage{Foreign: true, Path: "/p/img1"}
	svc3 := tplSvc(t, true)
	_, err = svc3.PrepareTemplate(context.Background(), m, PrepareTemplateInput{Host: "hv1", Image: "img1"})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindConflict || len(m.calls) != 0 {
		t.Errorf("foreign: %v calls=%q", err, m.calls)
	}
}

func TestPrepareTemplateValidates(t *testing.T) {
	svc := tplSvc(t, true)
	for _, img := range []string{"", "../x", "a/b", ".hidden"} {
		m := &tmplHost{}
		_, err := svc.PrepareTemplate(context.Background(), m, PrepareTemplateInput{Host: "hv1", Image: img})
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindInvalid || len(m.calls) != 0 || m.uploads != 0 {
			t.Errorf("image %q: %v calls=%q", img, err, m.calls)
		}
	}
	// a named pool that is not thin is refused before any upload
	m := &tmplHost{}
	m.pools = thinTestPools
	_, err := svc.PrepareTemplate(context.Background(), m, PrepareTemplateInput{Host: "hv1", Image: "img1", Pool: "vmtool"})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindInvalid || m.uploads != 0 || len(m.calls) != 0 {
		t.Errorf("non-thin pool: %v uploads=%d calls=%q", err, m.uploads, m.calls)
	}
}

// A Manager without templates (local libvirt) has none: list is empty,
// prepare is invalid, delete is not found, prune is a no-op.
func TestTemplatesOnHostWithoutThinPools(t *testing.T) {
	svc := tplSvc(t, true)
	m := &createVMs{}
	ctx := context.Background()
	if l, err := svc.ListTemplates(ctx, m); err != nil || len(l) != 0 || l == nil {
		t.Errorf("list = %v, %v", l, err)
	}
	_, err := svc.PrepareTemplate(ctx, m, PrepareTemplateInput{Host: "hv1", Image: "img1"})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindInvalid || !errors.Is(err, ErrNoThinPools) {
		t.Errorf("prepare: %v", err)
	}
	_, err = svc.DeleteTemplate(ctx, m, "img1", "")
	if !AsError(err, &ae) || ae.Kind != KindNotFound || !errors.Is(err, vmtool.ErrNotFound) {
		t.Errorf("delete: %v", err)
	}
	if r, err := svc.PruneTemplates(ctx, m); err != nil || len(r) != 0 {
		t.Errorf("prune: %v %v", r, err)
	}
}

func tpl(name, lv, origin string, stale bool) TemplateInfo {
	t := vmtool.Template{Name: name, LV: lv, Pool: "thin-test", Origin: origin}
	if origin == vmtool.TemplateOriginImage {
		t.Image = name
	}
	return TemplateInfo{Template: t, Stale: stale}
}

// Prune deletes the listed stale templates, each by its LV name and pool, and
// nothing else.
func TestPruneTemplatesDeletesOnlyStale(t *testing.T) {
	svc := tplSvc(t, false)
	m := &tmplHost{list: []TemplateInfo{
		tpl("img1", "vmtpl-aaaaaaaaaaaa", vmtool.TemplateOriginImage, true),
		tpl("img1", "vmtpl-bbbbbbbbbbbb", vmtool.TemplateOriginImage, false),
		tpl("proj", "vmtpl-000000000001", vmtool.TemplateOriginSaved, false),
		tpl("img2", "vmtpl-cccccccccccc", vmtool.TemplateOriginImage, true),
	}}
	removed, err := svc.PruneTemplates(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	want := "ListTemplates|DeleteTemplate vmtpl-aaaaaaaaaaaa thin-test|DeleteTemplate vmtpl-cccccccccccc thin-test"
	if strings.Join(m.calls, "|") != want || len(removed) != 2 {
		t.Errorf("calls %q removed %d, want %q", m.calls, len(removed), want)
	}

	// one failing delete does not stop the others; the error says which
	m = &tmplHost{list: []TemplateInfo{tpl("a", "vmtpl-aaaaaaaaaaaa", vmtool.TemplateOriginImage, true), tpl("b", "vmtpl-bbbbbbbbbbbb", vmtool.TemplateOriginImage, true)}, delErr: errors.New("busy")}
	removed, err = svc.PruneTemplates(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "vmtpl-aaaaaaaaaaaa") || !strings.Contains(err.Error(), "busy") || len(removed) != 0 || len(m.calls) != 3 {
		t.Errorf("failing delete: %v removed=%d calls=%q", err, len(removed), m.calls)
	}
}

func TestDeleteTemplateValidatesName(t *testing.T) {
	svc := tplSvc(t, false)
	for _, n := range []string{"", "../x", "a/b", "/dev/vg-test/vm1"} {
		m := &tmplHost{}
		_, err := svc.DeleteTemplate(context.Background(), m, n, "")
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindInvalid || len(m.calls) != 0 {
			t.Errorf("name %q: %v calls=%q", n, err, m.calls)
		}
	}
}

// ListAllTemplates: one entry per host in order; a failing, unopenable or slow
// host is an error entry, and a host without thin pools has no rows.
func TestListAllTemplates(t *testing.T) {
	svc := &Service{}
	block := make(chan struct{})
	defer close(block)
	open := func(host string) (Manager, func(), error) {
		switch host {
		case "hv1":
			return &tmplHost{list: []TemplateInfo{tpl("img1", "vmtpl-aaaaaaaaaaaa", vmtool.TemplateOriginImage, false)}}, func() {}, nil
		case "plain":
			return &createVMs{}, func() {}, nil
		case "slow":
			<-block
			return nil, nil, errors.New("late")
		}
		return nil, nil, errors.New("no such host")
	}
	got := svc.ListAllTemplates(context.Background(), []string{"hv1", "plain", "slow", "gone"}, open, 50*time.Millisecond)
	if len(got) != 4 || got[0].Host != "hv1" || len(got[0].Templates) != 1 || got[0].Err != nil {
		t.Fatalf("hv1: %+v", got)
	}
	if got[1].Err != nil || len(got[1].Templates) != 0 {
		t.Errorf("plain: %+v", got[1])
	}
	if got[2].Err == nil || !strings.Contains(got[2].Err.Error(), "timed out") {
		t.Errorf("slow: %+v", got[2])
	}
	if got[3].Err == nil || got[3].Host != "gone" {
		t.Errorf("gone: %+v", got[3])
	}
}

// ListAllTemplates does not open or ask a network home: its row says so.
func TestListAllTemplatesSkipsNetworkHome(t *testing.T) {
	svc := &Service{NetworkHome: func(h string) bool { return h == "home1" }}
	var mu sync.Mutex
	var opened []string
	open := func(host string) (Manager, func(), error) {
		mu.Lock()
		opened = append(opened, host)
		mu.Unlock()
		if host == "home1" {
			return nil, nil, errors.New("home1 is unreachable")
		}
		return &tmplHost{list: []TemplateInfo{tpl("img1", "vmtpl-aaaaaaaaaaaa", vmtool.TemplateOriginImage, false)}}, func() {}, nil
	}
	start := time.Now()
	got := svc.ListAllTemplates(context.Background(), []string{"hv1", "home1"}, open, 5*time.Second)
	if time.Since(start) > time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if len(got) != 2 || got[0].Err != nil || len(got[0].Templates) != 1 {
		t.Fatalf("hv1: %+v", got)
	}
	if got[1].Host != "home1" || !got[1].NetworkHome || got[1].Err != nil || len(got[1].Templates) != 0 {
		t.Errorf("home1 row: %+v", got[1])
	}
	for _, h := range opened {
		if h == "home1" {
			t.Error("the network home was opened")
		}
	}
}
