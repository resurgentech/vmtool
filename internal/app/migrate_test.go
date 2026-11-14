package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// listVMs is a Manager that only answers List.
type listVMs struct {
	Manager
	vms []vmtool.VMInfo
}

func (f listVMs) List() ([]vmtool.VMInfo, error) { return f.vms, nil }

func mkCreds(t *testing.T, dir, user string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: "+user+"\npassword: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readUser(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "password.yml"))
	if err != nil {
		return ""
	}
	return string(b)
}

func migrationFixture(t *testing.T) (*Service, Manager) {
	t.Helper()
	t.Chdir(t.TempDir())
	m := filepath.Join(".machines")
	mkCreds(t, filepath.Join(m, "web"), "web-by-name")       // local VM name
	mkCreds(t, filepath.Join(m, "10.0.0.7"), "db-by-ip")     // IP held by db now
	mkCreds(t, filepath.Join(m, "10.0.0.5"), "web-by-ip")    // IP of web: name wins, conflict
	mkCreds(t, filepath.Join(m, "10.0.0.99"), "gone")        // nobody holds it
	mkCreds(t, filepath.Join(m, "oldvm"), "gone")            // no such VM
	mkCreds(t, filepath.Join(m, "cache"), "cache-by-name")   // target already exists
	mkCreds(t, filepath.Join(m, "jetfire", "cache"), "keep") // existing target
	mkCreds(t, filepath.Join(m, "veritech01", "x"), "host")  // configured host dir
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	vms := listVMs{vms: []vmtool.VMInfo{
		{Name: "web", IP: "10.0.0.5"},
		{Name: "db", IP: "10.0.0.7"},
		{Name: "cache", IP: "10.0.0.8"},
		{Name: "off"},
	}}
	return &Service{HostNames: []string{"jetfire", "veritech01"}}, vms
}

func TestPlanMigration(t *testing.T) {
	s, vms := migrationFixture(t)
	steps, err := s.PlanMigration(context.Background(), vms, "jetfire")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by source. A name match claims its target before an IP match
	// (.machines/web beats .machines/10.0.0.5 for jetfire/web).
	want := []MigrationStep{
		{Source: ".machines/10.0.0.5", Target: ".machines/jetfire/web", Reason: ".machines/web moves to .machines/jetfire/web"},
		{Source: ".machines/10.0.0.7", Target: ".machines/jetfire/db", Move: true},
		{Source: ".machines/10.0.0.99", Reason: "not a local VM name or the IP of a local VM"},
		{Source: ".machines/cache", Target: ".machines/jetfire/cache", Reason: ".machines/jetfire/cache already exists"},
		{Source: ".machines/oldvm", Reason: "not a local VM name or the IP of a local VM"},
		{Source: ".machines/web", Target: ".machines/jetfire/web", Move: true},
	}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps:\n%+v", len(steps), steps)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Errorf("step %d:\n got %+v\nwant %+v", i, steps[i], want[i])
		}
	}
	// Dry run: nothing moved.
	if readUser(t, ".machines/web") == "" || readUser(t, ".machines/10.0.0.7") == "" {
		t.Fatal("PlanMigration moved something")
	}
	if _, err := os.Stat(".machines/jetfire/web"); err == nil {
		t.Fatal("PlanMigration created a target")
	}
}

func TestApplyMigration(t *testing.T) {
	s, vms := migrationFixture(t)
	steps, err := s.PlanMigration(context.Background(), vms, "jetfire")
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.ApplyMigration(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("moved %+v", done)
	}
	for dir, want := range map[string]string{
		".machines/jetfire/web":   "web-by-name",
		".machines/jetfire/db":    "db-by-ip",
		".machines/10.0.0.5":      "web-by-ip",
		".machines/10.0.0.99":     "gone",
		".machines/oldvm":         "gone",
		".machines/cache":         "cache-by-name",
		".machines/jetfire/cache": "keep",
		".machines/veritech01/x":  "host",
	} {
		if got := readUser(t, dir); got != "username: "+want+"\npassword: x\n" {
			t.Errorf("%s: %q, want %s", dir, got, want)
		}
	}
	for _, gone := range []string{".machines/web", ".machines/10.0.0.7"} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", gone, err)
		}
	}
	if !vmtool.IsMachine("nuc1") {
		t.Fatal("metal machine touched")
	}
	if st, err := os.Stat(".machines/jetfire"); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf(".machines/jetfire: %v %v", st, err)
	}
	// Re-running finds nothing to move and never overwrites.
	steps, err = s.PlanMigration(context.Background(), vms, "jetfire")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range steps {
		if st.Move {
			t.Errorf("second plan moves %+v", st)
		}
	}
}

// A target that appears between plan and apply is not overwritten.
func TestApplyMigrationNeverOverwrites(t *testing.T) {
	s, vms := migrationFixture(t)
	steps, err := s.PlanMigration(context.Background(), vms, "jetfire")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".machines/jetfire/web", 0o700); err != nil {
		t.Fatal(err)
	}
	done, err := s.ApplyMigration(context.Background(), steps)
	if err == nil {
		t.Fatal("apply over an existing target succeeded")
	}
	if readUser(t, ".machines/web") == "" {
		t.Fatal("source moved onto an existing target")
	}
	for _, d := range done {
		if d.Target == ".machines/jetfire/web" {
			t.Fatalf("reported moved: %+v", d)
		}
	}
}

func TestPlanMigrationNoMachinesDir(t *testing.T) {
	t.Chdir(t.TempDir())
	steps, err := (&Service{}).PlanMigration(context.Background(), listVMs{}, "jetfire")
	if err != nil || len(steps) != 0 {
		t.Fatalf("steps %+v, %v", steps, err)
	}
	if _, err := (&Service{}).PlanMigration(context.Background(), listVMs{}, "../x"); err == nil {
		t.Fatal("invalid local name accepted")
	}
}
