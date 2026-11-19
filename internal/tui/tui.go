package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	promptStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	borderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	logRunning    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	logDone       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	logError      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	logSelected   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
)

type viewMode int

const (
	modeList viewMode = iota
	modeCreate
	modePlaybook
	modeOutput
	modeConfirm
	modeBridge
	modeAddMachine
)

type pane int

const (
	paneVMs pane = iota
	paneLog
)

// fieldKind determines how a form field is edited.
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldSelect
)

type formField struct {
	label    string
	def      string
	kind     fieldKind
	options  []string
	hidden   bool
	password bool
}

const (
	fName = iota
	fImage
	fPool
	fDiskSize
	fExtraDiskSize
	fVCPUs
	fMemory
	fNetType
	fNetSource
	fMacvtapMode
	fSSHUser
	fSSHPass
	fPlaybook
	fNoclone
	fFirmware
	fDiskCache
	fWait
	numCreateFields
)

const (
	bfName = iota
	bfNIC
	bfIPMode
	bfIP
	bfGateway
	bfDNS
	bfSudoPass
	numBridgeFields
)

const (
	mfName = iota
	mfHost
	mfKind
	mfCommission
	numMachineFields
)

// --- Command log ---

type logStatus int

const (
	logStatusRunning logStatus = iota
	logStatusDone
	logStatusError
)

type logEntry struct {
	title  string
	status logStatus
	output string
}

// msgSender is tea.Program.Send, set once the program exists. The model holds
// a pointer so every copy of it sees the same sender.
type msgSender struct{ fn func(tea.Msg) }

func (s *msgSender) Send(msg tea.Msg) {
	if s != nil && s.fn != nil {
		s.fn(msg)
	}
}

// progressMsg retitles a running log entry (create's upload progress).
type progressMsg struct {
	logIdx int
	title  string
}

// uploadTitle is a log title for an upload progress event, or "" when ev is
// not one.
func uploadTitle(name string, ev app.ProgressEvent) string {
	if ev.Stage != "upload" {
		return ""
	}
	switch ev.Status {
	case "hash":
		return fmt.Sprintf("creating %s: hashing image...", name)
	case "progress":
		pct := int64(100)
		if ev.Total > 0 {
			pct = ev.Sent * 100 / ev.Total
		}
		return fmt.Sprintf("creating %s: uploading image %d%% (%d / %d MiB)", name, pct, ev.Sent>>20, ev.Total>>20)
	}
	return ""
}

// --- Model ---

type model struct {
	// manager, when set, is the local host's Manager (tests). Run leaves it
	// nil: the local host is resolved through resolve when an action needs
	// it, so nothing opens libvirt at start, and a listed own name goes to
	// its daemon.
	manager app.Manager
	// ownListed is whether the own name has a daemon (no local pools).
	ownListed bool
	// store is the run's session with the central store (nil: none).
	store *app.StoreSession
	// imageDir is the config's resolved image_dir ("" when unset), where
	// create looks for base images after the local pools.
	imageDir string
	// resolve returns the Manager for a host and its release func; every VM
	// action resolves the VM's host through it.
	resolve func(host string) (app.Manager, func(), error)
	// host is the local vmtool host; rows name VMs host/vm.
	host string
	// hostNames are every vmtool host (app.Service.HostNames).
	hostNames []string
	// listAll lists every host's VMs concurrently with a per-host timeout
	// (app.ListAll over the resolver); nil falls back to the local manager
	// alone.
	listAll func(ctx context.Context) []app.HostVMs
	// vms are the VM rows of every host, and an error row for a host that
	// did not answer.
	vms []vmRow
	// machines (metal/alien) are listed after vms; cursor spans both.
	machines   []app.MachineInfo
	machineErr error
	cursor     int
	err        error

	mode       viewMode
	focus      pane
	formField  int
	formFields []formField
	formValues []string

	playbooks   []string
	playbookIdx int

	// command log
	log       []logEntry
	logCursor int

	// confirm dialog
	confirmMsg    string
	confirmAction func() (tea.Model, tea.Cmd)

	// output modal
	outputTitle  string
	outputLines  []string
	outputScroll int

	termHeight int
	termWidth  int

	playbookDir   string
	inventoryPath string
	// ipTimeout and sshTimeout are create's waits from config.yml; zero
	// means the app's default.
	ipTimeout, sshTimeout time.Duration
	// tunnel is how guest SSH and ansible reach VMs (see app.Service.Tunnel);
	// the CLI passes it, with the verified token, when it starts the TUI.
	tunnel vmtool.Tunnel
	// send delivers a message to the running program from a command's
	// goroutine (create's upload progress); nil outside Run.
	send *msgSender

	// cached for dynamic net source options
	bridges  []string
	networks []string
	nics     []string

	// bridge creation form
	bridgeField  int
	bridgeFields []formField
	bridgeValues []string

	// add machine form
	machineField  int
	machineFields []formField
	machineValues []string
}

// vmRow is one VM row: the VM and the host it lives on. A host that did not
// answer is a row with Err set and no VM.
type vmRow struct {
	vmtool.VMInfo
	Host string
	Err  error
}

// fetchRows lists every host's VMs: through listAll when set, else the local
// manager alone. It never returns nil, so a result always replaces the rows.
func (m model) fetchRows() []vmRow {
	var hv []app.HostVMs
	switch {
	case m.listAll != nil:
		hv = m.listAll(context.Background())
	default:
		mgr, release, err := m.local()
		if err != nil {
			hv = []app.HostVMs{{Host: m.host, Err: err}}
			break
		}
		defer release()
		vms, err := mgr.List()
		hv = []app.HostVMs{{Host: m.host, VMs: vms, Err: err}}
	}
	rows := []vmRow{}
	for _, h := range hv {
		if h.Err != nil {
			rows = append(rows, vmRow{Host: h.Host, Err: h.Err})
			continue
		}
		for _, vm := range h.VMs {
			rows = append(rows, vmRow{VMInfo: vm, Host: h.Host})
		}
	}
	return rows
}

type refreshMsg struct {
	vms        []vmRow
	err        error
	machines   []app.MachineInfo
	machineErr error
}

type cmdDoneMsg struct {
	logIdx int
	title  string
	status logStatus
	output string
	vms    []vmRow
	err    error
	// machines, if non-nil, replaces the machine rows.
	machines []app.MachineInfo
}

type sshDoneMsg struct {
	name string
	err  error
}

type bridgeCreatedMsg struct {
	logIdx  int
	name    string
	bridges []string
}

// refreshCmd lists every host's VMs (the fan-out, 5 s per host) and probes
// machines concurrently. It is a tea.Cmd, so the update loop never waits on
// it; a dead host costs its timeout here, as an error row, not the other
// hosts' rows.
func (m model) refreshCmd() tea.Cmd {
	svc := m.svc()
	return func() tea.Msg {
		var msg refreshMsg
		done := make(chan struct{})
		go func() {
			msg.machines, msg.machineErr = svc.ListMachines(context.Background())
			close(done)
		}()
		msg.vms = m.fetchRows()
		<-done
		return msg
	}
}

func (m model) rowCount() int {
	return len(m.vms) + len(m.machines)
}

// selectedVM is the VM row under the cursor; an error row is not a VM.
func (m model) selectedVM() (vmRow, bool) {
	if m.cursor < len(m.vms) && m.vms[m.cursor].Err == nil {
		return m.vms[m.cursor], true
	}
	return vmRow{}, false
}

func (m model) selectedMachine() (app.MachineInfo, bool) {
	i := m.cursor - len(m.vms)
	if i >= 0 && i < len(m.machines) {
		return m.machines[i], true
	}
	return app.MachineInfo{}, false
}

func (m *model) clampCursor() {
	if n := m.rowCount(); m.cursor >= n && n > 0 {
		m.cursor = n - 1
	}
}

func (m *model) addLog(title string) int {
	m.log = append(m.log, logEntry{title: title, status: logStatusRunning})
	return len(m.log) - 1
}

func (m model) svc() *app.Service {
	s := &app.Service{PlaybookDir: m.playbookDir, InventoryPath: m.inventoryPath, HostNames: m.hostNames,
		IPTimeout: m.ipTimeout, SSHTimeout: m.sshTimeout}
	// create on a remote host uploads the image from this host's pools.
	// The TUI is a workstation tool: a miss refreshes the pools once. With
	// the own name listed there are no local pools; with no libvirt either
	// the answer is the same: not found, so the host's cached copy is used.
	pools := func(name string) (string, error) {
		if m.ownListed {
			return "", fmt.Errorf("image %q: %s has a daemon, so it has no local pools: %w", name, m.host, vmtool.ErrNotFound)
		}
		mgr, release, err := m.local()
		if err != nil {
			return "", fmt.Errorf("image %q: %v: %w", name, err, vmtool.ErrNotFound)
		}
		defer release()
		return app.FindLocalImage(mgr, true, name)
	}
	s.Images = app.LocalImages{Dir: m.imageDir, Pools: pools}
	s.Store = m.store
	return s.WithTunnel(m.tunnel)
}

// vmRef names vm on the host its row came from (this TUI's host when the row
// carries none).
func (m model) vmRef(vm vmRow) app.VMRef {
	host := vm.Host
	if host == "" {
		host = m.host
	}
	return app.VMRef{Host: host, Name: vm.Name}
}

// local is the local host's Manager and its release func: the fixed manager
// when one was set, else resolved now through the resolver (the daemon when
// the own name is listed, libvirt otherwise). Nothing is opened until asked.
func (m model) local() (app.Manager, func(), error) {
	if m.manager != nil {
		return m.manager, func() {}, nil
	}
	if m.resolve == nil {
		return nil, nil, fmt.Errorf("host %q: no host resolver", m.host)
	}
	return m.resolve(m.host)
}

// onHost runs fn on the Manager of host, resolved for this one action and
// released after it: local libvirt or the host's remote client.
func (m model) onHost(host string, fn func(app.Manager) error) error {
	if m.resolve == nil {
		return fmt.Errorf("host %q: no host resolver", host)
	}
	mgr, release, err := m.resolve(host)
	if err != nil {
		return err
	}
	defer release()
	return fn(mgr)
}

// runLifecycle runs fn on ref's host, then refreshes the rows from the local
// host.
func (m model) runLifecycle(op string, ref app.VMRef, fn func(*app.Service, app.Manager) error) (tea.Model, tea.Cmd) {
	name := ref.String()
	idx := m.addLog(fmt.Sprintf("%s %s...", op, name))
	svc := m.svc()
	return m, func() tea.Msg {
		err := m.onHost(ref.Host, func(target app.Manager) error { return fn(svc, target) })
		title := fmt.Sprintf("%s %s", op, name)
		status := logStatusDone
		if err != nil {
			title = fmt.Sprintf("%s %s: error: %v", op, name, err)
			status = logStatusError
		}
		return cmdDoneMsg{logIdx: idx, title: title, status: status, vms: m.fetchRows()}
	}
}

func initialModel(mgr app.Manager, host, playbookDir, inventoryPath string) model {
	return model{
		manager:       mgr,
		host:          host,
		hostNames:     []string{host},
		playbookDir:   playbookDir,
		inventoryPath: inventoryPath,
	}
}

func (m model) Init() tea.Cmd {
	return m.refreshCmd()
}

// --- Update dispatch ---

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termHeight = msg.Height
		m.termWidth = msg.Width
		return m, nil

	case refreshMsg:
		m.vms = msg.vms
		m.err = msg.err
		m.machines = msg.machines
		m.machineErr = msg.machineErr
		m.clampCursor()
		return m, nil

	case sshDoneMsg:
		if msg.err != nil {
			idx := m.addLog(fmt.Sprintf("ssh %s: %v", msg.name, msg.err))
			m.log[idx].status = logStatusError
		}
		return m, m.refreshCmd()

	case progressMsg:
		if msg.logIdx >= 0 && msg.logIdx < len(m.log) && m.log[msg.logIdx].status == logStatusRunning {
			m.log[msg.logIdx].title = msg.title
		}
		return m, nil

	case cmdDoneMsg:
		if msg.logIdx >= 0 && msg.logIdx < len(m.log) {
			m.log[msg.logIdx].status = msg.status
			m.log[msg.logIdx].title = msg.title
			m.log[msg.logIdx].output = msg.output
		}
		if msg.vms != nil {
			m.vms = msg.vms
			m.err = msg.err
		}
		if msg.machines != nil {
			m.machines = msg.machines
			m.machineErr = nil
		}
		m.clampCursor()
		return m, nil

	case bridgeCreatedMsg:
		if msg.logIdx >= 0 && msg.logIdx < len(m.log) {
			m.log[msg.logIdx].status = logStatusDone
			m.log[msg.logIdx].title = fmt.Sprintf("bridge %s created", msg.name)
		}
		m.bridges = msg.bridges
		// Update net source options if we're in bridge mode
		if m.formValues != nil && len(m.formValues) > fNetType && m.formValues[fNetType] == "bridge" {
			opts := append([]string{}, m.bridges...)
			opts = append(opts, "New Bridge...")
			m.formFields[fNetSource] = formField{label: "Bridge", kind: fieldSelect, options: opts}
			m.formValues[fNetSource] = msg.name
		}
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeConfirm:
			return m.updateConfirm(msg)
		case modeOutput:
			return m.updateOutput(msg)
		case modeCreate:
			return m.updateCreate(msg)
		case modeBridge:
			return m.updateBridge(msg)
		case modePlaybook:
			return m.updatePlaybook(msg)
		case modeAddMachine:
			return m.updateAddMachine(msg)
		default:
			return m.updateList(msg)
		}
	}
	return m, nil
}

// --- List mode ---

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		if m.focus == paneVMs {
			m.focus = paneLog
		} else {
			m.focus = paneVMs
		}
	case "up", "k":
		if m.focus == paneVMs {
			if m.cursor > 0 {
				m.cursor--
			}
		} else {
			if m.logCursor > 0 {
				m.logCursor--
			}
		}
	case "down", "j":
		if m.focus == paneVMs {
			if m.cursor < m.rowCount()-1 {
				m.cursor++
			}
		} else {
			if m.logCursor < len(m.log)-1 {
				m.logCursor++
			}
		}
	case "enter":
		if m.focus == paneLog && len(m.log) > 0 {
			entry := m.log[m.logCursor]
			if entry.output != "" {
				m.outputTitle = entry.title
				m.outputLines = strings.Split(strings.TrimRight(entry.output, "\n"), "\n")
				m.outputScroll = 0
				m.mode = modeOutput
			}
		}
	case "t":
		if m.focus == paneVMs && m.rowCount() > 0 {
			return m.sshSelected()
		}
	case "r":
		return m, m.refreshCmd()
	case "c":
		if m.focus == paneVMs {
			return m.enterCreateMode()
		}
	case "m":
		if m.focus == paneVMs {
			return m.enterAddMachineMode()
		}
	case "b":
		if m.focus == paneVMs {
			return m.confirmReboot()
		}
	case "p":
		if m.focus == paneVMs {
			return m.enterPlaybookMode()
		}
	case "s", "S":
		if vm, ok := m.selectedVM(); ok && m.focus == paneVMs {
			switch vm.State {
			case vmtool.StateShutoff:
				return m.runLifecycle("start", m.vmRef(vm), func(svc *app.Service, mgr app.Manager) error {
					_, err := svc.StartVM(context.Background(), mgr, vm.Name)
					return err
				})
			case vmtool.StateRunning:
				vmName := vm.Name
				m.confirmMsg = fmt.Sprintf("Shut down %q?", vmName)
				m.confirmAction = func() (tea.Model, tea.Cmd) {
					m.mode = modeList
					return m.runLifecycle("stop", m.vmRef(vm), func(svc *app.Service, mgr app.Manager) error {
						_, err := svc.StopVM(context.Background(), mgr, vmName)
						return err
					})
				}
				m.mode = modeConfirm
			}
		}
	case "a":
		if vm, ok := m.selectedVM(); ok && m.focus == paneVMs {
			newState := !vm.Autostart
			label := "enable"
			if !newState {
				label = "disable"
			}
			return m.runLifecycle("autostart", m.vmRef(vm), func(svc *app.Service, mgr app.Manager) error {
				_, err := svc.SetAutostart(context.Background(), mgr, vm.Name, newState)
				if err == nil {
					_ = label
				}
				return err
			})
		}
	case "D":
		if mi, ok := m.selectedMachine(); ok && m.focus == paneVMs {
			name := mi.Name
			m.confirmMsg = fmt.Sprintf("Forget %s %q? Removes .machines/%s; the machine itself is untouched.", mi.Kind, name, name)
			m.confirmAction = func() (tea.Model, tea.Cmd) {
				m.mode = modeList
				return m.runMachineOp("forget", name, func(svc *app.Service) error {
					return svc.RemoveMachine(context.Background(), name)
				})
			}
			m.mode = modeConfirm
		}
		if vm, ok := m.selectedVM(); ok && m.focus == paneVMs {
			ref := m.vmRef(vm)
			m.confirmMsg = fmt.Sprintf("Delete %q? This will destroy the VM and remove its disk.", ref.String())
			m.confirmAction = func() (tea.Model, tea.Cmd) {
				m.mode = modeList
				return m.runLifecycle("delete", ref, func(svc *app.Service, mgr app.Manager) error {
					return svc.DeleteVM(context.Background(), mgr, ref, false)
				})
			}
			m.mode = modeConfirm
		}
	}
	return m, nil
}

func (m model) sshSelected() (tea.Model, tea.Cmd) {
	if mi, ok := m.selectedMachine(); ok {
		cmd, err := m.svc().MachineSSHCmd(mi.Name)
		if err != nil {
			idx := m.addLog(fmt.Sprintf("ssh %s: %v", mi.Name, err))
			m.log[idx].status = logStatusError
			return m, nil
		}
		name := mi.Name
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
			return sshDoneMsg{name: name, err: err}
		})
	}
	vm, _ := m.selectedVM()
	ref := m.vmRef(vm)
	if vm.State != vmtool.StateRunning || vm.IP == "" {
		idx := m.addLog(fmt.Sprintf("ssh %s: no IP", ref))
		m.log[idx].status = logStatusError
		return m, nil
	}
	cmd, err := m.svc().SSHCmd(ref)
	if err != nil {
		idx := m.addLog(fmt.Sprintf("ssh %s: %v", ref, err))
		m.log[idx].status = logStatusError
		return m, nil
	}
	name := ref.String()
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return sshDoneMsg{name: name, err: err}
	})
}

// --- Confirm dialog ---

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		return m.confirmAction()
	case "n", "N", "esc":
		m.mode = modeList
	}
	return m, nil
}

// --- Create mode ---

// hostDefaultPool is the create form's first Pool option: no pool, so the
// host's default applies: its default_pool in the workstation's config.yml
// when set, else the default of the host that owns libvirt (default on a
// workstation, vmtool on a hypervisor).
const hostDefaultPool = "(host default)"

// formPool is the pool the create form's Pool value names: empty for
// hostDefaultPool.
func formPool(v string) string {
	v = strings.TrimSpace(v)
	if v == hostDefaultPool {
		return ""
	}
	if m := poolOptionRE.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return v
}

// poolOption is a Pool picker entry: the name, then the pool's type in
// parentheses when known ("nvme-thin (thin)"). formPool strips it again.
func poolOption(p vmtool.PoolInfo) string {
	if p.Type == "" {
		return p.Name
	}
	return p.Name + " (" + p.Type + ")"
}

var poolOptionRE = regexp.MustCompile(`^(.*\S) \([a-z]+\)$`)

func (m model) enterCreateMode() (tea.Model, tea.Cmd) {
	// The pickers come from the local host's Manager, resolved now: the
	// daemon when the own name is listed, else libvirt. When it cannot be
	// reached the form still opens, with empty pickers, and the log says why.
	var images, networks, bridges []string
	var poolInfos []vmtool.PoolInfo
	if mgr, release, err := m.local(); err != nil {
		m.addLog(fmt.Sprintf("%s: %v", m.host, err))
	} else {
		images = flatImages(mgr)
		networks, _ = mgr.ListNetworks()
		bridges, _ = mgr.ListBridges()
		// The named pools are this host's.
		poolInfos, _ = mgr.ListPools()
		release()
	}
	playbooks, _ := vmtool.ListPlaybooks(m.playbookDir)

	pbOptions := []string{"(none)"}
	pbOptions = append(pbOptions, playbooks...)

	if len(networks) == 0 {
		networks = []string{"default"}
	}
	nics, _ := vmtool.ListPhysicalNICs()

	// The first option, selected by default, sends no pool: the target
	// host picks its own default.
	pools := make([]string, 0, len(poolInfos)+1)
	pools = append(pools, hostDefaultPool)
	for _, p := range poolInfos {
		pools = append(pools, poolOption(p))
	}

	fields := []formField{
		{label: "Name", kind: fieldText},
		{label: "Image", kind: fieldSelect, options: images},
		{label: "Pool", kind: fieldSelect, options: pools},
		{label: "Disk size (GB)", def: "default", kind: fieldText},
		{label: "Extra disk (GB)", def: "none", kind: fieldText},
		{label: "VCPUs", def: "2", kind: fieldText},
		{label: "Memory (GiB)", def: "2", kind: fieldText},
		{label: "Net type", kind: fieldSelect, options: []string{"nat", "bridge", "direct"}},
		{label: "Net source", kind: fieldSelect, options: networks},
		{label: "Macvtap mode", kind: fieldSelect, options: []string{"bridge", "vepa", "private", "passthrough"}, hidden: true},
		{label: "SSH user", def: "packer", kind: fieldText},
		{label: "SSH pass", def: "packer", kind: fieldText},
		{label: "Playbook", kind: fieldSelect, options: pbOptions},
		{label: "Noclone", kind: fieldSelect, options: []string{"false", "true"}},
		{label: "Firmware", kind: fieldSelect, options: []string{"efi", "bios"}},
		// default: for a clone none in a logical pool, unsafe in any other
		// pool; none named for --noclone (resolved in app).
		// unsafe can corrupt the disk on a host crash or power loss.
		{label: "Disk cache", kind: fieldSelect, options: []string{"default", "none", "writeback", "writethrough", "unsafe"}},
		// no: create returns right after start (NoWait); a guest whose address
		// vmtool cannot see. Refused with a playbook.
		{label: "Wait for address", kind: fieldSelect, options: []string{"yes", "no"}},
	}

	values := make([]string, len(fields))
	for i, f := range fields {
		if f.kind == fieldSelect && len(f.options) > 0 {
			values[i] = f.options[0]
		} else {
			values[i] = f.def
		}
	}
	// Store lists for dynamic switching
	m.bridges = bridges
	m.networks = networks
	m.nics = nics

	m.mode = modeCreate
	m.formField = 0
	m.formFields = fields
	m.formValues = values
	return m, nil
}

func (m *model) nextVisibleField(from, dir int) int {
	i := from + dir
	for i >= 0 && i < len(m.formFields) {
		if !m.formFields[i].hidden {
			return i
		}
		i += dir
	}
	return from
}

func (m model) updateCreate(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	f := m.formFields[m.formField]

	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = modeList
		return m, nil
	case "tab", "down":
		m.formField = m.nextVisibleField(m.formField, 1)
	case "shift+tab", "up":
		m.formField = m.nextVisibleField(m.formField, -1)
	case "left":
		if f.kind == fieldSelect {
			m.cycleSelect(-1)
		}
	case "right":
		if f.kind == fieldSelect {
			m.cycleSelect(1)
		}
	case "backspace":
		if f.kind == fieldText {
			v := m.formValues[m.formField]
			if len(v) > 0 {
				m.formValues[m.formField] = v[:len(v)-1]
			}
		}
	case "enter":
		// "New Bridge..." opens bridge creation form
		if m.formField == fNetSource && m.formValues[fNetSource] == "New Bridge..." {
			return m.enterBridgeMode()
		}
		next := m.nextVisibleField(m.formField, 1)
		if next != m.formField {
			m.formField = next
			return m, nil
		}
		return m.submitCreate()
	default:
		if f.kind == fieldText && len(key) == 1 {
			m.formValues[m.formField] += key
		}
	}
	return m, nil
}

func (m *model) cycleSelect(dir int) {
	f := m.formFields[m.formField]
	if len(f.options) == 0 {
		return
	}
	cur := 0
	for i, o := range f.options {
		if o == m.formValues[m.formField] {
			cur = i
			break
		}
	}
	cur += dir
	if cur < 0 {
		cur = len(f.options) - 1
	} else if cur >= len(f.options) {
		cur = 0
	}
	m.formValues[m.formField] = f.options[cur]

	// When net type changes, update net source options
	if m.formField == fNetType {
		m.updateNetSourceOptions()
	}
}

// updateNetSourceOptions switches the Net source field options based on the
// currently selected Net type.
func (m *model) updateNetSourceOptions() {
	netType := m.formValues[fNetType]
	switch vmtool.NetworkType(netType) {
	case vmtool.NetworkBridge:
		opts := append([]string{}, m.bridges...)
		opts = append(opts, "New Bridge...")
		m.formFields[fNetSource] = formField{label: "Bridge", kind: fieldSelect, options: opts}
		m.formValues[fNetSource] = opts[0]
		m.formFields[fMacvtapMode].hidden = true
	case vmtool.NetworkDirect:
		if len(m.nics) > 0 {
			m.formFields[fNetSource] = formField{label: "Host NIC", kind: fieldSelect, options: m.nics}
			m.formValues[fNetSource] = m.nics[0]
		} else {
			m.formFields[fNetSource] = formField{label: "Host NIC", kind: fieldText}
			m.formValues[fNetSource] = ""
		}
		m.formFields[fMacvtapMode].hidden = false
	default: // nat
		m.formFields[fNetSource] = formField{label: "Net source", kind: fieldSelect, options: m.networks}
		if len(m.networks) > 0 {
			m.formValues[fNetSource] = m.networks[0]
		}
		m.formFields[fMacvtapMode].hidden = true
	}
}

func (m model) submitCreate() (tea.Model, tea.Cmd) {
	// Redirect to bridge creation if "New Bridge..." is still selected
	if m.formValues[fNetSource] == "New Bridge..." {
		return m.enterBridgeMode()
	}

	in, ok, err := m.createInput()
	if err != nil {
		idx := m.addLog(fmt.Sprintf("create: %v", err))
		m.log[idx].status = logStatusError
		m.mode = modeList
		return m, nil
	}
	if !ok {
		return m, nil
	}
	name := app.VMRef{Host: in.Host, Name: in.Name}.String()

	logIdx := m.addLog(fmt.Sprintf("creating %s...", name))
	m.mode = modeList
	svc := m.svc()
	return m, func() tea.Msg {
		var buf strings.Builder
		in.OnProgress = func(ev app.ProgressEvent) {
			if t := uploadTitle(name, ev); t != "" {
				m.send.Send(progressMsg{logIdx: logIdx, title: t})
				return
			}
			if ev.Output != "" {
				buf.WriteString(ev.Output)
			}
			if ev.Detail != "" && (ev.Status == "done" || ev.Status == "error") {
				buf.WriteString(ev.Detail)
				buf.WriteByte('\n')
			}
		}
		var info *vmtool.VMInfo
		err := m.onHost(in.Host, func(target app.Manager) error {
			var err error
			info, err = svc.CreateVM(context.Background(), target, in)
			return err
		})
		title := fmt.Sprintf("%s: created", name)
		status := logStatusDone
		if info != nil && info.IP != "" {
			title = fmt.Sprintf("%s: IP %s", name, info.IP)
		}
		if err != nil {
			title = fmt.Sprintf("%s: error: %v", name, err)
			status = logStatusError
			var ae *app.Error
			if app.AsError(err, &ae) && ae.Output != "" {
				buf.WriteString(ae.Output)
			}
		}
		return cmdDoneMsg{logIdx: logIdx, title: title, status: status, output: buf.String(), vms: m.fetchRows()}
	}
}

// createInput is the CreateVMInput the create form describes. ok is false,
// with no error, while the form is incomplete or a size is not a number
// (submit then does nothing); err is a host/name that does not parse.
func (m model) createInput() (in app.CreateVMInput, ok bool, err error) {
	name := strings.TrimSpace(m.formValues[fName])
	image := m.formValues[fImage]
	if name == "" || image == "" {
		return in, false, nil
	}

	var diskSizeGB uint
	diskSizeStr := strings.TrimSpace(m.formValues[fDiskSize])
	if diskSizeStr != "" && diskSizeStr != "default" {
		ds, err := strconv.ParseUint(diskSizeStr, 10, 32)
		if err != nil || ds == 0 {
			return in, false, nil
		}
		diskSizeGB = uint(ds)
	}

	var extraDiskSizeGB uint
	extraDiskStr := strings.TrimSpace(m.formValues[fExtraDiskSize])
	if extraDiskStr != "" && extraDiskStr != "none" && extraDiskStr != "0" {
		eds, err := strconv.ParseUint(extraDiskStr, 10, 32)
		if err != nil || eds == 0 {
			return in, false, nil
		}
		extraDiskSizeGB = uint(eds)
	}

	vcpus, _ := strconv.ParseUint(strings.TrimSpace(m.formValues[fVCPUs]), 10, 32)
	mem, _ := strconv.ParseUint(strings.TrimSpace(m.formValues[fMemory]), 10, 32)
	playbookName := m.formValues[fPlaybook]
	if playbookName == "(none)" {
		playbookName = ""
	}

	// The form takes a bare name (created on this host) or host/name for
	// any host the resolver serves. The form's pickers list this host's
	// images, networks and pools; on another host the image is uploaded
	// from this host's pool unless that host already has the same bytes.
	ref := app.VMRef{Host: m.host, Name: name}
	if strings.Contains(name, "/") {
		r, err := app.ParseVMRef(name, m.host)
		if err != nil {
			return in, false, err
		}
		ref = r
	}

	in = app.CreateVMInput{
		Host:            ref.Host,
		Name:            ref.Name,
		Image:           image,
		VCPUs:           uint(vcpus),
		Memory:          uint(mem),
		DiskSizeGB:      diskSizeGB,
		Pool:            formPool(m.formValues[fPool]),
		NetType:         m.formValues[fNetType],
		NetSource:       strings.TrimSpace(m.formValues[fNetSource]),
		MacvtapMode:     m.formValues[fMacvtapMode],
		SSHUser:         strings.TrimSpace(m.formValues[fSSHUser]),
		SSHPass:         strings.TrimSpace(m.formValues[fSSHPass]),
		Playbook:        playbookName,
		Noclone:         m.formValues[fNoclone] == "true",
		Firmware:        m.formValues[fFirmware],
		DiskCache:       diskCacheChoice(m.formValues[fDiskCache]),
		NoWait:          m.formValues[fWait] == "no",
		ExtraDiskSizeGB: extraDiskSizeGB,
	}

	return in, true, nil
}

// diskCacheChoice maps the form's "default" to "" (the app resolves it).
func diskCacheChoice(v string) string {
	if v == "default" {
		return ""
	}
	return v
}

// --- Bridge creation mode ---

func (m model) enterBridgeMode() (tea.Model, tea.Cmd) {
	nics := m.nics
	if len(nics) == 0 {
		return m, nil
	}

	nicInfo := vmtool.GetNICInfo(nics[0])

	fields := []formField{
		{label: "Bridge name", def: "br0", kind: fieldText},
		{label: "Host NIC", kind: fieldSelect, options: nics},
		{label: "IP mode", kind: fieldSelect, options: []string{"dhcp", "static"}},
		{label: "IP/CIDR", kind: fieldText},
		{label: "Gateway", kind: fieldText},
		{label: "DNS", kind: fieldText},
		{label: "Sudo password", kind: fieldText, password: true},
	}

	values := make([]string, numBridgeFields)
	values[bfName] = "br0"
	values[bfNIC] = nics[0]
	if nicInfo.IP != "" {
		values[bfIPMode] = "static"
		values[bfIP] = nicInfo.IP
		values[bfGateway] = nicInfo.Gateway
		values[bfDNS] = nicInfo.DNS
	} else {
		values[bfIPMode] = "dhcp"
	}

	m.mode = modeBridge
	m.bridgeField = 0
	m.bridgeFields = fields
	m.bridgeValues = values
	m.updateBridgeFieldVisibility()
	return m, nil
}

func (m *model) updateBridgeFieldVisibility() {
	isStatic := m.bridgeValues[bfIPMode] == "static"
	m.bridgeFields[bfIP].hidden = !isStatic
	m.bridgeFields[bfGateway].hidden = !isStatic
	m.bridgeFields[bfDNS].hidden = !isStatic
}

func (m *model) nextVisibleBridgeField(from, dir int) int {
	i := from + dir
	for i >= 0 && i < len(m.bridgeFields) {
		if !m.bridgeFields[i].hidden {
			return i
		}
		i += dir
	}
	return from
}

func (m model) updateBridge(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	f := m.bridgeFields[m.bridgeField]

	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = modeCreate
		return m, nil
	case "tab", "down":
		m.bridgeField = m.nextVisibleBridgeField(m.bridgeField, 1)
	case "shift+tab", "up":
		m.bridgeField = m.nextVisibleBridgeField(m.bridgeField, -1)
	case "left":
		if f.kind == fieldSelect {
			m.cycleBridgeSelect(-1)
		}
	case "right":
		if f.kind == fieldSelect {
			m.cycleBridgeSelect(1)
		}
	case "backspace":
		if f.kind == fieldText {
			v := m.bridgeValues[m.bridgeField]
			if len(v) > 0 {
				m.bridgeValues[m.bridgeField] = v[:len(v)-1]
			}
		}
	case "enter":
		next := m.nextVisibleBridgeField(m.bridgeField, 1)
		if next != m.bridgeField {
			m.bridgeField = next
			return m, nil
		}
		return m.submitBridge()
	default:
		if f.kind == fieldText && len(key) == 1 {
			m.bridgeValues[m.bridgeField] += key
		}
	}
	return m, nil
}

func (m *model) cycleBridgeSelect(dir int) {
	f := m.bridgeFields[m.bridgeField]
	if len(f.options) == 0 {
		return
	}
	cur := 0
	for i, o := range f.options {
		if o == m.bridgeValues[m.bridgeField] {
			cur = i
			break
		}
	}
	cur += dir
	if cur < 0 {
		cur = len(f.options) - 1
	} else if cur >= len(f.options) {
		cur = 0
	}
	m.bridgeValues[m.bridgeField] = f.options[cur]

	// When NIC changes, update IP defaults
	if m.bridgeField == bfNIC {
		nicInfo := vmtool.GetNICInfo(m.bridgeValues[bfNIC])
		if nicInfo.IP != "" {
			m.bridgeValues[bfIPMode] = "static"
			m.bridgeValues[bfIP] = nicInfo.IP
			m.bridgeValues[bfGateway] = nicInfo.Gateway
			m.bridgeValues[bfDNS] = nicInfo.DNS
		} else {
			m.bridgeValues[bfIPMode] = "dhcp"
			m.bridgeValues[bfIP] = ""
			m.bridgeValues[bfGateway] = ""
			m.bridgeValues[bfDNS] = ""
		}
		m.updateBridgeFieldVisibility()
	}
	if m.bridgeField == bfIPMode {
		m.updateBridgeFieldVisibility()
	}
}

func (m model) submitBridge() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.bridgeValues[bfName])
	nic := m.bridgeValues[bfNIC]
	if name == "" || nic == "" {
		return m, nil
	}

	sudoPass := m.bridgeValues[bfSudoPass]
	if sudoPass == "" {
		return m, nil
	}

	cfg := vmtool.BridgeConfig{
		Name:     name,
		NIC:      nic,
		DHCP:     m.bridgeValues[bfIPMode] == "dhcp",
		IP:       strings.TrimSpace(m.bridgeValues[bfIP]),
		Gateway:  strings.TrimSpace(m.bridgeValues[bfGateway]),
		DNS:      strings.TrimSpace(m.bridgeValues[bfDNS]),
		SudoPass: sudoPass,
	}

	m.confirmMsg = fmt.Sprintf("Create bridge %q on %s? This will disrupt networking on %s.", name, nic, nic)
	m.confirmAction = func() (tea.Model, tea.Cmd) {
		brCfg := cfg
		logIdx := m.addLog(fmt.Sprintf("creating bridge %s on %s...", brCfg.Name, brCfg.NIC))
		m.mode = modeList
		return m, func() tea.Msg {
			err := vmtool.CreateBridge(brCfg)
			if err != nil {
				return cmdDoneMsg{
					logIdx: logIdx,
					title:  fmt.Sprintf("bridge %s: error: %v", brCfg.Name, err),
					status: logStatusError,
				}
			}
			// Refresh bridge list after creation
			var newBridges []string
			if mgr, release, err := m.local(); err == nil {
				newBridges, _ = mgr.ListBridges()
				release()
			}
			return bridgeCreatedMsg{
				logIdx:  logIdx,
				name:    brCfg.Name,
				bridges: newBridges,
			}
		}
	}
	m.mode = modeConfirm
	return m, nil
}

// --- Playbook mode ---

func (m model) enterPlaybookMode() (tea.Model, tea.Cmd) {
	mi, isMachine := m.selectedMachine()
	if isMachine {
		if mi.State != app.MachineReachable {
			return m, nil
		}
	} else if vm, ok := m.selectedVM(); !ok || vm.State != vmtool.StateRunning || vm.IP == "" {
		return m, nil
	}

	playbooks, err := vmtool.ListPlaybooks(m.playbookDir)
	if err != nil {
		return m, nil
	}
	if isMachine {
		// The service refuses commission.yml on machines; don't offer it.
		kept := playbooks[:0]
		for _, pb := range playbooks {
			if pb != "commission.yml" {
				kept = append(kept, pb)
			}
		}
		playbooks = kept
	}
	if len(playbooks) == 0 {
		return m, nil
	}

	m.mode = modePlaybook
	m.playbooks = playbooks
	m.playbookIdx = 0
	return m, nil
}

func (m model) updatePlaybook(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = modeList
		return m, nil
	case "up", "k":
		if m.playbookIdx > 0 {
			m.playbookIdx--
		}
	case "down", "j":
		if m.playbookIdx < len(m.playbooks)-1 {
			m.playbookIdx++
		}
	case "enter":
		return m.runPlaybook()
	}
	return m, nil
}

func (m model) runPlaybook() (tea.Model, tea.Cmd) {
	pb := m.playbooks[m.playbookIdx]
	if mi, ok := m.selectedMachine(); ok {
		name := mi.Name
		m.mode = modeList
		return m.runMachineOutput(pb+" on "+name, func(svc *app.Service) (string, error) {
			res, err := svc.RunMachinePlaybook(context.Background(), name, pb, nil)
			if res == nil {
				return "", err
			}
			return res.Output, err
		})
	}
	vm, _ := m.selectedVM()
	ref := m.vmRef(vm)

	logIdx := m.addLog(fmt.Sprintf("running %s on %s...", pb, ref))
	m.mode = modeList

	vmName := ref.String()
	svc := m.svc()
	return m, func() tea.Msg {
		var res *app.PlaybookResult
		err := m.onHost(ref.Host, func(target app.Manager) error {
			var err error
			res, err = svc.RunPlaybook(context.Background(), target, ref, pb, vmtool.DefaultAuth(), nil)
			return err
		})
		out := ""
		if res != nil {
			out = res.Output
		}
		status := logStatusDone
		title := fmt.Sprintf("%s on %s: done", pb, vmName)
		if err != nil {
			status = logStatusError
			title = fmt.Sprintf("%s on %s: error: %v", pb, vmName, err)
		}
		return cmdDoneMsg{logIdx: logIdx, title: title, status: status, output: out, vms: m.fetchRows()}
	}
}

// --- Output mode ---

func (m model) updateOutput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	maxVisible := m.termHeight - 6
	if maxVisible < 5 {
		maxVisible = 5
	}
	switch msg.String() {
	case "q", "esc", "enter":
		m.mode = modeList
		return m, nil
	case "up", "k":
		if m.outputScroll > 0 {
			m.outputScroll--
		}
	case "down", "j":
		if m.outputScroll < len(m.outputLines)-maxVisible {
			m.outputScroll++
		}
	case "g":
		m.outputScroll = 0
	case "G":
		end := len(m.outputLines) - maxVisible
		if end < 0 {
			end = 0
		}
		m.outputScroll = end
	}
	return m, nil
}

// --- Views ---

func (m model) View() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("vmtool"))
	b.WriteString("\n\n")

	switch m.mode {
	case modeCreate:
		return b.String() + m.viewCreate()
	case modeBridge:
		return b.String() + m.viewBridge()
	case modePlaybook:
		return b.String() + m.viewPlaybook()
	case modeOutput:
		return b.String() + m.viewOutput()
	case modeConfirm:
		return b.String() + m.viewConfirm()
	case modeAddMachine:
		return b.String() + m.viewAddMachine()
	}

	// --- VM list pane ---
	vmFocus := m.focus == paneVMs

	if m.err != nil {
		b.WriteString(fmt.Sprintf("Error: %v\n", m.err))
	}
	if m.machineErr != nil {
		b.WriteString(fmt.Sprintf("Machines error: %v\n", m.machineErr))
	}
	if m.rowCount() == 0 {
		if m.err == nil {
			b.WriteString("  No VMs or machines found.\n")
		}
	} else {
		header := fmt.Sprintf("  %-28s %-6s %-12s %-6s %-10s %-11s %s", "NAME", "KIND", "STATE", "VCPUS", "MEMORY", "AUTOSTART", "IP/HOST")
		b.WriteString(statusStyle.Render(header))
		b.WriteString("\n")

		lines := make([]string, 0, m.rowCount())
		for _, vm := range m.vms {
			if vm.Err != nil {
				lines = append(lines, fmt.Sprintf("  %-28s ERROR: %s", vm.Host, strings.Join(strings.Fields(vm.Err.Error()), " ")))
				continue
			}
			ip := vm.IP
			if ip == "" {
				ip = "-"
			}
			autostart := "off"
			if vm.Autostart {
				autostart = "on"
			}
			lines = append(lines, fmt.Sprintf("  %-28s %-6s %-12s %-6d %-10s %-11s %s",
				m.vmRef(vm).String(), "vm", vm.State, vm.VCPUs, fmt.Sprintf("%d GiB", vm.Memory), autostart, ip))
		}
		for _, mi := range m.machines {
			lines = append(lines, fmt.Sprintf("  %-28s %-6s %-12s %-6s %-10s %-11s %s",
				mi.Name, mi.Kind, mi.State, "-", "-", "-", mi.Host))
		}

		for i, line := range lines {
			if i == m.cursor && vmFocus {
				b.WriteString(selectedStyle.Render("> " + line[2:]))
			} else if i == m.cursor {
				b.WriteString(statusStyle.Render("> " + line[2:]))
			} else {
				b.WriteString(line)
			}
			b.WriteString("\n")
		}
	}

	// --- Separator ---
	sep := strings.Repeat("─", 70)
	if m.termWidth > 4 {
		sep = strings.Repeat("─", m.termWidth-2)
	}
	label := " Commands "
	if len(sep) > len(label)+4 {
		sep = sep[:2] + label + sep[2+len(label):]
	}
	b.WriteString(borderStyle.Render(sep))
	b.WriteString("\n")

	// --- Command log pane ---
	logFocus := m.focus == paneLog

	logHeight := 8
	if len(m.log) == 0 {
		b.WriteString(statusStyle.Render("  No commands yet."))
		b.WriteString("\n")
	} else {
		start := 0
		if len(m.log) > logHeight {
			start = len(m.log) - logHeight
			if logFocus && m.logCursor < start {
				start = m.logCursor
			}
		}
		end := start + logHeight
		if end > len(m.log) {
			end = len(m.log)
		}

		for i := start; i < end; i++ {
			entry := m.log[i]
			prefix := "  "
			var style lipgloss.Style

			switch entry.status {
			case logStatusRunning:
				style = logRunning
				prefix = "⟳ "
			case logStatusDone:
				style = logDone
				prefix = "✓ "
			case logStatusError:
				style = logError
				prefix = "✗ "
			}

			line := prefix + entry.title
			if entry.output != "" {
				line += " [↵]"
			}

			if i == m.logCursor && logFocus {
				b.WriteString(logSelected.Render("> " + line[2:]))
			} else {
				b.WriteString(style.Render(line))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	if m.focus == paneVMs {
		b.WriteString(helpStyle.Render("c=create  m=add machine  s=start/stop  b=reboot  a=autostart  p=playbook  t=ssh  D=delete/forget  r=refresh  tab=log  q=quit"))
	} else {
		b.WriteString(helpStyle.Render("enter=view output  tab=vms  q=quit"))
	}
	b.WriteString("\n")

	return b.String()
}

func (m model) viewCreate() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("Create VM"))
	b.WriteString("\n\n")

	for i, f := range m.formFields {
		if f.hidden {
			continue
		}
		cursor := "  "
		if i == m.formField {
			cursor = "> "
		}
		val := m.formValues[i]
		if f.kind == fieldSelect {
			if i == m.formField {
				val = fmt.Sprintf("◀ %s ▶", val)
			}
		} else {
			if i == m.formField {
				val += "█"
			}
		}
		b.WriteString(fmt.Sprintf("%s%-20s %s\n", cursor, f.label+":", val))
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("tab/↓=next  ↑=prev  ←/→=select  enter=submit  esc=cancel"))
	b.WriteString("\n")
	return b.String()
}

func (m model) viewBridge() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("Create Bridge"))
	b.WriteString("\n\n")

	for i, f := range m.bridgeFields {
		if f.hidden {
			continue
		}
		cursor := "  "
		if i == m.bridgeField {
			cursor = "> "
		}
		val := m.bridgeValues[i]
		if f.password {
			val = strings.Repeat("•", len(val))
		}
		if f.kind == fieldSelect {
			if i == m.bridgeField {
				val = fmt.Sprintf("◀ %s ▶", val)
			}
		} else {
			if i == m.bridgeField {
				val += "█"
			}
		}
		b.WriteString(fmt.Sprintf("%s%-20s %s\n", cursor, f.label+":", val))
	}

	b.WriteString("\n")
	b.WriteString(logError.Render("  ⚠  This will disrupt networking on the selected NIC"))
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("tab/↓=next  ↑=prev  ←/→=select  enter=submit  esc=back"))
	b.WriteString("\n")
	return b.String()
}

func (m model) viewPlaybook() string {
	var b strings.Builder
	name, addr := "", ""
	if mi, ok := m.selectedMachine(); ok {
		name, addr = mi.Name, mi.Host
	} else if vm, ok := m.selectedVM(); ok {
		name, addr = m.vmRef(vm).String(), vm.IP
	}
	b.WriteString(promptStyle.Render(fmt.Sprintf("Run playbook on %s (%s)", name, addr)))
	b.WriteString("\n\n")

	for i, pb := range m.playbooks {
		if i == m.playbookIdx {
			b.WriteString(selectedStyle.Render("> " + pb))
		} else {
			b.WriteString("  " + pb)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓=select  enter=run  esc=cancel"))
	b.WriteString("\n")
	return b.String()
}

func (m model) viewOutput() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render(m.outputTitle))
	b.WriteString("\n\n")

	maxVisible := m.termHeight - 6
	if maxVisible < 5 {
		maxVisible = 5
	}

	end := m.outputScroll + maxVisible
	if end > len(m.outputLines) {
		end = len(m.outputLines)
	}

	for _, line := range m.outputLines[m.outputScroll:end] {
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	scrollInfo := fmt.Sprintf("[%d-%d of %d]", m.outputScroll+1, end, len(m.outputLines))
	b.WriteString(helpStyle.Render(fmt.Sprintf("↑/↓=scroll  g/G=top/bottom  esc=close  %s", scrollInfo)))
	b.WriteString("\n")
	return b.String()
}

func (m model) viewConfirm() string {
	var b strings.Builder
	b.WriteString(logError.Render("⚠  " + m.confirmMsg))
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("y/enter=confirm  n/esc=cancel"))
	b.WriteString("\n")
	return b.String()
}

// flatImages is every image in mgr's pools, pools in name order.
func flatImages(mgr app.Manager) []string {
	byPool, err := mgr.ListImagesByPool()
	if err != nil {
		return nil
	}
	pools := make([]string, 0, len(byPool))
	for p := range byPool {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	var out []string
	for _, p := range pools {
		out = append(out, byPool[p]...)
	}
	return out
}

// Run launches the interactive TUI. Rows list every host the resolver
// serves, each queried concurrently with a 5 s timeout (a dead host is an
// error row); every action on a VM goes through the resolver for the VM's host (the
// local libvirt connection is reused; another host gets a remote client
// carrying the resolver's token). Machine names may not take any host name.
// tunnel is how guests are reached (the CLI's own executable and token).
//
// store is the session with the optional central store, already decided for
// this run (the CLI decides at start); nil means no server: is configured.
func Run(r *hosts.Resolver, tunnel vmtool.Tunnel, ipWait, sshWait time.Duration, imageDir string, store *app.StoreSession) error {
	m := newModel(r, tunnel, ipWait, sshWait)
	m.imageDir = imageDir
	m.store = store
	m.send = &msgSender{}
	p := tea.NewProgram(m)
	m.send.fn = p.Send
	_, err := p.Run()
	return err
}

// newModel builds the TUI's model from the resolver. It opens nothing: no
// libvirt, no daemon connection. The local host is resolved when an action
// needs it, through the resolver, so a machine whose own name is listed never
// opens libvirt and one with neither still starts (its own row says why).
func newModel(r *hosts.Resolver, tunnel vmtool.Tunnel, ipWait, sshWait time.Duration) model {
	host := r.Local()
	playbookDir := filepath.Join("ansible", "playbooks")
	inventoryPath := filepath.Join("ansible", "inventory.yml")

	m := initialModel(nil, host, playbookDir, inventoryPath)
	m.ownListed = r.OwnListed()
	m.hostNames = r.AllNames()
	m.resolve = r.Resolve
	m.listAll = func(ctx context.Context) []app.HostVMs {
		return app.ListAll(ctx, m.hostNames, m.resolve, app.ListTimeout)
	}
	m.tunnel = tunnel
	m.ipTimeout, m.sshTimeout = ipWait, sshWait
	return m
}
