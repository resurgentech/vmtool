package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// storeImagePaths are the image commands that need the store. Like the
// top-level storeCommands they decide the mode at their start; `images list`
// decides it only with --store (it lists a host's pools otherwise).
// `templates prepare` syncs the image to the host as create does; the other
// template commands never use the store.
var storeImagePaths = map[string]bool{"images push": true, "images pull": true, "templates prepare": true}

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
				return printAppErrFor(cmd, err)
			}
			fmt.Fprintf(humanOut(cmd), "Image %q is in the store (%d bytes, sha256 %s)\n", img.Name, img.Size, img.SHA256)
			if jsonMode(cmd) {
				return emit(cmd, storeImageObject(*img))
			}
			return nil
		},
	}
	cmd.Example = `  vmtool images push ubuntu-24.04.qcow2
  vmtool images push --json ubuntu-24.04.qcow2 --name base.qcow2

  Tip: push blocks until the file is hashed and uploaded. With --json stdout
  is {"name", "size", "sha256"} of the store's image; the hashing and upload
  progress go to stderr.`
	cmd.Flags().StringVar(&name, "name", "", "image name in the store (default: the file's base name)")
	return cmd
}

// isHostImageRef reports whether an `images pull` argument names an image of a
// host (host/<name>) rather than of the store: image names cannot contain '/'.
func isHostImageRef(arg string) bool { return strings.Contains(arg, "/") }

func imagesPullCmd(e *env) *cobra.Command {
	var to string
	var force bool
	cmd := &cobra.Command{
		Use:   "pull <name> | <host>/<name>",
		Short: "Download a base image from the store, or a cached image from a host",
		Long: "Two forms, told apart by the argument: image names cannot contain '/'.\n\n" +
			"  <name>         (a bare name) download the image from the store. It is written to a .part file\n" +
			"                 beside the destination, checked against the store's SHA-256 and size, and\n" +
			"                 renamed. The default destination is <image_dir>/<name> when image_dir is set in\n" +
			"                 config.yml, else ./<name>; --to is the destination file. An existing file is\n" +
			"                 refused unless --force. Needs a store.\n\n" +
			"  <host>/<name>  download an image from the image cache of <host>'s daemon, one the daemon created\n" +
			"                 itself (an upload), or a template's export on its NVMe array under the template's\n" +
			"                 name (see below); never a file it did not create.\n" +
			"                 It is written to <dir>/<name>, where <dir> is --to (a directory) or image_dir, else\n" +
			"                 the current directory: a .part file, checked against the size and SHA-256 the\n" +
			"                 daemon sent, then linked into place. A file of that name that is already there with\n" +
			"                 the same SHA-256 is left as it is; with a different one the pull is refused, and\n" +
			"                 there is no --force. Needs no store.\n\n" +
			"A template is per host. 'vmtool templates export <host>/<name>' writes a saved template as a qcow2 volume on\n" +
			"<host>'s NVMe array, not into its image cache, and keeps it 24 hours; 'images pull <host>/<name>' (the\n" +
			"template's name) then brings it to this machine's image_dir, from where a create on any host uses it as a\n" +
			"base image, and 'vmtool images push' (or 'templates export --push', which skips the local file) puts it in\n" +
			"the store. When the daemon does not know the export's SHA-256 yet it computes it while it sends and sends\n" +
			"it as a trailer after the last byte; the pull compares then, and on a mismatch, or when none arrives,\n" +
			"nothing is kept. A name that is neither a cached image nor an export is refused (exports are removed\n" +
			"after 24 hours). Only an image the daemon created can be pulled this way.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if isHostImageRef(args[0]) {
				if force {
					return errors.New("--force applies to a store pull only: a pull from a host never replaces a file")
				}
				return pullFromHost(cmd, e, args[0], to)
			}
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
				return printAppErrFor(cmd, err)
			}
			fmt.Fprintf(humanOut(cmd), "Image %q written to %s (%d bytes, sha256 %s)\n", img.Name, dest, img.Size, img.SHA256)
			if jsonMode(cmd) {
				return emit(cmd, pulledJSON{Name: img.Name, Path: dest, Size: img.Size, SHA256: img.SHA256})
			}
			return nil
		},
	}
	cmd.Example = `  vmtool images pull ubuntu-24.04.qcow2 --to ./base.qcow2
  vmtool images pull --json hv1/t1-tpl --to /srv/images

  Tip: pull blocks until the file is written and checked. With --json stdout
  is {"name", "path", "size", "sha256"} (a pull from a host adds "host" and
  "unchanged"); the progress goes to stderr.`
	cmd.Flags().StringVar(&to, "to", "", "store pull: destination file (default: image_dir/<name>, else ./<name>); host pull: destination directory (default: image_dir, else .)")
	cmd.Flags().BoolVar(&force, "force", false, "store pull only: replace the destination if it exists")
	return cmd
}

// pullFromHost is `images pull <host>/<name>`.
func pullFromHost(cmd *cobra.Command, e *env, ref, to string) error {
	host, name, ok := strings.Cut(ref, "/")
	if !ok || !vmtool.ValidMachineName(host) || !vmtool.ValidImageName(name) {
		return fmt.Errorf("%q: want <host>/<image name> (letters, digits, '.', '_', '-'), or a bare image name for the store", ref)
	}
	dir := to
	if dir == "" {
		if dir = e.imageDir(); dir == "" {
			dir = "."
		}
	}
	m, release, err := e.resolver().Resolve(host)
	if err != nil {
		return err
	}
	defer release()
	errw := cmd.ErrOrStderr()
	tp := transferProgress(errw, "pulling")
	img, err := e.service("").PullHostImage(cmd.Context(), m, app.PullHostImageInput{Host: host, Name: name, Dir: dir,
		OnProgress: func(ev app.ProgressEvent) {
			switch {
			case ev.Stage != "pull":
			case ev.Status == "start":
				fmt.Fprintln(errw, ev.Detail)
			case ev.Status == "progress":
				tp(ev.Sent, ev.Total)
			}
		}})
	if err != nil {
		return printAppErrFor(cmd, err)
	}
	out := humanOut(cmd)
	if img.Unchanged {
		fmt.Fprintf(out, "Image %s/%s is already at %s (same SHA-256 %s); nothing to do\n", host, img.Name, img.Path, img.SHA256)
	} else {
		fmt.Fprintf(out, "Image %s/%s written to %s (%d bytes, sha256 %s)\n", host, img.Name, img.Path, img.Size, img.SHA256)
	}
	if jsonMode(cmd) {
		return emit(cmd, pulledJSON{Host: host, Name: img.Name, Path: img.Path, Size: img.Size, SHA256: img.SHA256, Unchanged: &img.Unchanged})
	}
	return nil
}

// storeImageJSON is an image of the store: its name, size and digest, and when
// it was last written (omitted when the answer did not carry it).
type storeImageJSON struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Updated string `json:"updated,omitempty"`
}

func storeImageObject(i app.StoreImage) storeImageJSON {
	out := storeImageJSON{Name: i.Name, Size: i.Size, SHA256: i.SHA256}
	if !i.Updated.IsZero() {
		out.Updated = i.Updated.UTC().Format(time.RFC3339)
	}
	return out
}

// pulledJSON is `images pull --json`: the file written. A pull from a host
// names the host and says whether the file was already there (unchanged).
type pulledJSON struct {
	Host      string `json:"host,omitempty"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Unchanged *bool  `json:"unchanged,omitempty"`
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
