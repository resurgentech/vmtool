package hosts

import (
	"errors"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
)

func failing(t *testing.T) Connector {
	return func() (app.Manager, func(), error) {
		t.Error("local connector called")
		return nil, nil, errors.New("must not connect")
	}
}

func TestResolveLocal(t *testing.T) {
	called, closed := 0, 0
	r := NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) {
		called++
		return nil, func() { closed++ }, nil
	})
	if r.Local() != "box" {
		t.Fatalf("Local() = %q", r.Local())
	}
	_, done, err := r.Resolve("box")
	if err != nil || called != 1 {
		t.Fatalf("Resolve local: err=%v called=%d", err, called)
	}
	done()
	if closed != 1 {
		t.Fatalf("close not forwarded")
	}
}

func TestResolveUnknown(t *testing.T) {
	r := NewWith("box", config.RoleWorkstation, nil, failing(t))
	_, _, err := r.Resolve("other")
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("err = %v, want ErrUnknownHost", err)
	}
	for _, want := range []string{`"other"`, "box"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestListAndNames(t *testing.T) {
	cfg := &config.Config{Name: "ws", Role: config.RoleWorkstation, Hosts: map[string]config.Host{"zeta": {}, "alpha": {}}}
	r := New(cfg)
	got := r.List()
	if len(got) != 3 || got[0] != (Host{"ws", config.RoleWorkstation, true}) ||
		got[1] != (Host{"alpha", config.RoleHypervisor, false}) || got[2].Name != "zeta" {
		t.Fatalf("List() = %+v", got)
	}
	if n := r.Names(); len(n) != 1 || n[0] != "ws" {
		t.Fatalf("Names() = %v", n)
	}
	// A configured remote host is known to List but not yet served.
	_, _, err := r.Resolve("alpha")
	if !errors.Is(err, ErrUnknownHost) || !strings.Contains(err.Error(), "not served") {
		t.Fatalf("Resolve(alpha) = %v", err)
	}
}

func TestAllNamesAndParseVM(t *testing.T) {
	r := New(&config.Config{Name: "ws", Hosts: map[string]config.Host{"zeta": {}, "alpha": {}}})
	if got := strings.Join(r.AllNames(), ","); got != "ws,alpha,zeta" {
		t.Fatalf("AllNames() = %s", got)
	}
	if ref, err := r.ParseVM("ws/web"); err != nil || ref != (app.VMRef{Host: "ws", Name: "web"}) {
		t.Fatalf("ParseVM = %+v, %v", ref, err)
	}
	if _, err := r.ParseVM("web"); err == nil || !strings.Contains(err.Error(), "did you mean ws/web?") {
		t.Fatalf("bare: %v", err)
	}
}

func TestNewNilConfig(t *testing.T) {
	short, err := config.ShortHostname()
	if err != nil {
		t.Skip(err)
	}
	r := New(nil)
	if r.Local() != short {
		t.Fatalf("Local() = %q, want %q", r.Local(), short)
	}
}
