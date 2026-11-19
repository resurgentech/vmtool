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
	// Kind is DiskBackingFile or DiskBackingBlock; empty is resolved by Define
	// from the volume libvirt finds at Path (a path it does not know is a file).
	Kind string
}

// VMConfig holds the parameters for defining a VM.
type VMConfig struct {
	Name            string
	VCPUs           uint
	Memory          uint // GiB
	DiskPath        string
	DiskKind        string // kind of vda, as DiskDevice.Kind; empty is resolved by Define
	DiskSizeGB      uint   // 0 means use the base image size as-is
	Pool            string // empty is the host's default pool (see DefaultPool, HypervisorPool)
	Network         NetworkConfig
	SSHUser         string
	SSHPass         string
	Noclone         bool
	UUID            string       // empty: Define generates a v4 UUID on the defining host
	Firmware        string       // FirmwareEFI or FirmwareBIOS; empty is efi
	DiskCache       string       // see ValidDiskCache; empty writes no cache attribute (the hypervisor's default)
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
		SSHUser:  "packer",
		SSHPass:  "packer",
		Firmware: FirmwareEFI,
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
	// Firmware is efi or bios, read from the domain XML by Info; List leaves it empty.
	Firmware string `json:"firmware,omitempty"`
	// DiskCache is the cache mode on vda's driver, read from the domain XML by
	// Info; empty when the disk names none. List leaves it empty.
	DiskCache string `json:"disk_cache,omitempty"`
	// DiskBacking is file or block for vda, read from the domain XML by Info;
	// empty when unknown. List leaves it empty.
	DiskBacking string `json:"disk_backing,omitempty"`
	// UUID is the libvirt domain UUID, filled by Info; empty when unknown
	// (List, or a daemon that predates the field).
	UUID string `json:"uuid,omitempty"`
}
