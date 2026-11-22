package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/internal/tokenhash"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V3, the hop to the host: the real store (in-process, httptest) allocates
// the VNI and subnet, and a fake host decides what to do with them by the
// same PlanNetwork the daemon's Manager uses.

// planHost is a host whose libvirt is a list of networks. CreateNetworkVNI is
// PlanNetwork over it, as vmtool.Manager.CreateNetworkVNI is.
type planHost struct {
	Manager
	nets []vmtool.NetworkInfo
}

func (h *planHost) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	spec, err := vmtool.PlanNetwork(name, subnet, domain, h.nets, []netip.Prefix(nil), vni)
	if err != nil {
		return nil, err
	}
	v, _ := vmtool.BridgeVNI(spec.Bridge)
	n := vmtool.NetworkInfo{Name: name, Bridge: spec.Bridge, VNI: v, Vmtool: true, Subnet: spec.Subnet.String(), Domain: spec.Domain}
	h.nets = append(h.nets, n)
	return &n, nil
}

func (h *planHost) DeleteNetwork(name string) error {
	for i, n := range h.nets {
		if n.Name == name {
			h.nets = append(h.nets[:i], h.nets[i+1:]...)
		}
	}
	return nil
}

func realStoreSession(t *testing.T) (*StoreSession, *storeclient.Client) {
	t.Helper()
	sess, cl, _ := realStoreSessionDB(t)
	return sess, cl
}

// realStoreSessionDB is realStoreSession with the store's database, for a test
// that drives the store's own clock and sweep.
func realStoreSessionDB(t *testing.T) (*StoreSession, *storeclient.Client, *db.DB) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(dir, "store.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := &store.Config{Listen: "127.0.0.1:8095", Tokens: map[string]string{"alice": tokenhash.Hash(stTok)}}
	var logs bytes.Buffer
	ts := httptest.NewTLSServer(store.NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), store.Deps{Blob: blob.NewMemory(), DB: d}).Handler())
	t.Cleanup(ts.Close)
	pool := ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	opts := []storeclient.Option{storeclient.WithRootCAs(pool)}
	cl, err := storeclient.New(ts.URL, stTok, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return OpenStoreSession(ts.URL, func() (string, error) { return stTok, nil }, opts, nil), cl, d
}

func TestCreateNetworkEndToEndWithTheRealStore(t *testing.T) {
	sess, cl := realStoreSession(t)
	s := &Service{Store: sess}
	ctx := context.Background()
	h := &planHost{}
	n1, err := s.CreateNetworkOnHost(ctx, "home1", h, "net1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n1.Bridge != "vmt-002710" || n1.Subnet != "10.77.0.0/24" {
		t.Errorf("net1 = %+v, want the registry's first VNI 10000 and /24", n1)
	}
	n2, err := s.CreateNetworkOnHost(ctx, "home1", h, "net2", "", "")
	if err != nil || n2.Bridge != "vmt-002711" || n2.Subnet != "10.77.1.0/24" {
		t.Errorf("net2 = %+v, %v", n2, err)
	}
	if r, err := cl.GetNetwork(ctx, "net2"); err != nil || r.Vni != 10001 || r.Home != "home1" {
		t.Errorf("registry net2 = %+v, %v", r, err)
	}

	// The host already has a bridge of the number the registry hands out next
	// (10002): the host's 409 comes through and the registry entry is gone.
	h.nets = append(h.nets, vmtool.NetworkInfo{Name: "stray", Bridge: "vmt-002712", Subnet: "192.0.2.0/24"})
	_, err = s.CreateNetworkOnHost(ctx, "home1", h, "net3", "", "")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "vmt-002712") {
		t.Fatalf("clash: %v", err)
	}
	if _, err := cl.GetNetwork(ctx, "net3"); !errors.Is(err, storeclient.ErrNotFound) {
		t.Errorf("registry net3 after a failed create: %v, want not found", err)
	}
	// The number is free again in the registry: with the stray gone it is reused.
	h.DeleteNetwork("stray")
	n3, err := s.CreateNetworkOnHost(ctx, "home1", h, "net3", "", "")
	if err != nil || n3.Bridge != "vmt-002712" {
		t.Errorf("net3 = %+v, %v, want the reused VNI 10002", n3, err)
	}

	// An old host numbers the bridge itself (vmt-000001): undone on both sides.
	old := &vniMgr{oldHost: true}
	_, err = s.CreateNetworkOnHost(ctx, "home1", old, "net4", "", "")
	if err == nil || !strings.Contains(err.Error(), "just deploy home1") {
		t.Fatalf("old host: %v", err)
	}
	if _, err := cl.GetNetwork(ctx, "net4"); !errors.Is(err, storeclient.ErrNotFound) {
		t.Errorf("registry net4 after an old host: %v, want not found", err)
	}
}
