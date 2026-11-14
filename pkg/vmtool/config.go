package vmtool

// NetworkType defines how a VM connects to the network.
type NetworkType string

const (
	// NetworkNAT uses the default libvirt NAT network.
	NetworkNAT NetworkType = "nat"
	// NetworkBridge connects the VM to a host bridge interface.
	NetworkBridge NetworkType = "bridge"
	// NetworkDirect uses macvtap for direct host NIC attachment.
	NetworkDirect NetworkType = "direct"
)

// MacvtapMode defines the macvtap operating mode for direct-attached interfaces.
type MacvtapMode string

const (
	MacvtapBridge      MacvtapMode = "bridge"
	MacvtapVEPA        MacvtapMode = "vepa"
	MacvtapPrivate     MacvtapMode = "private"
	MacvtapPassthrough MacvtapMode = "passthrough"
)

// NetworkConfig describes the VM's network attachment.
type NetworkConfig struct {
	Type        NetworkType
	Source      string      // network name (nat), bridge name (bridge), or host interface (direct)
	MacvtapMode MacvtapMode // only used for NetworkDirect
}

// DiskDevice is a file-backed virtio disk on a domain.
type DiskDevice struct {
	Path   string
	Target string // guest device name, e.g. vda, vdb
}

// VMConfig holds the parameters for defining a VM.
type VMConfig struct {
	Name            string
	VCPUs           uint
	Memory          uint // GiB
	DiskPath        string
	DiskSizeGB      uint   // 0 means use the base image size as-is
	Pool            string // empty is the host's default pool (see DefaultPool, HypervisorPool)
	Network         NetworkConfig
	SSHUser         string
	SSHPass         string
	Noclone         bool
	ExtraDiskSizeGB uint         // 0 means no second disk
	ExtraDiskPool   string       // empty uses Pool
	ExtraDisks      []DiskDevice // extra disks included in domain XML
}

// Storage pools. An empty pool name anywhere (VMConfig.Pool, CloneImage,
// CreateVolume, AddDisk, DeleteImage) means "this host's default pool": the
// libvirt Manager uses DefaultPool; on a hypervisor daemon the host resolver
// (internal/hosts) substitutes HypervisorPool before libvirt sees it.
const (
	// DefaultPool is libvirt's conventional pool, the workstation's default.
	DefaultPool = "default"
	// HypervisorPool is the pool a hypervisor daemon caches images in and
	// puts VM disks in when a request names no pool.
	HypervisorPool = "vmtool"
)

// DefaultConfig returns a VMConfig with sensible defaults.
func DefaultConfig(name, diskPath string) VMConfig {
	return VMConfig{
		Name:     name,
		VCPUs:    2,
		Memory:   2,
		DiskPath: diskPath,
		Network: NetworkConfig{
			Type:   NetworkNAT,
			Source: "default",
		},
		SSHUser: "packer",
		SSHPass: "packer",
	}
}

// VMState represents the running state of a VM.
type VMState string

const (
	StateRunning   VMState = "running"
	StateShutoff   VMState = "shutoff"
	StatePaused    VMState = "paused"
	StateCrashed   VMState = "crashed"
	StateUndefined VMState = "undefined"
	StateUnknown   VMState = "unknown"
)

// VMInfo holds runtime information about a VM.
type VMInfo struct {
	Name      string  `json:"name"`
	State     VMState `json:"state"`
	VCPUs     uint    `json:"vcpus"`
	Memory    uint    `json:"memory"` // GiB
	IP        string  `json:"ip"`
	Autostart bool    `json:"autostart"`
}
