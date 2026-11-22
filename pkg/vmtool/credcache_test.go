package vmtool

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/testexec"
)

const (
	cacheUUID = "00000000-0000-4000-8000-000000000001"
	cacheSrv  = "PLACEHOLDER-not-a-secret-1"
	cachePass = "PLACEHOLDER-not-a-secret-2"
)

func TestSourceMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadSourceMarker(dir); ok || err != nil {
		t.Fatalf("no marker: ok=%v err=%v", ok, err)
	}
	for _, m := range []SourceMarker{
		{State: SourceStore, UUID: cacheUUID},
		{State: SourcePending, UUID: cacheUUID},
		{State: SourceStore}, // machines have no UUID
	} {
		if err := WriteSourceMarker(dir, m); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadSourceMarker(dir)
		if err != nil || !ok || got != m {
			t.Fatalf("round trip %+v: got %+v ok=%v err=%v", m, got, ok, err)
		}
		st, _ := os.Stat(filepath.Join(dir, SourceMarkerFile))
		if st.Mode().Perm() != 0o600 {
			t.Errorf("marker mode %o", st.Mode().Perm())
		}
	}
	// A marker that says nothing we know counts as no marker (pending, UUID
	// unknown), never as an error that blocks the command.
	if err := os.WriteFile(filepath.Join(dir, SourceMarkerFile), []byte("garbage line here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadSourceMarker(dir); ok || err != nil {
		t.Fatalf("garbage marker: ok=%v err=%v", ok, err)
	}
}

func TestWriteFileAtomicLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := WriteFileAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "two" {
		t.Fatalf("content %q", b)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestLookupVMAuthTellsMissingFromPresent(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, ok := LookupVMAuth("hv1", "web"); ok {
		t.Fatal("missing folder reported present")
	}
	if HasVMCreds("hv1", "web") {
		t.Fatal("HasVMCreds on a missing folder")
	}
	dir := CredsDir("hv1", "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A folder with only the marker is not credentials.
	if err := WriteSourceMarker(dir, SourceMarker{State: SourcePending}); err != nil {
		t.Fatal(err)
	}
	if _, ok := LookupVMAuth("hv1", "web"); ok {
		t.Fatal("marker-only folder reported present")
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: bot\npassword: "+cachePass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, ok := LookupVMAuth("hv1", "web")
	if !ok || a.User != "bot" || a.Password != cachePass {
		t.Fatalf("present: %+v ok=%v", a, ok)
	}
	// ResolveVMAuth is unchanged: missing falls back to defaults.
	if got := ResolveVMAuth("hv1", "nosuch", Auth{}); got.User != "packer" {
		t.Fatalf("ResolveVMAuth fallback %+v", got)
	}
}

func TestMoveStaleKeepsTheFolder(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := CredsDir("hv1", "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	dest, err := MoveStale(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(".machines", ".stale", "web.hv1.vmtool-2026-11-18")
	if dest != want {
		t.Fatalf("dest %q, want %q", dest, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("source folder still there")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "password.yml")); err != nil || !strings.Contains(string(b), "bot") {
		t.Fatalf("moved content: %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(".machines", ".stale")); st.Mode().Perm() != 0o700 {
		t.Fatalf(".stale mode %o", st.Mode().Perm())
	}
	// A second one the same day does not overwrite the first.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dest2, err := MoveStale(dir, day)
	if err != nil || dest2 == dest {
		t.Fatalf("second move: %q %v", dest2, err)
	}
	// .stale is not a machine and not a VM folder.
	if IsMachine(".stale") || ValidMachineName(".stale") {
		t.Fatal(".stale looks like a machine name")
	}
}

// VMTOOL_SERVER_TOKEN never reaches a child vmtool builds for ssh, sshpass or
// ansible (S7 A7/A12).
func TestChildEnvironmentsDropTheServerToken(t *testing.T) {
	t.Setenv(ServerTokenEnv, cacheSrv)
	has := func(env []string) bool {
		return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, ServerTokenEnv+"=") })
	}
	for _, tok := range []string{"", "PLACEHOLDER-host-token"} {
		tn := Tunnel{Exe: "/opt/vmtool/vmtool", Token: tok}
		if has(tn.Environ(os.Environ())) {
			t.Errorf("Tunnel.Environ (token %q) kept %s", tok, ServerTokenEnv)
		}
		if has(ansibleEnv(tn)) {
			t.Errorf("ansibleEnv (token %q) kept %s", tok, ServerTokenEnv)
		}
	}
	// The host token's own handling is unchanged: it is set for the child.
	if env := (Tunnel{Exe: "/x", Token: "PLACEHOLDER-host-token"}).Environ(os.Environ()); !slices.Contains(env, TokenEnv+"=PLACEHOLDER-host-token") {
		t.Error("host token not passed to the tunnel child")
	}

	testexec.Stub(t, map[string]string{"ssh": "#!/bin/sh\n", "sshpass": "#!/bin/sh\n"})
	key := filepath.Join(t.TempDir(), "id")
	for name, auth := range map[string]Auth{
		"key":      {User: "bot", KeyPath: key},
		"password": {User: "bot", Password: cachePass},
	} {
		if c := sshCommandOpts("10.0.0.5", auth, nil, nil); has(c.Environ()) {
			t.Errorf("direct ssh (%s) kept %s", name, ServerTokenEnv)
		}
		if c, err := RemoteCmd(Tunnel{Exe: "/opt/vmtool/vmtool"}, "hv1", "web", []string{"true"}); err != nil || has(c.Environ()) {
			t.Errorf("tunnel ssh (%s) kept %s (err %v)", name, ServerTokenEnv, err)
		}
	}
}
