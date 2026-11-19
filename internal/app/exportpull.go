package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Templates to other hosts: `templates export` makes a hypervisor's daemon
// write a template into its image cache as a qcow2 image, and `images pull
// host/<name>` streams a cached image the daemon created into a local
// directory. See docs of exportTemplate and getImageBlob in the spec.

// TemplateVolumeExporter is the daemon's side of an export: a hypervisor
// daemon's thin-pool Manager (every wrapper in internal/hosts forwards it).
// The daemon's HTTP handler owns the image cache; this owns the volume.
type TemplateVolumeExporter interface {
	// LookupTemplate resolves name (a template's name or its LV name,
	// optionally only in pool) to exactly one template, as DeleteTemplate
	// does: not found, or a conflict naming the candidates.
	LookupTemplate(name, pool string) (TemplateInfo, error)
	// ExportTemplateVolume writes the template's volume as a qcow2 file to
	// dest, a `.<name>.part` path in the image cache that does not exist. It
	// names the volume only if it is still that template.
	ExportTemplateVolume(t TemplateInfo, dest string) error
	// RecordTemplateSHA records on the template's tags the SHA-256 of the
	// image that was exported from it.
	RecordTemplateSHA(t TemplateInfo, sha256 string) error
}

// ExportEvent is one progress line of an export. Stage is "export" (the
// qcow2 file is being written: Received bytes so far of at most Total) or
// "hash" (the file is being hashed).
type ExportEvent struct {
	Stage           string
	Received, Total int64
}

// ExportedImage is the daemon's answer to an export: the cache entry.
type ExportedImage struct {
	Name   string
	SHA256 string
	Size   int64
	Path   string
}

// TemplateExporter is implemented by a Manager whose host can export a
// template into its image cache (the remote client for a hypervisor daemon).
// on gets every progress line.
type TemplateExporter interface {
	ExportTemplate(ctx context.Context, name, pool string, on func(ExportEvent)) (ExportedImage, error)
}

// ImageBlobInfo is what a host says about an image it is about to stream.
type ImageBlobInfo struct {
	Size   int64
	SHA256 string
}

// ImageBlobSource is implemented by a Manager whose host can stream an image
// of its cache that it created (the remote client). The caller checks the
// bytes against info and closes the reader.
type ImageBlobSource interface {
	OpenImageBlob(ctx context.Context, name string) (io.ReadCloser, ImageBlobInfo, error)
}

// ExportTemplateInput is a request to export a template as an image.
type ExportTemplateInput struct {
	Host string
	// Name is the template's name or LV name.
	Name string
	// Pool limits the match to one thin pool; empty is any.
	Pool       string
	OnProgress ProgressFunc
}

// ExportTemplate has the host write the template in.Name into its image
// cache as an image of the template's name. The host does the work; this
// forwards progress (stage "export").
func (s *Service) ExportTemplate(ctx context.Context, m Manager, in ExportTemplateInput) (ExportedImage, error) {
	const op = "export_template"
	if !vmtool.ValidImageName(in.Name) {
		return ExportedImage{}, invalid(op, fmt.Errorf("template name %q must be a plain name (letters, digits, '.', '_', '-')", in.Name))
	}
	x, ok := m.(TemplateExporter)
	if !ok {
		return ExportedImage{}, invalid(op, fmt.Errorf("%s has no daemon with thin pools to export templates from", hostText(in.Host)))
	}
	in.OnProgress.emitEvent(ProgressEvent{Stage: "export", Status: "start",
		Detail: fmt.Sprintf("exporting %s/%s as an image in %s's image cache", in.Host, in.Name, in.Host)})
	img, err := x.ExportTemplate(ctx, in.Name, in.Pool, func(e ExportEvent) {
		detail := "write"
		if e.Stage == "hash" {
			detail = "hash"
		}
		in.OnProgress.emitEvent(ProgressEvent{Stage: "export", Status: "progress", Detail: detail, Sent: e.Received, Total: e.Total})
	})
	if err != nil {
		in.OnProgress.emit("export", "error", err.Error(), "")
		return ExportedImage{}, wrap(op, err)
	}
	in.OnProgress.emit("export", "done", fmt.Sprintf("%s/%s is in the image cache (%d bytes, sha256 %s)", in.Host, img.Name, img.Size, img.SHA256), "")
	return img, nil
}

func hostText(h string) string {
	if h == "" {
		return "this host"
	}
	return "host " + h
}

// PullHostImageInput is a request to download an image of a host's cache.
type PullHostImageInput struct {
	Host string
	Name string
	// Dir is the directory the image is written into, as Dir/Name; it must
	// exist.
	Dir        string
	OnProgress ProgressFunc
}

// PulledImage is the result of PullHostImage. Unchanged means the file was
// already there with the same SHA-256 and nothing was written.
type PulledImage struct {
	Name      string
	SHA256    string
	Size      int64
	Path      string
	Unchanged bool
}

// PullHostImage downloads the image in.Name from the host's image cache to
// in.Dir/in.Name. The bytes go to a `.<name>.part` file in Dir, are checked
// against the size (Content-Length) and SHA-256 (X-Image-SHA256) the host
// sent, and are then linked into place with os.Link, which fails instead of
// overwriting: a file of that name that exists, or appears meanwhile, is never
// replaced. If it exists with the same SHA-256 nothing is done; with a
// different one the pull is refused (a conflict). Any failure removes the
// temporary file and nothing else. There is no force.
func (s *Service) PullHostImage(ctx context.Context, m Manager, in PullHostImageInput) (PulledImage, error) {
	const op = "images pull"
	if !vmtool.ValidImageName(in.Name) {
		return PulledImage{}, invalid(op, fmt.Errorf("image name %q must be a plain file name (letters, digits, '.', '_', '-'; not starting with '.')", in.Name))
	}
	if in.Dir == "" {
		return PulledImage{}, invalid(op, errors.New("no destination directory"))
	}
	if st, err := os.Stat(in.Dir); err != nil || !st.IsDir() {
		return PulledImage{}, invalid(op, fmt.Errorf("%s is not a directory (a host pull writes <dir>/%s)", in.Dir, in.Name))
	}
	src, ok := m.(ImageBlobSource)
	if !ok {
		return PulledImage{}, invalid(op, fmt.Errorf("%s has no daemon to pull images from", hostText(in.Host)))
	}
	dest := filepath.Join(in.Dir, in.Name)
	_, existedErr := os.Lstat(dest)
	existed := existedErr == nil

	rc, info, err := src.OpenImageBlob(ctx, in.Name)
	if err != nil {
		return PulledImage{}, wrap(op, err)
	}
	defer rc.Close()
	want := strings.ToLower(info.SHA256)
	if info.Size <= 0 {
		return PulledImage{}, badGateway(op, fmt.Errorf("%s did not state the image's size", hostText(in.Host)))
	}
	if len(want) != 64 || strings.Trim(want, "0123456789abcdef") != "" {
		return PulledImage{}, badGateway(op, fmt.Errorf("%s did not send a SHA-256 digest for the image", hostText(in.Host)))
	}
	res := PulledImage{Name: in.Name, SHA256: want, Size: info.Size, Path: dest}

	if existed {
		same, err := sameFile(dest, info.Size, want)
		if err != nil {
			return PulledImage{}, wrap(op, err)
		}
		if !same {
			return PulledImage{}, conflict(op, fmt.Errorf("%s already exists and is not the host's image; vmtool will not replace it: remove it or pick another --to", dest))
		}
		res.Unchanged = true
		in.OnProgress.emit("pull", "done", fmt.Sprintf("%s is already here (same SHA-256); nothing to do", dest), "")
		return res, nil
	}

	in.OnProgress.emit("pull", "start", fmt.Sprintf("pulling %s/%s (%d bytes) to %s", in.Host, in.Name, info.Size, dest), "")
	part := filepath.Join(in.Dir, "."+in.Name+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return PulledImage{}, conflict(op, fmt.Errorf("%s exists: another pull of %s may be running; remove it if not", part, in.Name))
		}
		return PulledImage{}, wrap(op, err)
	}
	// From here the temporary file is ours; every failure removes it, and only it.
	defer func() {
		f.Close()
		os.Remove(part)
	}()
	h := sha256.New()
	step := uploadStep(info.Size)
	next := int64(0)
	body := &progressReader{r: io.TeeReader(io.LimitReader(rc, info.Size+1), h), total: info.Size, step: step,
		progress: func(done, total int64) {
			if done >= next || done == total {
				next = done + step
				in.OnProgress.emitEvent(ProgressEvent{Stage: "pull", Status: "progress", Detail: "bytes", Sent: done, Total: total})
			}
		}}
	n, err := io.Copy(f, body)
	if err != nil {
		return PulledImage{}, badGateway(op, fmt.Errorf("download %s from %s: %w", in.Name, hostText(in.Host), err))
	}
	if n != info.Size {
		return PulledImage{}, badGateway(op, fmt.Errorf("download %s: got %s bytes, %s said %d", in.Name, sizeWord(n, info.Size), hostText(in.Host), info.Size))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return PulledImage{}, badGateway(op, fmt.Errorf("download %s: SHA-256 is %s, %s said %s; nothing was kept", in.Name, got, hostText(in.Host), want))
	}
	if err := f.Sync(); err != nil {
		return PulledImage{}, wrap(op, err)
	}
	if err := f.Close(); err != nil {
		return PulledImage{}, wrap(op, err)
	}
	// os.Link never replaces: a file that appeared since the check is kept.
	if err := os.Link(part, dest); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return PulledImage{}, conflict(op, fmt.Errorf("%s appeared while the image downloaded; vmtool will not replace it", dest))
		}
		return PulledImage{}, wrap(op, err)
	}
	in.OnProgress.emit("pull", "done", fmt.Sprintf("%s written (%d bytes, sha256 %s)", dest, n, want), "")
	return res, nil
}

func sizeWord(n, size int64) string {
	if n > size {
		return fmt.Sprintf("more than %d", size)
	}
	return fmt.Sprint(n)
}

// sameFile reports whether path is a regular file (never a symlink) of
// exactly size bytes whose SHA-256 is want.
func sameFile(path string, size int64, want string) (bool, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() || st.Size() != size {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}
