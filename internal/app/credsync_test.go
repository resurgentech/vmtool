package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// S8: the client's side of the credentials in the store. Every test runs in a
// temp working directory, against an in-process store and stub ssh/ansible
// (testexec); nothing real is reached.

const (
	csKey    = "PLACEHOLDER-KEY-not-a-key"
	csPub    = "PLACEHOLDER-pub-not-a-key"
	csOld    = "PLACEHOLDER-OLD-not-a-secret"
	csOtherU = "00000000-0000-4000-8000-000000000002"
)

// credHarness is a store-mode Service for VM hv1/web whose ssh and
// ansible-playbook are stubs. ssh appends its argv to argv and counts calls in
// count; STUB_SSH is "first255" or "always255". ansible-playbook writes a
// credentials folder into $STUB_PB_DIR when STUB_PB_WRITE is set and exits with
// $STUB_PB_EXIT.
type credHarness struct {
	t     *testing.T
	f     *fakeStoreServer
	sess  *StoreSession
	svc   *Service
	vms   *sshVMs
	ref   VMRef
	notes *notices
	argv  string // file: every ssh/ansible-playbook argv, one per line
	count string // file: ssh call count
	env   string // file: the last ssh child's environment
}

func newCredHarness(t *testing.T, uuid string) *credHarness {
	t.Helper()
	t.Chdir(t.TempDir())
	h := &credHarness{t: t, f: newFakeStoreServer(t), notes: &notices{}, ref: VMRef{Host: "hv1", Name: "web"}}
	h.sess = h.f.session(t, stTok, h.notes.add)
	d := t.TempDir()
	h.argv, h.count, h.env = filepath.Join(d, "argv"), filepath.Join(d, "count"), filepath.Join(d, "env")
	testexec.Stub(t, map[string]string{
		"ssh": "#!/bin/sh\nn=$(cat '" + h.count + "' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + h.count + "'\n" +
			"printf '%s\\n' \"$@\" >> '" + h.argv + "'; echo ---- >> '" + h.argv + "'\nenv > '" + h.env + "'\n" +
			"[ \"$STUB_SSH\" = always255 ] && exit 255\n[ \"$STUB_SSH\" = first255 ] && [ \"$n\" = 1 ] && exit 255\nexit 0\n",
		"sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
		"ansible": "#!/bin/sh\nexit 0\n",
		"ansible-playbook": "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + h.argv + "'; echo ---- >> '" + h.argv + "'\n" +
			"if [ -n \"$STUB_PB_WRITE\" ]; then mkdir -p \"$STUB_PB_DIR\"; printf 'username: bot\\npassword: " + stPass + "\\n' > \"$STUB_PB_DIR/password.yml\"; " +
			"printf '" + csKey + "' > \"$STUB_PB_DIR/id_ed25519\"; printf '" + csPub + "' > \"$STUB_PB_DIR/id_ed25519.pub\"; fi\nexit ${STUB_PB_EXIT:-0}\n",
	})
	abs, err := vmtool.AbsCredsDir("hv1", "web")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_PB_DIR", abs)
	h.vms = &sshVMs{createVMs{ip: "10.0.0.5", uuid: uuid}}
	h.svc = (&Service{}).WithTunnel(appTunnel()).WithStore(h.sess)
	return h
}

func (h *credHarness) dir() string { return vmtool.CredsDir("hv1", "web") }

// folder writes a credentials folder (the cache) with the given marker; an
// empty state writes no marker.
func (h *credHarness) folder(pass, key, state, uuid string) {
	h.t.Helper()
	if err := writeCredDir(h.dir(), VMCredential{Username: "bot", Password: pass, PrivateKey: key, PublicKey: csPub}); err != nil {
		h.t.Fatal(err)
	}
	if state != "" {
		if err := vmtool.WriteSourceMarker(h.dir(), vmtool.SourceMarker{State: state, UUID: uuid}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// record puts a VM record straight into the store double.
func (h *credHarness) record(uuid, pass, key string) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.creds["hv1/web"] = map[string]any{"uuid": uuid, "username": "bot", "password": pass, "private_key": key, "public_key": csPub}
}

func (h *credHarness) stored() map[string]any {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return h.f.creds["hv1/web"]
}

func (h *credHarness) marker() (vmtool.SourceMarker, bool) {
	h.t.Helper()
	m, ok, err := vmtool.ReadSourceMarker(h.dir())
	if err != nil {
		h.t.Fatal(err)
	}
	return m, ok
}

func (h *credHarness) exec() *ExecResult {
	h.t.Helper()
	res, err := h.svc.ExecVM(context.Background(), h.vms, h.ref, []string{"true"}, "")
	if err != nil {
		h.t.Fatalf("exec: %v", err)
	}
	return res
}

func (h *credHarness) playbook() (*PlaybookResult, error) {
	h.t.Helper()
	return h.svc.RunPlaybook(context.Background(), h.vms, h.ref, "setup_bot.yml", vmtool.DefaultAuth(), nil)
}

func (h *credHarness) lines() []string {
	h.notes.mu.Lock()
	defer h.notes.mu.Unlock()
	return append([]string(nil), h.notes.list...)
}

func (h *credHarness) oneNote(want string) {
	h.t.Helper()
	l := h.lines()
	if len(l) != 1 || !strings.Contains(l[0], want) {
		h.t.Fatalf("notices %q, want exactly one containing %q", l, want)
	}
	if strings.Contains(l[0], "\n") || strings.Contains(l[0], stPass) || strings.Contains(l[0], csKey) || strings.Contains(l[0], stTok) {
		h.t.Fatalf("notice is not one clean line: %q", l[0])
	}
}

func (h *credHarness) noNotes() {
	h.t.Helper()
	if l := h.lines(); len(l) != 0 {
		h.t.Fatalf("notices %q, want none", l)
	}
}

func (h *credHarness) sshCalls() string {
	b, _ := os.ReadFile(h.count)
	return strings.TrimSpace(string(b))
}

func (h *credHarness) argvText() string {
	b, _ := os.ReadFile(h.argv)
	return string(b)
}

func readFolderFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- Reconciliation: one test per case of the design. ----

func TestReconcilePendingCurrentUUIDPushes(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourcePending, stUUID)
	h.exec()
	rec := h.stored()
	if h.f.puts.Load() != 1 || rec["uuid"] != stUUID || rec["password"] != stPass || rec["private_key"] != csKey {
		t.Fatalf("puts %d record %v", h.f.puts.Load(), rec)
	}
	if m, ok := h.marker(); !ok || m != (vmtool.SourceMarker{State: vmtool.SourceStore, UUID: stUUID}) {
		t.Fatalf("marker %+v", m)
	}
	h.oneNote("pushed")
}

func TestReconcilePendingOtherUUIDIsMovedAsideNotPushed(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourcePending, csOtherU)
	h.exec()
	if h.f.puts.Load() != 0 || h.stored() != nil {
		t.Fatalf("a stale folder was pushed: puts %d record %v", h.f.puts.Load(), h.stored())
	}
	if _, err := os.Stat(h.dir()); !os.IsNotExist(err) {
		t.Fatal("stale folder left in place")
	}
	ents, err := os.ReadDir(filepath.Join(".machines", ".stale"))
	if err != nil || len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "web.hv1.vmtool-") {
		t.Fatalf(".stale: %v %v", ents, err)
	}
	if got := readFolderFile(t, filepath.Join(".machines", ".stale", ents[0].Name()), "password.yml"); !strings.Contains(got, stPass) {
		t.Fatal("the stale folder lost its content: it is moved, never deleted")
	}
	h.oneNote("stale")
}

func TestReconcileNoMarkerStoreEmptyPushesWithCurrentUUID(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, "", "")
	h.exec()
	if h.f.puts.Load() != 1 || h.stored()["uuid"] != stUUID {
		t.Fatalf("puts %d record %v", h.f.puts.Load(), h.stored())
	}
	if m, ok := h.marker(); !ok || m != (vmtool.SourceMarker{State: vmtool.SourceStore, UUID: stUUID}) {
		t.Fatalf("marker %+v", m)
	}
	h.oneNote("pushed")
}

func TestReconcileNoMarkerStoreHasCurrentRecordStoreWins(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(csOld, "PLACEHOLDER-OLD-KEY", "", "")
	h.record(stUUID, stPass, csKey)
	h.exec()
	if h.f.puts.Load() != 0 {
		t.Fatal("the folder was pushed over the store's record")
	}
	if got := readFolderFile(t, h.dir(), "password.yml"); !strings.Contains(got, stPass) || strings.Contains(got, csOld) {
		t.Fatalf("folder not refreshed from the store: %q", got)
	}
	if got := readFolderFile(t, h.dir(), "id_ed25519"); got != csKey {
		t.Fatalf("key not refreshed: %q", got)
	}
	if m, ok := h.marker(); !ok || m != (vmtool.SourceMarker{State: vmtool.SourceStore, UUID: stUUID}) {
		t.Fatalf("marker %+v", m)
	}
	h.noNotes()
}

func TestReconcileStoreMarkerRecordLostPutsBack(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID) // the store has no record
	h.exec()
	if h.f.puts.Load() != 1 || h.stored()["password"] != stPass {
		t.Fatalf("puts %d record %v", h.f.puts.Load(), h.stored())
	}
	h.oneNote("lost")
}

func TestReconcileDaemonWithoutUUIDNoPush(t *testing.T) {
	h := newCredHarness(t, "") // a pre-2.3.0 daemon reports no uuid
	h.folder(stPass, csKey, "", "")
	h.exec()
	if h.f.puts.Load() != 0 || h.f.gets.Load() != 0 {
		t.Fatalf("talked to the store without a UUID: puts %d gets %d", h.f.puts.Load(), h.f.gets.Load())
	}
	h.oneNote("redeploy hv1")
	if got := readFolderFile(t, h.dir(), "password.yml"); !strings.Contains(got, stPass) {
		t.Fatal("the cache was touched")
	}
}

// Beyond the six cases: a store-marked cache for another UUID, and a store
// record for another UUID next to an unmarked folder, are stale too.
func TestReconcileOtherStaleShapes(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, csOtherU)
	h.exec()
	if h.f.puts.Load() != 0 {
		t.Fatal("pushed a cache for another VM")
	}
	if _, err := os.Stat(h.dir()); !os.IsNotExist(err) {
		t.Fatal("store-marked cache of another VM kept")
	}
	h.oneNote("stale")

	h = newCredHarness(t, stUUID)
	h.folder(stPass, csKey, "", "")
	h.record(csOtherU, csOld, csKey)
	h.exec()
	if h.f.puts.Load() != 0 {
		t.Fatal("overwrote a store record for another VM with an unmarked folder")
	}
	if _, err := os.Stat(h.dir()); !os.IsNotExist(err) {
		t.Fatal("unmarked folder next to a stale record kept")
	}
	h.oneNote("stale")
}

// A cache that is current and whose record is there costs one GET and says
// nothing.
func TestReconcileCurrentStoreCacheIsQuiet(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID)
	h.record(stUUID, stPass, csKey)
	h.exec()
	if h.f.puts.Load() != 0 {
		t.Fatal("pushed a current cache")
	}
	h.noNotes()
}

// ---- Fetch, refetch, hand-off, delete ----

func TestFetchWhenFolderMissing(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.record(stUUID, stPass, csKey)
	h.exec()
	if got := readFolderFile(t, h.dir(), "id_ed25519"); got != csKey {
		t.Fatalf("key %q", got)
	}
	assertMode(t, h.dir(), 0o700)
	for _, n := range []string{"password.yml", "id_ed25519", "id_ed25519.pub", vmtool.SourceMarkerFile} {
		assertMode(t, filepath.Join(h.dir(), n), 0o600)
	}
	if m, _ := h.marker(); m != (vmtool.SourceMarker{State: vmtool.SourceStore, UUID: stUUID}) {
		t.Fatalf("marker %+v", m)
	}
	// ssh reads the key from the cache: -i names the folder, no secret in argv.
	a := h.argvText()
	if !strings.Contains(a, filepath.Join(".machines", "web.hv1.vmtool", "id_ed25519")) {
		t.Fatalf("ssh did not use the cache key:\n%s", a)
	}
	for _, secret := range []string{stPass, csKey, stTok} {
		if strings.Contains(a, secret) {
			t.Fatalf("secret %q in ssh argv:\n%s", secret, a)
		}
	}
	h.noNotes()
}

func TestFetchNeverUsesAStaleRecord(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.record(csOtherU, stPass, csKey)
	h.exec()
	if _, err := os.Stat(h.dir()); !os.IsNotExist(err) {
		t.Fatal("a record for another UUID was written to the cache")
	}
	if strings.Contains(h.argvText(), "id_ed25519") {
		t.Fatal("ssh used a key from a stale record")
	}
}

func TestRefetchOnExit255RetriesOnceWhenChanged(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(csOld, "PLACEHOLDER-OLD-KEY", vmtool.SourceStore, stUUID)
	h.record(stUUID, csOld, "PLACEHOLDER-OLD-KEY")
	// The record changes after the first GET: the first ssh runs with the
	// cache as it was and fails with 255.
	h.f.onGet = func(n int32) {
		if n == 1 {
			h.record(stUUID, stPass, csKey)
		}
	}
	t.Setenv("STUB_SSH", "first255")
	res := h.exec()
	if res.ExitCode != 0 || h.sshCalls() != "2" {
		t.Fatalf("exit %d after %s ssh calls, want 0 after 2", res.ExitCode, h.sshCalls())
	}
	if got := readFolderFile(t, h.dir(), "id_ed25519"); got != csKey {
		t.Fatalf("cache not refetched: %q", got)
	}
	if h.f.gets.Load() != 2 {
		t.Fatalf("%d GETs, want the reconcile and one refetch", h.f.gets.Load())
	}
}

func TestRefetchOnExit255NoRetryWhenUnchanged(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID)
	h.record(stUUID, stPass, csKey)
	t.Setenv("STUB_SSH", "always255")
	res := h.exec()
	if res.ExitCode != 255 || h.sshCalls() != "1" {
		t.Fatalf("exit %d after %s ssh calls, want 255 after exactly 1", res.ExitCode, h.sshCalls())
	}
	if h.f.gets.Load() != 2 {
		t.Fatalf("%d GETs: one refetch expected after the 255", h.f.gets.Load())
	}
}

func TestRefetchOnlyOnce(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(csOld, "PLACEHOLDER-OLD-KEY", vmtool.SourceStore, stUUID)
	h.record(stUUID, csOld, "PLACEHOLDER-OLD-KEY")
	h.f.onGet = func(n int32) { h.record(stUUID, stPass+string(rune('a'+n)), csKey) } // changes on every GET
	t.Setenv("STUB_SSH", "always255")
	res := h.exec()
	if res.ExitCode != 255 || h.sshCalls() != "2" {
		t.Fatalf("exit %d after %s ssh calls, want 255 after 2 (one retry)", res.ExitCode, h.sshCalls())
	}
}

func TestHandOffAfterPlaybook(t *testing.T) {
	for name, tc := range map[string]struct {
		exit    string
		wantErr bool
	}{"success": {"0", false}, "failure": {"2", true}} {
		t.Run(name, func(t *testing.T) {
			h := newCredHarness(t, stUUID)
			t.Setenv("STUB_PB_WRITE", "1")
			t.Setenv("STUB_PB_EXIT", tc.exit)
			res, err := h.playbook()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v", err)
			}
			rec := h.stored()
			if h.f.puts.Load() != 1 || rec["uuid"] != stUUID || rec["password"] != stPass || rec["private_key"] != csKey {
				t.Fatalf("hand-off after a %s run: puts %d record %v", name, h.f.puts.Load(), rec)
			}
			if m, _ := h.marker(); m != (vmtool.SourceMarker{State: vmtool.SourceStore, UUID: stUUID}) {
				t.Fatalf("marker %+v", m)
			}
			h.noNotes()
			if strings.Contains(res.Output, stPass) {
				t.Fatal("playbook output carries the password")
			}
			if strings.Contains(h.argvText(), stPass) || strings.Contains(h.argvText(), csKey) {
				t.Fatalf("credential in ansible argv:\n%s", h.argvText())
			}
		})
	}
}

func TestNoHandOffWhenTheFolderDidNotChange(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID)
	h.record(stUUID, stPass, csKey)
	// A rerun of setup_bot.yml keeps the key: it writes the same files.
	t.Setenv("STUB_PB_WRITE", "1")
	if _, err := h.playbook(); err != nil {
		t.Fatal(err)
	}
	if h.f.puts.Load() != 0 {
		t.Fatalf("pushed an unchanged folder (%d PUTs)", h.f.puts.Load())
	}
}

// The store answers whoami and then fails: the hand-off cannot land. The
// command succeeds, the folder is written pending, and exactly one line names
// the store and says the credentials are only here.
func TestStoreFailingMidCommandWritesPending(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.sess.Mode(context.Background()) // the command's start: store mode
	h.f.failCreds.Store(true)
	t.Setenv("STUB_PB_WRITE", "1")
	res, err := h.playbook()
	if err != nil {
		t.Fatalf("a failed hand-off failed the command: %v", err)
	}
	h.oneNote("127.0.0.1")
	if l := h.lines()[0]; !strings.Contains(l, "only on this machine") {
		t.Fatalf("notice %q does not say the credentials are only here", l)
	}
	if m, ok := h.marker(); !ok || m != (vmtool.SourceMarker{State: vmtool.SourcePending, UUID: stUUID}) {
		t.Fatalf("marker %+v", m)
	}
	if got := readFolderFile(t, h.dir(), "id_ed25519"); got != csKey {
		t.Fatal("the folder was not written")
	}
	if strings.Contains(res.Output, stPass) {
		t.Fatal("output carries the password")
	}

	// The store comes back: the next command that uses the VM completes the
	// hand-off (the design's rerun of setup_bot.yml / any command).
	h.f.failCreds.Store(false)
	h.notes.list = nil
	h.exec()
	if h.f.puts.Load() != 1 || h.stored()["private_key"] != csKey {
		t.Fatalf("pending hand-off not completed: puts %d", h.f.puts.Load())
	}
	if m, _ := h.marker(); m.State != vmtool.SourceStore {
		t.Fatalf("marker %+v", m)
	}
}

// A dead store makes several reads and writes fail in one command; the user
// still gets one line.
func TestStoreFailureIsOneLinePerCommand(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, "PLACEHOLDER-OLD-KEY", vmtool.SourcePending, stUUID) // the playbook changes the key
	h.sess.Mode(context.Background())
	h.f.failCreds.Store(true)
	t.Setenv("STUB_PB_WRITE", "1")
	t.Setenv("STUB_PB_EXIT", "0")
	if _, err := h.playbook(); err != nil { // the prepare PUT fails, then the hand-off PUT would
		t.Fatal(err)
	}
	h.oneNote("only on this machine")
}

func TestDeleteRemovesStoreRecordAndCache(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID)
	h.record(stUUID, stPass, csKey)
	// noclone keeps the disk, so the record and the cache stay.
	if err := h.svc.DeleteVM(context.Background(), h.vms, h.ref, true); err != nil {
		t.Fatal(err)
	}
	if h.stored() == nil || !exists(h.dir()) || h.f.dels.Load() != 0 {
		t.Fatal("--noclone delete removed credentials")
	}
	if err := h.svc.DeleteVM(context.Background(), h.vms, h.ref, false); err != nil {
		t.Fatal(err)
	}
	if h.stored() != nil || exists(h.dir()) {
		t.Fatalf("delete kept the record (%v) or the cache (%v)", h.stored() != nil, exists(h.dir()))
	}
	h.noNotes()
}

func TestDeleteWithStoreFailingStillSucceedsOneLine(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID)
	h.sess.Mode(context.Background())
	h.f.failCreds.Store(true)
	if err := h.svc.DeleteVM(context.Background(), h.vms, h.ref, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if exists(h.dir()) {
		t.Fatal("cache kept")
	}
	h.oneNote("127.0.0.1")
}

func TestCreateReplaceRemovesStoreRecordAndCache(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(csOld, "PLACEHOLDER-OLD-KEY", vmtool.SourceStore, csOtherU)
	h.record(csOtherU, csOld, "PLACEHOLDER-OLD-KEY")
	if _, err := h.svc.CreateVM(context.Background(), h.vms, CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2", NoWait: true}); err != nil {
		t.Fatal(err)
	}
	if h.stored() != nil || exists(h.dir()) {
		t.Fatal("create's replace kept the old record or cache")
	}
}

// create's playbook (setup_bot.yml) writes the folder; the hand-off follows a
// run that failed as well as one that did not. No credential is in a progress
// event, an error or an argv.
func TestCreateHandsOffAfterPlaybook(t *testing.T) {
	for name, exit := range map[string]string{"success": "0", "failure": "2"} {
		t.Run(name, func(t *testing.T) {
			h := newCredHarness(t, stUUID)
			t.Setenv("STUB_PB_WRITE", "1")
			t.Setenv("STUB_PB_EXIT", exit)
			var evs []ProgressEvent
			_, err := h.svc.CreateVM(context.Background(), h.vms, CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2",
				Playbook: "setup_bot.yml", OnProgress: func(ev ProgressEvent) { evs = append(evs, ev) }})
			if (err != nil) != (exit != "0") {
				t.Fatalf("err %v", err)
			}
			if rec := h.stored(); h.f.puts.Load() != 1 || rec["uuid"] != stUUID || rec["password"] != stPass {
				t.Fatalf("puts %d record %v", h.f.puts.Load(), rec)
			}
			var all strings.Builder
			for _, ev := range evs {
				all.WriteString(ev.Stage + " " + ev.Status + " " + ev.Detail + " " + ev.Output + "\n")
			}
			if err != nil {
				all.WriteString(err.Error())
			}
			all.WriteString(h.argvText())
			for _, secret := range []string{stPass, csKey, stTok} {
				if strings.Contains(all.String(), secret) {
					t.Fatalf("secret %q in events, error or argv:\n%s", secret, all.String())
				}
			}
		})
	}
}

// With the store failing in create, the line also goes to the progress
// stream (the loopback service's header may already be written), still once.
func TestCreateStoreFailureNoticeInProgress(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.sess.Mode(context.Background())
	h.f.failCreds.Store(true)
	t.Setenv("STUB_PB_WRITE", "1")
	var evs []ProgressEvent
	if _, err := h.svc.CreateVM(context.Background(), h.vms, CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2",
		Playbook: "setup_bot.yml", OnProgress: func(ev ProgressEvent) { evs = append(evs, ev) }}); err != nil {
		t.Fatal(err)
	}
	h.oneNote("only on this machine")
	n := 0
	for _, ev := range evs {
		if ev.Stage == "credentials" && ev.Status == "notice" {
			n++
			if ev.Detail != h.lines()[0] {
				t.Fatalf("progress notice %q differs from the stderr line %q", ev.Detail, h.lines()[0])
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d credentials notices in the progress stream, want 1", n)
	}
}

func TestLocalModeWritesNoMarker(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.svc = (&Service{}).WithTunnel(appTunnel()) // no server: block
	t.Setenv("STUB_PB_WRITE", "1")
	if _, err := h.playbook(); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.marker(); ok {
		t.Fatal("local mode wrote a .vmtool-source marker")
	}
	if h.f.puts.Load()+h.f.gets.Load() != 0 {
		t.Fatal("local mode talked to the store")
	}
}

// ---- creds fetch / forget ----

func TestFetchVMCredsWritesTheCache(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.record(stUUID, stPass, csKey)
	if err := h.svc.FetchVMCreds(context.Background(), h.vms, h.ref); err != nil {
		t.Fatal(err)
	}
	if got := readFolderFile(t, h.dir(), "id_ed25519"); got != csKey {
		t.Fatalf("key %q", got)
	}
	assertMode(t, h.dir(), 0o700)
	assertMode(t, filepath.Join(h.dir(), "id_ed25519"), 0o600)
	// Overwrites an existing cache: the store's record is the truth.
	h.record(stUUID, csOld, csKey)
	if err := h.svc.FetchVMCreds(context.Background(), h.vms, h.ref); err != nil {
		t.Fatal(err)
	}
	if got := readFolderFile(t, h.dir(), "password.yml"); !strings.Contains(got, csOld) {
		t.Fatalf("fetch did not refresh: %q", got)
	}
}

func TestFetchVMCredsErrors(t *testing.T) {
	h := newCredHarness(t, stUUID)
	err := h.svc.FetchVMCreds(context.Background(), h.vms, h.ref)
	if !vmtool.IsNotFound(err) {
		t.Fatalf("no record: %v", err)
	}
	h.record(csOtherU, stPass, csKey)
	err = h.svc.FetchVMCreds(context.Background(), h.vms, h.ref)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale: %v", err)
	}
	if exists(h.dir()) {
		t.Fatal("a stale record was cached")
	}
	h.vms.uuid = ""
	if err := h.svc.FetchVMCreds(context.Background(), h.vms, h.ref); err == nil || !strings.Contains(err.Error(), "redeploy hv1") {
		t.Fatalf("old daemon: %v", err)
	}
	// Local mode: a clear error, nothing written.
	local := (&Service{}).WithTunnel(appTunnel())
	h.vms.uuid = stUUID
	if err := local.FetchVMCreds(context.Background(), h.vms, h.ref); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("local mode: %v", err)
	}
	if exists(h.dir()) {
		t.Fatal("local-mode fetch wrote a folder")
	}
}

func TestForgetVMCredsRemovesOnlyTheCache(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.folder(stPass, csKey, vmtool.SourceStore, stUUID)
	h.record(stUUID, stPass, csKey)
	if err := h.svc.ForgetVMCreds(context.Background(), h.ref); err != nil {
		t.Fatal(err)
	}
	if exists(h.dir()) {
		t.Fatal("cache kept")
	}
	if h.stored() == nil || h.f.dels.Load() != 0 {
		t.Fatal("forget touched the store's record")
	}
	// Local mode: .machines/ is the only copy; refuse rather than lose it.
	h.folder(stPass, csKey, "", "")
	local := (&Service{}).WithTunnel(appTunnel())
	if err := local.ForgetVMCreds(context.Background(), h.ref); err == nil {
		t.Fatal("local-mode forget removed the only copy")
	}
	if !exists(h.dir()) {
		t.Fatal("local-mode forget deleted the folder")
	}
}

// ---- Metal ----

func (h *credHarness) machine(pass, key string) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.machines["nuc1"] = map[string]any{"kind": "metal", "address": "10.0.0.42", "username": "bot", "password": pass, "private_key": key, "public_key": csPub}
}

func TestMachineExecUsesTheStoreRecord(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.machine(stPass, csKey)
	if _, err := h.svc.ExecMachine(context.Background(), "nuc1", []string{"true"}, ""); err != nil {
		t.Fatal(err)
	}
	dir := vmtool.MachineDir("nuc1")
	if got := readFolderFile(t, dir, "id_ed25519"); got != csKey {
		t.Fatalf("machine key not cached: %q", got)
	}
	assertMode(t, dir, 0o700)
	for _, n := range []string{"machine.yml", "password.yml", "id_ed25519", vmtool.SourceMarkerFile} {
		assertMode(t, filepath.Join(dir, n), 0o600)
	}
	a := h.argvText()
	if !strings.Contains(a, filepath.Join(".machines", "nuc1", "id_ed25519")) || !strings.Contains(a, "bot@10.0.0.42") {
		t.Fatalf("ssh argv:\n%s", a)
	}
	for _, secret := range []string{stPass, csKey, stTok} {
		if strings.Contains(a, secret) {
			t.Fatalf("secret %q in ssh argv", secret)
		}
	}
}

func TestMachineListAndRemoveUseTheStore(t *testing.T) {
	h := newCredHarness(t, stUUID)
	h.machine(stPass, csKey)
	ms, err := h.svc.ListMachines(context.Background())
	if err != nil || len(ms) != 1 || ms[0].Name != "nuc1" || ms[0].Host != "10.0.0.42" || ms[0].Kind != vmtool.MachineMetal {
		t.Fatalf("list %+v %v", ms, err)
	}
	if err := h.svc.RemoveMachine(context.Background(), "nuc1"); err != nil {
		t.Fatal(err)
	}
	h.f.mu.Lock()
	_, there := h.f.machines["nuc1"]
	h.f.mu.Unlock()
	if there || exists(vmtool.MachineDir("nuc1")) {
		t.Fatalf("rm kept the store record (%v) or the cache (%v)", there, exists(vmtool.MachineDir("nuc1")))
	}
	if err := h.svc.RemoveMachine(context.Background(), "nuc1"); !vmtool.IsNotFound(err) {
		t.Fatalf("second rm: %v", err)
	}
}

func TestMachineAddPushesTheRecord(t *testing.T) {
	h := newCredHarness(t, stUUID)
	// The probe's ssh answers like an Ubuntu box with passwordless sudo.
	testexec.Stub(t, map[string]string{
		"ssh":              "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + h.argv + "'\necho ID=ubuntu\necho SUDO=ok\n",
		"sshpass":          "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
		"ansible-playbook": "#!/bin/sh\nmkdir -p \"$STUB_PB_DIR\"; printf 'username: bot\\npassword: " + stPass + "\\n' > \"$STUB_PB_DIR/password.yml\"; printf '" + csKey + "' > \"$STUB_PB_DIR/id_ed25519\"; exit 0\n",
	})
	abs, _ := vmtool.AbsMachineDir("nuc1")
	t.Setenv("STUB_PB_DIR", abs)
	info, err := h.svc.AddMachine(context.Background(), nil, AddMachineInput{Name: "nuc1", Host: "10.0.0.42"})
	if err != nil || info == nil {
		t.Fatalf("add: %v", err)
	}
	h.f.mu.Lock()
	rec := h.f.machines["nuc1"]
	h.f.mu.Unlock()
	if rec == nil || rec["address"] != "10.0.0.42" || rec["kind"] != "metal" || rec["private_key"] != csKey || rec["password"] != stPass {
		t.Fatalf("store record %v", rec)
	}
	if m, ok, _ := vmtool.ReadSourceMarker(vmtool.MachineDir("nuc1")); !ok || m.State != vmtool.SourceStore {
		t.Fatalf("machine marker %+v ok=%v", m, ok)
	}
	for _, secret := range []string{stPass, csKey} {
		if strings.Contains(h.argvText(), secret) {
			t.Fatalf("secret in argv: %s", h.argvText())
		}
	}
}

func TestMachineAddWithTheStoreFailingStaysLocalAndSaysSo(t *testing.T) {
	h := newCredHarness(t, stUUID)
	testexec.Stub(t, map[string]string{
		"ssh":              "#!/bin/sh\necho ID=ubuntu\necho SUDO=ok\n",
		"sshpass":          "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
		"ansible-playbook": "#!/bin/sh\nmkdir -p \"$STUB_PB_DIR\"; printf 'username: bot\\npassword: " + stPass + "\\n' > \"$STUB_PB_DIR/password.yml\"; exit 0\n",
	})
	abs, _ := vmtool.AbsMachineDir("nuc1")
	t.Setenv("STUB_PB_DIR", abs)
	h.sess.Mode(context.Background())
	h.f.failCreds.Store(true)
	if _, err := h.svc.AddMachine(context.Background(), nil, AddMachineInput{Name: "nuc1", Host: "10.0.0.42"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	h.oneNote("only on this machine")
	if m, ok, _ := vmtool.ReadSourceMarker(vmtool.MachineDir("nuc1")); !ok || m.State != vmtool.SourcePending {
		t.Fatalf("machine marker %+v ok=%v", m, ok)
	}
}
