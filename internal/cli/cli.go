package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/server"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/internal/tui"
	"github.com/resurgentech/vmtool/internal/vmnotes"
	"github.com/resurgentech/vmtool/pkg/vmtool"
	"github.com/spf13/cobra"
)

func Execute() error {
	dir, err := cwdDir()
	if err != nil {
		return err
	}
	code, err := run(newRootCmd(&env{dir: dir}), os.Args[1:], os.Stderr)
	switch {
	case code == exitOK:
		return nil
	case code == exitError && err != nil:
		// main prints it and exits 1, as it always has.
		return err
	}
	// A usage error (2), a remote command's own status, or an error already
	// printed as the JSON error object.
	os.Exit(code)
	return nil
}

// env is what the root command shares with its subcommands.
type env struct {
	// dir is the working directory: config.yml, .env and .machines/.
	dir string
	// cfg and token are set by the gate before any RunE. token is the
	// verified VMTOOL_TOKEN; never print or log it.
	cfg   *config.Config
	token string
	// tokenSource is where token came from: "env", ".env" or "file".
	tokenSource string
	// tunnelExe is the vmtool path guest SSH uses as its ProxyCommand; empty
	// means the running executable. Tests set it to a stub path, since under
	// go test the running executable is refused (vmtool.CurrentExe).
	tunnelExe string
	// errw is where a command's stderr goes (set by the gate's hook before
	// any RunE); nil means os.Stderr.
	errw io.Writer
	// store is this command's session with the optional central store (nil
	// when config.yml has no server: block), opened on first use by session.
	store     *app.StoreSession
	storeOnce sync.Once
	// now is the clock create's elapsed stamps read; nil means time.Now.
	// Tests set it.
	now func() time.Time
}

// storeClientOptions are extra storeclient options for the CLI's store
// client; tests inject their CA here. Production leaves it empty: the system
// trust store.
var storeClientOptions []storeclient.Option

// session is this command's StoreSession: nil when no server: is configured.
// It reads the store key (VMTOOL_SERVER_TOKEN from the environment, else .env,
// else the token file) when first opened,
// makes no network call until the mode is needed, and prints the one
// fallback line to stderr when the store is not usable.
func (e *env) session() *app.StoreSession {
	e.storeOnce.Do(func() {
		if !e.cfg.StoreConfigured() {
			return
		}
		e.store = app.OpenStoreSession(e.cfg.Server.URL,
			func() (string, error) { k, _, err := config.StoreKey(e.dir); return k, err },
			storeClientOptions,
			func(line string) { fmt.Fprintln(e.stderr(), line) })
	})
	return e.store
}

func (e *env) stderr() io.Writer {
	if e.errw != nil {
		return e.errw
	}
	return os.Stderr
}

// storeCommands are the top-level commands that use guest credentials or base
// images. Each decides the store mode at its start (the design: "at the start
// of every command that uses credentials or base images"); no other command
// pays for the probe.
var storeCommands = map[string]bool{
	"create": true, "delete": true, "ssh": true, "exec": true, "playbook": true,
	"resize-disk": true, "i": true, "metal": true, "creds": true,
}

func usesStore(cmd *cobra.Command) bool {
	top := cmd
	for top.HasParent() && top.Parent().HasParent() {
		top = top.Parent()
	}
	if top.HasParent() && storeCommands[top.Name()] {
		return true
	}
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	// `images pull host/<name>` pulls from a host's daemon: no store, so no
	// probe of one (a bare name is the store's).
	if path == "images pull" {
		if args := cmd.Flags().Args(); len(args) > 0 && isHostImageRef(args[0]) {
			return false
		}
	}
	// `templates export --push` uploads into the store; without --push it
	// never uses one.
	if path == "templates export" {
		push, _ := cmd.Flags().GetBool("push")
		return push
	}
	return storeImagePaths[path]
}

// gateError is the gate's refusal. Error is one line naming the fix; Unwrap
// gives the config error (ErrNotFound, ErrNoToken, ErrTokenMismatch, ...).
type gateError struct {
	msg string
	err error
}

func (g *gateError) Error() string { return g.msg }
func (g *gateError) Unwrap() error { return g.err }

// loadConfig loads config.yml, mapping failures to a *gateError.
func (e *env) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(e.dir)
	switch {
	case errors.Is(err, config.ErrNotFound):
		return nil, &gateError{err: err, msg: fmt.Sprintf("vmtool: no %s in %s; run 'just install' (or 'just deploy <host>' on a hypervisor)",
			config.FileName, e.dir)}
	case err != nil:
		return nil, &gateError{err: err, msg: "vmtool: " + err.Error()}
	}
	return cfg, nil
}

// serverGate is the gate for `vmtool server`: a valid config.yml with
// token_sha256, but no local token. A hypervisor holds only the hash (deploy
// writes config.yml with --token-stdin and stores no plain token), and the
// server checks the bearer token of every request it serves against the hash.
// The server itself further requires tls_cert and tls_key.
func (e *env) serverGate() error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if cfg.TokenSHA256 == "" {
		return &gateError{err: config.ErrTokenMismatch, msg: fmt.Sprintf("vmtool: %s has no token_sha256; run 'just install' (or 'just deploy <host>' on a hypervisor)",
			config.FileName)}
	}
	e.cfg = cfg
	return nil
}

// isServerCmd reports whether cmd is the top-level `server` command.
func isServerCmd(cmd *cobra.Command) bool {
	return cmd.Name() == "server" && cmd.HasParent() && !cmd.Parent().HasParent()
}

// exemptFromGate reports whether cmd runs without a valid token: cobra's
// help, completion and shell-completion request commands, `config write`
// (the bootstrap that creates config.yml; it still needs a token present) and
// the `key` commands (they are how a key is requested, shown and checked).
func exemptFromGate(cmd *cobra.Command) bool {
	if !cmd.HasParent() {
		return false
	}
	top := cmd
	for top.Parent().HasParent() {
		top = top.Parent()
	}
	switch top.Name() {
	case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	case "key":
		return true
	case "config":
		return cmd.Parent() == top && cmd.Name() == "write"
	}
	return false
}

func newRootCmd(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:   "vmtool",
		Short: "Manage KVM/QEMU virtual machines",
		Long: `Manage KVM/QEMU virtual machines.

Every command except help, completion, 'config write' and 'key' needs
config.yml in the working directory and a key: VMTOOL_TOKEN (environment, else
.env) matching its token_sha256, or else the key in the token file, which the
local service (vmtool.service) must accept. 'server' needs config.yml with
token_sha256 but no local token; it checks the bearer token of every request
instead. 'config write' only ever uses VMTOOL_TOKEN (or --token-stdin).

Agent tips:
  - Use --json: every command that is not interactive prints exactly one JSON
    object on stdout (lists under a named key); human text and progress go to
    stderr. An error is {"error":{"code":"...","message":"..."}} on stderr,
    with the same codes as the REST API. Exit codes: 0 success, 1 error,
    2 usage; exec exits with the remote command's code. See docs/CLI.md.
  - create blocks until the VM has an address and answers SSH
    (ip_wait_seconds and ssh_wait_seconds in config.yml, 120 s each by
    default; --no-wait returns once it starts). playbook, migrate-disk,
    resize-disk, images push/pull and templates export block until done.
  - Keys: the order is VMTOOL_TOKEN from the environment, then from .env in
    the working directory ('just token-fetch' fetches it), then the token file;
    the store's key is VMTOOL_SERVER_TOKEN found the same way, then the same
    token file. The token file is VMTOOL_TOKEN_FILE, else token_file in
    config.yml, else $XDG_CONFIG_HOME/vmtool/token (~/.config/vmtool/token); it
    must be a regular file, mode 0600 or tighter. With no key anywhere, a run
    that is not in a terminal files one key request (or resumes the pending
    one), prints where to approve it and exits 75: run the command again after
    approving. In a terminal it waits. --no-key-request turns that off.
    'vmtool key status' shows where the key comes from and any pending
    request; 'vmtool key check' shows who accepts the token file's key.
  - Interactive (no --json; with it they exit 2): i, ssh, tunnel, server and
    metal ssh.
  - Nothing prompts; each command's own help has its tips and examples.

  vmtool --json list
  vmtool create --help`,
		// The token gate. No subcommand defines its own PersistentPreRun(E)
		// (cobra would run only the nearest one); TestGateEveryCommand checks
		// every command in the tree is refused without a valid token.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if jsonMode(cmd) {
				if cmd.Annotations[annotationInteractive] == "true" {
					return usageError(errors.New(interactiveRefusal))
				}
			} else {
				// run silenced cobra for an argument that looked like --json
				// but belongs to another program (after exec's target).
				cmd.Root().SilenceErrors, cmd.Root().SilenceUsage = false, false
			}
			if exemptFromGate(cmd) {
				return nil
			}
			gate := func() error { return e.gate(cmd) }
			if isServerCmd(cmd) {
				gate = e.serverGate
			}
			if err := gate(); err != nil {
				// One line, printed once by main; no usage block.
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return err
			}
			e.errw = cmd.ErrOrStderr()
			if !isServerCmd(cmd) && usesStore(cmd) {
				e.session().Mode(cmd.Context())
			}
			return nil
		},
	}

	root.AddCommand(
		createCmd(e),
		credsCmd(e),
		startCmd(e),
		stopCmd(e),
		resumeCmd(e),
		poweroffCmd(e),
		deleteCmd(e),
		listCmd(e),
		inventoryCmd(e),
		hostsCmd(e),
		infoCmd(e),
		sshCmd(e),
		interactiveTunnel(tunnelCmd(e)),
		sshConfigCmd(e),
		execCmd(e),
		rebootCmd(e),
		autostartCmd(e),
		commentCmd(e),
		interactiveCmd(e),
		networksCmd(e),
		netCmd(e),
		bridgesCmd(e),
		playbookCmd(e),
		imagesCmd(e),
		templatesCmd(e),
		poolCmd(e),
		migrateDiskCmd(e),
		resizeDiskCmd(e),
		addDiskCmd(e),
		metalCmd(e),
		serverCmd(e),
		keyCmd(e),
		configCmd(func() (string, error) { return e.dir, nil }),
	)
	root.PersistentFlags().Bool(noKeyRequestFlag, false, "do not file a key request when no key is found (see 'vmtool key status')")
	root.PersistentFlags().String(keyRequestURLFlag, "", "absolute https URL where a key request is filed, instead of the one the store names (or $VMTOOL_KEY_REQUEST_URL)")
	root.PersistentFlags().Bool(jsonFlagName, false, "print the result as one JSON object on stdout, and an error as one on stderr (see docs/CLI.md)")
	markRunErrors(root)
	return root
}

// interactiveTunnel marks the tunnel interactive (it is a byte stream for
// ssh's ProxyCommand) and gives its help an example. tunnelCmd is in
// tunnel.go; the mark is made where the command joins the tree.
func interactiveTunnel(c *cobra.Command) *cobra.Command {
	c.Annotations = map[string]string{annotationInteractive: "true"}
	if c.Example == "" {
		c.Example = "  vmtool tunnel hv1/web"
	}
	return c
}

// remoteExitError is a remote command's exit status. Execute exits with
// that code and does not print an extra error line.
type remoteExitError struct {
	code int
}

func (e remoteExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

// newResolver builds the resolver for the config the gate loaded: the local
// libvirt and a remote client per hosts: entry. Tests replace it so no test
// opens libvirt or reaches a host.
var newResolver = func(e *env) *hosts.Resolver { return hosts.New(e.cfg) }

// resolver maps host names to Managers; remote hosts are reached with the
// token the gate verified.
func (e *env) resolver() *hosts.Resolver { return newResolver(e).WithToken(e.token) }

// withManager runs fn on the local host's Manager.
func withManager(e *env, fn func(m app.Manager, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return withHost(e, nil, fn)
}

// withHost runs fn on the Manager of *host (the --host flag), or of the local
// host when host is nil or empty. An unknown host fails before any Manager
// opens.
func withHost(e *env, host *string, fn func(m app.Manager, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		r := e.resolver()
		name := r.Local()
		if host != nil && *host != "" {
			name = *host
		}
		m, release, err := r.Resolve(name)
		if err != nil {
			return err
		}
		defer release()
		return fn(m, cmd, args)
	}
}

// hostFlag adds --host to cmd and returns where it lands.
func hostFlag(cmd *cobra.Command) *string {
	var host string
	cmd.Flags().StringVar(&host, "host", "", "vmtool host to act on (default: this host)")
	return &host
}

// withVM parses args[0] as host/vm and runs fn on that host's Manager. A bare
// name or a host this vmtool does not serve fails before any Manager opens.
func withVM(e *env, fn func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		r := e.resolver()
		ref, err := r.ParseVM(args[0])
		if err != nil {
			return err
		}
		m, release, err := r.Resolve(ref.Host)
		if err != nil {
			return err
		}
		defer release()
		return fn(m, ref, cmd, args)
	}
}

// service is the workflow layer for this config: HostNames lets AddMachine
// and migration recognise host directories.
func (e *env) service(inventory string) *app.Service {
	// A failed executable lookup leaves Exe empty: guest SSH then fails with
	// a clear error, and metal commands, which never tunnel, are unaffected.
	t, _ := e.tunnel()
	return (&app.Service{
		PlaybookDir:   filepath.Join("ansible", "playbooks"),
		InventoryPath: inventory,
		HostNames:     e.resolver().AllNames(),
		Images:        app.LocalImages{Dir: e.imageDir(), Pools: e.localImages},
		Store:         e.session(),
		// Networks that span hosts reach a network's home and its other members.
		OpenHost:    e.resolver().Resolve,
		HostAddr:    e.hostAddr,
		NetworkHome: e.resolver().NetworkHome,
		// create's waits come from the workstation's config.yml (nil: 120s).
		IPTimeout:  e.cfg.IPWait(),
		SSHTimeout: e.cfg.SSHWait(),
		Now:        e.now,
	}).WithTunnel(t)
}

// hostAddr is the address config.yml gives host's daemon ("" for a host it does
// not list).
func (e *env) hostAddr(host string) string {
	if e.cfg == nil {
		return ""
	}
	return e.cfg.Hosts[host].Address
}

// imageDir is the config's resolved image_dir; "" when unset.
func (e *env) imageDir() string {
	if e.cfg == nil {
		return ""
	}
	return e.cfg.ResolvedImageDir()
}

// localImages finds a base image in this machine's own libvirt pools: create
// on a remote host uploads it from there. Local libvirt is opened only for the
// lookup, and only when the own name has no daemon: with the own name listed
// there are no local pools to search. A machine with neither a daemon nor
// libvirt has none either. Both answer "not found", so create falls back to
// the host's cached copy.
func (e *env) localImages(name string) (string, error) {
	r := e.resolver()
	if r.OwnListed() {
		return "", fmt.Errorf("image %q: %s has a daemon, so it has no local pools: %w", name, r.Local(), vmtool.ErrNotFound)
	}
	m, release, err := r.Resolve(r.Local())
	if err != nil {
		return "", fmt.Errorf("image %q: %v: %w", name, err, vmtool.ErrNotFound)
	}
	defer release()
	return app.FindLocalImage(m, e.cfg == nil || e.cfg.Role != config.RoleHypervisor, name)
}

// libvirtNameCheck is the local libvirt for refusing VM names (metal add):
// nil, and a no-op release, when local libvirt is not used (the own name is
// listed) or cannot be opened. Machines work without it, so neither says
// anything.
func (e *env) libvirtNameCheck() (app.Manager, func()) {
	r := e.resolver()
	if r.OwnListed() {
		return nil, func() {}
	}
	m, release, err := r.Resolve(r.Local())
	if err != nil {
		return nil, func() {}
	}
	return m, release
}

// transferWord is the progress word of an image-transfer stage: the host
// fetching from the store, this machine relaying the store's bytes, or this
// machine uploading its own copy. "" for any other stage.
func transferWord(stage string) string {
	switch stage {
	case "fetch":
		return "fetching"
	case "relay":
		return "relaying"
	case "upload":
		return "uploading"
	}
	return ""
}

// imageTransferProgress renders create's image-transfer events (stages fetch,
// relay and upload) on stderr as one updating line; it reports whether it
// handled ev.
func imageTransferProgress(w io.Writer, ev app.ProgressEvent) bool {
	word := transferWord(ev.Stage)
	if word == "" {
		return false
	}
	switch ev.Status {
	case "hash":
		fmt.Fprintf(w, "%s...\n", ev.Detail)
		return true
	case "progress":
		pct := int64(100)
		if ev.Total > 0 {
			pct = ev.Sent * 100 / ev.Total
		}
		fmt.Fprintf(w, "\r%s: %3d%% (%d / %d MiB)", word, pct, ev.Sent>>20, ev.Total>>20)
		if ev.Sent >= ev.Total {
			fmt.Fprintln(w)
		}
		return true
	case "start":
		fmt.Fprintln(w, ev.Detail)
		return true
	}
	return false
}

// tunnel is how this process reaches guests: its own absolute executable as
// the ProxyCommand, and the token the gate verified, passed to children in
// VMTOOL_TOKEN (never in argv).
func (e *env) tunnel() (vmtool.Tunnel, error) {
	if e.tunnelExe != "" {
		return vmtool.Tunnel{Exe: e.tunnelExe, Token: e.token}, nil
	}
	exe, err := vmtool.CurrentExe()
	return vmtool.Tunnel{Exe: exe, Token: e.token}, err
}

func printAppErr(err error) error {
	var ae *app.Error
	if errors.As(err, &ae) && ae.Output != "" {
		fmt.Print(ae.Output)
	}
	return err
}

// printAppErrFor is printAppErr for a command that has --json: the output the
// failed step captured goes to stderr then, so stdout stays clear.
func printAppErrFor(cmd *cobra.Command, err error) error {
	if !jsonMode(cmd) {
		return printAppErr(err)
	}
	var ae *app.Error
	if errors.As(err, &ae) && ae.Output != "" {
		fmt.Fprint(cmd.ErrOrStderr(), ae.Output)
	}
	return err
}

// apiVMInfo is a VM as the REST API's VMInfo (the server builds the same
// from the same fields; pkg/vmtool's own JSON spells the cards differently).
func apiVMInfo(info *vmtool.VMInfo) api.VMInfo {
	if info == nil {
		return api.VMInfo{}
	}
	out := api.VMInfo{
		Name:      info.Name,
		State:     api.VMInfoState(info.State),
		Vcpus:     int(info.VCPUs),
		Memory:    int(info.Memory),
		Ip:        info.IP,
		Autostart: info.Autostart,
	}
	if info.DiskCache != "" {
		v := api.VMInfoDiskCache(info.DiskCache)
		out.DiskCache = &v
	}
	if info.DiskBacking != "" {
		v := api.VMInfoDiskBacking(info.DiskBacking)
		out.DiskBacking = &v
	}
	if info.UUID != "" {
		u := info.UUID
		out.Uuid = &u
	}
	if info.Firmware != "" {
		v := api.VMInfoFirmware(info.Firmware)
		out.Firmware = &v
	}
	if len(info.Networks) > 0 {
		cards := make([]api.NetworkAttachment, len(info.Networks))
		for i, a := range info.Networks {
			cards[i] = api.NetworkAttachment{Network: a.Network, Mac: a.MAC, Ip: a.IP}
		}
		out.Networks = &cards
	}
	return out
}

// emitVM prints a VM as the command's one object (the API's VMInfo).
func emitVM(cmd *cobra.Command, info *vmtool.VMInfo) error {
	return emit(cmd, apiVMInfo(info))
}

// progressOut is where a long command's progress goes: the process's stdout as
// it always did, and stderr with --json (stdout is then the object only).
func progressOut(cmd *cobra.Command) io.Writer {
	if jsonMode(cmd) {
		return cmd.ErrOrStderr()
	}
	return os.Stdout
}

// apiNotes is a VM's notes as the API's VMNotes (what comment --json prints).
func apiNotes(ref app.VMRef, n *app.VMNotes) api.VMNotes {
	info := infoObject(&vmtool.VMInfo{}, n)
	out := api.VMNotes{Host: ref.Host, Name: ref.Name, Uuid: n.UUID, Comment: n.Comment, Origin: info.Origin}
	return out
}

// infoJSON is `info --json`: the API's VMInfo and, when the store was read,
// the VM's notes beside it (origin and comment, in the API's spelling).
type infoJSON struct {
	api.VMInfo
	Origin  *api.InventoryOrigin `json:"origin,omitempty"`
	Comment *string              `json:"comment,omitempty"`
}

func infoObject(info *vmtool.VMInfo, notes *app.VMNotes) infoJSON {
	out := infoJSON{VMInfo: apiVMInfo(info)}
	if notes == nil {
		return out
	}
	nonEmpty := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	if o := notes.Origin; o != nil {
		out.Origin = &api.InventoryOrigin{Image: nonEmpty(o.Image), ImageSha256: nonEmpty(o.ImageSHA256),
			Template: nonEmpty(o.Template), Clone: nonEmpty(o.CloneKind), Pool: nonEmpty(o.Pool)}
		if !o.Created.IsZero() {
			out.Origin.Created = nonEmpty(o.Created.UTC().Format(time.RFC3339))
		}
	}
	c := notes.Comment
	out.Comment = &c
	return out
}

func createCmd(e *env) *cobra.Command {
	var (
		vcpus           uint
		memory          uint
		diskSizeGB      uint
		pool            string
		netType         string
		netSource       string
		macvtapMode     string
		sshUser         string
		sshPass         string
		inventory       string
		playbook        string
		noclone         bool
		noWait          bool
		extraDiskSizeGB uint
		extraDiskPool   string
		extraVars       []string
		networks        []string
		firmware        string
		diskCache       string
		comment         string
	)

	cmd := &cobra.Command{
		Use:   "create <host/name> <image>",
		Short: "Define and start a new VM",
		Long: `Define and start a new VM from a disk image on host.

By default the image is cloned so the original is unchanged.
Use --noclone to boot directly from the image; any changes (e.g. package
upgrades) will be written back to the source image.

--noclone gets the same SMBIOS serial as a clone, so a cloud-init image boots
in place as this VM: it uses up the image's first boot (name, keys and
machine-id), and every later clone of it would inherit them. Run
'sudo cloud-init clean --machine-id' in the guest before shutdown to remedy.

A clone in a dir pool is the file <name>.qcow2; in a logical pool it is an LV
named <name>, attached as a block disk. A disk in a logical pool is sized at
create (--disk-size) and cannot be grown later.

On a thin pool (--pool names one; 'vmtool pool list' shows TYPE thin) the clone
is a thin snapshot of a template, which takes a fraction of a second, not a
copy: the template of an image is built once per host and pool, from the host's
cached copy, by the first create (that one is slower), and is reused by every
later one. <image> may
also be the name of a template made with 'vmtool templates save'; a template is
per host, so a host without it needs it exported and pulled
('vmtool templates export', 'vmtool images pull'). The clone is a thin LV named
<name>, a block disk with cache 'none', and --disk-size grows it (it never
shrinks: a smaller size leaves the template's). A full pool is refused with
409 before anything is made (data or metadata use at or above 80 %, or promised
sizes over 4x the pool). --noclone and --extra-disk-size are refused on a thin
pool (a thin extra disk comes from add-disk), and a VM name may not start with
'vmtpl-'. A thick pool and its VMs are not affected. A template carries
whatever its disk holds, so its author must have made it fit to clone: see the
README, "What a template must be"; vmtool does not check it, and a clone that
fails to come up shows it at the wait for an address or SSH.

A clone gets disk cache mode 'unsafe' (in a logical or thin pool: 'none') unless
--disk-cache names another (none, writeback, writethrough, unsafe). unsafe
keeps guest writes that the guest believes are flushed in host memory; a
host crash or power loss can corrupt the VM's disk. Pick --disk-cache none
for a VM whose data matters. --noclone
gets no cache attribute unless a mode is named. Extra disks, and disks added
later by add-disk, take the VM's boot-disk mode.

create waits for the VM's address, then for SSH, because a VM without either
is not usable: the waits are ip_wait_seconds and ssh_wait_seconds in
config.yml (default 120 each). If either runs out the VM is left defined and
running, create exits non-zero and says so; delete the VM, or create it again
with --no-wait for a guest whose address or SSH vmtool cannot see. --no-wait
returns right after the VM starts (the inventory is still written) and cannot
be combined with --playbook; with --disk-size the volume is still resized
before boot, but the filesystem grow, which needs SSH, is skipped (cloud-init
grows it at first boot on images built with it).

With a store in use, create records in the store what the VM was made from
(its origin: image, its SHA-256 when known, how it was cloned, the template,
the pool, the time), once, right after the VM is defined. vmtool writes
nothing of its own into the VM's libvirt definition. --comment TEXT adds a
note to the same store record (at most 512 bytes, one line, no control
characters; stored in plain text, so never put a secret in it); change it
later with 'vmtool comment'. With no store in use no origin is recorded and
--comment is refused before anything is created. If the store fails after
the VM is defined, create still succeeds and says the notes were not saved.
'vmtool info' and 'vmtool inventory' show origin and comment from the store.

When create ends, success or failure, it prints one line of stage timings on
stdout, such as 'timings: clone 0.3s, start 1.1s, wait_ip 2.0s, wait_ssh 7.0s,
total 11.0s' (only the stages that ran). REST callers of createVM get none.

--network NAME[=IP] (repeatable) adds a card on a vmtool network of the
host (see 'vmtool net create') after the VM's own first card: vmtool picks the
MAC and reserves the address (the one given, or the next free one of the
network's DHCP range) under the VM's name before the VM starts, so 'web' and
'web.<network>.test' resolve on the network from the first boot. 'vmtool info'
shows the VM's cards; its IP stays the first card's address. Attach or detach
later with 'vmtool net attach' and 'vmtool net detach'; 'vmtool delete'
removes the reservations.

  vmtool create hv1/web ubuntu-24.04.qcow2
  vmtool create hv1/web img1 --pool nvme-thin --disk-size 40
  vmtool create hv1/web img1 --network net1 --network net2=10.77.2.50`,
		Example: `  vmtool create --json hv1/web img1

  Tip: create waits for the VM's address (ip_wait_seconds) and then for SSH
  (ssh_wait_seconds), 120 s each by default; --no-wait skips both. With
  --json the VM is the object on stdout, and progress and timings go to stderr.`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			extra, err := parseExtraVars(extraVars)
			if err != nil {
				return err
			}
			nets, err := app.ParseNetworkRequests(networks)
			if err != nil {
				return err
			}
			var evs []app.ProgressEvent
			// With --json progress is stderr only; stdout is the object.
			progress := io.Writer(os.Stdout)
			transfer := io.Writer(os.Stderr)
			if jsonMode(cmd) {
				progress, transfer = cmd.ErrOrStderr(), cmd.ErrOrStderr()
			}
			info, err := e.service(inventory).CreateVM(cmd.Context(), m, app.CreateVMInput{
				Networks:        nets,
				Host:            ref.Host,
				Name:            ref.Name,
				Image:           args[1],
				VCPUs:           vcpus,
				Memory:          memory,
				DiskSizeGB:      diskSizeGB,
				Pool:            pool,
				NetType:         netType,
				NetSource:       netSource,
				MacvtapMode:     macvtapMode,
				SSHUser:         sshUser,
				SSHPass:         sshPass,
				Playbook:        playbook,
				Noclone:         noclone,
				NoWait:          noWait,
				Firmware:        firmware,
				DiskCache:       diskCache,
				ExtraDiskSizeGB: extraDiskSizeGB,
				ExtraDiskPool:   extraDiskPool,
				ExtraVars:       extra,
				Comment:         comment,
				OnProgress: func(ev app.ProgressEvent) {
					evs = append(evs, ev)
					if imageTransferProgress(transfer, ev) {
						return
					}
					if ev.Output != "" {
						fmt.Fprint(progress, ev.Output)
					}
					if (ev.Status == "done" || ev.Status == "error") && ev.Detail != "" {
						fmt.Fprintln(progress, ev.Detail)
					}
				},
			})
			out := humanOut(cmd)
			if err != nil {
				// The line closes what create printed; the error itself follows
				// on stderr. Nothing for a create refused before it began.
				defer func() {
					if l := timingsLine(evs); l != "" {
						fmt.Fprintln(out, l)
					}
				}()
				// info is set once the domain exists: it is left running.
				if info != nil {
					fmt.Fprintf(out, "VM %q exists and is running, but create did not finish:\n", ref.String())
				}
				return printAppErrFor(cmd, err)
			}
			fmt.Fprintf(out, "VM %q created and started\n", ref.String())
			if info != nil && info.IP != "" {
				fmt.Fprintf(out, "IP: %s\n", info.IP)
			}
			if info != nil {
				writeCards(out, info.Networks)
			}
			if l := timingsLine(evs); l != "" {
				fmt.Fprintln(out, l)
			}
			if jsonMode(cmd) {
				if info == nil {
					info = &vmtool.VMInfo{Name: ref.Name}
				}
				return emitVM(cmd, info)
			}
			return nil
		}),
	}

	cmd.Flags().UintVar(&vcpus, "vcpus", 0, "number of virtual CPUs (default 2)")
	cmd.Flags().UintVar(&memory, "memory", 0, "memory in GiB (default 2)")
	cmd.Flags().UintVar(&diskSizeGB, "disk-size", 0, "disk size in GB (default: same as base image)")
	cmd.Flags().StringVar(&netType, "net-type", "", "network type: nat, bridge, direct")
	cmd.Flags().StringVar(&netSource, "net-source", "", "network source (network name, bridge, or host interface)")
	cmd.Flags().StringVar(&macvtapMode, "macvtap-mode", "", "macvtap mode when --net-type=direct (default bridge). One of: bridge, vepa, private, passthrough")
	cmd.Flags().StringVar(&sshUser, "ssh-user", "", "SSH username (default packer)")
	cmd.Flags().StringVar(&sshPass, "ssh-pass", "", "SSH password (default packer)")
	cmd.Flags().StringVar(&inventory, "inventory", "ansible/inventory.yml", "path to write Ansible inventory file")
	cmd.Flags().StringVar(&playbook, "playbook", "", "playbook name under ansible/playbooks, or a path")
	cmd.Flags().StringVar(&pool, "pool", "", "storage pool for the VM's disk (default: the host's default_pool in the workstation's config.yml, else \"default\" on a workstation, \"vmtool\" on a hypervisor). A dir pool holds <name>.qcow2; a logical pool an LV named <name>; a thin pool a thin snapshot of the image's template (or of a saved template named as <image>), an LV named <name>")
	cmd.Flags().BoolVar(&noclone, "noclone", false, "boot directly from the image without cloning (changes persist to source; uses up the image's cloud-init first boot, run 'sudo cloud-init clean --machine-id' in the guest before shutdown)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return right after the VM starts: no wait for an address, none for SSH, and --disk-size does not grow the filesystem. For a guest whose address vmtool cannot see (static address, foreign DHCP without a guest agent) or a very slow boot. Cannot be combined with --playbook")
	cmd.Flags().StringVar(&firmware, "firmware", "", "guest firmware: efi (UEFI, default) or bios. An image installed for BIOS needs --firmware bios")
	cmd.Flags().StringVar(&diskCache, "disk-cache", "", "disk cache mode: none, writeback, writethrough or unsafe (default for a clone: none in a logical or thin pool, unsafe in any other pool; the hypervisor's default with --noclone). unsafe keeps guest writes the guest believes are flushed in host memory; a host crash or power loss can corrupt the VM's disk. Pick none for a VM whose data matters")
	cmd.Flags().UintVar(&extraDiskSizeGB, "extra-disk-size", 0, "create a second empty virtio disk (vdb): <name>-2.qcow2 in a dir pool, LV <name>-2 in a logical pool. Refused on a thin pool: attach a thin extra disk with add-disk")
	cmd.Flags().StringVar(&extraDiskPool, "extra-disk-pool", "", "storage pool for the extra disk (default: same as --pool)")
	cmd.Flags().StringArrayVar(&extraVars, "extra-var", nil, "ansible extra-var key=value for --playbook (repeatable; name: letters, digits, _). Values go to ansible in its environment, not argv. deviceid defaults to the VM name")
	cmd.Flags().StringArrayVar(&networks, "network", nil, "an extra card on a vmtool network of the host, NAME or NAME=IP (repeatable). The card gets a MAC from vmtool and a reservation carrying the VM's name (the address given, or the next free one of the network's DHCP range), made before the VM starts; the VM's own NAT card stays first. See 'vmtool net create'")
	cmd.Flags().StringVar(&comment, "comment", "", "a note about the VM, kept in the store only (needs a store): at most 512 bytes, one line, no control characters. Stored in plain text; never put a secret in it")

	return cmd
}

func startCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "start <host/name>",
		Short: "Start a stopped VM",
		Example: `  vmtool start hv1/web
  vmtool start --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := e.service("").StartVM(cmd.Context(), m, ref)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emitVM(cmd, info)
			}
			fmt.Printf("VM %q started\n", args[0])
			return nil
		}),
	}
}

func resumeCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <host/name>",
		Short: "Resume a paused VM",
		Example: `  vmtool resume hv1/web
  vmtool resume --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := e.service("").ResumeVM(cmd.Context(), m, ref)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emitVM(cmd, info)
			}
			fmt.Printf("VM %q resumed\n", args[0])
			return nil
		}),
	}
}

func stopCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <host/name>",
		Short: "Gracefully shut down a VM",
		Example: `  vmtool stop hv1/web
  vmtool stop --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := e.service("").StopVM(cmd.Context(), m, ref)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emitVM(cmd, info)
			}
			fmt.Printf("VM %q shutdown requested\n", args[0])
			return nil
		}),
	}
}

func poweroffCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "poweroff <host/name>",
		Short: "Force power-off a VM",
		Example: `  vmtool poweroff hv1/web
  vmtool poweroff --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := e.service("").PoweroffVM(cmd.Context(), m, ref)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emitVM(cmd, info)
			}
			fmt.Printf("VM %q powered off\n", args[0])
			return nil
		}),
	}
}

func deleteCmd(e *env) *cobra.Command {
	var noclone bool

	cmd := &cobra.Command{
		Use:   "delete <host/name>",
		Short: "Stop and undefine a VM",
		Long: `Stop and undefine a VM.

By default also deletes the cloned disk volume.
Use --noclone to skip volume deletion (use when the VM was created with --noclone).`,
		Example: `  vmtool delete hv1/web
  vmtool delete --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if err := e.service("").DeleteVM(cmd.Context(), m, ref, noclone); err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emit(cmd, map[string]string{"deleted": ref.String()})
			}
			fmt.Printf("VM %q deleted\n", args[0])
			return nil
		}),
	}

	cmd.Flags().BoolVar(&noclone, "noclone", false, "undefine only, do not delete the disk volume")
	return cmd
}

func infoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "info <host/name>",
		Short: "Show details of a VM",
		Example: `  vmtool info hv1/web
  vmtool info --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			svc := e.service("")
			info, err := svc.GetVM(cmd.Context(), m, ref.Name)
			if err != nil {
				return err
			}
			notes := svc.Notes(cmd.Context(), ref, info.UUID)
			if jsonMode(cmd) {
				return emit(cmd, infoObject(info, notes))
			}
			writeVMInfo(cmd.OutOrStdout(), info, notes)
			return nil
		}),
	}
}

// writeVMInfo prints `vmtool info`; an unknown UUID (a daemon older than API
// 2.3.0) shows as "unknown". notes are the VM's origin and comment from the
// store; nil (no store in use, or it did not answer) prints neither line.
func writeVMInfo(out io.Writer, info *vmtool.VMInfo, notes *app.VMNotes) {
	ip := info.IP
	if ip == "" {
		ip = "-"
	}
	autostart := "off"
	if info.Autostart {
		autostart = "on"
	}
	uuid := info.UUID
	if uuid == "" {
		uuid = "unknown"
	}
	fmt.Fprintf(out, "Name:      %s\n", info.Name)
	fmt.Fprintf(out, "State:     %s\n", info.State)
	fmt.Fprintf(out, "VCPUs:     %d\n", info.VCPUs)
	fmt.Fprintf(out, "Memory:    %d GiB\n", info.Memory)
	fmt.Fprintf(out, "Autostart: %s\n", autostart)
	fmt.Fprintf(out, "IP:        %s\n", ip)
	fmt.Fprintf(out, "UUID:      %s\n", uuid)
	writeCards(out, info.Networks)
	if notes == nil {
		return
	}
	writeOrigin(out, notes.Origin)
	comment := notes.Comment
	if comment == "" {
		comment = "-"
	}
	fmt.Fprintf(out, "Comment:   %s\n", comment)
}

// writeOrigin prints what the VM was made from: one line, then the digest and
// the template when there are any. "-" for a VM with no origin (made outside
// vmtool, or with no store in use).
func writeOrigin(out io.Writer, o *vmnotes.Origin) {
	if o == nil {
		fmt.Fprintf(out, "Origin:    -\n")
		return
	}
	image := o.Image
	if image == "" {
		image = "-"
	}
	line := fmt.Sprintf("%s, clone kind %s", image, o.CloneKind)
	if o.Pool != "" {
		line += ", pool " + o.Pool
	}
	if !o.Created.IsZero() {
		line += ", created " + o.Created.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(out, "Origin:    %s\n", line)
	if o.ImageSHA256 != "" {
		fmt.Fprintf(out, "           sha256 %s\n", o.ImageSHA256)
	}
	if o.Template != "" {
		fmt.Fprintf(out, "           template %s\n", o.Template)
	}
}

func sshCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <host/name> [-- ssh-args...]",
		Short: "SSH into a VM using stored .machines credentials",
		Long: `SSH into a running VM. If .machines/<name>.<host>.vmtool/ exists, uses the
username and key stored there. Otherwise falls back to packer. Host key
checking is disabled.

  vmtool ssh hv1/web`,
		Annotations: map[string]string{annotationInteractive: "true"},
		Args:        cobra.MinimumNArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := m.Info(ref.Name)
			if err != nil {
				return err
			}
			if info.IP == "" {
				return fmt.Errorf("VM %q has no IP address", ref.String())
			}

			svc := e.service("")
			// Store mode: reconcile the credentials folder (and fetch it when it
			// is missing) before ssh reads its key.
			if err := svc.PrepareVMCreds(cmd.Context(), m, ref); err != nil {
				return err
			}
			c, err := svc.SSHCmd(ref, args[1:]...)
			if err != nil {
				return err
			}
			return c.Run()
		}),
	}
}

func execCmd(e *env) *cobra.Command {
	var tty bool
	cmd := &cobra.Command{
		Use:   "exec <host/name> [--] <arg>...",
		Short: "Run a program on a VM over SSH",
		Long: `Run a program on a running VM and pass through stdin, stdout, stderr,
and the remote exit code. Uses .machines credentials when present.

The arguments are the exact argv: each reaches the VM as one argument, with
no shell parsing (spaces, quotes, $ and | are plain characters). For shell
features, run a shell. -- right after the target ends vmtool's flags and is
not sent; everything after it is the command, exactly.

  vmtool exec hv1/web uname -a
  vmtool exec hv1/web -- sh -c 'echo hi | wc -c'

-t / --tty gives the command a live terminal, like ssh -t or docker exec -it:
your terminal goes into raw mode and its size and resizes are forwarded.
Without a terminal on stdin (a pipe or a file) -t still forces a remote pty:
the program sees a terminal, stdout and stderr are merged, input is echoed.
Put -t before the VM; after it, -t belongs to the remote command.

  vmtool exec -t hv1/web -- top`,
		Example: `  vmtool exec --json hv1/web uname -a

  exec exits with the remote command's exit code. With --json it prints
  {"exit_code", "stdout", "stderr"} and still exits with that code; -t cannot
  be combined with --json.`,
		Args:          execArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			asJSON := jsonMode(cmd)
			if asJSON && tty {
				return usageError(errors.New("--tty cannot be combined with --json"))
			}
			info, err := m.Info(ref.Name)
			if err != nil {
				return err
			}
			if info.IP == "" {
				return fmt.Errorf("VM %q has no IP address", ref.String())
			}

			svc := e.service("")
			if err := svc.PrepareVMCreds(cmd.Context(), m, ref); err != nil {
				return err
			}
			// With --json the program's streams are captured into the object.
			var stdout, stderr bytes.Buffer
			run := func() error {
				c, err := svc.RemoteCmd(ref, execCommand(args), ttyOpts(tty)...)
				if err != nil {
					return err
				}
				// RemoteCmd (exact argv) leaves stdio unset (it is also used for captured runs).
				if asJSON {
					stdout.Reset()
					stderr.Reset()
					c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), &stdout, &stderr
				} else {
					c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
				}
				return c.Run()
			}
			err = run()
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() == 255 && svc.RefetchVMCreds(cmd.Context(), m, ref) {
				// ssh failed (255) and the store has newer credentials: once more.
				err = run()
			}
			code := 0
			if err != nil {
				if !errors.As(err, &ee) {
					return err
				}
				code = ee.ExitCode()
			}
			if asJSON {
				if err := emit(cmd, api.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}); err != nil {
					return err
				}
			}
			if code != 0 {
				return remoteExitError{code: code}
			}
			return nil
		}),
	}
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "force a remote terminal (ssh -tt) and attach yours live")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// ttyOpts is the ssh option for -t/--tty: none when off, so the command is
// exactly the plain one.
func ttyOpts(tty bool) []vmtool.SSHOption {
	if tty {
		return []vmtool.SSHOption{vmtool.WithTTY()}
	}
	return nil
}

func rebootCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "reboot <host/name>",
		Short: "Reboot a running VM",
		Long: `Ask the guest to reboot (ACPI). The domain stays defined.

Unlike stop (ACPI shutdown, then shutoff) or poweroff (force destroy),
the VM comes back up on its own if the guest handles the reboot.`,
		Example: `  vmtool reboot hv1/web
  vmtool reboot --json hv1/web`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := e.service("").RebootVM(cmd.Context(), m, ref)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emitVM(cmd, info)
			}
			fmt.Printf("VM %q rebooted\n", args[0])
			return nil
		}),
	}
}

func autostartCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "autostart <host/name> <on|off>",
		Short: "Enable or disable autostart for a VM",
		Example: `  vmtool autostart hv1/web on
  vmtool autostart --json hv1/web off`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			name := args[0]
			var enable bool
			switch args[1] {
			case "on", "true", "yes", "1":
				enable = true
			case "off", "false", "no", "0":
				enable = false
			default:
				return fmt.Errorf("invalid value %q: use on or off", args[1])
			}
			info, err := e.service("").SetAutostart(cmd.Context(), m, ref, enable)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emitVM(cmd, info)
			}
			label := "disabled"
			if enable {
				label = "enabled"
			}
			fmt.Printf("Autostart %s for VM %q\n", label, name)
			return nil
		}),
	}
}

func commentCmd(e *env) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "comment <host/name> [TEXT]",
		Short: "Set or clear a VM's comment",
		Long: `Set a VM's comment, or clear it with --clear. The comment is kept in the
store only (the VM's notes, beside its origin), never in the VM's libvirt
definition, so it needs a store: with none in use the command fails. It is at
most 512 bytes of UTF-8 on one line with no control characters, stored in
plain text: never put a secret in it. The VM's host is not asked, so this
works while the host sleeps; the store must list the VM in its inventory (its
host has reported it, and it is not gone). 'vmtool info' and 'vmtool
inventory' show it.

  vmtool comment hv1/web "build box for the nightly job"
  vmtool comment hv1/web --clear`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := ""
			switch {
			case clear && len(args) == 2:
				return fmt.Errorf("give TEXT or --clear, not both")
			case !clear && len(args) < 2:
				return fmt.Errorf("give the comment TEXT, or --clear to remove it")
			case len(args) == 2:
				text = args[1]
			}
			// The host is checked, never opened: the comment is a store write.
			r := e.resolver()
			ref, err := r.ParseVM(args[0])
			if err != nil {
				return err
			}
			if err := r.Check(ref.Host); err != nil {
				return err
			}
			notes, err := e.service("").SetComment(cmd.Context(), ref, text)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emit(cmd, apiNotes(ref, notes))
			}
			if text == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Comment cleared for VM %q\n", ref.String())
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Comment set for VM %q\n", ref.String())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the comment")
	return cmd
}

func interactiveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "i",
		Short:   "Launch interactive TUI",
		Aliases: []string{"interactive"},
		Example: "  vmtool i",
		// A TUI: no --json.
		Annotations: map[string]string{annotationInteractive: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := e.tunnel()
			if err != nil {
				return err
			}
			return tui.Run(e.resolver(), t, e.cfg.IPWait(), e.cfg.SSHWait(), e.imageDir(), e.session())
		},
	}
}

func networksCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "networks",
		Short: "List available libvirt networks (vmtool's are marked, member forms too)",
		Example: `  vmtool networks
  vmtool networks --host hv1 --json`,
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		nets, err := e.service("").ListNetworkMarks(cmd.Context(), m)
		if err != nil {
			return err
		}
		if jsonMode(cmd) {
			rows := make([]networkJSON, len(nets))
			for i, n := range nets {
				rows[i] = networkJSON{Name: n.Name, Vmtool: n.Vmtool, Member: n.Member}
			}
			return emit(cmd, map[string]any{"networks": rows})
		}
		for _, n := range nets {
			if n.Vmtool && n.Member {
				fmt.Fprintf(cmd.OutOrStdout(), "%s (vmtool, member)\n", n.Name)
			} else if n.Vmtool {
				fmt.Fprintf(cmd.OutOrStdout(), "%s (vmtool)\n", n.Name)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), n.Name)
			}
		}
		return nil
	})
	return cmd
}

// networkJSON is one row of `networks --json`.
type networkJSON struct {
	Name   string `json:"name"`
	Vmtool bool   `json:"vmtool"`
	Member bool   `json:"member"`
}

func bridgesCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bridges",
		Short: "List host bridges usable with --net-type=bridge",
		Example: `  vmtool bridges
  vmtool bridges --host hv1 --json`,
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		brs, err := e.service("").ListBridges(cmd.Context(), m)
		if err != nil {
			return err
		}
		if jsonMode(cmd) {
			if brs == nil {
				brs = []string{}
			}
			return emit(cmd, map[string]any{"bridges": brs})
		}
		for _, b := range brs {
			fmt.Println(b)
		}
		return nil
	})
	return cmd
}

func parseExtraVars(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --extra-var %q: want key=value", p)
		}
		out[k] = v
	}
	if err := vmtool.CheckExtraVars(out); err != nil {
		return nil, err
	}
	return out, nil
}

func playbookCmd(e *env) *cobra.Command {
	var (
		sshUser   string
		sshPass   string
		extraVars []string
	)

	cmd := &cobra.Command{
		Use:   "playbook <host/vm-name> <playbook-path>",
		Short: "Run an Ansible playbook against a running VM",
		Example: `  vmtool playbook hv1/web site.yml
  vmtool playbook --json hv1/web site.yml

  Tip: playbook blocks until ansible ends. With --json ansible's output goes to
  stderr and stdout carries {"vm", "playbook", "completed"}.`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			extra, err := parseExtraVars(extraVars)
			if err != nil {
				return err
			}
			res, err := e.service("").RunPlaybook(cmd.Context(), m, ref, args[1], vmtool.Auth{User: sshUser, Password: sshPass}, extra)
			if res != nil && res.Output != "" {
				fmt.Fprint(progressOut(cmd), res.Output)
			}
			if err != nil {
				return printAppErrFor(cmd, err)
			}
			if jsonMode(cmd) {
				return emit(cmd, map[string]any{"vm": ref.String(), "playbook": args[1], "completed": true})
			}
			fmt.Printf("Playbook %s completed\n", args[1])
			return nil
		}),
	}

	cmd.Flags().StringVar(&sshUser, "ssh-user", "packer", "SSH username if no .machines entry exists")
	cmd.Flags().StringVar(&sshPass, "ssh-pass", "packer", "SSH password if no .machines entry exists")
	cmd.Flags().StringArrayVar(&extraVars, "extra-var", nil, "ansible extra-var key=value (repeatable; name: letters, digits, _). Values go to ansible in its environment, not argv. deviceid defaults to the VM name")

	return cmd
}

func imagesCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Manage disk images",
	}
	cmd.AddCommand(imagesListCmd(e), imagesDeleteCmd(e), imagesPushCmd(e), imagesPullCmd(e))
	return cmd
}

func imagesListCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available disk images grouped by pool",
		Example: `  vmtool images list
  vmtool images list --host hv1 --json
  vmtool images list --store

  Tip: with --json stdout is {"pools": [{"name", "images"}]}; with --store it
  is {"images": [{"name", "size", "sha256", "updated"}]}, the store's images.`,
	}
	host := hostFlag(cmd)
	var fromStore bool
	cmd.Flags().BoolVar(&fromStore, "store", false, "list the base images the store holds instead of a host's pools (needs a store)")
	hostList := withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		byPool, err := e.service("").ListImages(cmd.Context(), m)
		if err != nil {
			return err
		}
		pools := make([]string, 0, len(byPool))
		for p := range byPool {
			pools = append(pools, p)
		}
		sort.Strings(pools)
		if jsonMode(cmd) {
			obj := imagesJSON{Pools: make([]imagePoolJSON, 0, len(pools))}
			for _, p := range pools {
				imgs := append([]string{}, byPool[p]...)
				sort.Strings(imgs)
				obj.Pools = append(obj.Pools, imagePoolJSON{Name: p, Images: imgs})
			}
			return emit(cmd, obj)
		}
		for _, p := range pools {
			fmt.Printf("%s:\n", p)
			imgs := byPool[p]
			sort.Strings(imgs)
			for _, img := range imgs {
				fmt.Printf("  %s\n", img)
			}
		}
		return nil
	})
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !fromStore {
			return hostList(cmd, args)
		}
		// The mode is decided here, once, rather than at the command's start:
		// without --store this command never touches the store.
		imgs, err := e.service("").ListStoreImages(cmd.Context())
		if err != nil {
			return printAppErrFor(cmd, err)
		}
		if jsonMode(cmd) {
			obj := storeImagesJSON{Images: make([]storeImageJSON, len(imgs))}
			for i, img := range imgs {
				obj.Images[i] = storeImageObject(img)
			}
			return emit(cmd, obj)
		}
		return writeStoreImages(cmd.OutOrStdout(), imgs)
	}
	return cmd
}

// imagesJSON is `images list --json`: the images of each pool of the host.
type imagesJSON struct {
	Pools []imagePoolJSON `json:"pools"`
}

type imagePoolJSON struct {
	Name   string   `json:"name"`
	Images []string `json:"images"`
}

// storeImagesJSON is `images list --store --json`.
type storeImagesJSON struct {
	Images []storeImageJSON `json:"images"`
}

func imagesDeleteCmd(e *env) *cobra.Command {
	var pool, host string
	cmd := &cobra.Command{
		Use:   "delete <image>",
		Short: "Delete a disk image from a storage pool",
		Example: `  vmtool images delete base.qcow2
  vmtool images delete --json base.qcow2 --pool fast --host hv1

  Tip: with --json stdout is {"deleted": <image>, "pool": <pool>}; pool is
  empty when the host's default pool was used.`,
		Args: cobra.ExactArgs(1),
		RunE: withHost(e, &host, func(m app.Manager, cmd *cobra.Command, args []string) error {
			if err := e.service("").DeleteImage(cmd.Context(), m, args[0], pool); err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emit(cmd, map[string]string{"deleted": args[0], "pool": pool})
			}
			if pool == "" {
				fmt.Printf("Image %q deleted from the host's default pool\n", args[0])
			} else {
				fmt.Printf("Image %q deleted from pool %q\n", args[0], pool)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&pool, "pool", "", "storage pool containing the image (default: the host's default pool: \"default\" on a workstation, \"vmtool\" on a hypervisor)")
	cmd.Flags().StringVar(&host, "host", "", "vmtool host to act on (default: this host)")
	return cmd
}

func poolCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Manage storage pools",
	}
	cmd.AddCommand(poolListCmd(e), poolCreateCmd(e))
	return cmd
}

func poolListCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List storage pools",
		Aliases: []string{"ls"},
		Long: `List the storage pools of a host: its libvirt pools and, on a hypervisor daemon
that has them, its LVM thin pools (TYPE thin).

Columns: NAME, TYPE, SIZE, USED, VIRTUAL, DATA%, META%, ACTIVE, PATH. SIZE and
USED are filled for an active libvirt pool and for a thin pool. VIRTUAL is the
sum of the sizes of every thin volume in the pool (what it has promised); DATA%
and META% are the thin pool's data and metadata use. They are blank for a
libvirt pool, and any cell is blank when the host does not report it (a daemon
older than API 2.4.0). A thin pool gets a '!' after its name when DATA% or META%
is at or above 80: from there new thin volumes, add-disk and resize-disk are
refused with 409, as are promised sizes over 4x the pool's SIZE.`,
		Example: `  vmtool pool list
  vmtool pool list --host hv1 --json

  Tip: with --json stdout is {"pools": [...]}, each the API's PoolInfo (name,
  path, active, and type, size_bytes, used_bytes, virtual_bytes, data_percent,
  metadata_percent when the host reports them) in bytes and percent.`,
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		pools, err := e.service("").ListPools(cmd.Context(), m)
		if err != nil {
			return err
		}
		if jsonMode(cmd) {
			return emit(cmd, poolsObject(pools))
		}
		return writePoolTable(os.Stdout, pools)
	})
	return cmd
}

// poolsJSON is `pool list --json`: the API's PoolInfo for each pool.
type poolsJSON struct {
	Pools []api.PoolInfo `json:"pools"`
}

func poolsObject(pools []vmtool.PoolInfo) poolsJSON {
	out := poolsJSON{Pools: make([]api.PoolInfo, len(pools))}
	for i, p := range pools {
		out.Pools[i] = api.PoolInfo{Name: p.Name, Path: p.Path, Active: p.Active,
			SizeBytes: p.SizeBytes, UsedBytes: p.UsedBytes, VirtualBytes: p.VirtualBytes,
			DataPercent: p.DataPercent, MetadataPercent: p.MetaPercent}
		if p.Type != "" {
			t := p.Type
			out.Pools[i].Type = &t
		}
	}
	return out
}

// poolWarnPercent is where a thin pool's name gets a "!": at or above it,
// data or metadata, the pool is near the 80 % refusal line of thin creates.
const poolWarnPercent = 80.0

// formatPoolBytes is a compact binary size: 512B, 1K, 1.5M, 60G, 1.5T.
func formatPoolBytes(n int64) string {
	if n <= 0 {
		return "0"
	}
	if n < 1<<10 {
		return fmt.Sprintf("%dB", n)
	}
	v, unit := float64(n), ""
	for _, u := range []string{"K", "M", "G", "T"} {
		v /= 1024
		unit = u
		if v < 1024 {
			break
		}
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + unit
}

func poolBytesCell(n *int64) string {
	if n == nil {
		return ""
	}
	return formatPoolBytes(*n)
}

func poolPercentCell(f *float64) string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%.1f", *f)
}

// writePoolTable prints the pool list. An unknown type shows as "-" and an
// unknown size, usage or percentage as a blank cell. A thin pool at or above
// poolWarnPercent data or metadata gets a "!" after its name.
func writePoolTable(out io.Writer, pools []vmtool.PoolInfo) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tSIZE\tUSED\tVIRTUAL\tDATA%\tMETA%\tACTIVE\tPATH")
	for _, p := range pools {
		active := "no"
		if p.Active {
			active = "yes"
		}
		typ := p.Type
		if typ == "" {
			typ = "-"
		}
		name := p.Name
		if p.Type == vmtool.PoolTypeThin &&
			((p.DataPercent != nil && *p.DataPercent >= poolWarnPercent) || (p.MetaPercent != nil && *p.MetaPercent >= poolWarnPercent)) {
			name += "!"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, typ,
			poolBytesCell(p.SizeBytes), poolBytesCell(p.UsedBytes), poolBytesCell(p.VirtualBytes),
			poolPercentCell(p.DataPercent), poolPercentCell(p.MetaPercent), active, p.Path)
	}
	return w.Flush()
}

func poolCreateCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name> <path>",
		Short: "Create a new directory-type storage pool",
		Example: `  vmtool pool create fast /srv/fast
  vmtool pool create --json fast /srv/fast --host hv1

  Tip: with --json stdout is {"name", "path"} of the new pool.`,
		Args: cobra.ExactArgs(2),
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		if err := e.service("").CreatePool(cmd.Context(), m, args[0], args[1]); err != nil {
			return err
		}
		if jsonMode(cmd) {
			return emit(cmd, map[string]string{"name": args[0], "path": args[1]})
		}
		fmt.Printf("Pool %q created at %s\n", args[0], args[1])
		return nil
	})
	return cmd
}

func migrateDiskCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-disk <host/vm> <pool>",
		Short: "Move a VM's disk to a different storage pool",
		Long: `Stop the VM (if running), copy its disk volume to the target pool,
redefine the domain with the new path, then restart (if it was running).

A block disk (an LV in a logical pool, or a thin LV), or a logical pool as the
target, is refused before the VM is stopped. A thin pool is not a libvirt pool,
so it cannot be a target either.`,
		Example: `  vmtool migrate-disk hv1/web fast
  vmtool migrate-disk --json hv1/web fast

  Tip: migrate-disk blocks until the copy is done. With --json the progress
  line goes to stderr and stdout is the VM.`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			vmName := args[0]
			pool := args[1]
			fmt.Fprintf(progressOut(cmd), "Migrating disk for VM %q to pool %q...\n", vmName, pool)
			info, err := e.service("").MigrateDisk(cmd.Context(), m, ref, pool)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				fmt.Fprintf(cmd.ErrOrStderr(), "VM %q disk migrated to pool %q\n", vmName, pool)
				return emitVM(cmd, info)
			}
			fmt.Printf("VM %q disk migrated to pool %q\n", vmName, pool)
			return nil
		}),
	}
}

func resizeDiskCmd(e *env) *cobra.Command {
	var (
		sshUser string
		sshPass string
	)

	cmd := &cobra.Command{
		Use:   "resize-disk <host/vm> <size-gb>",
		Short: "Grow a VM's disk to a new size",
		Long: `Resize a VM's disk volume to the given size in GB (must be larger).

If the VM is running, also notifies the guest of the new size and expands
the root partition and filesystem via SSH.

A disk on a thin pool (a thin LV) grows at once: the volume is extended and,
if the VM is running, the VM is told. The pool must have room (data and
metadata use under 80 %, promised sizes within 4x the pool); otherwise it is
refused with the numbers.

A disk in a thick logical pool (an LV) cannot be grown: it is sized at create
(--disk-size). resize-disk refuses it before touching the VM.`,
		Example: `  vmtool resize-disk hv1/web 40
  vmtool resize-disk --json hv1/web 40

  Tip: with --json stdout is {"vm", "filesystem_grown", "grow_error"}; a failed
  filesystem grow still fails the command (exit 1).`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			vmName := args[0]
			sizeGB, err := strconv.ParseUint(args[1], 10, 32)
			if err != nil || sizeGB == 0 {
				return fmt.Errorf("invalid size %q: must be a positive integer (GB)", args[1])
			}

			fmt.Fprintf(progressOut(cmd), "Resizing disk for VM %q to %dGB...\n", vmName, sizeGB)
			res, err := e.service("").ResizeDisk(cmd.Context(), m, ref, uint(sizeGB), vmtool.Auth{User: sshUser, Password: sshPass})
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				obj := api.ResizeDiskResult{Vm: apiVMInfo(&res.VM), FilesystemGrown: res.FilesystemGrown}
				if res.GrowError != "" {
					g := res.GrowError
					obj.GrowError = &g
				}
				if err := emit(cmd, obj); err != nil {
					return err
				}
				if res.GrowError != "" {
					return fmt.Errorf("growing filesystem: %s", res.GrowError)
				}
				return nil
			}
			fmt.Printf("VM %q disk resized to %dGB\n", vmName, sizeGB)
			if res.FilesystemGrown {
				fmt.Println("Disk partition and filesystem expanded")
				return nil
			}
			if res.GrowError != "" {
				fmt.Println(res.GrowError)
				return fmt.Errorf("growing filesystem: %s", res.GrowError)
			}
			fmt.Println("VM is not running with an IP; grow the filesystem after start")
			return nil
		}),
	}

	cmd.Flags().StringVar(&sshUser, "ssh-user", "packer", "SSH username if no .machines entry exists")
	cmd.Flags().StringVar(&sshPass, "ssh-pass", "packer", "SSH password if no .machines entry exists")
	return cmd
}

func addDiskCmd(e *env) *cobra.Command {
	var pool string

	cmd := &cobra.Command{
		Use:   "add-disk <host/vm> <size-gb>",
		Short: "Attach a new empty disk to a VM",
		Long: `Create an empty volume and attach it as the next virtio disk (vdb, vdc, …).

In a dir pool the volume is <vm>-2.qcow2 for the first extra disk,
<vm>-3.qcow2 if called again, and so on; in a logical pool it is an LV named
<vm>-2, <vm>-3, …, attached as a block disk; in a thin pool it is a thin LV
of that name (refused when the pool has no room: data or metadata use at or
above 80 %, or promised sizes over 4x the pool). If the VM is running the disk
is hot-plugged and persisted. If it is shut off the change is persisted and
appears on the next start. The guest sees a raw block device; it is not
partitioned or formatted.

  vmtool add-disk hv1/web 50
  vmtool add-disk hv1/web 100 --pool fast`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			vmName := args[0]
			sizeGB, err := strconv.ParseUint(args[1], 10, 32)
			if err != nil || sizeGB == 0 {
				return fmt.Errorf("invalid size %q: must be a positive integer (GB)", args[1])
			}

			res, err := e.service("").AddDisk(cmd.Context(), m, ref, uint(sizeGB), pool)
			if err != nil {
				return err
			}
			if jsonMode(cmd) {
				return emit(cmd, api.AddDiskResult{Vm: apiVMInfo(&res.VM), Path: res.Path, Target: res.Target})
			}
			fmt.Printf("Attached %s (%dGB) to VM %q as %s\n", res.Path, sizeGB, vmName, res.Target)
			return nil
		}),
	}

	cmd.Flags().StringVar(&pool, "pool", "", "storage pool for the new volume (default: the host's default_pool in the workstation's config.yml, else \"default\" on a workstation, \"vmtool\" on a hypervisor). A dir pool holds <vm>-N.qcow2; a logical pool an LV named <vm>-N")
	return cmd
}

// serverFrom builds the server from the config.yml the gate loaded. listen
// overrides config.yml listen only when the flag was set.
func serverFrom(cfg *config.Config, listen string, listenSet bool) *server.Server {
	s := &server.Server{Config: cfg}
	if listenSet {
		s.Listen = listen
	}
	return s
}

func serverCmd(e *env) *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the REST API server",
		Long: `Start the vmtool REST API over HTTPS, reading config.yml from the working
directory. config.yml must set tls_cert, tls_key and token_sha256. Every route
except GET /, /openapi.yaml and /openapi.json needs
"Authorization: Bearer <token>" (checked against token_sha256). Listen
address: --listen if given, else config.yml listen, else 127.0.0.1:9473.`,
		Example: `  vmtool server
  vmtool server --listen 127.0.0.1:9473`,
		// A server: no --json.
		Annotations: map[string]string{annotationInteractive: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return serverFrom(e.cfg, listen, cmd.Flags().Changed("listen")).ListenAndServe()
		},
	}
	cmd.Flags().StringVar(&listen, "listen", config.DefaultListen, "address to listen on (overrides config.yml listen)")
	return cmd
}

// timingsLine is create's closing line, built from the events' elapsed
// stamps: each stage that started is listed in the order it began, with the
// time from its first event to its last (start to done, or to the error that
// ended it), then the time of the last event as the total. A stage whose only
// events are a lone note (no start, no hash) did not run and is left out.
// "" when there were no events.
func timingsLine(evs []app.ProgressEvent) string {
	if len(evs) == 0 {
		return ""
	}
	var tm app.CreateTimer
	for _, ev := range evs {
		tm.Add(ev)
	}
	t := tm.Timings()
	secs := func(ms int64) string { return fmt.Sprintf("%.1fs", float64(ms)/1000) }
	var parts []string
	for _, st := range t.Stages {
		parts = append(parts, st.Stage+" "+secs(st.MS))
	}
	parts = append(parts, "total "+secs(t.TotalMS))
	return "timings: " + strings.Join(parts, ", ")
}
