package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jaredeh/vmtool/internal/config"
	"github.com/spf13/cobra"
)

func cwdDir() (string, error) { return os.Getwd() }

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
		Args:  cobra.NoArgs,
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
				return err
			}
			if err := config.Write(dir, token, opts); err != nil {
				return err
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
	f.BoolVar(&tokenStdin, "token-stdin", false, "read the token from one line of stdin")

	serverURL := &cobra.Command{
		Use:   "server-url",
		Short: "Print this client's server.url (nothing when it has no store)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			cfg, err := config.Load(dir)
			if err != nil {
				return err
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
		Args:  cobra.ExactArgs(1),
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
			fmt.Fprintf(c.OutOrStdout(), "%s %s %d\n", h.Address, h.SSHUser, port)
			return nil
		},
	}

	var (
		sh     config.Host
		shPort int
	)
	setHost := &cobra.Command{
		Use:   "set-host <name>",
		Short: "Add or replace one hosts: entry in config.yml, keeping everything else",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir, err := dirFn()
			if err != nil {
				return err
			}
			// Required flags are checked here, not with MarkFlagRequired:
			// the gate test runs every command with positional dummies only.
			if sh.Address == "" || sh.SSHUser == "" {
				return errors.New("set-host needs --address and --ssh-user")
			}
			sh.Port = shPort
			if err := config.SetHost(dir, args[0], sh); err != nil {
				return err
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

	cmd.AddCommand(write, host, setHost, serverURL)
	return cmd
}
