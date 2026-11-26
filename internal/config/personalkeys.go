package config

import (
	"github.com/resurgentech/vmtool/internal/keycfg"
)

// Defaults of the pulled key list's timings. The URL has no default.
const (
	DefaultKeyRefresh       = keycfg.DefaultRefresh
	DefaultKeyRefreshOnMiss = keycfg.DefaultRefreshOnMiss
)

// PersonalKeys is config.yml's personal_keys: block, the daemon's settings for
// per-person API keys (see keycfg.Settings, shared with the store). Every key
// is off unless set.
type PersonalKeys = keycfg.Settings
