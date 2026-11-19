// Package store is vmtool-store: the image and credential store. It imports
// neither pkg/vmtool nor internal/config (both link libvirt through cgo); the
// command must build with CGO_ENABLED=0.
//
// Nothing here logs or echoes a token, a ticket, a password, a key or a
// request body.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"

	"github.com/jaredeh/vmtool/internal/tokenhash"
	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigPath = "/etc/vmtool-store/config.yaml"
	DefaultTokensPath = "/etc/vmtool-store/tokens.yaml"
	DefaultKeyPath    = "/etc/vmtool-store/enc.key"
	DefaultListen     = "127.0.0.1:8095"
	DefaultDataDir    = "/var/lib/vmtool-store"

	// S3AccessKeyEnv and S3SecretKeyEnv carry the S3 credentials; they are
	// never read from a file.
	S3AccessKeyEnv = "VMTOOL_STORE_S3_ACCESS_KEY"
	S3SecretKeyEnv = "VMTOOL_STORE_S3_SECRET_KEY"
)

// Config is config.yaml plus what Load reads from the environment and from
// the token file.
type Config struct {
	Listen      string `yaml:"listen"`
	DataDir     string `yaml:"data_dir"`
	Bucket      string `yaml:"bucket"`
	S3Endpoint  string `yaml:"s3_endpoint"`
	BehindProxy bool   `yaml:"behind_proxy"`
	TokensFile  string `yaml:"tokens_file"`
	KeyFile     string `yaml:"key_file"`
	TLSCert     string `yaml:"tls_cert"`
	TLSKey      string `yaml:"tls_key"`

	// S3AccessKey and S3SecretKey come from the environment.
	S3AccessKey string `yaml:"-"`
	S3SecretKey string `yaml:"-"`
	// Tokens maps a caller's name to the SHA-256 hex of its token.
	Tokens map[string]string `yaml:"-"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Load reads and validates the config at path, the token file and the
// environment. It refuses to return a Config the server must not run with.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.TokensFile == "" {
		c.TokensFile = DefaultTokensPath
	}
	if c.KeyFile == "" {
		c.KeyFile = DefaultKeyPath
	}
	c.S3AccessKey = os.Getenv(S3AccessKeyEnv)
	c.S3SecretKey = os.Getenv(S3SecretKeyEnv)

	if err := c.loadTokens(); err != nil {
		return nil, err
	}
	if err := privateFile("key file", c.KeyFile, true); err != nil {
		return nil, err
	}
	if err := c.checkListen(); err != nil {
		return nil, err
	}
	return c, nil
}

// privateFile refuses a file with any group or other permission bit. A
// missing file is accepted when optional.
func privateFile(what, path string, optional bool) error {
	fi, err := os.Stat(path)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s %s is not a regular file", what, path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s %s is accessible by group or others (mode %04o); chmod 600 it", what, path, fi.Mode().Perm())
	}
	return nil
}

func (c *Config) loadTokens() error {
	if err := privateFile("tokens file", c.TokensFile, false); err != nil {
		return err
	}
	b, err := os.ReadFile(c.TokensFile)
	if err != nil {
		return fmt.Errorf("tokens file: %w", err)
	}
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		// The yaml error can quote a value; say only where it failed.
		return fmt.Errorf("tokens file %s is not a {name: sha256-hex} map", c.TokensFile)
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !nameRE.MatchString(n) {
			return fmt.Errorf("tokens file %s: caller name %q is not 1-64 of letters, digits, '.', '_', '-'", c.TokensFile, n)
		}
		if !tokenhash.Valid(m[n]) {
			return fmt.Errorf("tokens file %s: the hash for %q must be 64 lowercase hex characters", c.TokensFile, n)
		}
	}
	if len(m) == 0 {
		return fmt.Errorf("tokens file %s has no token, so no request could authenticate", c.TokensFile)
	}
	c.Tokens = m
	return nil
}

// checkListen is the listening rule: plain HTTP on loopback; off loopback
// plain HTTP only with behind_proxy, otherwise TLS cert and key are required.
// Every bind requires at least one token hash.
func (c *Config) checkListen() error {
	if len(c.Tokens) == 0 {
		return errors.New("no token hash configured, so no request could authenticate")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q is not host:port: %w", c.Listen, err)
	}
	if isLoopback(host) || c.BehindProxy {
		if (c.TLSCert == "") != (c.TLSKey == "") {
			return errors.New("set both tls_cert and tls_key, or neither")
		}
		return nil
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		return fmt.Errorf("listen %q is off loopback: set tls_cert and tls_key (TLS), or behind_proxy: true behind a TLS-terminating proxy", c.Listen)
	}
	return nil
}

// UsesTLS reports whether the server terminates TLS itself.
func (c *Config) UsesTLS() bool { return c.TLSCert != "" && c.TLSKey != "" }

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
