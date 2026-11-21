package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// vmtool info shows origin and comment, and "-" for a VM that has neither.
func TestWriteVMInfoOriginAndComment(t *testing.T) {
	sha := strings.Repeat("a", 64)
	var b bytes.Buffer
	writeVMInfo(&b, &vmtool.VMInfo{Name: "vm1", Comment: "placeholder comment", Origin: &vmtool.VMOrigin{
		Image: "img1", ImageSHA256: sha, Template: "img1", TemplateLV: "vmtpl-aaaaaaaaaaaa",
		CloneKind: vmtool.CloneKindThin, Pool: "nvme-thin", Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}})
	for _, want := range []string{"Origin:    img1", "thin", "nvme-thin", sha, "vmtpl-aaaaaaaaaaaa", "2026-11-21T04:05:06Z",
		"Comment:   placeholder comment\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	writeVMInfo(&b, &vmtool.VMInfo{Name: "vm1"})
	for _, want := range []string{"Origin:    -\n", "Comment:   -\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info lacks %q:\n%s", want, b.String())
		}
	}
}

// commentFake records the comment calls and the config create defined.
type commentFake struct {
	createFake
	rec *commentLog
}

type commentLog struct {
	mu      sync.Mutex
	set     []string
	defined []vmtool.VMConfig
}

func (c commentFake) SetComment(n, text string) error {
	c.rec.mu.Lock()
	c.rec.set = append(c.rec.set, fmt.Sprintf("%s %q", n, text))
	c.rec.mu.Unlock()
	return nil
}
func (c commentFake) Info(n string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: n, State: vmtool.StateShutoff}, nil
}
func (c commentFake) Define(cfg vmtool.VMConfig) error {
	c.rec.mu.Lock()
	c.rec.defined = append(c.rec.defined, cfg)
	c.rec.mu.Unlock()
	return c.createFake.Define(cfg)
}

func runComment(t *testing.T, args ...string) (*commentLog, string, error) {
	t.Helper()
	rec := &commentLog{}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith("jetfire", config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return commentFake{createFake: createFake{log: &createLog{}, ip: "10.0.0.5"}, rec: rec}, func() {}, nil
		})
	}
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	var out bytes.Buffer
	root, e := rootFor(dir, &out, io.Discard)
	e.tunnelExe = stubTunnelExe
	root.SetArgs(args)
	err := root.Execute()
	return rec, out.String(), err
}

func TestCommentCommand(t *testing.T) {
	rec, out, err := runComment(t, "comment", "jetfire/web", "placeholder comment")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.set) != 1 || rec.set[0] != `web "placeholder comment"` {
		t.Errorf("SetComment calls %q", rec.set)
	}
	if len(rec.defined) != 0 {
		t.Error("a comment change redefined the VM")
	}
	if !strings.Contains(out, "jetfire/web") {
		t.Errorf("output %q", out)
	}

	rec, _, err = runComment(t, "comment", "jetfire/web", "--clear")
	if err != nil || len(rec.set) != 1 || rec.set[0] != `web ""` {
		t.Errorf("--clear: %q %v", rec.set, err)
	}
}

func TestCommentCommandRefusals(t *testing.T) {
	for name, args := range map[string][]string{
		"no text":        {"comment", "jetfire/web"},
		"text and clear": {"comment", "jetfire/web", "x", "--clear"},
		"newline":        {"comment", "jetfire/web", "a\nb"},
		"too long":       {"comment", "jetfire/web", strings.Repeat("a", 513)},
	} {
		rec, _, err := runComment(t, args...)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(rec.set) != 0 {
			t.Errorf("%s: reached the host: %q", name, rec.set)
		}
		if name != "no text" && name != "text and clear" && !strings.Contains(fmt.Sprint(err), "comment") {
			t.Errorf("%s: error %v does not name comment", name, err)
		}
	}
}

func TestCreateCommentFlag(t *testing.T) {
	rec, _, err := runComment(t, "create", "jetfire/web", "u.qcow2", "--comment", "placeholder comment", "--no-wait", "--inventory", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.defined) != 1 || rec.defined[0].Comment != "placeholder comment" {
		t.Fatalf("defined %+v", rec.defined)
	}
	if o := rec.defined[0].Origin; o == nil || o.Image != "u.qcow2" || o.CloneKind != vmtool.CloneKindFile {
		t.Errorf("origin %+v", o)
	}
	rec, _, err = runComment(t, "create", "jetfire/web", "u.qcow2", "--comment", "a\nb", "--no-wait", "--inventory", "")
	if err == nil || !strings.Contains(err.Error(), "comment") || len(rec.defined) != 0 {
		t.Errorf("bad comment: err %v defined %d", err, len(rec.defined))
	}
}
