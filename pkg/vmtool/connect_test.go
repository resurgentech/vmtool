package vmtool

import (
	"errors"
	"strings"
	"testing"
)

// G1: under go test, every libvirt connection is refused before the binding is
// called. These tests may only ever run with that guard in place: without it
// NewManager would open the machine's real libvirt.

func checkRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrLibvirtInTest) {
		t.Fatalf("%s under go test = %v, want ErrLibvirtInTest", what, err)
	}
	for _, want := range []string{"vmtool: tests must not connect to libvirt", "qemu:///system", "use a fake Manager or hosts.NewWith"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s error %q lacks %q", what, err, want)
		}
	}
}

func TestConnectRefusedUnderTest(t *testing.T) {
	c, err := connect("qemu:///system")
	if c != nil {
		t.Fatal("connect returned a connection under go test")
	}
	checkRefusal(t, "connect", err)
}

func TestNewManagerRefusedUnderTest(t *testing.T) {
	m, err := NewManager()
	if m != nil {
		t.Fatal("NewManager returned a Manager under go test")
	}
	checkRefusal(t, "NewManager", err)
}

func TestWatchNetworkStartedRefusedUnderTest(t *testing.T) {
	stop, err := WatchNetworkStarted(func(string) { t.Error("callback ran") })
	if stop != nil {
		stop()
		t.Fatal("WatchNetworkStarted returned a stop func under go test")
	}
	checkRefusal(t, "WatchNetworkStarted", err)
}

// No environment value turns the refusal off.
func TestConnectRefusalIgnoresEnvironment(t *testing.T) {
	for _, kv := range [][2]string{
		{"VMTOOL_TEST_STUBS", t.TempDir()},
		{"VMTOOL_ALLOW_LIBVIRT", "1"},
		{"LIBVIRT_DEFAULT_URI", "test:///default"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	_, err := connect("test:///default")
	if !errors.Is(err, ErrLibvirtInTest) {
		t.Fatalf("connect with environment set = %v, want ErrLibvirtInTest", err)
	}
}
