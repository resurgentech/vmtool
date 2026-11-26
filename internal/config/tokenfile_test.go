package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fileKey = "file-key-7f3a91"

func writeKeyFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestTokenFilePathOrder(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if p, _ := TokenFilePath(dir); p != filepath.Join(home, ".config", "vmtool", "token") {
		t.Errorf("home rule: %s", p)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if p, _ := TokenFilePath(dir); p != filepath.Join(xdg, "vmtool", "token") {
		t.Errorf("xdg rule: %s", p)
	}
	for _, c := range []struct{ yml, want string }{
		{"token_file: keys/tok\n", filepath.Join(dir, "keys", "tok")},
		{"token_file: /abs/tok\n", "/abs/tok"},
		{"token_file: ~/k/tok\n", filepath.Join(home, "k", "tok")},
	} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(c.yml), 0o600); err != nil {
			t.Fatal(err)
		}
		if p, _ := TokenFilePath(dir); p != c.want {
			t.Errorf("%q: got %s want %s", c.yml, p, c.want)
		}
	}
	t.Setenv(TokenFileEnv, "/env/tok")
	if p, _ := TokenFilePath(dir); p != "/env/tok" {
		t.Errorf("env rule: %s", p)
	}
}

func TestReadTokenFileRefusals(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "good")
	writeKeyFile(t, good, " "+fileKey+" \n", 0o600)
	if k, err := ReadTokenFile(good); err != nil || k != fileKey {
		t.Fatalf("good: %q %v", k, err)
	}
	tight := filepath.Join(d, "tight")
	writeKeyFile(t, tight, fileKey, 0o400)
	if k, err := ReadTokenFile(tight); err != nil || k != fileKey {
		t.Fatalf("0400: %q %v", k, err)
	}
	if _, err := ReadTokenFile(filepath.Join(d, "absent")); !errors.Is(err, ErrNoTokenFile) {
		t.Fatalf("missing: %v", err)
	}
	loose := filepath.Join(d, "loose")
	writeKeyFile(t, loose, fileKey, 0o644)
	empty := filepath.Join(d, "empty")
	writeKeyFile(t, empty, " \n", 0o600)
	link := filepath.Join(d, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"0644": loose, "empty": empty, "symlink": link, "directory": d} {
		_, err := ReadTokenFile(p)
		if err == nil || errors.Is(err, ErrNoTokenFile) {
			t.Errorf("%s: want a refusal, got %v", name, err)
			continue
		}
		if !strings.Contains(err.Error(), p) || strings.Contains(err.Error(), fileKey) {
			t.Errorf("%s: error must name the file and hide the key: %v", name, err)
		}
	}
}

// Every row of both orders: env, .env, file, none.
func TestKeyOrders(t *testing.T) {
	for _, c := range []struct {
		name, envKey string
		fn           func(string) (string, string, error)
		none         error
	}{
		{TokenEnv, TokenEnv, DaemonKey, ErrNoToken},
		{ServerTokenEnv, ServerTokenEnv, StoreKey, ErrNoServerToken},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			tf := filepath.Join(t.TempDir(), "tok")
			t.Setenv(TokenFileEnv, tf)
			t.Setenv(c.envKey, "")
			if _, src, err := c.fn(dir); !errors.Is(err, c.none) || src != SourceNone {
				t.Fatalf("none: %q %v", src, err)
			}
			writeKeyFile(t, tf, fileKey, 0o600)
			if k, src, err := c.fn(dir); err != nil || k != fileKey || src != SourceFile {
				t.Fatalf("file: %q %q %v", k, src, err)
			}
			os.WriteFile(filepath.Join(dir, EnvFileName), []byte(c.envKey+"=dotenv-key\n"), 0o600)
			if k, src, err := c.fn(dir); err != nil || k != "dotenv-key" || src != SourceDotEnv {
				t.Fatalf(".env must win over the file: %q %q %v", k, src, err)
			}
			t.Setenv(c.envKey, "env-key")
			if k, src, err := c.fn(dir); err != nil || k != "env-key" || src != SourceEnv {
				t.Fatalf("env: %q %q %v", k, src, err)
			}
			// A refused file is an error only when nothing earlier answers.
			t.Setenv(c.envKey, "")
			os.Remove(filepath.Join(dir, EnvFileName))
			os.Chmod(tf, 0o644)
			if _, _, err := c.fn(dir); err == nil || errors.Is(err, c.none) || !strings.Contains(err.Error(), tf) {
				t.Fatalf("0644 file: %v", err)
			}
		})
	}
}

// Token keeps its meaning: environment, then .env, never the file.
func TestTokenIgnoresTokenFile(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(t.TempDir(), "tok")
	t.Setenv(TokenFileEnv, tf)
	t.Setenv(TokenEnv, "")
	writeKeyFile(t, tf, fileKey, 0o600)
	if _, err := Token(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Token read the file: %v", err)
	}
}
