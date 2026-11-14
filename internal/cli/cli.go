package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/server"
	"github.com/jaredeh/vmtool/internal/tui"
	"github.com/jaredeh/vmtool/pkg/vmtool"
	"github.com/spf13/cobra"
)

func Execute() error {
	dir, err := cwdDir()
	if err != nil {
		return err
	}
	err = newRootCmd(&env{dir: dir}).Execute()
	if err == nil {
		return nil
	}
	var remote remoteExitError
	if errors.As(err, &remote) {
		os.Exit(remote.code)
	}
	return err
}

// env is what the root command shares with its subcommands.
type env struct {
	// dir is the working directory: config.yml, .env and .machines/.
	dir string
	// cfg and token are set by the gate before any RunE. token is the
	// verified VMTOOL_TOKEN; never print or log it.
	cfg   *config.Config
	token string
}

// gateError is the gate's refusal. Error is one line naming the fix; Unwrap
// gives the config error (ErrNotFound, ErrNoToken, ErrTokenMismatch, ...).
type gateError struct {
	msg string
	err error
}

func (g *gateError) Error() string { return g.msg }
func (g *gateError) Unwrap() error { return g.err }

// gate loads config.yml and the token and checks one against the other. It
// fills e.cfg and e.token, or returns a *gateError.
func (e *env) gate() error {
	tok, err := config.Token(e.dir)
	switch {
	case errors.Is(err, config.ErrNoToken):
		return &gateError{err: err, msg: fmt.Sprintf("vmtool: no %s in the environment or %s; run 'just token' (new) or 'just token-fetch' (from OpenBao)",
			config.TokenEnv, filepath.Join(e.dir, config.EnvFileName))}
	case err != nil:
		return &gateError{err: err, msg: "vmtool: " + err.Error()}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if !config.Check(tok, cfg.TokenSHA256) {
		return &gateError{err: config.ErrTokenMismatch, msg: fmt.Sprintf("vmtool: %s does not match token_sha256 in %s; run 'just token-fetch', or 'just install' after 'just token'",
			config.TokenEnv, config.FileName)}
	}
	e.cfg, e.token = cfg, tok
	return nil
}

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
// help, completion and shell-completion request commands, and `config write`
// (the bootstrap that creates config.yml; it still needs a token present).
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

Every command except help, completion and 'config write' needs config.yml in
the working directory and VMTOOL_TOKEN (environment, else .env) matching its
token_sha256. 'server' needs config.yml with token_sha256 but no local token;
it checks the bearer token of every request instead.`,
		// The token gate. No subcommand defines its own PersistentPreRun(E)
		// (cobra would run only the nearest one); TestGateEveryCommand checks
		// every command in the tree is refused without a valid token.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if exemptFromGate(cmd) {
				return nil
			}
			gate := e.gate
			if isServerCmd(cmd) {
				gate = e.serverGate
			}
			if err := gate(); err != nil {
				// One line, printed once by main; no usage block.
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return err
			}
			return nil
		},
	}

	root.AddCommand(
		createCmd(e),
		startCmd(e),
		stopCmd(e),
		resumeCmd(e),
		poweroffCmd(e),
		deleteCmd(e),
		listCmd(e),
		hostsCmd(e),
		infoCmd(e),
		sshCmd(e),
		tunnelCmd(e),
		sshConfigCmd(e),
		cmdCmd(e),
		rebootCmd(e),
		autostartCmd(e),
		interactiveCmd(e),
		networksCmd(e),
		bridgesCmd(e),
		playbookCmd(e),
		imagesCmd(e),
		poolCmd(e),
		migrateDiskCmd(e),
		resizeDiskCmd(e),
		addDiskCmd(e),
		metalCmd(e),
		machinesCmd(e),
		serverCmd(e),
		configCmd(func() (string, error) { return e.dir, nil }),
	)
	return root
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
		LocalImages:   e.localImages,
	}).WithTunnel(t)
}

// localImages finds a base image in this machine's own libvirt pools: create
// on a remote host uploads it from there. The local Manager is opened only
// for the lookup (only a remote create asks).
func (e *env) localImages(name string) (string, error) {
	r := e.resolver()
	m, release, err := r.Resolve(r.Local())
	if err != nil {
		return "", err
	}
	defer release()
	return m.ImagePath(name)
}

// uploadProgress renders create's image-upload events on stderr as one
// updating line; it reports whether it handled ev.
func uploadProgress(w io.Writer, ev app.ProgressEvent) bool {
	if ev.Stage != "upload" {
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
		fmt.Fprintf(w, "\ruploading: %3d%% (%d / %d MiB)", pct, ev.Sent>>20, ev.Total>>20)
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
		extraDiskSizeGB uint
		extraDiskPool   string
		extraVars       []string
	)

	cmd := &cobra.Command{
		Use:   "create <host/name> <image>",
		Short: "Define and start a new VM",
		Long: `Define and start a new VM from a disk image on host.

By default the image is cloned so the original is unchanged.
Use --noclone to boot directly from the image; any changes (e.g. package
upgrades) will be written back to the source image.

  vmtool create jetfire/web ubuntu-24.04.qcow2`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			extra, err := parseExtraVars(extraVars)
			if err != nil {
				return err
			}
			info, err := e.service(inventory).CreateVM(cmd.Context(), m, app.CreateVMInput{
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
				ExtraDiskSizeGB: extraDiskSizeGB,
				ExtraDiskPool:   extraDiskPool,
				ExtraVars:       extra,
				OnProgress: func(ev app.ProgressEvent) {
					if uploadProgress(os.Stderr, ev) {
						return
					}
					if ev.Output != "" {
						fmt.Print(ev.Output)
					}
					if (ev.Status == "done" || ev.Status == "error") && ev.Detail != "" {
						fmt.Println(ev.Detail)
					}
				},
			})
			if info != nil {
				fmt.Printf("VM %q created and started\n", ref.String())
				if info.IP != "" {
					fmt.Printf("IP: %s\n", info.IP)
				}
			}
			return printAppErr(err)
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
	cmd.Flags().StringVar(&pool, "pool", "", "storage pool for the cloned disk (default: default)")
	cmd.Flags().BoolVar(&noclone, "noclone", false, "boot directly from the image without cloning (changes persist to source)")
	cmd.Flags().UintVar(&extraDiskSizeGB, "extra-disk-size", 0, "create a second empty virtio disk (vdb) named <name>-2.qcow2")
	cmd.Flags().StringVar(&extraDiskPool, "extra-disk-pool", "", "storage pool for the extra disk (default: same as --pool)")
	cmd.Flags().StringArrayVar(&extraVars, "extra-var", nil, "ansible extra-var key=value for --playbook (repeatable). deviceid defaults to the VM name")

	return cmd
}

func startCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "start <host/name>",
		Short: "Start a stopped VM",
		Args:  cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if _, err := e.service("").StartVM(cmd.Context(), m, ref.Name); err != nil {
				return err
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
		Args:  cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if _, err := e.service("").ResumeVM(cmd.Context(), m, ref.Name); err != nil {
				return err
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
		Args:  cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if _, err := e.service("").StopVM(cmd.Context(), m, ref.Name); err != nil {
				return err
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
		Args:  cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if _, err := e.service("").PoweroffVM(cmd.Context(), m, ref.Name); err != nil {
				return err
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
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if err := e.service("").DeleteVM(cmd.Context(), m, ref, noclone); err != nil {
				return err
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
		Args:  cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := e.service("").GetVM(cmd.Context(), m, ref.Name)
			if err != nil {
				return err
			}
			ip := info.IP
			if ip == "" {
				ip = "-"
			}
			autostart := "off"
			if info.Autostart {
				autostart = "on"
			}
			fmt.Printf("Name:      %s\n", info.Name)
			fmt.Printf("State:     %s\n", info.State)
			fmt.Printf("VCPUs:     %d\n", info.VCPUs)
			fmt.Printf("Memory:    %d GiB\n", info.Memory)
			fmt.Printf("Autostart: %s\n", autostart)
			fmt.Printf("IP:        %s\n", ip)
			return nil
		}),
	}
}

func sshCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <host/name> [-- ssh-args...]",
		Short: "SSH into a VM using stored .machines credentials",
		Long: `SSH into a running VM. If .machines/<host>/<name>/ exists, uses the
username and key stored there. Otherwise falls back to packer. Host key
checking is disabled.

  vmtool ssh jetfire/web`,
		Args: cobra.MinimumNArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := m.Info(ref.Name)
			if err != nil {
				return err
			}
			if info.IP == "" {
				return fmt.Errorf("VM %q has no IP address", ref.String())
			}

			c, err := e.service("").SSHCmd(ref, args[1:]...)
			if err != nil {
				return err
			}
			return c.Run()
		}),
	}
}

func cmdCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cmd <host/name> <command> [args...]",
		Short: "Run a command on a VM over SSH",
		Long: `Run a command on a running VM and pass through stdin, stdout, stderr,
and the remote exit code. Uses .machines credentials when present.

  vmtool cmd jetfire/web uname -a
  vmtool cmd jetfire/web -- sh -c 'echo hi | wc -c'`,
		Args:          cobra.MinimumNArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			info, err := m.Info(ref.Name)
			if err != nil {
				return err
			}
			if info.IP == "" {
				return fmt.Errorf("VM %q has no IP address", ref.String())
			}

			c, err := e.service("").RemoteCmd(ref, args[1:])
			if err != nil {
				return err
			}
			// RemoteCmd leaves stdio unset (it is also used for captured runs).
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			err = c.Run()
			if err == nil {
				return nil
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return remoteExitError{code: ee.ExitCode()}
			}
			return err
		}),
	}
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func rebootCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "reboot <host/name>",
		Short: "Reboot a running VM",
		Long: `Ask the guest to reboot (ACPI). The domain stays defined.

Unlike stop (ACPI shutdown, then shutoff) or poweroff (force destroy),
the VM comes back up on its own if the guest handles the reboot.`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if _, err := e.service("").RebootVM(cmd.Context(), m, ref.Name); err != nil {
				return err
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
		Args:  cobra.ExactArgs(2),
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
			if _, err := e.service("").SetAutostart(cmd.Context(), m, ref.Name, enable); err != nil {
				return err
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

func interactiveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "i",
		Short:   "Launch interactive TUI",
		Aliases: []string{"interactive"},
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := e.tunnel()
			if err != nil {
				return err
			}
			return tui.Run(e.resolver(), t)
		},
	}
}

func networksCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "networks",
		Short: "List available libvirt networks",
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		nets, err := e.service("").ListNetworks(cmd.Context(), m)
		if err != nil {
			return err
		}
		for _, n := range nets {
			fmt.Println(n)
		}
		return nil
	})
	return cmd
}

func bridgesCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bridges",
		Short: "List host bridges usable with --net-type=bridge",
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		brs, err := e.service("").ListBridges(cmd.Context(), m)
		if err != nil {
			return err
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
		Args:  cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			extra, err := parseExtraVars(extraVars)
			if err != nil {
				return err
			}
			res, err := e.service("").RunPlaybook(cmd.Context(), m, ref, args[1], vmtool.Auth{User: sshUser, Password: sshPass}, extra)
			if res != nil && res.Output != "" {
				fmt.Print(res.Output)
			}
			if err != nil {
				return printAppErr(err)
			}
			fmt.Printf("Playbook %s completed\n", args[1])
			return nil
		}),
	}

	cmd.Flags().StringVar(&sshUser, "ssh-user", "packer", "SSH username if no .machines entry exists")
	cmd.Flags().StringVar(&sshPass, "ssh-pass", "packer", "SSH password if no .machines entry exists")
	cmd.Flags().StringArrayVar(&extraVars, "extra-var", nil, "ansible extra-var key=value (repeatable). deviceid defaults to the VM name")

	return cmd
}

func imagesCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Manage disk images",
	}
	cmd.AddCommand(imagesListCmd(e), imagesDeleteCmd(e))
	return cmd
}

func imagesListCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available disk images grouped by pool",
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		byPool, err := e.service("").ListImages(cmd.Context(), m)
		if err != nil {
			return err
		}
		pools := make([]string, 0, len(byPool))
		for p := range byPool {
			pools = append(pools, p)
		}
		sort.Strings(pools)
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
	return cmd
}

func imagesDeleteCmd(e *env) *cobra.Command {
	var pool, host string
	cmd := &cobra.Command{
		Use:   "delete <image>",
		Short: "Delete a disk image from a storage pool",
		Args:  cobra.ExactArgs(1),
		RunE: withHost(e, &host, func(m app.Manager, cmd *cobra.Command, args []string) error {
			if err := e.service("").DeleteImage(cmd.Context(), m, args[0], pool); err != nil {
				return err
			}
			fmt.Printf("Image %q deleted from pool %q\n", args[0], pool)
			return nil
		}),
	}
	cmd.Flags().StringVar(&pool, "pool", "default", "storage pool containing the image")
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
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		pools, err := e.service("").ListPools(cmd.Context(), m)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tPATH\tACTIVE")
		for _, p := range pools {
			active := "no"
			if p.Active {
				active = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, p.Path, active)
		}
		return w.Flush()
	})
	return cmd
}

func poolCreateCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name> <path>",
		Short: "Create a new directory-type storage pool",
		Args:  cobra.ExactArgs(2),
	}
	host := hostFlag(cmd)
	cmd.RunE = withHost(e, host, func(m app.Manager, cmd *cobra.Command, args []string) error {
		if err := e.service("").CreatePool(cmd.Context(), m, args[0], args[1]); err != nil {
			return err
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
redefine the domain with the new path, then restart (if it was running).`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			vmName := args[0]
			pool := args[1]
			fmt.Printf("Migrating disk for VM %q to pool %q...\n", vmName, pool)
			if _, err := e.service("").MigrateDisk(cmd.Context(), m, ref.Name, pool); err != nil {
				return err
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
the root partition and filesystem via SSH.`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			vmName := args[0]
			sizeGB, err := strconv.ParseUint(args[1], 10, 32)
			if err != nil || sizeGB == 0 {
				return fmt.Errorf("invalid size %q: must be a positive integer (GB)", args[1])
			}

			fmt.Printf("Resizing disk for VM %q to %dGB...\n", vmName, sizeGB)
			res, err := e.service("").ResizeDisk(cmd.Context(), m, ref, uint(sizeGB), vmtool.Auth{User: sshUser, Password: sshPass})
			if err != nil {
				return err
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
		Long: `Create an empty qcow2 volume and attach it as the next virtio disk (vdb, vdc, …).

Volume name is <vm>-2.qcow2 for the first extra disk, <vm>-3.qcow2 if called
again, and so on. If the VM is running the disk is hot-plugged and persisted.
If it is shut off the change is persisted and appears on the next start. The
guest sees a raw block device; it is not partitioned or formatted.

  vmtool add-disk jetfire/web 50
  vmtool add-disk jetfire/web 100 --pool fast`,
		Args: cobra.ExactArgs(2),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			vmName := args[0]
			sizeGB, err := strconv.ParseUint(args[1], 10, 32)
			if err != nil || sizeGB == 0 {
				return fmt.Errorf("invalid size %q: must be a positive integer (GB)", args[1])
			}

			res, err := e.service("").AddDisk(cmd.Context(), m, ref.Name, uint(sizeGB), pool)
			if err != nil {
				return err
			}
			fmt.Printf("Attached %s (%dGB) to VM %q as %s\n", res.Path, sizeGB, vmName, res.Target)
			return nil
		}),
	}

	cmd.Flags().StringVar(&pool, "pool", "", "storage pool for the new volume (default: default)")
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return serverFrom(e.cfg, listen, cmd.Flags().Changed("listen")).ListenAndServe()
		},
	}
	cmd.Flags().StringVar(&listen, "listen", config.DefaultListen, "address to listen on (overrides config.yml listen)")
	return cmd
}
