package vmtool

import (
	"errors"
	"testing"

	"libvirt.org/go/libvirt"
)

func TestPoolUsage(t *testing.T) {
	info := &libvirt.StoragePoolInfo{Capacity: 100 << 30, Allocation: 25 << 30}
	size, used := poolUsage(true, info, nil)
	if size == nil || *size != 100<<30 || used == nil || *used != 25<<30 {
		t.Errorf("active pool: %v %v", size, used)
	}
	// An inactive pool's numbers are not trusted; a failed GetInfo is unknown.
	if s, u := poolUsage(false, info, nil); s != nil || u != nil {
		t.Errorf("inactive pool: %v %v", s, u)
	}
	if s, u := poolUsage(true, nil, errors.New("boom")); s != nil || u != nil {
		t.Errorf("failed GetInfo: %v %v", s, u)
	}
	// A known zero stays a zero.
	if s, u := poolUsage(true, &libvirt.StoragePoolInfo{Capacity: 10}, nil); s == nil || u == nil || *u != 0 {
		t.Errorf("zero used: %v %v", s, u)
	}
}
