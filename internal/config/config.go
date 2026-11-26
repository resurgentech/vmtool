// Package config loads vmtool's config.yml and the shared bearer token.
//
// config.yml sits in vmtool's working directory next to .machines/. The token
// (VMTOOL_TOKEN) comes from the environment, else from .env in that directory.
// Only the SHA-256 of the token is stored (token_sha256). No error returned by
// this package contains the token.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/resurgentech/vmtool/internal/keycfg"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/resurgentech/vmtool/internal/tokenhash"
	"github.com/resurgentech/vmtool/pkg/vmtool"
	"gopkg.in/yaml.v3"
)

const (
	// FileName is the config file name inside the config directory.
	FileName = "config.yml"
	// EnvFileName is the dotenv file consulted for the token.
	EnvFileName = ".env"
	// TokenEnv is the environment variable (and .env key) holding the token.
	TokenEnv = "VMTOOL_TOKEN"
	// ServerTokenEnv is the environment variable (and .env key) holding the
	// client's token for the store named by server.url. It is never read from
	// config.yml.
	ServerTokenEnv = "VMTOOL_SERVER_TOKEN"

	RoleWorkstation = "workstation"
	RoleHypervisor  = "hypervisor"

	// DefaultListen is used when listen is not set.
	DefaultListen = "127.0.0.1:9473"
	// DefaultWait is create's address and SSH wait when config.yml sets none.
	DefaultWait = 120 * time.Second
	// MaxWaitSeconds is the longest wait config.yml may ask for: the
	// daemon's waitVMIP limit.
	MaxWaitSeconds = 3600
	// DefaultPort is a host's daemon port when not set.
	DefaultPort = 9473
)

var (
	// ErrNotFound means config.yml does not exist in the directory.
	ErrNotFound = errors.New("config.yml not found")
	// ErrNoToken means neither VMTOOL_TOKEN nor .env supplied a token.
	ErrNoToken = errors.New("no VMTOOL_TOKEN in environment or .env")
	// ErrNoServerToken means neither VMTOOL_SERVER_TOKEN nor .env supplied
	// the store token.
	ErrNoServerToken = errors.New("no VMTOOL_SERVER_TOKEN in environment or .env")
	// ErrTokenMismatch means the token does not match token_sha256.
	ErrTokenMismatch = errors.New("token does not match token_sha256 in config.yml")
)

var poolRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	// DefaultPool is the pool new VM disks of the local host go to when a
	// request names none (a plain pool name). Optional; a remote host's is
	// hosts.<name>.default_pool. Images stay in the cache pool.
	DefaultPool string `yaml:"default_pool"`
	// ImageDir is where base images are looked up. Optional; relative to the
	// config directory unless absolute. Callers use ResolvedImageDir.
	ImageDir string `yaml:"image_dir"`
	// IPWaitSeconds and SSHWaitSeconds bound create's wait for an address and
	// for SSH. 0 or absent is DefaultWait; otherwise 1..MaxWaitSeconds.
	// Create runs on the workstation, so only its config.yml governs; a
	// hypervisor accepts and ignores them.
	IPWaitSeconds  int `yaml:"ip_wait_seconds"`
	SSHWaitSeconds int `yaml:"ssh_wait_seconds"`
	// Server names the optional central store (tranche 10). Absent: images
	// and guest credentials stay on this machine.
	Server StoreServer `yaml:"server"`
	// StoreURL is the store a hypervisor daemon fetches base images from
	// (fetchImage): the one address it dials, https only. `just deploy`
	// writes it from the client's server.url. Optional; unused by a
	// workstation, and never used for anything but that download.
	StoreURL string `yaml:"store_url"`
	// NetworkHome is true for a hypervisor daemon whose host is a network
	// home: it runs no VMs, so its defineVM, cloneVolume, createVolume and
	// createTemplate answer 409. `just deploy` writes it (config write
	// --network-home) from the client's hosts.<h>.vms: false; it is
	// configuration, never an annotation in libvirt.
	NetworkHome bool `yaml:"network_home"`
	// StoreTokenFile is the path of the file holding a hypervisor daemon's
	// host token for the store (tranche 12): the one credential it uses to
	// report its VMs to store_url. A path, like tls_key (relative to the
	// config directory); the token itself is never in config.yml. Optional:
	// without it, or without store_url, the daemon does not report.
	StoreTokenFile string `yaml:"store_token_file"`
	// TokenFile is where this client keeps its own key (token_file); see
	// TokenFilePath. A path, never a key.
	TokenFile string `yaml:"token_file"`
	// PersonalKeys is the optional personal_keys: block (personalkeys.go).
	PersonalKeys PersonalKeys `yaml:"personal_keys"`
	// Dir is the directory config.yml was loaded from.
	Dir string `yaml:"-"`
}

// StoreServer is config.yml's server: block. The store token is not part of
// it: it comes from VMTOOL_SERVER_TOKEN (ServerToken).
type StoreServer struct {
	// URL is the store's base URL, https only, for example
	// https://proxy.example/vmstore.
	URL string `yaml:"url"`
}

// StoreConfigured reports whether config.yml names a store.
func (c *Config) StoreConfigured() bool { return c != nil && c.Server.URL != "" }

// checkServerURL validates server.url. The message never echoes the value:
// a URL can carry credentials.
func checkServerURL(raw string) error { return checkStoreAddress("server.url", raw) }

// checkStoreAddress is the check both store keys share: server.url on a
// client, store_url on a hypervisor. key is only named in the message.
func checkStoreAddress(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New(key + " must be an https URL with a host, like https://proxy.example/vmstore (no credentials, query or fragment)")
	}
	return nil
}

// StoreHostToken reads the host token named by store_token_file. It fails,
// with a reason that never contains the token or any part of the file, when
// store_url or store_token_file is not set, the file does not exist or cannot
// be read, its mode is not 0600, or it holds nothing. The caller treats any
// failure as "do not report".
func (c *Config) StoreHostToken() (string, error) {
	if c.StoreURL == "" {
		return "", errors.New("store_url is not set")
	}
	if c.StoreTokenFile == "" {
		return "", errors.New("store_token_file is not set")
	}
	p := c.Path(c.StoreTokenFile)
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", errors.New("store_token_file does not exist")
	case err != nil:
		return "", errors.New("store_token_file cannot be read")
	case !fi.Mode().IsRegular():
		return "", errors.New("store_token_file is not a regular file")
	case fi.Mode().Perm() != 0o600:
		return "", errors.New("store_token_file is not 0600")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", errors.New("store_token_file cannot be read")
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("store_token_file is empty")
	}
	return tok, nil
}

// Host is a remote hypervisor the workstation can reach.
type Host struct {
	Address string `yaml:"address"`
	SSHUser string `yaml:"ssh_user"`
	Port    int    `yaml:"port"`
	// DefaultPool is where new VM disks on this host go when a request names
	// no pool (a plain pool name). Optional.
	DefaultPool string `yaml:"default_pool"`
	// VMs is false for a network home: a host that runs no VMs (it routes,
	// addresses and names for networks that span hosts). Absent means true.
	VMs *bool `yaml:"vms"`
}

// HoldsVMs is whether VMs may be created on the host: true unless its entry
// says vms: false.
func (h Host) HoldsVMs() bool { return h.VMs == nil || *h.VMs }

// IPWait is how long create waits for an address.
func (c *Config) IPWait() time.Duration {
	if c == nil {
		return DefaultWait
	}
	return waitOf(c.IPWaitSeconds)
}

// SSHWait is how long create waits for SSH.
func (c *Config) SSHWait() time.Duration {
	if c == nil {
		return DefaultWait
	}
	return waitOf(c.SSHWaitSeconds)
}

func waitOf(sec int) time.Duration {
	if sec <= 0 {
		return DefaultWait
	}
	return time.Duration(sec) * time.Second
}

// Path resolves p against the config directory unless it is absolute or empty.
func (c *Config) Path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// ResolvedImageDir is ImageDir resolved by Path; "" when image_dir is unset.
func (c *Config) ResolvedImageDir() string { return c.Path(c.ImageDir) }

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
	// The key list's timings start at their defaults, so a file that names
	// refresh_on_miss: 0s (off) is told from one that leaves it out.
	c := Config{PersonalKeys: keycfg.Defaults()}
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
	for _, w := range []struct {
		key string
		v   int
	}{{"ip_wait_seconds", c.IPWaitSeconds}, {"ssh_wait_seconds", c.SSHWaitSeconds}} {
		if w.v < 0 || w.v > MaxWaitSeconds {
			return fmt.Errorf("%s must be 0 (default 120) or from 1 to %d seconds", w.key, MaxWaitSeconds)
		}
	}
	if c.Server.URL != "" {
		if err := checkServerURL(c.Server.URL); err != nil {
			return err
		}
	}
	if c.StoreURL != "" {
		if err := checkStoreAddress("store_url", c.StoreURL); err != nil {
			return err
		}
	}
	if err := c.PersonalKeys.Check(); err != nil {
		return err
	}
	if c.TokenSHA256 != "" && !tokenhash.Valid(c.TokenSHA256) {
		return errors.New("token_sha256 must be 64 lowercase hex characters")
	}
	if c.DefaultPool != "" && !poolRE.MatchString(c.DefaultPool) {
		return errors.New("default_pool must be a plain pool name (letters, digits, '.', '_', '-'; not starting with a punctuation mark)")
	}
	_, listedName := c.Hosts[c.Name]
	_, listedShort := c.Hosts[short]
	if listedName || listedShort {
		if c.DefaultPool != "" {
			return fmt.Errorf("default_pool must not be set when this host (%q) is listed under hosts: use hosts.%s.default_pool", c.Name, c.Name)
		}
	}
	if err := dotFree(c.Name); err != nil {
		return err
	}
	if err := c.notAMachine(c.Name); err != nil {
		return err
	}
	for name, h := range c.Hosts {
		if err := dotFree(name); err != nil {
			return err
		}
		if err := c.notAMachine(name); err != nil {
			return err
		}
		if !vmtool.ValidHostName(name) {
			return fmt.Errorf("host name %q is not a valid host name", name)
		}
		if (name == short || name == c.Name) && c.Role != RoleWorkstation {
			return fmt.Errorf("host %q has the same name as the local host; only a workstation may list itself", name)
		}
		if h.Port < 0 || h.Port > 65535 {
			return fmt.Errorf("hosts.%s.port must be 0 (default %d) or from 1 to 65535", name, DefaultPort)
		}
		if h.DefaultPool != "" && !poolRE.MatchString(h.DefaultPool) {
			return fmt.Errorf("hosts.%s.default_pool must be a plain pool name (letters, digits, '.', '_', '-'; not starting with a punctuation mark)", name)
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

// dotFree refuses a host name with a dot: VM SSH names are
// <vm>.<host>.vmtool and are split at the last dot.
func dotFree(name string) error {
	if strings.Contains(name, ".") {
		return fmt.Errorf("host name %q must not contain '.': VM SSH names are <vm>.<host>.vmtool", name)
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

// Hash returns the lowercase hex SHA-256 of token. It lives in the
// libvirt-free tokenhash package; this is the same function.
func Hash(token string) string { return tokenhash.Hash(token) }

// Check reports whether token hashes to hashHex, in constant time.
func Check(token, hashHex string) bool { return tokenhash.Check(token, hashHex) }

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
func Token(dir string) (string, error) { return envToken(dir, TokenEnv, ErrNoToken) }

// ServerToken returns VMTOOL_SERVER_TOKEN from the environment, else from
// dir/.env: the client's token for the store. It returns ErrNoServerToken
// when there is none. Never print or log the result.
func ServerToken(dir string) (string, error) {
	return envToken(dir, ServerTokenEnv, ErrNoServerToken)
}

func envToken(dir, key string, none error) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, EnvFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", none
		}
		return "", fmt.Errorf("read %s: %w", EnvFileName, err)
	}
	if v := parseDotEnv(b)[key]; v != "" {
		return v, nil
	}
	return "", none
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
	// StoreURL is a hypervisor's store_url (https only; validated).
	StoreURL string
	// StoreTokenFile is a hypervisor's store_token_file: a path, never a token.
	StoreTokenFile string
	// KeyRequestURL is personal_keys.request_url (validated; see PersonalKeys).
	KeyRequestURL string
	// NetworkHome sets network_home: true; false leaves the key as is.
	NetworkHome bool
}

// Write creates or updates dir/config.yml: it applies opts, sets
// token_sha256 to Hash(token), and keeps everything else (hosts, unknown
// keys). The result is validated before it replaces the file, which is
// written atomically with mode 0600.
func Write(dir, token string, opts WriteOptions) error {
	if token == "" {
		return ErrNoToken
	}
	return edit(dir, func(root *yaml.Node) error {
		for _, kv := range []struct{ k, v string }{
			{"name", opts.Name}, {"role", opts.Role}, {"listen", opts.Listen},
			{"tls_cert", opts.TLSCert}, {"tls_key", opts.TLSKey}, {"store_url", opts.StoreURL},
			{"store_token_file", opts.StoreTokenFile},
			{"token_sha256", Hash(token)},
		} {
			if kv.v != "" {
				setKey(root, kv.k, kv.v)
			}
		}
		if opts.KeyRequestURL != "" {
			setNested(root, "personal_keys", "request_url", opts.KeyRequestURL)
		}
		if opts.NetworkHome {
			setNode(root, "network_home", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		}
		return nil
	})
}

// SetHost adds or replaces the hosts.<name> entry with h and keeps every other
// key and entry (and comments). The entry is replaced whole: a zero Port or
// empty DefaultPool leaves that key out. No token is needed. The result is
// validated before it replaces the file, which is written atomically with
// mode 0600; a missing file is created.
func SetHost(dir, name string, h Host) error {
	if h.Address == "" || h.SSHUser == "" {
		return errors.New("host entry needs an address and an ssh_user")
	}
	return edit(dir, func(root *yaml.Node) error {
		str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			str("address"), str(h.Address), str("ssh_user"), str(h.SSHUser)}}
		if h.Port != 0 {
			entry.Content = append(entry.Content, str("port"),
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(h.Port)})
		}
		if h.DefaultPool != "" {
			entry.Content = append(entry.Content, str("default_pool"), str(h.DefaultPool))
		}
		if h.VMs != nil && !*h.VMs {
			entry.Content = append(entry.Content, str("vms"),
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
		}
		var hosts *yaml.Node
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "hosts" {
				hosts = root.Content[i+1]
			}
		}
		if hosts == nil || hosts.Kind != yaml.MappingNode {
			if hosts != nil && hosts.Kind != yaml.ScalarNode {
				return errors.New("hosts must be a mapping")
			}
			hosts = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNode(root, "hosts", hosts)
		}
		for i := 0; i+1 < len(hosts.Content); i += 2 {
			if hosts.Content[i].Value == name {
				// vms is set only by an explicit h.VMs; otherwise the
				// existing entry's key (a network home) is carried over.
				if h.VMs == nil && hosts.Content[i+1].Kind == yaml.MappingNode {
					old := hosts.Content[i+1].Content
					for j := 0; j+1 < len(old); j += 2 {
						if old[j].Value == "vms" {
							entry.Content = append(entry.Content, old[j], old[j+1])
						}
					}
				}
				hosts.Content[i+1] = entry
				return nil
			}
		}
		hosts.Content = append(hosts.Content, str(name), entry)
		return nil
	})
}

// SetHostVMs edits only the vms key of the existing hosts.<name> entry: false
// writes `vms: false`, true removes the key. The rest of the entry and of
// config.yml (comments, order, unknown keys) is left as it is. A name with no
// entry is an error and writes nothing.
func SetHostVMs(dir, name string, vms bool) error {
	return edit(dir, func(root *yaml.Node) error {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != "hosts" || root.Content[i+1].Kind != yaml.MappingNode {
				continue
			}
			hosts := root.Content[i+1]
			for j := 0; j+1 < len(hosts.Content); j += 2 {
				if hosts.Content[j].Value != name {
					continue
				}
				entry := hosts.Content[j+1]
				if entry.Kind != yaml.MappingNode {
					return fmt.Errorf("hosts.%s must be a mapping", name)
				}
				for k := 0; k+1 < len(entry.Content); k += 2 {
					if entry.Content[k].Value == "vms" {
						entry.Content = append(entry.Content[:k], entry.Content[k+2:]...)
						break
					}
				}
				if !vms {
					entry.Content = append(entry.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "vms"},
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
				}
				return nil
			}
		}
		return fmt.Errorf("unknown host %q in %s: set-host needs --address and --ssh-user to create the entry", name, FileName)
	})
}

// edit reads dir/config.yml as a node tree (empty if missing), applies fn,
// validates the result with parse and writes it atomically with mode 0600.
func edit(dir string, fn func(root *yaml.Node) error) error {
	path := filepath.Join(dir, FileName)
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	var doc *yaml.Node
	if b, err := os.ReadFile(path); err == nil {
		doc = &yaml.Node{}
		if err := yaml.Unmarshal(b, doc); err != nil {
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
	if err := fn(root); err != nil {
		return err
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

func setNode(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
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

// setNested sets block.key to val, creating the block when it is absent (or
// not a mapping) and keeping the block's other keys.
func setNested(root *yaml.Node, block, key, val string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == block && root.Content[i+1].Kind == yaml.MappingNode {
			setKey(root.Content[i+1], key, val)
			return
		}
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setKey(m, key, val)
	setNode(root, block, m)
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
