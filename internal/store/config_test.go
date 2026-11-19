package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/tokenhash"
)

const (
	tokA = "PLACEHOLDER-not-a-secret-1"
	tokB = "PLACEHOLDER-not-a-secret-2"
)

// fixture writes tokens.yaml (0600), enc.key (0600) and a config.yaml with
// extra lines appended; it returns the config path.
type fixture struct {
	dir    string
	tokens string // tokens.yaml body; "" means the default two tokens
	extra  string // appended to config.yaml
	tokenM os.FileMode
	keyM   os.FileMode
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("VMTOOL_STORE_S3_ACCESS_KEY", "")
	t.Setenv("VMTOOL_STORE_S3_SECRET_KEY", "")
	return &fixture{dir: t.TempDir(), tokenM: 0o600, keyM: 0o600}
}

func (f *fixture) write(t *testing.T) string {
	t.Helper()
	tokens := f.tokens
	if tokens == "" {
		tokens = "alice: " + tokenhash.Hash(tokA) + "\nbob: " + tokenhash.Hash(tokB) + "\n"
	}
	tp := filepath.Join(f.dir, "tokens.yaml")
	kp := filepath.Join(f.dir, "enc.key")
	for p, c := range map[string]struct {
		body string
		mode os.FileMode
	}{tp: {tokens, f.tokenM}, kp: {strings.Repeat("\x01", 32), f.keyM}} {
		if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, c.mode); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "tokens_file: " + tp + "\nkey_file: " + kp + "\ndata_dir: " + f.dir + "\n" + f.extra
	cp := filepath.Join(f.dir, "config.yaml")
	if err := os.WriteFile(cp, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cp
}

func mustLoad(t *testing.T, f *fixture) *Config {
	t.Helper()
	c, err := Load(f.write(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func mustRefuse(t *testing.T, f *fixture, want string) {
	t.Helper()
	_, err := Load(f.write(t))
	if err == nil {
		t.Fatalf("Load accepted; want error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
	for _, secret := range []string{tokA, tokB, tokenhash.Hash(tokA)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks a token or hash: %q", err)
		}
	}
}

func TestLoadDefaultsAndTokens(t *testing.T) {
	f := newFixture(t)
	t.Setenv("VMTOOL_STORE_S3_ACCESS_KEY", "PLACEHOLDER-ak")
	t.Setenv("VMTOOL_STORE_S3_SECRET_KEY", "PLACEHOLDER-sk")
	f.extra = "bucket: placeholder-bucket\ns3_endpoint: 127.0.0.1:9000\n"
	c := mustLoad(t, f)
	if c.Listen != "127.0.0.1:8095" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.Bucket != "placeholder-bucket" || c.S3Endpoint != "127.0.0.1:9000" {
		t.Errorf("bucket/endpoint wrong: %+v", c)
	}
	if c.S3AccessKey != "PLACEHOLDER-ak" || c.S3SecretKey != "PLACEHOLDER-sk" {
		t.Errorf("S3 keys not read from the environment")
	}
	if len(c.Tokens) != 2 || c.Tokens["alice"] != tokenhash.Hash(tokA) {
		t.Errorf("Tokens = %v", c.Tokens)
	}
}

func TestDefaultPaths(t *testing.T) {
	if DefaultConfigPath != "/etc/vmtool-store/config.yaml" ||
		DefaultTokensPath != "/etc/vmtool-store/tokens.yaml" ||
		DefaultKeyPath != "/etc/vmtool-store/enc.key" {
		t.Fatal("default paths changed")
	}
}

func TestLoadRefusals(t *testing.T) {
	good := tokenhash.Hash(tokA)
	cases := []struct {
		name  string
		tweak func(*fixture)
		want  string
	}{
		{"empty token file", func(f *fixture) { f.tokens = "{}\n" }, "no token"},
		{"non-hex hash", func(f *fixture) { f.tokens = "alice: not-hex\n" }, "64 lowercase hex"},
		{"uppercase hash", func(f *fixture) { f.tokens = "alice: " + strings.ToUpper(good) + "\n" }, "64 lowercase hex"},
		{"short hash", func(f *fixture) { f.tokens = "alice: " + good[:63] + "\n" }, "64 lowercase hex"},
		{"bad caller name", func(f *fixture) { f.tokens = "\"a b\\nc\": " + good + "\n" }, "name"},
		{"token file group-readable", func(f *fixture) { f.tokenM = 0o640 }, "tokens"},
		{"token file world-readable", func(f *fixture) { f.tokenM = 0o604 }, "tokens"},
		{"key file group-readable", func(f *fixture) { f.keyM = 0o640 }, "key"},
		{"key file world-writable", func(f *fixture) { f.keyM = 0o602 }, "key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.tweak(f)
			mustRefuse(t, f, c.want)
		})
	}
}

func TestLoadMissingFiles(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("missing config accepted")
	}
	f := newFixture(t)
	cp := f.write(t)
	if err := os.Remove(filepath.Join(f.dir, "tokens.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cp); err == nil {
		t.Fatal("missing tokens file accepted")
	}
}

// The listening rule (question 34): loopback may be plain; off loopback is
// plain only behind_proxy, else it needs TLS; every bind needs a token.
func TestListeningRule(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		extra  string
		want   string // "" = accepted
	}{
		{"loopback v4 plain", "127.0.0.1:8095", "", ""},
		{"loopback other 127/8 plain", "127.9.9.9:8095", "", ""},
		{"loopback v6 plain", "[::1]:8095", "", ""},
		{"localhost plain", "localhost:8095", "", ""},
		{"all interfaces plain refused", "0.0.0.0:8095", "", "TLS"},
		{"bare port plain refused", ":8095", "", "TLS"},
		{"lan address plain refused", "192.0.2.10:8095", "", "TLS"},
		{"v6 any plain refused", "[::]:8095", "", "TLS"},
		{"all interfaces behind_proxy", "0.0.0.0:8095", "behind_proxy: true\n", ""},
		{"bare port behind_proxy", ":8095", "behind_proxy: true\n", ""},
		{"all interfaces with tls", "0.0.0.0:8095", "tls_cert: /x/c.pem\ntls_key: /x/k.pem\n", ""},
		{"off loopback cert without key", "0.0.0.0:8095", "tls_cert: /x/c.pem\n", "TLS"},
		{"unparseable listen", "8095", "", "listen"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.extra = "listen: \"" + c.listen + "\"\n" + c.extra
			if c.want == "" {
				mustLoad(t, f)
				return
			}
			mustRefuse(t, f, c.want)
		})
	}
}

func TestEveryBindNeedsAToken(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8095", "0.0.0.0:8095"} {
		f := newFixture(t)
		f.tokens = "{}\n"
		f.extra = "listen: \"" + listen + "\"\nbehind_proxy: true\n"
		mustRefuse(t, f, "no token")
	}
}
