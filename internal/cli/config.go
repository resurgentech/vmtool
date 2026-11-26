package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/spf13/cobra"
)

func cwdDir() (string, error) { return os.Getwd() }

// tokenIsFileKey reports whether token equals the key in the token file. A
// child of vmtool carries the caller's key as VMTOOL_TOKEN, so `config write`
// must not hash it. The file is read directly (even with a mode ReadTokenFile
// refuses, or through a symlink): only the comparison matters, and neither
// value is ever shown. Reading more files than the gate would only refuses
// more.
func tokenIsFileKey(dir, token string) bool {
	p, err := config.TokenFilePath(dir)
	if err != nil {
		return false
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(p)
	return err == nil && strings.TrimSpace(string(b)) == token
}

// configCmd is the `vmtool config` group. dirFn returns the directory holding
// config.yml and .env (the working directory in production).
func configCmd(dirFn func() (string, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage config.yml",
	}

	var (
		opts       config.WriteOptions
		tokenStdin bool
	)
	write := &cobra.Command{
		Use:   "write",
		Short: "Create or update config.yml, setting token_sha256 from the token",
		Example: `  vmtool config write --name ws
  printf '%s\n' "$TOKEN" | vmtool config write --json --token-stdin --role hypervisor

  Tip: with --json stdout is {"wrote": "config.yml"}. The token is read from
  VMTOOL_TOKEN, .env or --token-stdin and is never printed, nor is its hash.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			var token string
			if tokenStdin {
				line, _ := bufio.NewReader(c.InOrStdin()).ReadString('\n')
				token = strings.TrimSpace(line)
				if token == "" {
					return config.ErrNoToken
				}
			} else if token, err = config.Token(dir); err != nil {
				// Never the token file: a personal key must not become token_sha256.
				if p, perr := config.TokenFilePath(dir); perr == nil && errors.Is(err, config.ErrNoToken) {
					if _, serr := os.Lstat(p); serr == nil {
						return fmt.Errorf("%w; the token file is not used here: export the operator token (VMTOOL_TOKEN) for this shell, or pass --token-stdin", err)
					}
				}
				return err
			}
			if tokenIsFileKey(dir, token) {
				return errors.New("the token is the personal key in the token file and must not become token_sha256; export the operator token (VMTOOL_TOKEN) for this shell, or pass it with --token-stdin")
			}
			if err := config.Write(dir, token, opts); err != nil {
				return err
			}
			if jsonMode(c) {
				return emit(c, map[string]string{"wrote": config.FileName})
			}
			fmt.Fprintf(c.OutOrStdout(), "wrote %s\n", config.FileName)
			return nil
		},
	}
	f := write.Flags()
	f.StringVar(&opts.Name, "name", "", "host name (default: short hostname)")
	f.StringVar(&opts.Role, "role", "", "workstation or hypervisor")
	f.StringVar(&opts.Listen, "listen", "", "listen address (default 127.0.0.1:9473)")
	f.StringVar(&opts.TLSCert, "tls-cert", "", "TLS certificate path")
	f.StringVar(&opts.TLSKey, "tls-key", "", "TLS key path")
	f.StringVar(&opts.StoreURL, "store-url", "", "https URL of the store a hypervisor daemon fetches images from")
	f.StringVar(&opts.StoreTokenFile, "store-token-file", "", "path of the file holding the host token a hypervisor daemon reports to the store with (a path, never the token)")
	f.StringVar(&opts.KeyRequestURL, "key-request-url", "", "absolute https URL where a tool files a request for an API key (plain http only to a loopback host); the daemon reports it at GET /v1/key-request")
	f.StringVar(&opts.PersonalKeysURL, "personal-keys-url", "", "URL of the pulled key list (personal_keys.url); needs --personal-keys-cache-file (or one already in config.yml)")
	f.StringVar(&opts.PersonalKeysTokenFile, "personal-keys-token-file", "", "path of the file holding the pull token for the key list (personal_keys.token_file; a path, never the token)")
	f.StringVar(&opts.PersonalKeysCacheFile, "personal-keys-cache-file", "", "path where the last good key list is kept (personal_keys.cache_file; a path)")
	f.BoolVar(&opts.NoPersonalKeys, "no-personal-keys", false, "remove the whole personal_keys block first (the key list off), then apply the other personal-keys flags and --key-request-url if given")
	f.BoolVar(&opts.NetworkHome, "network-home", false, "write network_home: true, so this daemon refuses the VM-creating routes (the host is a network home)")
	f.BoolVar(&tokenStdin, "token-stdin", false, "read the token from one line of stdin")

	serverURL := &cobra.Command{
		Use:   "server-url",
		Short: "Print this client's server.url (nothing when it has no store)",
		Example: `  vmtool config server-url
  vmtool config server-url --json

  Tip: with --json stdout is {"url": "<server.url>"}; url is empty when the
  client has no store.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			if jsonMode(c) {
				url := ""
				if cfg.StoreConfigured() {
					url = cfg.Server.URL
				}
				return emit(c, map[string]string{"url": url})
			}
			if cfg.StoreConfigured() {
				fmt.Fprintln(c.OutOrStdout(), cfg.Server.URL)
			}
			return nil
		},
	}

	host := &cobra.Command{
		Use:   "host <name>",
		Short: "Print \"address ssh_user port\" for a configured host",
		Example: `  vmtool config host hv1
  vmtool config host --json hv1

  Tip: with --json stdout is {"name", "address", "ssh_user", "port"}; port is
  the daemon's default when the host has none.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			h, ok := cfg.Hosts[args[0]]
			if !ok {
				return fmt.Errorf("unknown host %q in %s", args[0], config.FileName)
			}
			port := h.Port
			if port == 0 {
				port = config.DefaultPort
			}
			if jsonMode(c) {
				return emit(c, map[string]any{"name": args[0], "address": h.Address, "ssh_user": h.SSHUser, "port": port})
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %s %d\n", h.Address, h.SSHUser, port)
			return nil
		},
	}

	hostVMs := &cobra.Command{
		Use:   "host-vms <name>",
		Short: "Print \"true\" or \"false\": the client's hosts.<name>.vms (false for a network home)",
		Example: `  vmtool config host-vms hv1
  vmtool config host-vms --json hv1

  Tip: with --json stdout is {"name", "vms"}; vms is false for a network home.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			h, ok := cfg.Hosts[args[0]]
			if !ok {
				return fmt.Errorf("unknown host %q in %s", args[0], config.FileName)
			}
			if jsonMode(c) {
				return emit(c, map[string]any{"name": args[0], "vms": h.HoldsVMs()})
			}
			fmt.Fprintln(c.OutOrStdout(), h.HoldsVMs())
			return nil
		},
	}

	var (
		sh     config.Host
		shPort int
		shVMs  bool
	)
	setHost := &cobra.Command{
		Use:   "set-host <name>",
		Short: "Add or replace one hosts: entry in config.yml, keeping everything else",
		Example: `  vmtool config set-host hv1 --address 192.0.2.11 --ssh-user ubuntu
  vmtool config set-host --json hv1 --vms=false

  Tip: with --json stdout is {"host", "file"}: the host entry written and the
  file it is in.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			// Required flags are checked here, not with MarkFlagRequired:
			// the gate test runs every command with positional dummies only.
			f := c.Flags()
			if f.Changed("vms") && !f.Changed("address") && !f.Changed("ssh-user") &&
				!f.Changed("port") && !f.Changed("default-pool") {
				if err := config.SetHostVMs(dir, args[0], shVMs); err != nil {
					return err
				}
				if jsonMode(c) {
					return emit(c, map[string]string{"host": args[0], "file": config.FileName})
				}
				fmt.Fprintf(c.OutOrStdout(), "set host %s vms in %s\n", args[0], config.FileName)
				return nil
			}
			if sh.Address == "" || sh.SSHUser == "" {
				return errors.New("set-host needs --address and --ssh-user")
			}
			sh.Port = shPort
			if c.Flags().Changed("vms") {
				sh.VMs = &shVMs
			}
			if err := config.SetHost(dir, args[0], sh); err != nil {
				return err
			}
			if jsonMode(c) {
				return emit(c, map[string]string{"host": args[0], "file": config.FileName})
			}
			fmt.Fprintf(c.OutOrStdout(), "set host %s in %s\n", args[0], config.FileName)
			return nil
		},
	}
	sf := setHost.Flags()
	sf.StringVar(&sh.Address, "address", "", "host address (required)")
	sf.StringVar(&sh.SSHUser, "ssh-user", "", "SSH user (required)")
	sf.IntVar(&shPort, "port", 0, "daemon port (default 9473)")
	sf.StringVar(&sh.DefaultPool, "default-pool", "", "pool for new VM disks on this host")

	sf.BoolVar(&shVMs, "vms", true, "false marks a network home (hosts.<name>.vms: false); true removes the key; not given leaves it as it is")

	cmd.AddCommand(write, host, hostVMs, setHost, serverURL)
	return cmd
}
