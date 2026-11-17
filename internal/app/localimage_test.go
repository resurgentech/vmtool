package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

type refreshVMs struct {
	Manager
	refreshed bool
	refreshes int
}

func (r *refreshVMs) ImagePath(n string) (string, error) {
	if r.refreshed {
		return "/pool/" + n, nil
	}
	return "", fmt.Errorf("no %s: %w", n, vmtool.ErrNotFound)
}
func (r *refreshVMs) RefreshPools() error { r.refreshed = true; r.refreshes++; return nil }

func TestFindLocalImageRefreshesOnWorkstationOnly(t *testing.T) {
	ws := &refreshVMs{}
	p, err := FindLocalImage(ws, true, "u.qcow2")
	if err != nil || p != "/pool/u.qcow2" || ws.refreshes != 1 {
		t.Fatalf("workstation: %q %v refreshes=%d", p, err, ws.refreshes)
	}
	hv := &refreshVMs{}
	_, err = FindLocalImage(hv, false, "u.qcow2")
	if !vmtool.IsNotFound(err) || hv.refreshes != 0 {
		t.Fatalf("hypervisor: %v refreshes=%d", err, hv.refreshes)
	}
	// A Manager that cannot refresh just reports not found.
	_, err = FindLocalImage(&createVMsNoImage{}, true, "u.qcow2")
	if !vmtool.IsNotFound(err) {
		t.Fatalf("no refresher: %v", err)
	}
}

type createVMsNoImage struct{ Manager }

func (createVMsNoImage) ImagePath(n string) (string, error) {
	return "", fmt.Errorf("no %s: %w", n, vmtool.ErrNotFound)
}

func TestForeignImageErrSaysHowToGetPast(t *testing.T) {
	msg := foreignImageErr("u.qcow2", "hv1", "/p/u.qcow2").Error()
	for _, want := range []string{"will not replace it", "never replaces a file it did not create", "<name>-vmtool.qcow2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
}
