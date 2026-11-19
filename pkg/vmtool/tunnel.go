package vmtool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TokenEnv is the environment variable that carries the vmtool token to a
// child process. The token never goes in argv, an inventory or any file.
const TokenEnv = "VMTOOL_TOKEN"

// ServerTokenEnv is the environment variable that carries the client's store
// token (config.ServerTokenEnv). No child vmtool builds for ssh, sshpass or
// ansible gets it: those tools need nothing from the store.
const ServerTokenEnv = "VMTOOL_SERVER_TOKEN"

// Tunnel is how to reach guests: ssh runs `<Exe> tunnel host/vm` as its
// ProxyCommand, and the child needs Token in its environment to pass vmtool's
// token gate.
//
// ssh runs the ProxyCommand with the parent's working directory, where the
// `vmtool tunnel` child finds config.yml: vmtool's own directory for the CLI
// and TUI, the service's WorkingDirectory for the daemon.
type Tunnel struct {
	// Exe is the absolute path of the running vmtool.
	Exe string
	// Token is put in the child's environment as VMTOOL_TOKEN. Empty leaves
	// the inherited environment alone.
	Token string
}

// CurrentExe is the absolute, symlink-resolved path of the running vmtool.
// Under `go test`, or when the executable is named *.test, it is an error: a
// test binary used as the tunnel's ProxyCommand would re-run its own suite
// for every ssh it serves (the 2026-11-17 fork bomb). Tests set Tunnel.Exe to
// a stub instead.
func CurrentExe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the vmtool executable: %w", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if testing.Testing() || strings.HasSuffix(filepath.Base(p), ".test") {
		return "", testBinaryTunnelError(p)
	}
	return filepath.Abs(p)
}

// ProxyCommand is the shell command ssh runs to reach VM name on host. Exe is
// shell-quoted when it needs it, because ssh hands the line to the user's
// shell.
func (t Tunnel) ProxyCommand(host, name string) string {
	return shQuote(t.Exe) + " tunnel " + host + "/" + name
}

// Environ is base with VMTOOL_TOKEN set to the token, replacing any existing
// entry, and VMTOOL_SERVER_TOKEN (the store token) removed. With no token the
// host token is left as it was.
func (t Tunnel) Environ(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, ServerTokenEnv+"=") || (t.Token != "" && strings.HasPrefix(kv, TokenEnv+"=")) {
			continue
		}
		out = append(out, kv)
	}
	if t.Token != "" {
		out = append(out, TokenEnv+"="+t.Token)
	}
	return out
}

// childEnviron is this process's environment for an ssh or sshpass child, less
// the store token.
func childEnviron() []string {
	return Tunnel{}.Environ(os.Environ())
}

func (t Tunnel) check() error {
	if t.Exe == "" {
		return fmt.Errorf("cannot reach guests through the tunnel: vmtool executable path unknown")
	}
	if isTestBinary(t.Exe) {
		return testBinaryTunnelError(t.Exe)
	}
	return nil
}

// shQuote quotes s for a POSIX shell when it holds anything but plain
// path characters.
func shQuote(s string) string {
	const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:,@%"
	if s != "" && strings.Trim(s, plain) == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
