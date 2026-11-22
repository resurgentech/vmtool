// Package buildinfo reports which build of vmtool is running.
package buildinfo

import "runtime/debug"

// stamped is the revision a build was told about with
// -ldflags "-X github.com/resurgentech/vmtool/internal/buildinfo.stamped=<rev>".
// Nothing else sets it. It stands for a build that has no VCS information of
// its own, such as `just deploy`'s, which builds from a copy without .git.
var stamped string

// Version is the module version, else the VCS revision (with +dirty for a
// modified tree), else the stamped revision, else "unknown".
func Version() string {
	bi, ok := debug.ReadBuildInfo()
	return from(bi, ok, stamped)
}

func from(bi *debug.BuildInfo, ok bool, stamp string) string {
	unknown := "unknown"
	if stamp != "" {
		unknown = stamp
	}
	if !ok || bi == nil {
		return unknown
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
		return unknown
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
