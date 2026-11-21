package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// V1: named networks on one host. The workflows check the name before the
// host is asked, and sort the host's refusals into the kinds REST maps to
// 400, 404 and 409.

type netMgr struct {
	Manager // nil: any other call is a bug
	calls   []string
	err     error
	info    *vmtool.NetworkInfo
}

func (f *netMgr) CreateNetwork(name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	f.calls = append(f.calls, fmt.Sprintf("create %s %s %s", name, subnet, domain))
	return f.info, f.err
}

func (f *netMgr) Network(name string) (*vmtool.NetworkInfo, error) {
	f.calls = append(f.calls, "show "+name)
	return f.info, f.err
}

func (f *netMgr) DeleteNetwork(name string) error {
	f.calls = append(f.calls, "delete "+name)
	return f.err
}

func kindOfErr(t *testing.T, err error) Kind {
	t.Helper()
	var ae *Error
	if !AsError(err, &ae) {
		t.Fatalf("err = %v, not an *app.Error", err)
	}
	return ae.Kind
}

func TestCreateNetworkChecksNameBeforeAskingTheHost(t *testing.T) {
	s := &Service{}
	for _, bad := range []string{"", "default", "a b", "a/b"} {
		f := &netMgr{}
		_, err := s.CreateNetwork(context.Background(), f, bad, "", "")
		if err == nil || kindOfErr(t, err) != KindInvalid {
			t.Errorf("name %q: err = %v, want KindInvalid", bad, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("name %q reached the host: %v", bad, f.calls)
		}
	}
}

func TestCreateNetworkPassesAndClassifies(t *testing.T) {
	s := &Service{}
	want := &vmtool.NetworkInfo{Name: "net1", Bridge: "vmt-000001", Vmtool: true}
	f := &netMgr{info: want}
	got, err := s.CreateNetwork(context.Background(), f, "net1", "10.77.1.0/24", "lab.example")
	if err != nil || got != want {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(f.calls) != 1 || f.calls[0] != "create net1 10.77.1.0/24 lab.example" {
		t.Errorf("calls = %v", f.calls)
	}
	for _, c := range []struct {
		err  error
		kind Kind
	}{
		{fmt.Errorf("x: %w", vmtool.ErrInvalidNetwork), KindInvalid},
		{fmt.Errorf("x: %w", vmtool.ErrSubnetOverlap), KindInvalid},
		{fmt.Errorf("x: %w", vmtool.ErrNetworkExists), KindConflict},
		{fmt.Errorf("x: %w", vmtool.ErrNoFreeSubnet), KindConflict},
		{fmt.Errorf("x: %w", vmtool.ErrNotFound), KindNotFound},
		{fmt.Errorf("libvirt exploded"), KindInternal},
	} {
		_, err := s.CreateNetwork(context.Background(), &netMgr{err: c.err}, "net1", "", "")
		if err == nil || kindOfErr(t, err) != c.kind {
			t.Errorf("%v: err = %v, want kind %d", c.err, err, c.kind)
		}
	}
}

func TestDeleteNetworkClassifies(t *testing.T) {
	s := &Service{}
	f := &netMgr{}
	if err := s.DeleteNetwork(context.Background(), f, "net1"); err != nil || len(f.calls) != 1 || f.calls[0] != "delete net1" {
		t.Fatalf("delete: %v %v", err, f.calls)
	}
	// "default" is asked of the host, which refuses it as not vmtool's (409):
	// the name rule is for names vmtool gives.
	f = &netMgr{err: fmt.Errorf("x: %w", vmtool.ErrNetworkNotVmtool)}
	if err := s.DeleteNetwork(context.Background(), f, "default"); err == nil || kindOfErr(t, err) != KindConflict || len(f.calls) != 1 {
		t.Errorf("not vmtool: %v %v", err, f.calls)
	}
	f = &netMgr{err: fmt.Errorf("x: %w", vmtool.ErrNetworkInUse)}
	if err := s.DeleteNetwork(context.Background(), f, "net1"); err == nil || kindOfErr(t, err) != KindConflict {
		t.Errorf("in use: %v", err)
	}
	f = &netMgr{err: fmt.Errorf("x: %w", vmtool.ErrNotFound)}
	if err := s.DeleteNetwork(context.Background(), f, "net9"); err == nil || kindOfErr(t, err) != KindNotFound {
		t.Errorf("missing: %v", err)
	}
	if err := s.DeleteNetwork(context.Background(), &netMgr{}, ""); err == nil || kindOfErr(t, err) != KindInvalid {
		t.Errorf("empty name: %v", err)
	}
}

// The networks listing marks the networks vmtool made: listNetworks is only
// names, so each is asked of the host (getNetwork). One the host cannot
// describe is listed unmarked rather than failing the listing.
type marksMgr struct {
	Manager
	names []string
}

func (f *marksMgr) ListNetworks() ([]string, error) { return f.names, nil }
func (f *marksMgr) Network(name string) (*vmtool.NetworkInfo, error) {
	switch name {
	case "net1":
		return &vmtool.NetworkInfo{Name: name, Bridge: "vmt-000001", Vmtool: true}, nil
	case "default":
		return &vmtool.NetworkInfo{Name: name, Bridge: "virbr0"}, nil
	}
	return nil, vmtool.ErrNotFound
}

func TestListNetworkMarks(t *testing.T) {
	s := &Service{}
	got, err := s.ListNetworkMarks(context.Background(), &marksMgr{names: []string{"default", "net1", "gone"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []NetworkMark{{Name: "default"}, {Name: "net1", Vmtool: true}, {Name: "gone"}}
	if len(got) != len(want) {
		t.Fatalf("marks = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mark %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNetworkShow(t *testing.T) {
	s := &Service{}
	want := &vmtool.NetworkInfo{Name: "default", Bridge: "virbr0"}
	got, err := s.Network(context.Background(), &netMgr{info: want}, "default")
	if err != nil || got != want {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := s.Network(context.Background(), &netMgr{err: vmtool.ErrNotFound}, "net9"); err == nil || kindOfErr(t, err) != KindNotFound {
		t.Errorf("missing: %v", err)
	}
}
