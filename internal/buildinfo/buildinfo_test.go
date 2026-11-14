package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFrom(t *testing.T) {
	set := func(k, v string) debug.BuildSetting { return debug.BuildSetting{Key: k, Value: v} }
	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no info", nil, false, "unknown"},
		{"tagged", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, true, "v1.2.3"},
		{"devel with revision", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{set("vcs.revision", "abc123")}}, true, "abc123"},
		{"devel dirty", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{set("vcs.revision", "abc123"), set("vcs.modified", "true")}}, true, "abc123+dirty"},
		{"devel no vcs", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "unknown"},
		{"empty version", &debug.BuildInfo{}, true, "unknown"},
	} {
		if got := from(tc.bi, tc.ok); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if Version() == "" {
		t.Error("Version() is empty")
	}
}
