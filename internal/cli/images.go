package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jaredeh/vmtool/internal/app"
)

// storeImagePaths are the image commands that need the store. Like the
// top-level storeCommands they decide the mode at their start; `images list`
// decides it only with --store (it lists a host's pools otherwise).
var storeImagePaths = map[string]bool{"images push": true, "images pull": true}

// transferProgress renders a push or pull on w as one updating line.
func transferProgress(w io.Writer, verb string) func(done, total int64) {
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		fmt.Fprintf(w, "\r%s: %3d%% (%d / %d MiB)", verb, done*100/total, done>>20, total>>20)
		if done >= total {
			fmt.Fprintln(w)
		}
	}
}

func imagesPushCmd(e *env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "push <file>",
		Short: "Upload a base image file to the store",
		Long: "Upload a base image file to the store, where every hypervisor can fetch it.\n" +
			"The file is hashed first (the store takes the digest before the bytes; a file hashed before\n" +
			"is not hashed again) and streamed once; the store indexes it only when digest and size match.\n" +
			"Needs a store (server: in config.yml and VMTOOL_SERVER_TOKEN).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errw := cmd.ErrOrStderr()
			img, err := e.service("").PushImage(cmd.Context(), args[0], name,
				func() { fmt.Fprintf(errw, "hashing %s...\n", args[0]) },
				transferProgress(errw, "pushing"))
			if err != nil {
				return printAppErr(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Image %q is in the store (%d bytes, sha256 %s)\n", img.Name, img.Size, img.SHA256)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name in the store (default: the file's base name)")
	return cmd
}

func imagesPullCmd(e *env) *cobra.Command {
	var to string
	var force bool
	cmd := &cobra.Command{
		Use:   "pull <name>",
		Short: "Download a base image from the store to a file",
		Long: "Download a base image from the store. It is written to a .part file beside the destination,\n" +
			"checked against the store's SHA-256 and size, and renamed. The default destination is\n" +
			"<image_dir>/<name> when image_dir is set in config.yml, else ./<name>. An existing file is\n" +
			"refused unless --force. Needs a store.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dest := to
			if dest == "" {
				if dir := e.imageDir(); dir != "" {
					dest = filepath.Join(dir, filepath.Base(name))
				} else {
					dest = filepath.Base(name)
				}
			}
			img, err := e.service("").PullImage(cmd.Context(), name, dest, force, transferProgress(cmd.ErrOrStderr(), "pulling"))
			if err != nil {
				return printAppErr(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Image %q written to %s (%d bytes, sha256 %s)\n", img.Name, dest, img.Size, img.SHA256)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination file (default: image_dir/<name>, else ./<name>)")
	cmd.Flags().BoolVar(&force, "force", false, "replace the destination if it exists")
	return cmd
}

// writeStoreImages prints the store's images, one per line.
func writeStoreImages(out io.Writer, imgs []app.StoreImage) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSIZE\tSHA256\tUPDATED")
	for _, i := range imgs {
		sum := i.SHA256
		if len(sum) > 12 {
			sum = sum[:12]
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", i.Name, i.Size, sum, i.Updated.UTC().Format("2006-01-02 15:04"))
	}
	return w.Flush()
}
