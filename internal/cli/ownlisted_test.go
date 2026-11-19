package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ownResolver replaces the CLI's resolver for one test. The own name is
// whatever config.yml says; every host in config.yml hosts: (the own name
// too, when listed) is served by remote(host). The local connector counts its
// calls and, when strict, fails the test: a machine that lists its own name
// must never open libvirt.
type ownResolver struct {
	opened atomic.Int32
}

func installOwnResolver(t *testing.T, strict bool, localErr error, remote func(host string) app.Manager) *ownResolver {
	t.Helper()
	o := &ownResolver{}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		var configured []string
		for n := range e.cfg.Hosts {
			configured = append(configured, n)
		}
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, configured, func() (app.Manager, func(), error) {
			o.opened.Add(1)
			if strict {
				t.Error("local libvirt opened although the own name is listed")
			}
			if localErr != nil {
				return nil, nil, localErr
			}
			return nil, nil, errors.New("test: no local manager")
		}).WithRemote(func(host, token string) (app.Manager, func(), error) {
			return remote(host), func() {}, nil
		})
	}
	return o
}

func ownConfig(extraHosts string) string {
	return "name: jetfire\ntoken_sha256: " + config.Hash(gateToken) + "\nhosts:\n" +
		"  jetfire: { address: 127.0.0.1 }\n" + extraHosts
}

// withHost on the own name, listed, goes to the daemon: no libvirt.
func TestOwnListedHostCommandUsesDaemon(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	f := &fakeHosts{}
	o := installOwnResolver(t, true, nil, func(host string) app.Manager { return cliFake{host: host, f: f} })
	dir := writeWorkdir(t, ownConfig(""), "")
	r := runGate(t, dir, map[string]bool{"vmtool networks": true}, "networks")
	if r.err != nil {
		t.Fatal(r.err)
	}
	ops, _ := f.seen()
	if len(ops) != 1 || ops[0] != "jetfire ListNetworks" {
		t.Fatalf("ops %q, want the daemon's ListNetworks", ops)
	}
	if n := o.opened.Load(); n != 0 {
		t.Fatalf("local libvirt opened %d times", n)
	}
}

// The base-image lookup for create: with the own name listed there are no
// local pools (not found, so create uses the host's cached copy); with no
// daemon and no libvirt it is also not found, never a hard error.
func TestLocalImagesOwnListedOpensNothing(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	f := &fakeHosts{}
	o := installOwnResolver(t, true, nil, func(host string) app.Manager { return cliFake{host: host, f: f} })
	dir := writeWorkdir(t, ownConfig(""), "")
	r := runGate(t, dir, map[string]bool{"vmtool networks": true}, "networks")
	if r.err != nil {
		t.Fatal(r.err)
	}
	_, err := r.env.localImages("u.qcow2")
	if !vmtool.IsNotFound(err) {
		t.Fatalf("localImages = %v, want not found", err)
	}
	if n := o.opened.Load(); n != 0 {
		t.Fatalf("local libvirt opened %d times", n)
	}
}

func TestLocalImagesNoLibvirtIsNotFound(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	cfg := "name: laptop\ntoken_sha256: " + config.Hash(gateToken) + "\nhosts:\n  hv1: { address: 127.0.0.1 }\n"
	o := installOwnResolver(t, false, errors.New("libvirt: no socket"), nil)
	dir := writeWorkdir(t, cfg, "")
	r := runGate(t, dir, nil, "networks")
	if r.err != nil {
		t.Fatal(r.err)
	}
	_, err := r.env.localImages("u.qcow2")
	if !vmtool.IsNotFound(err) {
		t.Fatalf("localImages = %v, want not found", err)
	}
	if n := o.opened.Load(); n != 1 {
		t.Fatalf("connector called %d times, want the one lazy open", n)
	}
}

// metal add's libvirt name check is skipped silently when local libvirt is
// not used: listed (never opened) or unopenable.
func TestMetalNameCheckSkipped(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	f := &fakeHosts{}
	o := installOwnResolver(t, true, nil, func(host string) app.Manager { return cliFake{host: host, f: f} })
	dir := writeWorkdir(t, ownConfig(""), "")
	r := runGate(t, dir, nil, "networks")
	vms, release := r.env.libvirtNameCheck()
	release()
	if vms != nil {
		t.Fatalf("name check manager %v with the own name listed", vms)
	}
	if n := o.opened.Load(); n != 0 {
		t.Fatalf("local libvirt opened %d times", n)
	}

	cfg := "name: laptop\ntoken_sha256: " + config.Hash(gateToken) + "\n"
	o = installOwnResolver(t, false, errors.New("libvirt: no socket"), nil)
	dir = writeWorkdir(t, cfg, "")
	r = runGate(t, dir, nil, "networks")
	vms, release = r.env.libvirtNameCheck()
	release()
	if vms != nil {
		t.Fatalf("name check manager %v with no libvirt", vms)
	}
	if n := o.opened.Load(); n != 1 {
		t.Fatalf("connector called %d times, want 1", n)
	}
}

// With neither a daemon nor libvirt, list leaves the own name out, prints one
// stderr note and keeps the exit status it would have had.
func TestListWithNeitherLeavesOwnNameOut(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	cfg := "name: laptop\ntoken_sha256: " + config.Hash(gateToken) + "\nhosts:\n" +
		"  hv1: { address: 127.0.0.1 }\n  hv2: { address: 127.0.0.1 }\n"
	mgrs := map[string]listMgr{
		"hv1": {vms: []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning}}},
		"hv2": {err: errors.New("down")},
	}
	installOwnResolver(t, false, errors.New("libvirt: no socket"), func(host string) app.Manager { return mgrs[host] })
	dir := writeWorkdir(t, cfg, "")
	r := runGate(t, dir, map[string]bool{"vmtool list": true}, "list")
	if r.err != nil {
		t.Fatalf("exit status changed: %v", r.err)
	}
	if !strings.Contains(r.out, "hv1/web") || !strings.Contains(r.out, "hv2") {
		t.Errorf("other hosts missing:\n%s", r.out)
	}
	if strings.Contains(r.out, "laptop") {
		t.Errorf("own name still listed:\n%s", r.out)
	}
	if n := strings.Count(r.errOut, "\n"); n != 1 || !strings.Contains(r.errOut, "no daemon listed for laptop and no local libvirt") {
		t.Errorf("stderr %q, want one note", r.errOut)
	}

	// Naming it still fails with that sentence.
	r = runGate(t, dir, map[string]bool{"vmtool list": true}, "list", "laptop")
	if r.err == nil || !strings.Contains(r.err.Error(), "no daemon listed for laptop and no local libvirt") {
		t.Errorf("list laptop: %v", r.err)
	}
}

// Tunnel to the own name, listed, uses the daemon's tunnel route in both
// argument forms; nothing dials a local guest or opens libvirt.
func TestTunnelOwnListedUsesDaemon(t *testing.T) {
	for _, arg := range []string{"ws/web", "web.ws.vmtool"} {
		port, pool, paths := fakeDaemon(t, 101)
		f := fakeTunnel(t, pool)
		o := installOwnResolver(t, true, nil, nil)
		t.Setenv(config.TokenEnv, gateToken)
		cfg := "name: ws\ntoken_sha256: " + config.Hash(gateToken) + "\nhosts:\n  ws: { address: 127.0.0.1, port: " + strconv.Itoa(port) + " }\n"
		dir := writeWorkdir(t, cfg, "")
		stdinR, stdinW := io.Pipe()
		out := &signalWriter{want: "ssh bytes", got: make(chan struct{})}
		go func() {
			stdinW.Write([]byte("ssh bytes"))
			select {
			case <-out.got:
			case <-time.After(5 * time.Second):
			}
			stdinW.Close()
		}()
		if _, err := runTunnel(t, dir, stdinR, out, arg); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if p := paths(); len(p) != 1 || p[0] != "/v1/hosts/ws/vms/web/tunnel" {
			t.Fatalf("%s: daemon saw %v", arg, p)
		}
		if lookups, dials := f.calls(); len(lookups) != 0 || len(dials) != 0 {
			t.Fatalf("%s: local path used: lookups %v dials %v", arg, lookups, dials)
		}
		if n := o.opened.Load(); n != 0 {
			t.Fatalf("%s: libvirt opened %d times", arg, n)
		}
	}
}

// The service the CLI builds finds base images in image_dir (relative to the
// config directory) without opening libvirt when the own name is listed.
func TestServiceImagesFromImageDir(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	f := &fakeHosts{}
	o := installOwnResolver(t, true, nil, func(host string) app.Manager { return cliFake{host: host, f: f} })
	dir := writeWorkdir(t, ownConfig("")+"image_dir: imgs\n", "")
	if err := os.Mkdir(filepath.Join(dir, "imgs"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "imgs", "u.qcow2")
	if err := os.WriteFile(want, []byte("placeholder image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runGate(t, dir, map[string]bool{"vmtool networks": true}, "networks")
	if r.err != nil {
		t.Fatal(r.err)
	}
	svc := r.env.service("")
	if p, err := svc.Images.FindImage("u.qcow2"); err != nil || p != want {
		t.Fatalf("FindImage = %q, %v", p, err)
	}
	if _, err := svc.Images.FindImage("nope.qcow2"); !vmtool.IsNotFound(err) {
		t.Fatalf("miss = %v", err)
	}
	if n := o.opened.Load(); n != 0 {
		t.Fatalf("local libvirt opened %d times", n)
	}
}
