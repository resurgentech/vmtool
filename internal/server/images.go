package server

// The hypervisor's image cache: getImageManifest and uploadImage.
//
// Hypervisors are shared: other people's VMs, and their base images, may sit
// in the same libvirt pools. The daemon therefore owns a volume only if its
// own image-manifest.yml lists the name, and it only ever writes, replaces or
// deletes inside its `default` pool's directory:
//
//   - A name is foreign when the manifest does not list it and a volume of
//     that name exists: any pool has one (Manager.ImagePath searches them
//     all), or a file of that name is in the default pool's directory even
//     if libvirt has not seen it yet. A foreign name is refused (409) before
//     the body is read and is never touched.
//   - The body goes to `.<name>.part` (created O_EXCL, so a temporary file
//     that already exists is not ours either), hashed as it arrives. Any
//     failure removes the temporary file and nothing else.
//   - At commit, under the manifest's lock: the ownership check is repeated;
//     an old volume is deleted only if the manifest lists the name; the new
//     file is put in place with os.Link, which fails instead of overwriting a
//     file that appeared meanwhile; then the manifest and the pool are
//     updated.
//
// The deleteImage handler (handlers.go) drops a manifest entry when it
// deletes a listed image from the default pool; nothing else here deletes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	// cachePool is the pool images are cached in.
	cachePool = "default"
	// uploadIdle is how long an upload may send nothing before the read
	// fails. The server has no overall ReadTimeout (a multi-GB image takes
	// long); this bounds a stalled client instead.
	uploadIdle = 2 * time.Minute
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uploadRoute matches exactly the uploadImage operation; withMaxBytes
// exempts it from the request body cap.
var uploadRoute = func() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/hosts/{host}/images/{name}", func(http.ResponseWriter, *http.Request) {})
	return mux
}()

func isUpload(r *http.Request) bool {
	_, pattern := uploadRoute.Handler(r)
	return pattern != ""
}

func (h *handlers) manifest() *vmtool.ImageManifest {
	p := h.imageManifest
	if p == "" {
		p = vmtool.ImageManifestFile
	}
	return vmtool.OpenImageManifest(p)
}

func badImageName(name string) string {
	return fmt.Sprintf("image name %q must be a plain file name (letters, digits, '.', '_', '-'; not starting with '.'; at most 200 bytes)", name)
}

func foreignMsg(name, host, path string) string {
	return fmt.Sprintf("image %q already exists on %s at %s; vmtool did not create it and will not replace it", name, host, path)
}

// defaultPoolDir is the default pool's directory. There is no fallback: a
// host without one cannot cache images.
func defaultPoolDir(m app.Manager) (string, error) {
	pools, err := m.ListPools()
	if err != nil {
		return "", fmt.Errorf("list storage pools: %w", err)
	}
	for _, p := range pools {
		if p.Name == cachePool {
			if p.Path == "" || !filepath.IsAbs(p.Path) {
				return "", fmt.Errorf("storage pool %q has no directory path; vmtool caches images there", cachePool)
			}
			return filepath.Clean(p.Path), nil
		}
	}
	return "", fmt.Errorf("no storage pool %q; vmtool caches images there (run `just prereqs`)", cachePool)
}

// existingVolume reports whether a volume called name exists: in any pool
// libvirt knows, or as a file in dir (the default pool's directory) that
// libvirt has not seen. It fails closed: an error is never "absent".
func existingVolume(m app.Manager, name, dir string) (string, bool, error) {
	p, err := m.ImagePath(name)
	if err == nil {
		return p, true, nil
	}
	if !vmtool.IsNotFound(err) {
		return "", false, fmt.Errorf("look for volume %q in the storage pools: %w", name, err)
	}
	fp := filepath.Join(dir, name)
	if _, err := os.Lstat(fp); err == nil {
		return fp, true, nil
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("look for %s: %w", fp, err)
	}
	return "", false, nil
}

// refreshPool makes libvirt rescan pool, when the Manager can (the libvirt
// one can; see vmtool.Manager.RefreshPool).
func refreshPool(m app.Manager, pool string) error {
	if r, ok := m.(interface{ RefreshPool(string) error }); ok {
		return r.RefreshPool(pool)
	}
	return nil
}

// --- getImageManifest ---

func (h *handlers) GetImageManifest(ctx context.Context, req api.GetImageManifestRequestObject) (api.GetImageManifestResponseObject, error) {
	if !vmtool.ValidImageName(req.Name) {
		return api.GetImageManifest400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: badImageName(req.Name)}}, nil
	}
	if h.role != config.RoleHypervisor && req.Host == h.hosts.Local() {
		return api.GetImageManifest403JSONResponse{Error: fmt.Sprintf(
			"image manifests are kept by hypervisor daemons; %s is the workstation and keeps no image cache", req.Host)}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		if h.role != config.RoleHypervisor {
			// A workstation asks the configured host's daemon.
			st, ok := m.(app.ImageStore)
			if !ok {
				return nil, fmt.Errorf("host %s keeps no image cache", req.Host)
			}
			return st.ImageManifest(req.Name)
		}
		return h.imageState(m, req.Host, req.Name)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindNotFound:
			return api.GetImageManifest404JSONResponse(apiErr(err)), nil
		case app.KindInvalid:
			return api.GetImageManifest400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.GetImageManifest500JSONResponse(apiErr(err)), nil
	}
	ri := v.(app.RemoteImage)
	out := api.ImageManifestEntry{Name: ri.Name, Foreign: ri.Foreign}
	if ri.SHA256 != "" {
		out.Sha256 = &ri.SHA256
		out.Size = &ri.Size
	}
	if ri.Path != "" {
		out.Path = &ri.Path
	}
	return api.GetImageManifest200JSONResponse(out), nil
}

// imageState is this daemon's entry for name. A listed image reports its
// digest only while its file is in place with the listed size, so a client
// re-uploads a missing or truncated one.
func (h *handlers) imageState(m app.Manager, host, name string) (app.RemoteImage, error) {
	out := app.RemoteImage{Name: name}
	dir, err := defaultPoolDir(m)
	if err != nil {
		return out, err
	}
	e, listed, err := h.manifest().Get(name)
	if err != nil {
		return out, err
	}
	if listed {
		final := filepath.Join(dir, name)
		if st, err := os.Lstat(final); err == nil && st.Mode().IsRegular() && st.Size() == e.Size {
			out.SHA256, out.Size, out.Path = e.SHA256, e.Size, final
		}
		return out, nil
	}
	p, exists, err := existingVolume(m, name, dir)
	if err != nil {
		return out, err
	}
	out.Foreign, out.Path = exists, p
	return out, nil
}

// --- uploadImage ---

// UploadImage on the strict interface is never reached: tunnelMux serves the
// route itself so it can stream the body and refuse before reading it.
func (h *handlers) UploadImage(_ context.Context, _ api.UploadImageRequestObject) (api.UploadImageResponseObject, error) {
	return api.UploadImage500JSONResponse{Error: "uploadImage streams its body; served by tunnelMux"}, nil
}

func (t tunnelMux) UploadImage(w http.ResponseWriter, r *http.Request, host api.HostName, name api.ImageName, params api.UploadImageParams) {
	t.h.uploadImage(w, r, host, name, params.XImageSHA256)
}

type uploadErr struct {
	code int
	msg  string
}

func (h *handlers) uploadImage(w http.ResponseWriter, r *http.Request, host, name, wantSHA string) {
	entry, uerr := h.storeImage(w, r, host, name, wantSHA)
	if uerr != nil {
		if uerr.code >= 500 {
			slog.Error("uploadImage", "image", name, "err", uerr.msg)
		}
		e := api.Error{Error: uerr.msg}
		switch uerr.code {
		case http.StatusBadRequest:
			_ = api.UploadImage400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(e)}.VisitUploadImageResponse(w)
		case http.StatusForbidden:
			_ = api.UploadImage403JSONResponse(e).VisitUploadImageResponse(w)
		case http.StatusNotFound:
			_ = api.UploadImage404JSONResponse(e).VisitUploadImageResponse(w)
		case http.StatusConflict:
			_ = api.UploadImage409JSONResponse(e).VisitUploadImageResponse(w)
		case http.StatusLengthRequired:
			_ = api.UploadImage411JSONResponse(e).VisitUploadImageResponse(w)
		default:
			_ = api.UploadImage500JSONResponse(e).VisitUploadImageResponse(w)
		}
		return
	}
	_ = api.UploadImage201JSONResponse(entry).VisitUploadImageResponse(w)
}

func (h *handlers) storeImage(w http.ResponseWriter, r *http.Request, host, name, wantSHA string) (api.ImageManifestEntry, *uploadErr) {
	var zero api.ImageManifestEntry
	fail := func(code int, format string, a ...any) (api.ImageManifestEntry, *uploadErr) {
		return zero, &uploadErr{code: code, msg: fmt.Sprintf(format, a...)}
	}
	// Refusals that need nothing but the request come first.
	if h.role != config.RoleHypervisor {
		return fail(http.StatusForbidden, "uploadImage is served by hypervisor daemons, not by workstation %s; `vmtool create` uploads to a hypervisor's daemon itself", h.hosts.Local())
	}
	if !vmtool.ValidImageName(name) {
		return fail(http.StatusBadRequest, "%s", badImageName(name))
	}
	wantSHA = strings.ToLower(wantSHA)
	if !sha256Hex.MatchString(wantSHA) {
		return fail(http.StatusBadRequest, "X-Image-SHA256 must be 64 hex digits")
	}
	if r.ContentLength < 0 {
		return fail(http.StatusLengthRequired, "Content-Length is required: the image's size must be known before it is stored")
	}
	if r.ContentLength == 0 {
		return fail(http.StatusBadRequest, "the body is empty; an image has at least one byte")
	}
	size := r.ContentLength

	m, done, err := h.hosts.WithToken(requestToken(r.Context())).Resolve(host)
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return fail(http.StatusNotFound, "%v", err)
		}
		return fail(http.StatusInternalServerError, "%v", err)
	}
	defer done()
	dir, err := defaultPoolDir(m)
	if err != nil {
		return fail(http.StatusInternalServerError, "%v", err)
	}
	man := h.manifest()

	// Ownership, before a byte of the body is read.
	_, listed, err := man.Get(name)
	if err != nil {
		return fail(http.StatusInternalServerError, "%v", err)
	}
	if !listed {
		p, exists, err := existingVolume(m, name, dir)
		if err != nil {
			return fail(http.StatusInternalServerError, "%v", err)
		}
		if exists {
			return fail(http.StatusConflict, "%s", foreignMsg(name, host, p))
		}
	}

	final := filepath.Join(dir, name)
	part := filepath.Join(dir, "."+name+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fail(http.StatusConflict, "temporary file %s already exists: another upload of %q is running, or one was interrupted (remove the file by hand if no upload is running)", part, name)
		}
		return fail(http.StatusInternalServerError, "create %s: %v", part, err)
	}
	// From here the temporary file is ours, and every failure removes it
	// (and nothing else). After a successful link it has been removed already.
	partLive := true
	defer func() {
		if f != nil {
			f.Close()
		}
		if partLive {
			if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
				slog.Error("uploadImage: remove temporary file", "path", part, "err", err)
			}
		}
	}()

	sum := sha256.New()
	n, err := copyBody(w, r.Body, io.MultiWriter(f, sum), size)
	if err != nil {
		return fail(http.StatusBadRequest, "upload of %q ended after %d of %d bytes: %v", name, n, size, err)
	}
	if n != size {
		return fail(http.StatusBadRequest, "body is %d bytes, Content-Length said %d", n, size)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if got != wantSHA {
		return fail(http.StatusBadRequest, "SHA-256 of the body is %s, X-Image-SHA256 said %s; nothing was stored", got, wantSHA)
	}
	if err := f.Sync(); err != nil {
		return fail(http.StatusInternalServerError, "write %s: %v", part, err)
	}
	err = f.Close()
	f = nil
	if err != nil {
		return fail(http.StatusInternalServerError, "write %s: %v", part, err)
	}
	if h.beforeCommit != nil {
		h.beforeCommit(name)
	}

	// Commit, under the manifest's lock so the ownership decision, the
	// replacement and the record cannot interleave with another upload or a
	// deleteImage.
	man.Lock()
	defer man.Unlock()
	all, err := man.LoadLocked()
	if err != nil {
		return fail(http.StatusInternalServerError, "%v", err)
	}
	if _, listed := all[name]; listed {
		// Ours: delete the old volume, from the default pool only.
		if err := m.DeleteImage(name, cachePool); err != nil && !vmtool.IsNotFound(err) {
			return fail(http.StatusInternalServerError, "delete the old cached %q: %v", name, err)
		}
		// libvirt may not have seen the file (pool not refreshed).
		if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
			return fail(http.StatusInternalServerError, "delete the old cached %s: %v", final, err)
		}
	} else {
		p, exists, err := existingVolume(m, name, dir)
		if err != nil {
			return fail(http.StatusInternalServerError, "%v", err)
		}
		if exists {
			return fail(http.StatusConflict, "%s", foreignMsg(name, host, p))
		}
	}
	// A link never replaces an existing file: one that appeared since the
	// check above is foreign and stays as it is.
	if err := os.Link(part, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fail(http.StatusConflict, "%s", foreignMsg(name, host, final))
		}
		return fail(http.StatusInternalServerError, "put %s in place: %v", final, err)
	}
	partLive = false
	if err := os.Remove(part); err != nil {
		slog.Error("uploadImage: remove temporary file after link", "path", part, "err", err)
	}
	entry := vmtool.ImageEntry{SHA256: got, Size: n}
	all[name] = entry
	if err := man.SaveLocked(all); err != nil {
		// Unrecorded, the file would be foreign to us from now on: take back
		// the file this request just linked.
		if rerr := os.Remove(final); rerr != nil {
			slog.Error("uploadImage: remove unrecorded upload", "path", final, "err", rerr)
		}
		return fail(http.StatusInternalServerError, "record %q in %s: %v; the upload was discarded", name, man.Path(), err)
	}
	if err := refreshPool(m, cachePool); err != nil {
		return fail(http.StatusInternalServerError, "stored %s but refreshing pool %q failed: %v", final, cachePool, err)
	}
	return api.ImageManifestEntry{Name: name, Sha256: &entry.SHA256, Size: &entry.Size, Path: &final}, nil
}

// copyBody copies at most size+1 bytes of body to dst (one more than
// promised exposes an over-long body), pushing the connection's read
// deadline forward before every read so only a stalled client times out.
func copyBody(w http.ResponseWriter, body io.Reader, dst io.Writer, size int64) (int64, error) {
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	src := &idleReader{r: io.LimitReader(body, size+1), rc: rc}
	return io.Copy(dst, src)
}

type idleReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func (x *idleReader) Read(p []byte) (int, error) {
	_ = x.rc.SetReadDeadline(time.Now().Add(uploadIdle))
	return x.r.Read(p)
}
