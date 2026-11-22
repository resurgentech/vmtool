package hosts

import (
	"errors"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/remote"
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
	if len(got) != 3 || got[0] != (Host{Name: "ws", Role: config.RoleWorkstation, Local: true}) ||
		got[1] != (Host{Name: "alpha", Role: config.RoleHypervisor}) || got[2].Name != "zeta" {
		t.Fatalf("List() = %+v", got)
	}
	if n := r.Names(); len(n) != 3 || n[0] != "ws" {
		t.Fatalf("Names() = %v", n)
	}
	if !r.IsRemote("alpha") || r.IsRemote("ws") || r.IsRemote("nope") {
		t.Fatalf("IsRemote wrong")
	}
}

// A configured host resolves to a remote client carrying the caller's token;
// the token comes only from WithToken, and without one nothing is built.
func TestResolveRemote(t *testing.T) {
	type call struct{ host, token string }
	var calls []call
	released := 0
	r := NewWith("ws", config.RoleWorkstation, []string{"alpha"}, failing(t)).
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			calls = append(calls, call{host, token})
			return nil, func() { released++ }, nil
		})

	if _, _, err := r.Resolve("alpha"); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("Resolve without a token = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("remote built without a token: %v", calls)
	}

	tr := r.WithToken("tok-A")
	_, done, err := tr.Resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(calls) != 1 || calls[0] != (call{"alpha", "tok-A"}) || released != 1 {
		t.Fatalf("calls %v released %d", calls, released)
	}
	// WithToken copies: the original resolver is unchanged.
	if _, _, err := r.Resolve("alpha"); err == nil {
		t.Fatal("original resolver gained a token")
	}
	// Unknown hosts are still unknown, without building anything.
	if _, _, err := tr.Resolve("nope"); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("Resolve(nope) = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("remote built for an unknown host: %v", calls)
	}
	if got := strings.Join(tr.Names(), ","); got != "ws,alpha" {
		t.Fatalf("Names() = %s", got)
	}
}

// Without a remote connector (NewWith alone) a configured host is listed but
// not served.
func TestConfiguredWithoutConnector(t *testing.T) {
	r := NewWith("ws", config.RoleWorkstation, []string{"alpha"}, failing(t)).WithToken("tok")
	_, _, err := r.Resolve("alpha")
	if !errors.Is(err, ErrUnknownHost) || !strings.Contains(err.Error(), "not served") {
		t.Fatalf("Resolve(alpha) = %v", err)
	}
}

// New points each configured host's client at https://<address>:<port>,
// port defaulting to 9473, and makes no network call to do so.
func TestNewBuildsRemoteClients(t *testing.T) {
	cfg := &config.Config{Name: "ws", Role: config.RoleWorkstation, Hosts: map[string]config.Host{
		"alpha": {Address: "alpha.labtwo"},
		"beta":  {Address: "10.0.0.9", Port: 9999},
	}}
	r := New(cfg).WithToken("tok-B")
	for host, want := range map[string]string{"alpha": "https://alpha.labtwo:9473", "beta": "https://10.0.0.9:9999"} {
		m, done, err := r.Resolve(host)
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := m.(*remote.Manager)
		if !ok {
			t.Fatalf("%s: Manager is %T", host, m)
		}
		if rm.Name() != host || rm.BaseURL() != want {
			t.Errorf("%s: client for %s at %s, want %s", host, rm.Name(), rm.BaseURL(), want)
		}
		done()
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
