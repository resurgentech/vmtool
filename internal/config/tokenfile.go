package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TokenFileEnv names the environment variable that points at the token file.
const TokenFileEnv = "VMTOOL_TOKEN_FILE"

// Where a key came from, as `vmtool key status` reports it. Never the value.
const (
	SourceEnv     = "env"
	SourceDotEnv  = ".env"
	SourceFile    = "file"
	SourceNone    = "none"
	tokenFileName = "token"
)

// ErrNoTokenFile means there is no token file (it does not exist).
var ErrNoTokenFile = errors.New("no token file")

// TokenFilePath is where this client's key file is: VMTOOL_TOKEN_FILE, else
// config.yml's top-level token_file (a leading ~/ is the home directory, any
// other relative path is relative to the config directory), else
// $XDG_CONFIG_HOME/vmtool/token, else $HOME/.config/vmtool/token. A config.yml
// that is missing or unreadable is not an error here: the path then comes from
// the last two rules.
func TokenFilePath(dir string) (string, error) {
	if p := os.Getenv(TokenFileEnv); p != "" {
		return p, nil
	}
	if b, err := os.ReadFile(filepath.Join(dir, FileName)); err == nil {
		var c struct {
			TokenFile string `yaml:"token_file"`
		}
		if yaml.Unmarshal(b, &c) == nil && c.TokenFile != "" {
			return resolveTokenFile(dir, c.TokenFile)
		}
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "vmtool", tokenFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("token file: no XDG_CONFIG_HOME or home directory: %w", err)
	}
	return filepath.Join(home, ".config", "vmtool", tokenFileName), nil
}

func resolveTokenFile(dir, p string) (string, error) {
	switch {
	case strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("token_file %s: no home directory: %w", FileName, err)
		}
		return filepath.Join(home, p[2:]), nil
	case filepath.IsAbs(p):
		return p, nil
	}
	return filepath.Join(dir, p), nil
}

// ReadTokenFile reads the key in the file at path, trimmed. A missing file is
// ErrNoTokenFile. Anything else wrong (not a regular file, a symlink, group
// or other permission bits, empty) is an error that names the file and never
// shows its content.
func ReadTokenFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoTokenFile
		}
		return "", fmt.Errorf("token file %s: %s", path, errKind(err))
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file (a symlink or other file is refused)", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s has mode %04o; it must be 0600 or tighter (chmod 600 %s)", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file %s: %s", path, errKind(err))
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty; remove it to request a key again", path)
	}
	return tok, nil
}

func errKind(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "unreadable"
}

// envSource is envToken that also says where the value came from.
func envSource(dir, key string) (val, src string, err error) {
	if v := os.Getenv(key); v != "" {
		return v, SourceEnv, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, EnvFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", SourceNone, nil
		}
		return "", "", fmt.Errorf("read %s: %w", EnvFileName, err)
	}
	if v := parseDotEnv(b)[key]; v != "" {
		return v, SourceDotEnv, nil
	}
	return "", SourceNone, nil
}

// resolveKey is the order: the variable (environment, then .env), then the
// token file. none is returned when no source holds a key.
func resolveKey(dir, envKey string, none error) (key, src string, err error) {
	key, src, err = envSource(dir, envKey)
	if err != nil || key != "" {
		return key, src, err
	}
	path, err := TokenFilePath(dir)
	if err != nil {
		return "", "", err
	}
	key, err = ReadTokenFile(path)
	switch {
	case errors.Is(err, ErrNoTokenFile):
		return "", SourceNone, none
	case err != nil:
		return "", "", err
	}
	return key, SourceFile, nil
}

// DaemonKey is the key for the daemons and the local service: VMTOOL_TOKEN
// (environment, then .env), then the token file. src is "env", ".env" or
// "file". It returns ErrNoToken when none holds one, and an error naming the
// token file when the file is refused. Never print or log the key.
//
// Token stays the operator token alone (environment, .env): `config write`
// hashes it into token_sha256 and a personal key must never become that.
func DaemonKey(dir string) (key, src string, err error) {
	return resolveKey(dir, TokenEnv, ErrNoToken)
}

// StoreKey is the key for the store: VMTOOL_SERVER_TOKEN (environment, then
// .env), then the token file. It returns ErrNoServerToken when none holds one.
func StoreKey(dir string) (key, src string, err error) {
	return resolveKey(dir, ServerTokenEnv, ErrNoServerToken)
}
