package cli

import (
	"bufio"
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
	f.BoolVar(&tokenStdin, "token-stdin", false, "read the token from one line of stdin")

	host := &cobra.Command{
		Use:   "host <name>",
		Short: "Print \"address ssh_user\" for a configured host",
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
			fmt.Fprintf(c.OutOrStdout(), "%s %s\n", h.Address, h.SSHUser)
			return nil
		},
	}

	cmd.AddCommand(write, host)
	return cmd
}
