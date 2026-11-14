package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
)

// vmCommands are the VM commands and enough args after the VM to pass Args.
var vmCommands = map[string][]string{
	"create":       {"img"},
	"start":        nil,
	"stop":         nil,
	"resume":       nil,
	"poweroff":     nil,
	"delete":       nil,
	"info":         nil,
	"ssh":          nil,
	"cmd":          {"true"},
	"reboot":       nil,
	"autostart":    {"on"},
	"playbook":     {"x.yml"},
	"migrate-disk": {"pool"},
	"resize-disk":  {"10"},
	"add-disk":     {"10"},
}

// Every VM command takes host/vm. A bare name is refused with a suggestion,
// and a host this vmtool does not serve is "unknown host"; neither reaches a
// Manager (the real RunE runs against fake hosts, and both fail before a
// Manager is opened). Configured hosts: TestVMCommandsReachRemoteHost.
func TestVMCommandsTakeHostSlashVM(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	for name, rest := range vmCommands {
		f := installFakeHosts(t)
		keep := map[string]bool{"vmtool " + name: true}

		r := runGate(t, dir, keep, append([]string{name, "web"}, rest...)...)
		if r.err == nil || !strings.Contains(r.err.Error(), "did you mean jetfire/web?") {
			t.Errorf("%s web: %v", name, r.err)
		}

		r = runGate(t, dir, keep, append([]string{name, "nosuch/web"}, rest...)...)
		if !errors.Is(r.err, hosts.ErrUnknownHost) {
			t.Errorf("%s nosuch/web: %v, want unknown host", name, r.err)
		}
		if ops, tokens := f.seen(); len(ops) != 0 || len(tokens) != 0 {
			t.Errorf("%s: reached a Manager: %v %v", name, ops, tokens)
		}
	}
}

func TestVMCommandUsage(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	for name := range vmCommands {
		c, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(c.Use, "<host/") {
			t.Errorf("%s: Use %q does not name host/vm", name, c.Use)
		}
	}
	if c, _, err := root.Find([]string{"machines", "migrate"}); err != nil || c.Name() != "migrate" {
		t.Fatalf("machines migrate: %v", err)
	}
}
