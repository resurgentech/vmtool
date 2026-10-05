package vmtool

import (
	"errors"

	"libvirt.org/go/libvirt"
)

// ErrNotFound is returned when a domain, volume, or pool does not exist.
var ErrNotFound = errors.New("not found")

// IsNotFound reports whether err is a missing domain, volume, or pool.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	return errors.Is(err, libvirt.ERR_NO_DOMAIN) ||
		errors.Is(err, libvirt.ERR_NO_STORAGE_VOL) ||
		errors.Is(err, libvirt.ERR_NO_STORAGE_POOL)
}
