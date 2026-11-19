package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
	"gopkg.in/yaml.v3"
)

// ErrStaleCredential means a store record exists for (host, name) but for
// another domain UUID: the VM was recreated, and the record must not be used.
// It also matches vmtool.ErrNotFound, so a caller that only asks "is there a
// usable credential" treats it as none.
var ErrStaleCredential = fmt.Errorf("%w: the stored credentials are for another VM with this name (stale)", vmtool.ErrNotFound)

// VMCredential is one VM's guest login, as kept in .machines/<vm>.<host>.vmtool/
// (password.yml, id_ed25519, id_ed25519.pub) or in the store. UUID is the
// libvirt domain UUID the record belongs to; the local source has none.
type VMCredential struct {
	UUID       string
	Username   string
	Password   string
	PrivateKey string
	PublicKey  string
}

// MachineRecord is a metal or alien machine: its manifest and its login, as
// kept in .machines/<name>/ (machine.yml and the same credential files) or in
// the store. Address is the manifest's host.
type MachineRecord struct {
	Name       string
	Kind       vmtool.MachineKind
	Address    string
	Username   string
	Password   string
	PrivateKey string
	PublicKey  string
}

// MachineSummary is a machine's name and kind, for listings.
type MachineSummary struct {
	Name string
	Kind vmtool.MachineKind
}

// CredentialSource is where guest credentials and machine records live: this
// machine's .machines/ (LocalCredentials) or the store (StoreCredentials).
// A source is chosen once per command by StoreSession.
//
// Not-found errors match vmtool.IsNotFound. Deletes are idempotent: a record
// that is not there is not an error. Nothing here ever logs or returns a
// secret in an error.
type CredentialSource interface {
	// GetVM returns the VM's credentials. The store answers only for the
	// domain UUID given (a record for another UUID is ErrStaleCredential);
	// the local source has no UUID and ignores it.
	GetVM(ctx context.Context, host, name, uuid string) (*VMCredential, error)
	// PutVM replaces the record for (host, name).
	PutVM(ctx context.Context, host, name string, c VMCredential) error
	DeleteVM(ctx context.Context, host, name string) error
	ListMachines(ctx context.Context) ([]MachineSummary, error)
	GetMachine(ctx context.Context, name string) (*MachineRecord, error)
	// PutMachine replaces the record for m.Name.
	PutMachine(ctx context.Context, m MachineRecord) error
	DeleteMachine(ctx context.Context, name string) error
}

// LocalCredentials is the source for this machine's own files: .machines/ in
// the working directory, the layout setup_bot.yml writes. It is the source
// when no store is configured or reachable.
type LocalCredentials struct{}

const (
	passwordFile = "password.yml"
	privKeyFile  = "id_ed25519"
	pubKeyFile   = "id_ed25519.pub"
)

type passwordYML struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// readCredDir reads password.yml and the key files of dir. ok is false when
// the directory holds none of them.
func readCredDir(dir string) (c VMCredential, ok bool, err error) {
	if st, e := os.Stat(dir); e != nil || !st.IsDir() {
		return c, false, nil
	}
	if b, e := os.ReadFile(filepath.Join(dir, passwordFile)); e == nil {
		var p passwordYML
		if e := yaml.Unmarshal(b, &p); e != nil {
			return c, false, fmt.Errorf("%s: invalid %s", dir, passwordFile) // never echo content
		}
		c.Username, c.Password = p.Username, p.Password
	} else if !os.IsNotExist(e) {
		return c, false, e
	}
	for name, dst := range map[string]*string{privKeyFile: &c.PrivateKey, pubKeyFile: &c.PublicKey} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		switch {
		case e == nil:
			*dst = string(b)
		case !os.IsNotExist(e):
			return c, false, e
		}
	}
	ok = c.Username != "" || c.Password != "" || c.PrivateKey != "" || c.PublicKey != ""
	return c, ok, nil
}

// writeCredDir writes c's files into dir (0700, files 0600, each written
// atomically) and removes the ones c has no value for, so the folder is
// exactly the record. The .vmtool-source marker is not touched.
func writeCredDir(dir string, c VMCredential) error {
	if err := vmtool.EnsureCredsDir(dir); err != nil {
		return err
	}
	pw, err := yaml.Marshal(passwordYML{Username: c.Username, Password: c.Password})
	if err != nil {
		return err
	}
	files := []struct {
		name string
		data string
	}{{passwordFile, string(pw)}, {privKeyFile, c.PrivateKey}, {pubKeyFile, c.PublicKey}}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if f.data == "" && f.name != passwordFile {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := vmtool.WriteFileAtomic(p, []byte(f.data), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func vmKey(host, name string) error {
	if !vmtool.ValidHostName(host) || !vmtool.ValidMachineName(name) {
		return fmt.Errorf("invalid VM %q/%q: host must be a valid host name (no '.') and name a valid machine name", host, name)
	}
	return nil
}

func (LocalCredentials) GetVM(_ context.Context, host, name, _ string) (*VMCredential, error) {
	if err := vmKey(host, name); err != nil {
		return nil, err
	}
	c, ok, err := readCredDir(vmtool.CredsDir(host, name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("credentials for %s/%s: %w", host, name, vmtool.ErrNotFound)
	}
	return &c, nil
}

func (LocalCredentials) PutVM(_ context.Context, host, name string, c VMCredential) error {
	if err := vmKey(host, name); err != nil {
		return err
	}
	return writeCredDir(vmtool.CredsDir(host, name), c)
}

func (LocalCredentials) DeleteVM(_ context.Context, host, name string) error {
	return vmtool.ForgetVM(host, name)
}

func (LocalCredentials) ListMachines(context.Context) ([]MachineSummary, error) {
	ms, err := vmtool.ListMachines()
	if err != nil {
		return nil, err
	}
	out := make([]MachineSummary, 0, len(ms))
	for _, m := range ms {
		out = append(out, MachineSummary{Name: m.Name, Kind: m.Kind})
	}
	return out, nil
}

func (LocalCredentials) GetMachine(_ context.Context, name string) (*MachineRecord, error) {
	m, err := vmtool.LoadMachine(name)
	if err != nil {
		return nil, err
	}
	c, _, err := readCredDir(vmtool.MachineDir(name))
	if err != nil {
		return nil, err
	}
	return &MachineRecord{Name: name, Kind: m.Kind, Address: m.Host, Username: c.Username,
		Password: c.Password, PrivateKey: c.PrivateKey, PublicKey: c.PublicKey}, nil
}

func (LocalCredentials) PutMachine(_ context.Context, m MachineRecord) error {
	if err := vmtool.SaveMachine(vmtool.Machine{Name: m.Name, Kind: m.Kind, Host: m.Address}); err != nil {
		return err
	}
	return writeCredDir(vmtool.MachineDir(m.Name), VMCredential{Username: m.Username,
		Password: m.Password, PrivateKey: m.PrivateKey, PublicKey: m.PublicKey})
}

func (LocalCredentials) DeleteMachine(_ context.Context, name string) error {
	err := vmtool.RemoveMachine(name)
	if vmtool.IsNotFound(err) {
		return nil
	}
	return err
}

// StoreCredentials is the source backed by the store: thin calls of
// internal/storeclient.
type StoreCredentials struct{ Client *storeclient.Client }

// storeErr maps the client's sentinel errors onto the ones this package
// documents; anything else passes through (it carries no secret).
func storeErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storeclient.ErrStale):
		return fmt.Errorf("%v: %w", err, ErrStaleCredential)
	case errors.Is(err, storeclient.ErrNotFound):
		return fmt.Errorf("%v: %w", err, vmtool.ErrNotFound)
	}
	return err
}

func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s StoreCredentials) GetVM(ctx context.Context, host, name, uuid string) (*VMCredential, error) {
	r, err := s.Client.GetVMCredential(ctx, host, name, uuid)
	if err != nil {
		return nil, storeErr(err)
	}
	return &VMCredential{UUID: r.Uuid, Username: r.Username, Password: r.Password, PrivateKey: r.PrivateKey, PublicKey: r.PublicKey}, nil
}

func (s StoreCredentials) PutVM(ctx context.Context, host, name string, c VMCredential) error {
	_, err := s.Client.PutVMCredential(ctx, host, name, storeclient.VMCredentialInput{Uuid: c.UUID, Username: c.Username,
		Password: str(c.Password), PrivateKey: str(c.PrivateKey), PublicKey: str(c.PublicKey)})
	return storeErr(err)
}

func (s StoreCredentials) DeleteVM(ctx context.Context, host, name string) error {
	if err := storeErr(s.Client.DeleteVMCredential(ctx, host, name)); err != nil && !vmtool.IsNotFound(err) {
		return err
	}
	return nil
}

func (s StoreCredentials) ListMachines(ctx context.Context) ([]MachineSummary, error) {
	ms, err := s.Client.ListMachines(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	out := make([]MachineSummary, 0, len(ms))
	for _, m := range ms {
		out = append(out, MachineSummary{Name: m.Name, Kind: vmtool.MachineKind(m.Kind)})
	}
	return out, nil
}

func (s StoreCredentials) GetMachine(ctx context.Context, name string) (*MachineRecord, error) {
	m, err := s.Client.GetMachine(ctx, name)
	if err != nil {
		return nil, storeErr(err)
	}
	return &MachineRecord{Name: m.Name, Kind: vmtool.MachineKind(m.Kind), Address: m.Address, Username: m.Username,
		Password: m.Password, PrivateKey: m.PrivateKey, PublicKey: m.PublicKey}, nil
}

func (s StoreCredentials) PutMachine(ctx context.Context, m MachineRecord) error {
	_, err := s.Client.PutMachine(ctx, m.Name, storeclient.MachineInput{Kind: storeclient.MachineInputKind(m.Kind),
		Address: m.Address, Username: m.Username, Password: str(m.Password), PrivateKey: str(m.PrivateKey), PublicKey: str(m.PublicKey)})
	return storeErr(err)
}

func (s StoreCredentials) DeleteMachine(ctx context.Context, name string) error {
	if err := storeErr(s.Client.DeleteMachine(ctx, name)); err != nil && !vmtool.IsNotFound(err) {
		return err
	}
	return nil
}
