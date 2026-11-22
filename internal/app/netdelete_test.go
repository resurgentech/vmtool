package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/internal/tokenhash"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// G2: `net delete` of a registered network removes it everywhere and its
// registry entry. The registry is the real store (httptest) behind a handler
// that records every request and can fail one; the hosts are fakes.
// Placeholders: network net1; hosts home1, hv1, hv2; VNI 10000.

const delHostTok = "host-token-placeholder"

// faultStore is the real store with a request log and one-shot failures.
type faultStore struct {
	mu     sync.Mutex
	log    []string
	faults map[string]int // "METHOD path" -> times left to answer 500
}

func (f *faultStore) failOnce(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[key]++
}

func (f *faultStore) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// indexOf is the position of the first logged request with this exact text.
func indexOf(reqs []string, key string) int {
	for i, r := range reqs {
		if r == key {
			return i
		}
	}
	return -1
}

type delEnv struct {
	*spanEnv
	fs   *faultStore
	host *storeclient.Client // with hv1's host token
}

func newDelEnv(t *testing.T) *delEnv {
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
	c := &store.Config{Listen: "127.0.0.1:8095", Tokens: map[string]string{"alice": tokenhash.Hash(stTok)},
		HostTokens: map[string]string{"hv1": tokenhash.Hash(delHostTok)}}
	var logs bytes.Buffer
	inner := store.NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), store.Deps{Blob: blob.NewMemory(), DB: d}).Handler()
	fs := &faultStore{faults: map[string]int{}}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		fs.mu.Lock()
		fs.log = append(fs.log, key)
		fail := fs.faults[key] > 0
		if fail {
			fs.faults[key]--
		}
		fs.mu.Unlock()
		if fail {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	pool := ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	opts := []storeclient.Option{storeclient.WithRootCAs(pool)}
	cl, err := storeclient.New(ts.URL, stTok, opts...)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := storeclient.New(ts.URL, delHostTok, opts...)
	if err != nil {
		t.Fatal(err)
	}
	sess := OpenStoreSession(ts.URL, func() (string, error) { return stTok, nil }, opts, nil)
	e := &spanEnv{cl: cl, down: map[string]bool{}, hosts: map[string]*netHost{
		"home1": newNetHost("home1", "192.0.2.13"),
		"hv1":   newNetHost("hv1", "192.0.2.11"),
		"hv2":   newNetHost("hv2", "192.0.2.12"),
	}}
	e.s = &Service{Store: sess, OpenHost: e.open, HostAddr: func(h string) string {
		if m, ok := e.hosts[h]; ok {
			return m.ip
		}
		return ""
	}}
	return &delEnv{spanEnv: e, fs: fs, host: hc}
}

// span makes net1 on home1, joined by hv1 and hv2, with one recorded reservation.
func (e *delEnv) span(t *testing.T) {
	t.Helper()
	e.makeNet(t, "net1")
	ctx := context.Background()
	for _, h := range []string{"hv1", "hv2"} {
		if _, err := e.s.JoinNetwork(ctx, h, "net1", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.cl.PutNetworkReservation(ctx, "net1", "52:54:00:00:00:01", storeclient.NetworkReservationInput{Ip: "10.77.0.50", Host: "hv1", Vm: "web1"}); err != nil {
		t.Fatal(err)
	}
}

func (e *delEnv) deleteOn(host string) (*DeleteNetworkResult, error) {
	return e.s.DeleteNetworkOnHost(context.Background(), host, e.hosts[host], "net1")
}

// registryHas reports whether the registry still has net1.
func (e *delEnv) registryHas(t *testing.T) bool {
	t.Helper()
	_, err := e.cl.GetNetwork(context.Background(), "net1")
	if errors.Is(err, storeclient.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func (e *delEnv) hostHas(host string) bool {
	_, ok := e.hosts[host].nets["net1"]
	return ok
}

func TestDeleteNetworkAllAwakeRemovesItEverywhere(t *testing.T) {
	e := newDelEnv(t)
	e.span(t)
	res, err := e.deleteOn("home1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Registered || strings.Join(res.Members, ",") != "hv1,hv2" || len(res.Left) != 0 {
		t.Errorf("result = %+v", res)
	}
	for _, h := range []string{"home1", "hv1", "hv2"} {
		if e.hostHas(h) {
			t.Errorf("%s still has net1", h)
		}
	}
	for _, h := range []string{"hv1", "hv2", "home1"} {
		if len(e.hosts[h].unlinked) != 1 {
			t.Errorf("%s unlinked %v, want once (before it undefined)", h, e.hosts[h].unlinked)
		}
	}
	if e.registryHas(t) {
		t.Error("the registry still has net1")
	}
	// The order: members' rows, then the reservation records, the home's row,
	// the entry last.
	r := e.fs.requests()
	rows := "DELETE /v1/networks/net1/members/"
	res1, res2, rowHome, entry := indexOf(r, rows+"hv1"), indexOf(r, rows+"hv2"), indexOf(r, rows+"home1"), indexOf(r, "DELETE /v1/networks/net1")
	rsv := indexOf(r, "DELETE /v1/networks/net1/reservations/52:54:00:00:00:01")
	if res1 < 0 || res2 < 0 || rsv < 0 || rowHome < 0 || entry < 0 || !(res1 < rsv && res2 < rsv && rsv < rowHome && rowHome < entry) {
		t.Errorf("registry calls out of order or missing (hv1 %d, hv2 %d, reservation %d, home row %d, entry %d):\n%s", res1, res2, rsv, rowHome, entry, strings.Join(r, "\n"))
	}
}

func TestDeleteNetworkOneMemberAsleepIsLeftToCleanItself(t *testing.T) {
	e := newDelEnv(t)
	e.span(t)
	e.down["hv2"] = true
	res, err := e.deleteOn("home1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Members, ",") != "hv1" || len(res.Left) != 1 || !strings.Contains(res.Left[0], "hv2") || !strings.Contains(res.Left[0], "wakes") {
		t.Errorf("result = %+v, want hv1 done and one line naming hv2 as left to clean itself up when it wakes", res)
	}
	if !e.hostHas("hv2") {
		t.Error("the sleeping host's network was touched")
	}
	if e.hostHas("hv1") || e.hostHas("home1") || e.registryHas(t) {
		t.Error("the awake hosts or the registry still have the network")
	}
	if rows := e.fs.requests(); indexOf(rows, "DELETE /v1/networks/net1/members/hv2") < 0 {
		t.Error("the sleeping member's row was not removed")
	}
}

func TestDeleteNetworkACardOnAMemberStopsAndSaysWhatWasDone(t *testing.T) {
	e := newDelEnv(t)
	e.span(t)
	e.hosts["hv2"].nets["net1"].VMs = []string{"web2"}
	_, err := e.deleteOn("home1")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "web2") {
		t.Fatalf("err = %v, want a conflict naming web2", err)
	}
	// hv1 came first and was done; the message says so.
	if !strings.Contains(err.Error(), "hv1") || !strings.Contains(err.Error(), "again") {
		t.Errorf("err = %v, want it to say hv1 was done and to run it again", err)
	}
	if e.hostHas("hv1") || !e.hostHas("hv2") || !e.hostHas("home1") || !e.registryHas(t) {
		t.Error("state after the stop: hv1 should be done, hv2 and home1 and the registry untouched")
	}
	if len(e.hosts["hv2"].deleted) != 0 || len(e.hosts["hv2"].unlinked) != 0 {
		t.Errorf("hv2 was changed: unlinked %v deleted %v", e.hosts["hv2"].unlinked, e.hosts["hv2"].deleted)
	}
	// Detach, run again: hv1's network and row are gone already, and that is no error.
	e.hosts["hv2"].nets["net1"].VMs = nil
	res, err := e.deleteOn("home1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Members, ",") != "hv2" {
		t.Errorf("members = %v, want hv2 only", res.Members)
	}
	if e.hostHas("hv2") || e.hostHas("home1") || e.registryHas(t) {
		t.Error("the rerun did not finish")
	}
}

func TestDeleteNetworkRefusedWhileAVMHasACard(t *testing.T) {
	// A card on the home: the home's own check lists it.
	e := newDelEnv(t)
	e.span(t)
	e.hosts["home1"].nets["net1"].VMs = []string{"db1"}
	_, err := e.deleteOn("home1")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "home1/db1") || !strings.Contains(err.Error(), "vmtool net detach home1/db1 net1") {
		t.Fatalf("home card: err = %v", err)
	}
	if !e.hostHas("hv1") || !e.hostHas("hv2") || !e.hostHas("home1") || !e.registryHas(t) {
		t.Error("a refused delete changed something")
	}

	// A card on a member that its host reported to the store.
	e = newDelEnv(t)
	e.span(t)
	nets := []storeapi.VMNetwork{{Network: "net1", Mac: "52:54:00:00:00:02", Ip: "10.77.0.51"}}
	_, err = e.host.PutHostInventory(context.Background(), "hv1", storeclient.HostReport{
		DaemonVersion: "test", GeneratedAt: time.Now(),
		Vms: []storeclient.ReportVM{{Name: "web1", Uuid: "11111111-1111-1111-1111-111111111111", State: "running", Firmware: "bios", Disks: []storeapi.Disk{}, Networks: &nets}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.deleteOn("home1")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "hv1/web1") || !strings.Contains(err.Error(), "vmtool net detach hv1/web1 net1") {
		t.Fatalf("inventory card: err = %v", err)
	}
	if !e.hostHas("hv1") || !e.hostHas("hv2") || !e.hostHas("home1") || !e.registryHas(t) {
		t.Error("a refused delete changed something")
	}
}

func TestDeleteNetworkRefusals(t *testing.T) {
	e := newDelEnv(t)
	e.span(t)
	// A member is sent to leave, or to the home.
	_, err := e.deleteOn("hv1")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "vmtool net leave hv1 net1") || !strings.Contains(err.Error(), "home1") {
		t.Fatalf("member: err = %v", err)
	}
	if !e.hostHas("hv1") || !e.registryHas(t) {
		t.Error("a refused delete changed something")
	}

	// A store that is configured but does not answer: refused, nothing removed.
	f := newRegStore(t)
	f.whoami = ""
	h := newNetHost("home1", "192.0.2.13")
	s := &Service{Store: f.session(nil)}
	_, err = s.DeleteNetworkOnHost(context.Background(), "home1", h, "net1")
	if err == nil || kindOfErr(t, err) != KindUnavailable || !strings.Contains(err.Error(), "store is needed") || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("store down: err = %v", err)
	}
	if len(h.deleted) != 0 {
		t.Errorf("the home's network was deleted with the store down: %v", h.deleted)
	}
}

func TestDeleteNetworkWithNoStoreIsV1(t *testing.T) {
	h := newNetHost("home1", "192.0.2.13")
	h.nets["net1"] = &vmtool.NetworkInfo{Name: "net1", Bridge: "vmt-000001", Vmtool: true, VNI: 1, VMs: []string{}}
	s := &Service{}
	res, err := s.DeleteNetworkOnHost(context.Background(), "home1", h, "net1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Registered || len(h.deleted) != 1 {
		t.Errorf("result = %+v, deleted %v", res, h.deleted)
	}
	// In store mode a network the registry does not have is V1's too.
	e := newDelEnv(t)
	e.hosts["home1"].nets["lone"] = &vmtool.NetworkInfo{Name: "lone", Bridge: "vmt-000002", Vmtool: true, VNI: 2, VMs: []string{}}
	res, err = e.s.DeleteNetworkOnHost(context.Background(), "home1", e.hosts["home1"], "lone")
	if err != nil {
		t.Fatal(err)
	}
	if res.Registered || len(e.hosts["home1"].deleted) != 1 {
		t.Errorf("unregistered: result = %+v", res)
	}
}

// A failure at each step leaves a state that a rerun finishes.
func TestDeleteNetworkRerunsAfterAFailureAtEachStep(t *testing.T) {
	for name, c := range map[string]struct {
		fault func(e *delEnv) // makes one step fail once
		heal  func(e *delEnv) // undoes the fault, for a fake host's
	}{
		"a member's row": {fault: func(e *delEnv) { e.fs.failOnce("DELETE /v1/networks/net1/members/hv1") }},
		"a member's network": {
			fault: func(e *delEnv) { e.hosts["hv1"].delErr = errors.New("libvirt said no") },
			heal:  func(e *delEnv) { e.hosts["hv1"].delErr = nil },
		},
		"the home's network": {
			fault: func(e *delEnv) { e.hosts["home1"].delErr = errors.New("libvirt said no") },
			heal:  func(e *delEnv) { e.hosts["home1"].delErr = nil },
		},
		"the reservation records": {fault: func(e *delEnv) { e.fs.failOnce("DELETE /v1/networks/net1/reservations/52:54:00:00:00:01") }},
		"the home's row":          {fault: func(e *delEnv) { e.fs.failOnce("DELETE /v1/networks/net1/members/home1") }},
		"the entry":               {fault: func(e *delEnv) { e.fs.failOnce("DELETE /v1/networks/net1") }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDelEnv(t)
			e.span(t)
			c.fault(e)
			if _, err := e.deleteOn("home1"); err == nil {
				t.Fatal("the delete succeeded through a failing step")
			} else if !strings.Contains(err.Error(), "again") {
				t.Errorf("err = %v, want it to say to run the command again", err)
			}
			if c.heal != nil {
				c.heal(e)
			}
			res, err := e.deleteOn("home1")
			if err != nil {
				t.Fatalf("the rerun: %v", err)
			}
			if !res.Registered {
				t.Errorf("result = %+v", res)
			}
			for _, h := range []string{"home1", "hv1", "hv2"} {
				if e.hostHas(h) {
					t.Errorf("%s still has net1 after the rerun", h)
				}
			}
			if e.registryHas(t) {
				t.Error("the registry still has net1 after the rerun")
			}
			// And a third run has nothing left to delete: not found, not a crash.
			if _, err := e.deleteOn("home1"); err == nil {
				t.Error("a delete of a network that is gone everywhere succeeded")
			}
		})
	}
}

// What the CLI prints is built from the result; keep the lines honest.
func TestDeleteNetworkResultNamesWhatWasDone(t *testing.T) {
	e := newDelEnv(t)
	e.span(t)
	e.down["hv1"] = true
	res, err := e.deleteOn("home1")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(res.Members) != "[hv2]" || len(res.Left) != 1 {
		t.Errorf("result = %+v", res)
	}
}
