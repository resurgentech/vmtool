package app

import (
	"fmt"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// ImageSource finds this machine's copy of a base image for create to hash
// and upload to a host. It returns the path of a readable regular file, or an
// error wrapping vmtool.ErrNotFound when it has none (create then falls back
// to the host's cached copy). Any other error fails the create.
type ImageSource interface {
	FindImage(name string) (string, error)
}

// ImageSourceFunc adapts a function to ImageSource.
type ImageSourceFunc func(name string) (string, error)

func (f ImageSourceFunc) FindImage(name string) (string, error) { return f(name) }

// imageSearcher is optionally implemented by an ImageSource to say where it
// looks, for create's not-found message.
type imageSearcher interface{ Searched() string }

// LocalImages is the image source for this machine's own files: its libvirt
// pools first, then the image_dir folder.
type LocalImages struct {
	// Dir is the resolved image_dir; "" when unset.
	Dir string
	// Pools looks name up in the local libvirt pools. It must answer
	// not-found, without opening libvirt, when local libvirt is not in use
	// (the own name has a daemon) or cannot be opened. nil: no pools.
	Pools func(name string) (string, error)
}

func (l LocalImages) FindImage(name string) (string, error) {
	if l.Pools != nil {
		p, err := l.Pools(name)
		if err == nil {
			return p, nil
		}
		if !vmtool.IsNotFound(err) {
			return "", err
		}
	}
	return vmtool.FindImageInDir(l.Dir, name)
}

// Searched names the places FindImage looks.
func (l LocalImages) Searched() string {
	if l.Dir == "" {
		return "this machine's pools or image_dir (not set)"
	}
	return fmt.Sprintf("this machine's pools or image_dir (%s)", l.Dir)
}
