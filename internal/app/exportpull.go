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

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// Templates to other hosts: `templates export` makes a hypervisor's daemon
// write a template into its image cache as a qcow2 image, and `images pull
// host/<name>` streams a cached image the daemon created into a local
// directory. See docs of exportTemplate and getImageBlob in the spec.

// ExportVolume is a template's export volume: a thick LV in the array's
// volume group holding the template as a qcow2 file, kept for 24 hours from
// Created. SHA256 is empty until the digest is known. Reused is true when
// ExportTemplateVolume found it instead of making it.
type ExportVolume struct {
	vmtool.ThinExport
	Reused bool
}

// TemplateVolumeExporter is the daemon's side of an export: a hypervisor
// daemon's thin-pool Manager (every wrapper in internal/hosts forwards it).
// The daemon's HTTP handler owns the request; this owns the volumes.
type TemplateVolumeExporter interface {
	// LookupTemplate resolves name (a template's name or its LV name,
	// optionally only in pool) to exactly one template, as DeleteTemplate
	// does: not found, or a conflict naming the candidates.
	LookupTemplate(name, pool string) (TemplateInfo, error)
	// ExportTemplateVolume returns the template's export volume: the one that
	// exists (Reused), or a new one the template is converted into. on, when
	// not nil, gets the converter's percent done. With no room on the array
	// the error is a conflict matching vmtool.ErrThinNoRoom and nothing was
	// written. It names the volume only if the template is still that template.
	ExportTemplateVolume(t TemplateInfo, on func(pct float64)) (ExportVolume, error)
	// OpenExport reads the export volume name: exactly Bytes bytes of its
	// qcow2 file, and the volume as tagged so far. A short read is an error
	// from Read, never a plain EOF. The caller closes the reader.
	OpenExport(name string) (io.ReadCloser, ExportVolume, error)
	// RecordTemplateSHA records on the template's tags, and on its export
	// volume's, the SHA-256 of the image that was exported from it.
	RecordTemplateSHA(t TemplateInfo, sha256 string) error
}

// ExportEvent is one progress line of an export. Stage is "export" (the
// qcow2 file is being written: Received bytes so far of at most Total) or
// "hash" (the file is being hashed).
type ExportEvent struct {
	Stage           string
	Received, Total int64
}

// ExportedImage is the daemon's answer to an export: the name `images pull`
// asks for (the template's), the qcow2's byte count, and the digest only when
// it was asked for (hash) or already known. Path is set only for an image
// template's cached image.
type ExportedImage struct {
	Name   string
	SHA256 string
	Size   int64
	Path   string
}

// TemplateExporter is implemented by a Manager whose host can export a
// template onto its array (the remote client for a hypervisor daemon). hash
// asks for the digest in the answer (one read of the volume). on gets every
// progress line.
type TemplateExporter interface {
	ExportTemplate(ctx context.Context, name, pool string, hash bool, on func(ExportEvent)) (ExportedImage, error)
}

// ImageBlobInfo is what a host says about an image it is about to stream.
// SHA256 is empty when the host does not know the digest yet: it then sends it
// as a trailer after the last byte, and Trailer (never nil then) gives it once
// the body was read to its end ("" when none came).
type ImageBlobInfo struct {
	Size    int64
	SHA256  string
	Trailer func() string
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
	Pool string
	// Hash has the host read the export once and say its SHA-256 (the store
	// push needs it before the bytes).
	Hash       bool
	OnProgress ProgressFunc
}

// ExportTemplate has the host export the template in.Name onto its array as a
// volume that `images pull host/<name>` reads for the next 24 hours (the
// template's name is the image name). The host does the work; this forwards
// progress (stage "export").
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
		Detail: fmt.Sprintf("exporting %s/%s onto the NVMe array of %s", in.Host, in.Name, in.Host)})
	img, err := x.ExportTemplate(ctx, in.Name, in.Pool, in.Hash, func(e ExportEvent) {
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
	in.OnProgress.emit("export", "done", ExportedLine(in.Host, img), "")
	return img, nil
}

// ExportedLine is the one-line answer to an export: the template is exported
// onto the host's array, how big, the digest if known, and how long it is kept.
func ExportedLine(host string, img ExportedImage) string {
	sum := ""
	if img.SHA256 != "" {
		sum = ", sha256 " + img.SHA256
	}
	if img.Path != "" {
		return fmt.Sprintf("%s/%s is in the image cache (%d bytes%s)", host, img.Name, img.Size, sum)
	}
	return fmt.Sprintf("%s/%s is exported on the NVMe array of %s (%d bytes%s); it is kept %d hours, then removed",
		host, img.Name, host, img.Size, sum, int(vmtool.ExportTTL.Hours()))
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
	// The digest is in the header, or (a template export whose digest the host
	// does not know yet) it follows the last byte as a trailer and is compared
	// then.
	byTrailer := want == "" && info.Trailer != nil
	if info.Size <= 0 {
		return PulledImage{}, badGateway(op, fmt.Errorf("%s did not state the image's size", hostText(in.Host)))
	}
	if !byTrailer && !isHexDigest(want) {
		return PulledImage{}, badGateway(op, fmt.Errorf("%s did not send a SHA-256 digest for the image", hostText(in.Host)))
	}
	res := PulledImage{Name: in.Name, SHA256: want, Size: info.Size, Path: dest}

	if existed {
		var same bool
		var err error
		if byTrailer {
			same, want, err = sameFileByTrailer(dest, rc, info, in.Host)
			res.SHA256 = want
		} else {
			same, err = sameFile(dest, info.Size, want)
		}
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
	if byTrailer {
		// The body ended exactly at its stated size, so its end was reached and
		// the trailer is in. None, or a malformed one, keeps nothing.
		want = strings.ToLower(info.Trailer())
		if !isHexDigest(want) {
			return PulledImage{}, badGateway(op, fmt.Errorf("download %s: %s did not send the SHA-256 after the last byte; nothing was kept", in.Name, hostText(in.Host)))
		}
		res.SHA256 = want
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

func isHexDigest(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// sameFileByTrailer is sameFile for a host whose digest comes after the
// bytes: the existing file must be a regular file of the stated size; the
// host's stream is then read through (hashed, nothing written), its trailer
// must match what was read, and the digest of the stream must be the file's.
// It returns the stream's digest.
func sameFileByTrailer(path string, rc io.Reader, info ImageBlobInfo, host string) (bool, string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return false, "", err
	}
	if !st.Mode().IsRegular() || st.Size() != info.Size {
		return false, "", nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, info.Size+1))
	if err != nil {
		return false, "", badGateway("images pull", fmt.Errorf("download from %s: %w", hostText(host), err))
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want := strings.ToLower(info.Trailer()); n != info.Size || want != got {
		return false, "", badGateway("images pull", fmt.Errorf("%s's stream did not match its stated size and SHA-256 (%d bytes read); nothing was kept", hostText(host), n))
	}
	same, err := sameFile(path, info.Size, got)
	return same, got, err
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

// ExportedToStore is the result of ExportTemplateToStore: the daemon's cache
// entry and the store's image.
type ExportedToStore struct {
	Image ExportedImage
	Store StoreImage
}

// ExportTemplateToStore is `templates export --push`: ExportTemplate on the
// host, then the daemon's blob streamed straight into the store as the image
// of the template's name, with no local file. Store mode only: in local mode
// (no server, or the store did not answer) it is refused before the host is
// contacted, naming `images pull` and `images push`, the way to do it without
// a store. The store takes the size and SHA-256 before the bytes; they come
// from the blob's headers (Content-Length, X-Image-SHA256) and must agree
// with the export's done event, and the store verifies the stream against them
// before it indexes anything (it is not skipped here). Re-pushing what the
// store holds and pushing over a different image are as `images push`: the
// store decides. progress events: stage "export" (as ExportTemplate), then
// stage "push".
func (s *Service) ExportTemplateToStore(ctx context.Context, m Manager, in ExportTemplateInput) (ExportedToStore, error) {
	const op = "templates export --push"
	if !vmtool.ValidImageName(in.Name) {
		return ExportedToStore{}, invalid(op, fmt.Errorf("template name %q must be a plain name (letters, digits, '.', '_', '-')", in.Name))
	}
	if _, ok := m.(TemplateExporter); !ok {
		return ExportedToStore{}, invalid(op, fmt.Errorf("%s has no daemon with thin pools to export templates from", hostText(in.Host)))
	}
	src, ok := m.(ImageBlobSource)
	if !ok {
		return ExportedToStore{}, invalid(op, fmt.Errorf("%s has no daemon to pull images from", hostText(in.Host)))
	}
	if s.Store.Mode(ctx) != ModeStore {
		return ExportedToStore{}, invalid(op, fmt.Errorf("%w; --push needs the store (server: in config.yml and VMTOOL_SERVER_TOKEN). Without one: vmtool templates export %s/%s, then vmtool images pull %s/%s and vmtool images push <file>",
			ErrNoStore, in.Host, in.Name, in.Host, in.Name))
	}
	cat := s.Store.CatalogFor(ctx)

	// The store wants the digest before the bytes: have the host read the
	// export once now, so the blob that follows carries it in its header.
	in.Hash = true
	img, err := s.ExportTemplate(ctx, m, in)
	if err != nil {
		return ExportedToStore{}, err
	}
	rc, info, err := src.OpenImageBlob(ctx, img.Name)
	if err != nil {
		return ExportedToStore{}, wrap(op, err)
	}
	defer rc.Close()
	want := strings.ToLower(info.SHA256)
	if info.Size <= 0 {
		return ExportedToStore{}, badGateway(op, fmt.Errorf("%s did not state the image's size", hostText(in.Host)))
	}
	if len(want) != 64 || strings.Trim(want, "0123456789abcdef") != "" {
		return ExportedToStore{}, badGateway(op, fmt.Errorf("%s did not send a SHA-256 digest for the image", hostText(in.Host)))
	}
	if (img.SHA256 != "" && !strings.EqualFold(img.SHA256, want)) || (img.Size > 0 && img.Size != info.Size) {
		return ExportedToStore{}, badGateway(op, fmt.Errorf("%s's image %s is not the one just exported (export: %d bytes, sha256 %s; blob: %d bytes, sha256 %s); nothing was pushed",
			hostText(in.Host), img.Name, img.Size, img.SHA256, info.Size, want))
	}

	in.OnProgress.emit("push", "start", fmt.Sprintf("pushing %s/%s (%d bytes) to the store as %s", in.Host, img.Name, info.Size, img.Name), "")
	step := uploadStep(info.Size)
	next := int64(0)
	body := &progressReader{r: io.LimitReader(rc, info.Size), total: info.Size, step: step,
		progress: func(done, total int64) {
			if done >= next || done == total {
				next = done + step
				in.OnProgress.emitEvent(ProgressEvent{Stage: "push", Status: "progress", Detail: "bytes", Sent: done, Total: total})
			}
		}}
	pushed, err := cat.Push(ctx, img.Name, want, info.Size, body)
	if err != nil {
		in.OnProgress.emit("push", "error", "the store did not accept the image", "")
		return ExportedToStore{}, badGateway(op, fmt.Errorf("push %s to the store: %w; nothing was pushed", img.Name, err))
	}
	in.OnProgress.emit("push", "done", fmt.Sprintf("%s is in the store (%d bytes, sha256 %s)", pushed.Name, pushed.Size, pushed.SHA256), "")
	return ExportedToStore{Image: img, Store: *pushed}, nil
}
