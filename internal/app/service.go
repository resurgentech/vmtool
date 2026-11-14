package app

import (
	"path/filepath"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
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
	ListImagesByPool() (map[string][]string, error)
	DeleteImage(name, pool string) error
	ListPools() ([]vmtool.PoolInfo, error)
	CreatePool(name, path string) error
	CloneImage(basePath, newName, targetPool string) (string, error)
	ResizeVolume(volPath string, sizeBytes uint64) error
	Define(cfg vmtool.VMConfig) error
}

type Service struct {
	PlaybookDir   string
	InventoryPath string
	IPTimeout     time.Duration
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
}

// WithTunnel returns a shallow copy of s that reaches guests through t. The
// receiver is not changed, so a shared Service can serve concurrent requests,
// each with its own token.
func (s *Service) WithTunnel(t vmtool.Tunnel) *Service {
	c := *s
	c.Tunnel = t
	return &c
}

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

type ProgressFunc func(ProgressEvent)

type ProgressEvent struct {
	Stage  string
	Status string
	Detail string
	Output string
}

type CreateVMInput struct {
	// Host is the vmtool host the VM is created on; credentials are kept
	// under .machines/<Host>/<Name>/. The Manager only sees Name.
	Host            string
	Name            string
	Image           string
	VCPUs           uint
	Memory          uint // GiB
	DiskSizeGB      uint
	Pool            string
	NetType         string
	NetSource       string
	MacvtapMode     string
	SSHUser         string
	SSHPass         string
	Playbook        string
	Noclone         bool
	ExtraDiskSizeGB uint
	ExtraDiskPool   string
	ExtraVars       map[string]string
	OnProgress      ProgressFunc
}

type CmdResult struct {
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
	if f != nil {
		f(ProgressEvent{Stage: stage, Status: status, Detail: detail, Output: output})
	}
}
