// Package config loads vmtool's config.yml and the shared bearer token.
//
// config.yml sits in vmtool's working directory next to .machines/. The token
// (VMTOOL_TOKEN) comes from the environment, else from .env in that directory.
// Only the SHA-256 of the token is stored (token_sha256). No error returned by
// this package contains the token.
package config

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jaredeh/vmtool/pkg/vmtool"
	"gopkg.in/yaml.v3"
)

const (
	// FileName is the config file name inside the config directory.
	FileName = "config.yml"
	// EnvFileName is the dotenv file consulted for the token.
	EnvFileName = ".env"
	// TokenEnv is the environment variable (and .env key) holding the token.
	TokenEnv = "VMTOOL_TOKEN"

	RoleWorkstation = "workstation"
	RoleHypervisor  = "hypervisor"

	// DefaultListen is used when listen is not set.
	DefaultListen = "127.0.0.1:9473"
	// DefaultPort is a host's daemon port when not set.
	DefaultPort = 9473
)

var (
	// ErrNotFound means config.yml does not exist in the directory.
	ErrNotFound = errors.New("config.yml not found")
	// ErrNoToken means neither VMTOOL_TOKEN nor .env supplied a token.
	ErrNoToken = errors.New("no VMTOOL_TOKEN in environment or .env")
	// ErrTokenMismatch means the token does not match token_sha256.
	ErrTokenMismatch = errors.New("token does not match token_sha256 in config.yml")
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hostnameFn is os.Hostname; tests replace it.
var hostnameFn = os.Hostname

// Config is the parsed config.yml.
type Config struct {
	Name        string          `yaml:"name"`
	Role        string          `yaml:"role"`
	Listen      string          `yaml:"listen"`
	TokenSHA256 string          `yaml:"token_sha256"`
	TLSCert     string          `yaml:"tls_cert"`
	TLSKey      string          `yaml:"tls_key"`
	Hosts       map[string]Host `yaml:"hosts"`
	// Dir is the directory config.yml was loaded from.
	Dir string `yaml:"-"`
}

// Host is a remote hypervisor the workstation can reach.
type Host struct {
	Address string `yaml:"address"`
	SSHUser string `yaml:"ssh_user"`
	Port    int    `yaml:"port"`
}

// Path resolves p against the config directory unless it is absolute or empty.
func (c *Config) Path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// ShortHostname returns the local hostname up to the first dot.
func ShortHostname() (string, error) {
	h, err := hostnameFn()
	if err != nil {
		return "", err
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h, nil
}

// Load reads and validates dir/config.yml, applying defaults. A missing file
// returns an error matching ErrNotFound.
func Load(dir string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w in %s", ErrNotFound, dir)
		}
		return nil, fmt.Errorf("read %s: %w", FileName, err)
	}
	return parse(dir, b)
}

var (
	yamlSyntaxLine = regexp.MustCompile(`^yaml: line (\d+):`)
	yamlTypeLine   = regexp.MustCompile(`^line (\d+):`)
)

// yamlLine returns " at line N" from a yaml error, or "" when it gives no
// line. Only the digits are taken, anchored at the start of the message, so
// no file content can reach the result.
func yamlLine(err error) string {
	var te *yaml.TypeError
	var m []string
	if errors.As(err, &te) && len(te.Errors) > 0 {
		m = yamlTypeLine.FindStringSubmatch(te.Errors[0])
	} else {
		m = yamlSyntaxLine.FindStringSubmatch(err.Error())
	}
	if m == nil {
		return ""
	}
	return " at line " + m[1]
}

func parse(dir string, b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		// yaml errors can quote file content; name only the line.
		return nil, fmt.Errorf("%s: invalid YAML or field types%s", FileName, yamlLine(err))
	}
	c.Dir = dir
	if err := c.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &c, nil
}

func (c *Config) finish() error {
	short, err := ShortHostname()
	if err != nil {
		return fmt.Errorf("local hostname: %w", err)
	}
	if c.Name == "" {
		c.Name = short
	}
	if c.Role == "" {
		c.Role = RoleWorkstation
	}
	if c.Role != RoleWorkstation && c.Role != RoleHypervisor {
		return fmt.Errorf("role must be %q or %q", RoleWorkstation, RoleHypervisor)
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.TokenSHA256 != "" && !hashRE.MatchString(c.TokenSHA256) {
		return errors.New("token_sha256 must be 64 lowercase hex characters")
	}
	if err := c.notAMachine(c.Name); err != nil {
		return err
	}
	for name, h := range c.Hosts {
		if err := c.notAMachine(name); err != nil {
			return err
		}
		if !vmtool.ValidMachineName(name) {
			return fmt.Errorf("host name %q is not a valid machine name", name)
		}
		if name == short || name == c.Name {
			return fmt.Errorf("host %q has the same name as the local host", name)
		}
		if h.Address == "" {
			return fmt.Errorf("host %q: address is required", name)
		}
		if h.Port == 0 {
			h.Port = DefaultPort
			c.Hosts[name] = h
		}
	}
	return nil
}

// notAMachine refuses a host name that is registered as a metal/alien
// machine: .machines/<name>/ holds that host's VM credentials, so the two
// cannot share a name.
func (c *Config) notAMachine(name string) error {
	if !vmtool.ValidMachineName(name) {
		return nil // reported by the caller's own validation
	}
	p := filepath.Join(c.Dir, vmtool.MachinesDir, name, vmtool.MachineManifest)
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("host name %q is also a metal/alien machine (%s); rename one", name,
			filepath.Join(vmtool.MachinesDir, name, vmtool.MachineManifest))
	}
	return nil
}

// Hash returns the lowercase hex SHA-256 of token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Check reports whether token hashes to hashHex, in constant time.
func Check(token, hashHex string) bool {
	if token == "" || hashHex == "" {
		return false
	}
	want, err := hex.DecodeString(hashHex)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// Verify loads the token and config from dir and checks one against the
// other. It returns ErrNoToken, ErrTokenMismatch, or a Load error (including
// ErrNotFound). A config with no token_sha256 never matches.
func Verify(dir string) error {
	tok, err := Token(dir)
	if err != nil {
		return err
	}
	c, err := Load(dir)
	if err != nil {
		return err
	}
	if !Check(tok, c.TokenSHA256) {
		return ErrTokenMismatch
	}
	return nil
}

// Token returns VMTOOL_TOKEN from the environment, else from dir/.env.
func Token(dir string) (string, error) {
	if v := os.Getenv(TokenEnv); v != "" {
		return v, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, EnvFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoToken
		}
		return "", fmt.Errorf("read %s: %w", EnvFileName, err)
	}
	if v := parseDotEnv(b)[TokenEnv]; v != "" {
		return v, nil
	}
	return "", ErrNoToken
}

func parseDotEnv(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// WriteOptions are the fields `config write` may set; empty means leave as is.
type WriteOptions struct {
	Name, Role, Listen, TLSCert, TLSKey string
}

// Write creates or updates dir/config.yml: it applies opts, sets
// token_sha256 to Hash(token), and keeps everything else (hosts, unknown
// keys). The result is validated before it replaces the file, which is
// written atomically with mode 0600.
func Write(dir, token string, opts WriteOptions) error {
	if token == "" {
		return ErrNoToken
	}
	path := filepath.Join(dir, FileName)
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if b, err := os.ReadFile(path); err == nil {
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: invalid YAML%s", FileName, yamlLine(err))
		}
		if len(doc.Content) > 0 {
			if doc.Content[0].Kind != yaml.MappingNode {
				return fmt.Errorf("%s: top level must be a mapping", FileName)
			}
			root = doc.Content[0]
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", FileName, err)
	}
	for _, kv := range []struct{ k, v string }{
		{"name", opts.Name}, {"role", opts.Role}, {"listen", opts.Listen},
		{"tls_cert", opts.TLSCert}, {"tls_key", opts.TLSKey},
		{"token_sha256", Hash(token)},
	} {
		if kv.v != "" {
			setKey(root, kv.k, kv.v)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return fmt.Errorf("encode %s: %w", FileName, err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if _, err := parse(dir, buf.Bytes()); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes())
}

func setKey(m *yaml.Node, key, val string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val}
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config.yml.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
