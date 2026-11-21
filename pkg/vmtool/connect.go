package vmtool

import (
	"errors"
	"fmt"
	"testing"

	"libvirt.org/go/libvirt"
)

// ErrLibvirtInTest is the refusal connect returns under go test.
var ErrLibvirtInTest = errors.New("vmtool: tests must not connect to libvirt")

// refuseLibvirtInTest returns ErrLibvirtInTest, naming uri, when this process
// is a test binary (testing.Testing(), the check the tool guard in exec.go
// uses). Nothing turns it off: no variable, environment value or option.
func refuseLibvirtInTest(uri string) error {
	if testing.Testing() {
		return fmt.Errorf("%w (%s); use a fake Manager or hosts.NewWith", ErrLibvirtInTest, uri)
	}
	return nil
}

// connect is the one caller in this module of the libvirt binding's connect
// functions (NewConnect, NewConnectReadOnly, NewConnectWithAuth,
// NewConnectWithAuthDefault); a shape test (connect_shape_test.go) holds every
// other file to that. Under go test it refuses before calling the binding.
func connect(uri string) (*libvirt.Connect, error) {
	if err := refuseLibvirtInTest(uri); err != nil {
		return nil, err
	}
	return libvirt.NewConnect(uri)
}
