package vmtool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FindImageInDir looks name up as a regular file directly in dir: name
// itself, then name+".qcow2" when name has no .qcow2 suffix. A symlink to a
// regular file counts; a directory, a broken link or anything else does not.
// The answer for a miss, an unset dir (""), or a name ValidImageName refuses
// (so nothing can leave dir) is ErrNotFound.
func FindImageInDir(dir, name string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("image %q: image_dir is not set: %w", name, ErrNotFound)
	}
	if !ValidImageName(name) {
		return "", fmt.Errorf("image %q: not a valid image name: %w", name, ErrNotFound)
	}
	cands := []string{name}
	if !strings.HasSuffix(name, ".qcow2") {
		cands = append(cands, name+".qcow2")
	}
	for _, c := range cands {
		p := filepath.Join(dir, c)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("image %q not in %s: %w", name, dir, ErrNotFound)
}
