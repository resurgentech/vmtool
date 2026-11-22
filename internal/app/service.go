package app

import (
	"path/filepath"
	"time"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// Manager is the libvirt surface Service needs.
// *vmtool.Manager satisfies this. There is no Create method:
// CreateVM calls CloneImage / ResizeVolume / Define / Start itself.
type Manager interface {
	Start(name string) error
	Stop(name string) error
	Resume(name string) error
	Destroy(name string) error
	Delete(name string, noclone bool) error
	Info(name string) (*vmtool.VMInfo, error)
	List() ([]vmtool.VMInfo, error)
	WaitForIP(name string, timeout time.Duration) (string, error)
	SetAutostart(name string, enabled bool) error
	Reboot(name string) error
	ResizeDisk(name string, sizeGB uint) error
	MigrateDisk(name, pool string) error
	CreateVolume(pool, name string, sizeGB uint) (string, error)
	AddDisk(name string, sizeGB uint, pool string) (*vmtool.DiskDevice, error)
	ImagePath(name string) (string, error)
	ListNetworks() ([]string, error)
	ListBridges() ([]string, error)
	// CreateNetwork, Network and DeleteNetwork are named vmtool networks on
	// one host; Service.CreateNetwork, Network and DeleteNetwork are the
	// workflows over them.
	CreateNetwork(name, subnet, domain string) (*vmtool.NetworkInfo, error)
	Network(name string) (*vmtool.NetworkInfo, error)
	DeleteNetwork(name string) error
	// AddReservation, RemoveReservation, AttachInterface and DetachInterface
	// are a VM's cards on vmtool networks: the primitives behind
	// Service.AttachNetwork, DetachNetwork, CreateVM's networks and DeleteVM.
	// A card's reservation is made before the card and removed after it.
	AddReservation(network, vm, mac, ip string) (*vmtool.Reservation, error)
	RemoveReservation(network, mac string) error
	AttachInterface(vm, network, mac string) (*vmtool.NetAttachment, error)
	DetachInterface(vm, mac string) error
	ListImagesByPool() (map[string][]string, error)
	DeleteImage(name, pool string) error
	ListPools() ([]vmtool.PoolInfo, error)
	CreatePool(name, path string) error
	CloneImage(basePath, newName, targetPool string, sizeBytes uint64) (string, error)
	ResizeVolume(volPath string, sizeBytes uint64) error
	Define(cfg vmtool.VMConfig) error
}

type Service struct {
	PlaybookDir   string
	InventoryPath string
	IPTimeout     time.Duration
	// SSHTimeout bounds create's wait_ssh stage; zero means 120s.
	SSHTimeout time.Duration
	// SSHSleep replaces time.Sleep between SSH probes; tests pair it with Now.
	SSHSleep func(time.Duration)
	// HostNames are the local host and the configured hosts. Their
	// .machines/<name>/ directories hold VM credentials, so AddMachine
	// refuses them as machine names and migration leaves them alone.
	HostNames []string
	// Tunnel is how guest SSH, ansible and the inventory reach libvirt VMs:
	// through `vmtool tunnel host/vm`, with Tunnel.Token in the child's
	// environment. The token is per caller (the CLI's loaded token, the
	// daemon's per-request bearer), so a long-lived Service holds none and
	// callers derive one with WithTunnel. Metal and alien machines do not use
	// it: they keep direct SSH.
	Tunnel vmtool.Tunnel
	// Images finds this machine's copy of a base image (LocalImages: its
	// libvirt pools, then image_dir). CreateVM on a host whose Manager is an
	// ImageStore uploads the image from there. nil means this machine has no
	// local copy to offer: a host's cached copy is then used as it is.
	Images ImageSource
	// Store is this command's (or request's) view of the optional central
	// store: it decides the mode on first use and remembers it. nil means no
	// server: is configured: local mode, local sources.
	Store *StoreSession
	// ImageCachePath is this machine's image-hash cache; empty means
	// vmtool.ImageManifestFile in the working directory.
	ImageCachePath string
	// Now is the clock create's elapsed stamps read; nil means time.Now.
	Now func() time.Time
	// OpenHost opens another host's Manager (hosts.Resolver.Resolve has this
	// shape). The workflows of networks that span hosts use it to reach a
	// network's home and its members; nil means this command cannot reach any
	// host but the one it was given.
	OpenHost HostOpener
	// HostAddr is the address a host's daemon is reached at (config.yml
	// hosts.<name>.address), "" for a host it does not know. A host joining a
	// network asks the kernel which of its own addresses reaches the home's.
	HostAddr func(host string) string
	// NetworkHome reports whether a host runs no VMs (config.yml hosts.<name>.vms:
	// false): CreateVM refuses it. nil means no host is one.
	NetworkHome func(host string) bool
}

// WithTunnel returns a shallow copy of s that reaches guests through t. The
// receiver is not changed, so a shared Service can serve concurrent requests,
// each with its own token.
func (s *Service) WithTunnel(t vmtool.Tunnel) *Service {
	c := *s
	c.Tunnel = t
	return &c
}

// WithStore returns a shallow copy of s that uses session st for credentials
// and the image catalog. The receiver is not changed. The loopback service
// gives each request its own session.
func (s *Service) WithStore(st *StoreSession) *Service {
	c := *s
	c.Store = st
	return &c
}

// Credentials is where this command's guest credentials and machine records
// live: the store in store mode, .machines/ otherwise. The mode is decided on
// the first call that needs it.
func (s *Service) Credentials() CredentialSource { return s.Store.Credentials() }

// ImageCatalog is the store's side of base images in store mode; in local
// mode every call answers ErrNoStore.
func (s *Service) ImageCatalog() ImageCatalog { return s.Store.Catalog() }

func (s *Service) playbookDir() string {
	if s.PlaybookDir == "" {
		return filepath.Join("ansible", "playbooks")
	}
	return s.PlaybookDir
}

func (s *Service) ipTimeout() time.Duration {
	if s.IPTimeout <= 0 {
		return 120 * time.Second
	}
	return s.IPTimeout
}

func (s *Service) sshTimeout() time.Duration {
	if s.SSHTimeout <= 0 {
		return 120 * time.Second
	}
	return s.SSHTimeout
}

type ProgressFunc func(ProgressEvent)

type ProgressEvent struct {
	Stage  string
	Status string
	Detail string
	Output string
	// Sent and Total are bytes, for stages "upload", "fetch" and "relay", status "progress".
	Sent, Total int64
	// ElapsedMS is the milliseconds since the operation began, set by
	// CreateVM on every event it emits (API 2.4.0 ProgressEvent.elapsed_ms).
	ElapsedMS int64
}

type CreateVMInput struct {
	// Host is the vmtool host the VM is created on; credentials are kept
	// under .machines/<Host>/<Name>/. The Manager only sees Name.
	Host        string
	Name        string
	Image       string
	VCPUs       uint
	Memory      uint // GiB
	DiskSizeGB  uint
	Pool        string
	NetType     string
	NetSource   string
	MacvtapMode string
	SSHUser     string
	SSHPass     string
	Playbook    string
	Noclone     bool
	// Firmware is "efi" or "bios"; empty is efi (vmtool.DefaultConfig).
	Firmware string
	// DiskCache is "none", "writeback", "writethrough" or "unsafe". Empty:
	// unsafe for a clone, no cache attribute with Noclone.
	DiskCache       string
	ExtraDiskSizeGB uint
	ExtraDiskPool   string
	ExtraVars       map[string]string
	// Networks are extra cards on vmtool networks of Host, after the VM's own
	// first card: each gets a MAC from vmtool and a reservation (the address
	// given, or the next free one) carrying the VM's name, made before the VM
	// starts.
	Networks []NetworkRequest
	// NoWait returns right after start: no address wait, no SSH wait, no
	// filesystem grow. Refused together with Playbook.
	NoWait bool
	// Comment is written to the store with the VM's origin after define (the
	// VM's notes, plain text; nothing goes into the domain): at most 512
	// bytes of UTF-8, one line, no control characters. Empty is none. With
	// no store in use a comment is refused before anything is created.
	Comment    string
	OnProgress ProgressFunc
}

// ExecResult is what exec returns: the remote exit code and captured output.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type PlaybookResult struct {
	Output string
}

type ResizeResult struct {
	VM              vmtool.VMInfo
	FilesystemGrown bool
	GrowError       string
}

type AddDiskResult struct {
	VM     vmtool.VMInfo
	Path   string
	Target string
}

func emit(in CreateVMInput, stage, status, detail, output string) {
	in.OnProgress.emit(stage, status, detail, output)
}

func (f ProgressFunc) emit(stage, status, detail, output string) {
	f.emitEvent(ProgressEvent{Stage: stage, Status: status, Detail: detail, Output: output})
}

func (f ProgressFunc) emitEvent(ev ProgressEvent) {
	if f != nil {
		f(ev)
	}
}

// now reads the service's clock (time.Now when none was set).
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// stamped wraps f so every event carries ElapsedMS, the milliseconds since
// this call. The stamp is this machine's: an event relayed from a daemon
// (the fetch progress mapped to "fetch" events) gets the time on the clock
// that is timing the create, not the daemon's. A nil f stays nil.
func (s *Service) stamped(f ProgressFunc) ProgressFunc {
	if f == nil {
		return nil
	}
	start := s.now()
	return func(ev ProgressEvent) {
		ev.ElapsedMS = s.now().Sub(start).Milliseconds()
		f(ev)
	}
}
