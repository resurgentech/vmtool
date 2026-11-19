package app

// Optional Manager interfaces for hosts that keep LVM thin pools (a
// hypervisor daemon). A Manager that lacks them behaves exactly as before;
// every wrapper that can sit around such a Manager (internal/hosts) forwards
// them.

// TemplateCloner is implemented by a Manager whose clone can name the image
// it comes from: the remote client sends it as the clone request's `template`,
// and a hypervisor's thin-pool Manager uses it to find the image's template.
// It is CloneImage plus the image name; CloneImage stays the way to clone
// without one (and what the libvirt Manager implements).
type TemplateCloner interface {
	CloneImageFrom(image, basePath, newName, targetPool string, sizeBytes uint64) (string, error)
}

// ThinHost is implemented by a hypervisor daemon's Manager when it keeps thin
// pools. The daemon's path gate and getImagePath ask it.
type ThinHost interface {
	// ThinVolume reports whether path is exactly /dev/<vg>/<lv> of a thin LV
	// vmtool made, in a vmtool thin pool, and its kind (the vmtool.ThinKind*
	// constants). Any other path is not found, with no error.
	ThinVolume(path string) (kind string, found bool, err error)
	// SavedTemplatePath is the /dev/<vg>/<lv> path of a saved template of
	// that name, or an error matching vmtool.ErrNotFound.
	SavedTemplatePath(name string) (string, error)
}

// TemplateFinder is implemented by a Manager (the remote client) that can ask
// its host for the saved template of a thin pool called name: the host's
// getImagePath answers with the template's /dev/<vg>/<lv> when no pool has an
// image of that name. CreateVM asks it only when the host has no copy of the
// image and neither has this machine. It is not ImagePath itself: ImagePath
// is also how the image cache sees whether a name is taken.
type TemplateFinder interface {
	TemplatePath(name string) (string, error)
}
