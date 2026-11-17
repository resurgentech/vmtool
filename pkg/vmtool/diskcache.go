package vmtool

// Disk cache modes (VMConfig.DiskCache, VMInfo.DiskCache). The empty string
// is no cache attribute: the hypervisor's default.
const (
	DiskCacheNone         = "none"
	DiskCacheWriteback    = "writeback"
	DiskCacheWritethrough = "writethrough"
	// DiskCacheUnsafe keeps guest writes that the guest believes are flushed
	// in host memory; a host crash or power loss can corrupt the disk. The
	// default for a clone vmtool creates.
	DiskCacheUnsafe = "unsafe"
)

// ValidDiskCache reports whether s is "" or one of the four modes.
func ValidDiskCache(s string) bool {
	switch s {
	case "", DiskCacheNone, DiskCacheWriteback, DiskCacheWritethrough, DiskCacheUnsafe:
		return true
	}
	return false
}
