package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// createVMs is a Manager that fakes the create and delete paths.
type createVMs struct {
	Manager
	ip      string
	deleted []string
}

func (f *createVMs) ImagePath(name string) (string, error) { return "/pool/" + name, nil }
func (f *createVMs) CloneImage(base, name, pool string) (string, error) {
	return "/pool/" + name + ".qcow2", nil
}
func (f *createVMs) Define(vmtool.VMConfig) error { return nil }
func (f *createVMs) Start(string) error           { return nil }
func (f *createVMs) WaitForIP(string, time.Duration) (string, error) {
	return f.ip, nil
}
func (f *createVMs) Info(name string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: name, IP: f.ip, State: vmtool.StateRunning}, nil
}
func (f *createVMs) Delete(name string, _ bool) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Create drops stale creds for (host, vm) only. An IP-keyed entry and the
// same VM name on another host are left alone (IPs repeat across hosts).
func TestCreateVMForgetsByHostAndName(t *testing.T) {
	t.Chdir(t.TempDir())
	mkCreds(t, vmtool.CredsDir("jetfire", "web"), "stale")
	mkCreds(t, vmtool.CredsDir("veritech01", "web"), "other")
	mkCreds(t, filepath.Join(".machines", "10.0.0.5"), "ip")

	m := &createVMs{ip: "10.0.0.5"}
	info, err := stubSvc(t, &Service{}).CreateVM(context.Background(), m, CreateVMInput{Host: "jetfire", Name: "web", Image: "u.qcow2"})
	if err != nil || info == nil || info.IP != "10.0.0.5" {
		t.Fatalf("create: %+v, %v", info, err)
	}
	if exists(vmtool.CredsDir("jetfire", "web")) {
		t.Fatal("stale creds for jetfire/web kept")
	}
	if !exists(vmtool.CredsDir("veritech01", "web")) || !exists(filepath.Join(".machines", "10.0.0.5")) {
		t.Fatal("create removed creds it does not own")
	}
}

func TestCreateVMNeedsHost(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := (&Service{}).CreateVM(context.Background(), &createVMs{}, CreateVMInput{Name: "web", Image: "u.qcow2"})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, err)
	}
	_, err = (&Service{}).CreateVM(context.Background(), &createVMs{}, CreateVMInput{Host: "../x", Name: "web", Image: "u.qcow2"})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("bad host kind %v: %v", ae.Kind, err)
	}
}

func TestDeleteVMForgetsByHostAndName(t *testing.T) {
	t.Chdir(t.TempDir())
	mkCreds(t, vmtool.CredsDir("jetfire", "web"), "bot")
	mkCreds(t, vmtool.CredsDir("veritech01", "web"), "other")
	mkCreds(t, filepath.Join(".machines", "10.0.0.5"), "ip")

	m := &createVMs{ip: "10.0.0.5"}
	ref := VMRef{Host: "jetfire", Name: "web"}
	// noclone keeps the disk, so its creds stay.
	if err := (&Service{}).DeleteVM(context.Background(), m, ref, true); err != nil {
		t.Fatal(err)
	}
	if !exists(vmtool.CredsDir("jetfire", "web")) {
		t.Fatal("noclone delete removed creds")
	}
	if err := (&Service{}).DeleteVM(context.Background(), m, ref, false); err != nil {
		t.Fatal(err)
	}
	if exists(vmtool.CredsDir("jetfire", "web")) {
		t.Fatal("delete kept creds")
	}
	if !exists(vmtool.CredsDir("veritech01", "web")) || !exists(filepath.Join(".machines", "10.0.0.5")) {
		t.Fatal("delete removed creds it does not own")
	}
	if len(m.deleted) != 2 || m.deleted[0] != "web" {
		t.Fatalf("Manager.Delete got %v, want the bare VM name", m.deleted)
	}
}

func mkCreds(t *testing.T, dir, user string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: "+user+"\npassword: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
