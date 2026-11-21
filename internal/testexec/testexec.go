// Package testexec is the one way a test lets vmtool start an external tool.
//
// Under `go test`, pkg/vmtool refuses to start any command (ansible,
// ansible-playbook, ssh, sshpass, scp, sudo, or anything else) unless it
// resolves to a file inside the directory named by VMTOOL_TEST_STUBS. Stub
// writes the stubs, puts their directory first on PATH and sets that
// variable; nothing else should.
//
// Stub uses t.Setenv, so a test that installs stubs cannot run in parallel.
package testexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// StubsEnv names the stub directory pkg/vmtool allows under `go test`.
const StubsEnv = "VMTOOL_TEST_STUBS"

// Stub writes each script (name -> file contents, normally starting with
// "#!/bin/sh") as an executable in a new temp dir, prepends that dir to PATH,
// sets StubsEnv to it and returns it. A second call replaces the first: only
// the newest stub directory is allowed.
func Stub(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(StubsEnv, dir)
	return dir
}

// ansibleScript is the stub `ansible` and `ansible-playbook`. Each call appends
// one line to the log (@LOG@): "grow" for the grow-disk shell command, "other
// <argv>" for anything else. STUB_GROW_FAIL makes a grow exit 2 with output;
// STUB_GROW_OUT is what it prints.
const ansibleScript = `#!/bin/sh
case "$*" in
*growpart*)
  echo "grow" >> '@LOG@'
  if [ -n "$STUB_GROW_FAIL" ]; then echo "$STUB_GROW_OUT"; exit 2; fi ;;
*)
  echo "other $*" >> '@LOG@' ;;
esac
exit 0
`

// sshScript is the stub `ssh`, the SSH wait's probe. Each call appends
// "probe <argv>" to the log (@LOG@), or "probe (sshpass) <argv>" when SSHPASS
// is set (it came through sshpass -e), and exits 0; with STUB_WAIT_FAIL set it
// prints "wait boom: sshd not answering" and exits 255. sshpassScript runs
// the stub ssh with the arguments after "-e ssh".
const sshScript = `#!/bin/sh
echo "probe ${SSHPASS:+(sshpass) }$*" >> '@LOG@'
if [ -n "$STUB_WAIT_FAIL" ]; then echo "wait boom: sshd not answering"; exit 255; fi
exit 0
`

const sshpassScript = `#!/bin/sh
shift 2
exec ssh "$@"
`

// Ansible installs stub `ansible`, `ansible-playbook`, `ssh` and `sshpass`
// (see ansibleScript, sshScript) with Stub and returns the path of their
// shared call log.
func Ansible(t *testing.T) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	script := strings.ReplaceAll(ansibleScript, "@LOG@", log)
	Stub(t, map[string]string{
		"ansible":          script,
		"ansible-playbook": script,
		"ssh":              strings.ReplaceAll(sshScript, "@LOG@", log),
		"sshpass":          strings.ReplaceAll(sshpassScript, "@LOG@", log),
	})
	return log
}
