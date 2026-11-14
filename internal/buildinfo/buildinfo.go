// Package buildinfo reports which build of vmtool is running.
package buildinfo

import "runtime/debug"

// Version is the module version, else the VCS revision (with +dirty for a
// modified tree), else "unknown".
func Version() string {
	bi, ok := debug.ReadBuildInfo()
	return from(bi, ok)
}

func from(bi *debug.BuildInfo, ok bool) string {
	if !ok || bi == nil {
		return "unknown"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
