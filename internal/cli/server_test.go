package cli

import (
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
)

func TestServerFrom(t *testing.T) {
	cfg := &config.Config{Listen: "0.0.0.0:9999", TokenSHA256: config.Hash("test-token")}
	s := serverFrom(cfg, "127.0.0.1:9473", false)
	if s.Config != cfg {
		t.Fatalf("config not passed through: %+v", s.Config)
	}
	if s.Listen != "" {
		t.Fatalf("unset flag must defer to config listen, got %q", s.Listen)
	}
	if s = serverFrom(cfg, "127.0.0.1:1", true); s.Listen != "127.0.0.1:1" {
		t.Fatalf("flag must win: %+v", s)
	}
}
