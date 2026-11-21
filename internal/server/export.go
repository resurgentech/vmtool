package server

// exportTemplate and getImageBlob (spec/openapi/vmtool.yaml, API 2.4.0): how a
// template reaches another host. A hypervisor's daemon writes a template into
// its image cache as a qcow2 image (export), and a client downloads a cached
// image the daemon created (blob).
//
// exportTemplate
//   - {name} resolves to exactly one template (the host Manager's
//     LookupTemplate, as deleteTemplate does); the image is named after the
//     template. The name goes through beginReceiveAs, the receive path
//     uploadImage and fetchImage use: a foreign volume of that name is 409, a
//     second upload, fetch or export of the name is 409.
//   - A name the manifest lists is refused (409) unless it is the same
//     template re-exported: the template's recorded SHA-256 equals the manifest
//     entry's and the file is in place (one `done` line, no rewrite). Only a
//     saved template makes a new image; an image template is exported only as
//     the cached image it was built from.
//   - The template is converted into a thick export volume in the array's
//     volume group (or the template's existing one is reused; E2). Nothing is
//     written to the image directory: no file, no `.part`, no manifest entry.
//     done carries the template's name and the byte count; the digest only
//     with hash=true (one read of the volume, then the tags). No room on the
//     array is a plain 409 before the stream starts.
//   - From the 200 on every outcome is an NDJSON line: progress*, then one
//     done or one error. Refusals that need no conversion are plain JSON
//     statuses first. Request and response bodies are never logged.
//
// getImageBlob
//   - A manifest entry is served as before; a name it does not list is served
//     from a template export volume of that name (OpenExport, hashed while
//     sent: digest in the header when tagged, else X-Image-Size and the
//     X-Image-SHA256 trailer, the tag recorded only after the whole body went
//     out). The name must be a
//     valid image name; the file is the validated name joined to the vmtool
//     pool's directory (nothing else from the request reaches the
//     filesystem), opened with O_NOFOLLOW (a symlink is refused) and checked
//     to be a regular file of exactly the listed size. A name the manifest
//     does not list is 404, or 409 when a foreign volume has it (never
//     served). Content-Length and X-Image-SHA256 (the manifest's digest) are
//     sent; the client checks the bytes against them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// --- routing ---

// ExportTemplate on the strict interface is never reached: tunnelMux serves
// the route itself so it can stream NDJSON.
func (h *handlers) ExportTemplate(_ context.Context, _ api.ExportTemplateRequestObject) (api.ExportTemplateResponseObject, error) {
	return api.ExportTemplate500JSONResponse{Error: "exportTemplate streams NDJSON; served by tunnelMux"}, nil
}

func (t tunnelMux) ExportTemplate(w http.ResponseWriter, r *http.Request, host api.HostName, name string, params api.ExportTemplateParams) {
	setNote(r.Context(), "exportTemplate", r)
	t.h.exportTemplate(w, r, host, name, derefStr(params.Pool), params.Hash != nil && *params.Hash)
}

// GetImageBlob on the strict interface is never reached: tunnelMux serves the
// route itself so it can stream the file.
func (h *handlers) GetImageBlob(_ context.Context, _ api.GetImageBlobRequestObject) (api.GetImageBlobResponseObject, error) {
	return api.GetImageBlob500JSONResponse{Error: "getImageBlob streams a file; served by tunnelMux"}, nil
}

func (t tunnelMux) GetImageBlob(w http.ResponseWriter, r *http.Request, host api.HostName, name api.ImageName) {
	setNote(r.Context(), "getImageBlob", r)
	t.h.getImageBlob(w, r, host, name)
}

func writeExportStatus(w http.ResponseWriter, code int, msg string) {
	e := api.Error{Error: msg}
	switch code {
	case http.StatusBadRequest:
		_ = api.ExportTemplate400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(e)}.VisitExportTemplateResponse(w)
	case http.StatusForbidden:
		_ = api.ExportTemplate403JSONResponse(e).VisitExportTemplateResponse(w)
	case http.StatusNotFound:
		_ = api.ExportTemplate404JSONResponse(e).VisitExportTemplateResponse(w)
	case http.StatusConflict:
		_ = api.ExportTemplate409JSONResponse(e).VisitExportTemplateResponse(w)
	default:
		_ = api.ExportTemplate500JSONResponse(e).VisitExportTemplateResponse(w)
	}
}

func writeBlobStatus(w http.ResponseWriter, code int, msg string) {
	e := api.Error{Error: msg}
	switch code {
	case http.StatusBadRequest:
		_ = api.GetImageBlob400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(e)}.VisitGetImageBlobResponse(w)
	case http.StatusForbidden:
		_ = api.GetImageBlob403JSONResponse(e).VisitGetImageBlobResponse(w)
	case http.StatusNotFound:
		_ = api.GetImageBlob404JSONResponse(e).VisitGetImageBlobResponse(w)
	case http.StatusConflict:
		_ = api.GetImageBlob409JSONResponse(e).VisitGetImageBlobResponse(w)
	default:
		_ = api.GetImageBlob500JSONResponse(e).VisitGetImageBlobResponse(w)
	}
}

// statusFor is the HTTP status for an error from a Manager.
func statusFor(err error) int {
	if errors.Is(err, app.ErrNoThinPools) {
		return http.StatusNotFound
	}
	switch kindOf(err) {
	case app.KindInvalid:
		return http.StatusBadRequest
	case app.KindNotFound:
		return http.StatusNotFound
	case app.KindConflict:
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func badTemplateName(name string) string {
	return fmt.Sprintf("template name %q must be a plain name (letters, digits, '.', '_', '-'; not starting with '.'; at most 200 bytes)", name)
}

// --- exportTemplate ---

func (h *handlers) exportTemplate(w http.ResponseWriter, r *http.Request, host, name, pool string, hash bool) {
	if h.role != config.RoleHypervisor {
		if host == h.hosts.Local() && !h.hosts.OwnListed() {
			writeExportStatus(w, http.StatusForbidden, fmt.Sprintf(
				"exportTemplate is served by hypervisor daemons, not by workstation %s; a listed host's daemon exports", h.hosts.Local()))
			return
		}
		h.forwardExport(w, r, host, name, pool, hash)
		return
	}
	if !vmtool.ValidImageName(name) {
		writeExportStatus(w, http.StatusBadRequest, badTemplateName(name))
		return
	}
	// A hypervisor serves only itself.
	if err := h.hosts.Check(host); err != nil {
		writeExportStatus(w, http.StatusNotFound, err.Error())
		return
	}
	var tpl app.TemplateInfo
	var exp app.TemplateVolumeExporter
	rc, uerr := h.beginReceiveAs(r.Context(), host, "export", func(m app.Manager) (string, *uploadErr) {
		x, ok := m.(app.TemplateVolumeExporter)
		if !ok {
			return "", rerr(http.StatusNotFound, "template %q: %v", name, app.ErrNoThinPools)
		}
		t, err := x.LookupTemplate(name, pool)
		if err != nil {
			return "", rerr(statusFor(err), "%v", err)
		}
		tpl, exp = t, x
		return t.Name, nil
	})
	if uerr != nil {
		if uerr.code >= 500 {
			slog.Error("exportTemplate", "template", name, "err", uerr.msg)
		}
		writeExportStatus(w, uerr.code, uerr.msg)
		return
	}
	defer rc.release()
	image := rc.name

	// The manifest's view of the name, before anything is converted.
	entry, listed, err := rc.man.Get(image)
	if err != nil {
		slog.Error("exportTemplate", "image", image, "err", err)
		writeExportStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := newNDJSON(w)
	if listed {
		same := tpl.SHA256 != "" && tpl.SHA256 == entry.SHA256
		fi, lerr := os.Lstat(rc.final)
		present := lerr == nil && fi.Mode().IsRegular() && fi.Size() == entry.Size
		switch {
		case same && present:
			slog.Info("exportTemplate", "image", image, "outcome", "unchanged")
			st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventDone, Image: &api.ImageManifestEntry{
				Name: image, Sha256: &entry.SHA256, Size: &entry.Size, Path: ptrTo(rc.final)}})
			return
		case !same:
			writeExportStatus(w, http.StatusConflict, fmt.Sprintf(
				"image %q is already in the image cache of %s (sha256 %s) and is not an export of this template; vmtool will not replace it: remove it first with `vmtool images delete %s %s`",
				image, host, shortDigest(entry.SHA256), host, image))
			return
		}
	}
	if tpl.Origin != vmtool.TemplateOriginSaved {
		writeExportStatus(w, http.StatusConflict, fmt.Sprintf(
			"template %q was built from the cached image %q; only a saved template is exported as a new image, and an image template only as the image it came from, which %s does not hold with this template's SHA-256",
			tpl.Name, image, host))
		return
	}

	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		slog.Warn("exportTemplate", "image", image, "outcome", "error", "err", msg)
		st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventError, Error: &msg})
	}
	progress := func(stage api.ExportTemplateEventStage, received, total int64) {
		st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventProgress, Stage: &stage, Received: &received, Total: &total})
	}

	// The conversion into the export volume (the helper makes the thick LV and
	// converts into it; the volume is reused if the template has one). Its
	// progress is the converter's percent of the template's size. The stream
	// starts with the first progress line, so a refusal before any (no room on
	// the array) is a plain 409.
	ev, xerr := exp.ExportTemplateVolume(tpl, func(pct float64) {
		progress(api.Export, int64(pct/100*float64(tpl.SizeBytes)), tpl.SizeBytes)
	})
	if xerr != nil {
		if !st.started && errors.Is(xerr, vmtool.ErrThinNoRoom) {
			writeExportStatus(w, http.StatusConflict, fmt.Sprintf("%s: %v", host, xerr))
			return
		}
		fail("export of template %q failed: %v", tpl.Name, xerr)
		return
	}
	if ev.Bytes <= 0 {
		fail("export of template %q made no usable volume", tpl.Name)
		return
	}

	// done names the image as the template (what `images pull host/<name>`
	// asks for) and its size; the digest only when it was asked for.
	out := api.ImageManifestEntry{Name: image, Size: &ev.Bytes}
	if hash {
		sha := ev.SHA256
		if sha == "" {
			// One read of the volume, then the tags (the template's and the
			// volume's), so a later send puts the digest in its header.
			var err error
			sha, err = hashExport(exp, tpl, ev, func(n int64) { progress(api.Hash, n, ev.Bytes) })
			if err != nil {
				fail("%v", err)
				return
			}
		}
		out.Sha256 = &sha
	}
	slog.Info("exportTemplate", "image", image, "outcome", "done", "bytes", ev.Bytes, "export", ev.Name, "hashed", hash)
	st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventDone, Image: &out})
}

// hashExport reads the export volume once, hashing it, and records the digest
// on the template and the volume. The digest counts only if exactly the
// volume's byte count was read.
func hashExport(exp app.TemplateVolumeExporter, tpl app.TemplateInfo, ev app.ExportVolume, onProgress func(n int64)) (string, error) {
	src, _, err := exp.OpenExport(ev.Name)
	if err != nil {
		return "", fmt.Errorf("open export volume %s: %v", ev.Name, err)
	}
	sum := sha256.New()
	step := max(1, min(ev.Bytes/100, progressMaxStep))
	n, err := io.Copy(sum, &progressReader{r: src, step: step, next: step, emit: onProgress})
	if cerr := src.Close(); err == nil {
		err = cerr
	}
	if err != nil || n != ev.Bytes {
		return "", fmt.Errorf("read the export volume %s: read %d of %d bytes: %v", ev.Name, n, ev.Bytes, err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if err := exp.RecordTemplateSHA(tpl, got); err != nil {
		return "", fmt.Errorf("record the SHA-256 on template %q: %v", tpl.Name, err)
	}
	return got, nil
}

func shortDigest(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// forwardExport serves exportTemplate on a workstation for a listed host: the
// request goes to that host's daemon (remote.Manager.ExportTemplate) and its
// progress is relayed as it arrives.
func (h *handlers) forwardExport(w http.ResponseWriter, r *http.Request, host, name, pool string, hash bool) {
	if !vmtool.ValidImageName(name) {
		writeExportStatus(w, http.StatusBadRequest, badTemplateName(name))
		return
	}
	m, done, err := h.hosts.WithToken(requestToken(r.Context())).Resolve(host)
	if err != nil {
		writeExportStatus(w, statusFor(err), err.Error())
		return
	}
	defer done()
	x, ok := m.(app.TemplateExporter)
	if !ok {
		writeExportStatus(w, http.StatusInternalServerError, fmt.Sprintf("host %s keeps no image cache", host))
		return
	}
	st := newNDJSON(w)
	img, err := x.ExportTemplate(r.Context(), name, pool, hash, func(e app.ExportEvent) {
		stage := api.Export
		if e.Stage == "hash" {
			stage = api.Hash
		}
		st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventProgress, Stage: &stage, Received: &e.Received, Total: &e.Total})
	})
	if err != nil {
		if st.started {
			msg := err.Error()
			slog.Warn("exportTemplate forward", "template", name, "host", host, "err", msg)
			st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventError, Error: &msg})
			return
		}
		writeExportStatus(w, statusFor(err), err.Error())
		return
	}
	st.send(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventDone, Image: &api.ImageManifestEntry{
		Name: img.Name, Sha256: ptrToNonEmpty(img.SHA256), Size: &img.Size, Path: ptrToNonEmpty(img.Path)}})
}

func ptrToNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// --- getImageBlob ---

// blobIdle is how long a download may be unable to write before it fails.
const blobIdle = uploadIdle

func (h *handlers) getImageBlob(w http.ResponseWriter, r *http.Request, host, name string) {
	if h.role != config.RoleHypervisor {
		if host == h.hosts.Local() && !h.hosts.OwnListed() {
			writeBlobStatus(w, http.StatusForbidden, fmt.Sprintf(
				"getImageBlob is served by hypervisor daemons, not by workstation %s; a listed host's daemon serves", h.hosts.Local()))
			return
		}
		h.forwardBlob(w, r, host, name)
		return
	}
	if !vmtool.ValidImageName(name) {
		writeBlobStatus(w, http.StatusBadRequest, badImageName(name))
		return
	}
	// A hypervisor serves only itself.
	if err := h.hosts.Check(host); err != nil {
		writeBlobStatus(w, http.StatusNotFound, err.Error())
		return
	}
	entry, listed, err := h.manifest().Get(name)
	if err != nil {
		slog.Error("getImageBlob", "image", name, "err", err)
		writeBlobStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The manifest answers a listed name; libvirt is asked only for the pool
	// directory (kept after the first time) or to look for a foreign volume.
	m, done, err := h.hosts.WithToken(requestToken(r.Context())).Resolve(host)
	if err != nil {
		writeBlobStatus(w, statusFor(err), err.Error())
		return
	}
	defer done()
	if !listed {
		// Not a cached image: a template's export volume of that name is served
		// next (no libvirt needed). Only "there is none" falls through.
		if x, ok := m.(app.TemplateVolumeExporter); ok {
			rc, ev, err := x.OpenExport(name)
			switch {
			case err == nil:
				h.serveExport(w, x, name, host, rc, ev)
				return
			case errors.Is(err, app.ErrNoThinPools), kindOf(err) == app.KindNotFound:
				// no export of that name
			default:
				if st := statusFor(err); st >= 500 {
					slog.Error("getImageBlob", "image", name, "err", err)
				}
				writeBlobStatus(w, statusFor(err), err.Error())
				return
			}
		}
	}
	dir, err := h.poolDirOf(host, func() (string, error) { return cachePoolDir(m, host) })
	if err != nil {
		writeBlobStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !listed {
		p, exists, err := existingVolume(m, name, dir)
		switch {
		case err != nil:
			writeBlobStatus(w, http.StatusInternalServerError, err.Error())
		case exists:
			writeBlobStatus(w, http.StatusConflict, fmt.Sprintf(
				"image %q on %s at %s was not created by vmtool and will not be served: %s", name, host, p, app.ForeignImageHint))
		default:
			writeBlobStatus(w, http.StatusNotFound, fmt.Sprintf(
				"image %q is not in the image cache of %s and no template export has that name (exports are kept 24 hours; make one with `vmtool templates export %s/%s`): %v",
				name, host, host, name, vmtool.ErrNotFound))
		}
		return
	}
	if !sha256Hex.MatchString(entry.SHA256) || entry.Size <= 0 {
		writeBlobStatus(w, http.StatusConflict, fmt.Sprintf("the manifest's entry for %q has no usable SHA-256 and size", name))
		return
	}
	// The one path that reaches the filesystem: the validated name in the
	// pool's directory, with no symlink followed.
	final := filepath.Join(dir, name)
	if filepath.Dir(final) != dir || filepath.Base(final) != name {
		writeBlobStatus(w, http.StatusBadRequest, badImageName(name))
		return
	}
	f, err := os.OpenFile(final, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeBlobStatus(w, http.StatusNotFound, fmt.Sprintf("image %q is listed but its file is missing on %s; it must be uploaded or exported again", name, host))
		case errors.Is(err, syscall.ELOOP):
			writeBlobStatus(w, http.StatusConflict, fmt.Sprintf("image %q on %s is not a regular file (a symbolic link); it will not be served", name, host))
		default:
			slog.Error("getImageBlob", "image", name, "err", err)
			writeBlobStatus(w, http.StatusInternalServerError, fmt.Sprintf("open image %q on %s: %v", name, host, err))
		}
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		writeBlobStatus(w, http.StatusInternalServerError, fmt.Sprintf("stat image %q on %s: %v", name, host, err))
		return
	case !fi.Mode().IsRegular():
		writeBlobStatus(w, http.StatusConflict, fmt.Sprintf("image %q on %s is not a regular file; it will not be served", name, host))
		return
	case fi.Size() != entry.Size:
		writeBlobStatus(w, http.StatusConflict, fmt.Sprintf("image %q on %s is %d bytes but the manifest says %d; it must be uploaded or exported again", name, host, fi.Size(), entry.Size))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	w.Header().Set("X-Image-SHA256", entry.SHA256)
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(&idleWriter{w: w, rc: http.NewResponseController(w)}, io.LimitReader(f, entry.Size))
	if err != nil || n != entry.Size {
		// Headers are out: the client sees a short body and rejects it.
		slog.Warn("getImageBlob", "image", name, "outcome", "interrupted", "bytes", n)
		return
	}
	slog.Info("getImageBlob", "image", name, "outcome", "done", "bytes", n)
}

// serveExport streams the export volume ev from rc (exactly ev.Bytes bytes,
// or a read error). With its digest on the volume's tags the answer is as for
// a cached image (Content-Length, X-Image-SHA256). Without, it carries
// X-Image-Size, declares the trailer X-Image-SHA256 and sends the digest of
// what it sent after the last byte; the digest is recorded on the template and
// the volume only after the whole body was written, and only if exactly
// ev.Bytes bytes went out. A short send, or a client that went away, records
// nothing.
func (h *handlers) serveExport(w http.ResponseWriter, x app.TemplateVolumeExporter, name, host string, rc io.ReadCloser, ev app.ExportVolume) {
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	known := ev.SHA256 != ""
	if known {
		w.Header().Set("Content-Length", strconv.FormatInt(ev.Bytes, 10))
		w.Header().Set("X-Image-SHA256", ev.SHA256)
	} else {
		w.Header().Set("X-Image-Size", strconv.FormatInt(ev.Bytes, 10))
		w.Header().Set("Trailer", "X-Image-SHA256")
	}
	w.WriteHeader(http.StatusOK)
	sum := sha256.New()
	var dst io.Writer = &idleWriter{w: w, rc: http.NewResponseController(w)}
	if !known {
		dst = io.MultiWriter(dst, sum)
	}
	n, err := io.Copy(dst, io.LimitReader(rc, ev.Bytes))
	if err != nil || n != ev.Bytes {
		// Headers are out: the client sees a short body and rejects it. No
		// trailer is sent and nothing is recorded.
		slog.Warn("getImageBlob", "image", name, "export", ev.Name, "outcome", "interrupted", "bytes", n)
		return
	}
	slog.Info("getImageBlob", "image", name, "export", ev.Name, "outcome", "done", "bytes", n)
	if known {
		return
	}
	got := hex.EncodeToString(sum.Sum(nil))
	w.Header().Set("X-Image-SHA256", got)
	// The template is found by the LV the export was made from; if it is gone
	// the digest is not recorded (the export expires on its own).
	tpl, err := x.LookupTemplate(ev.From, "")
	if err == nil {
		err = x.RecordTemplateSHA(tpl, got)
	}
	if err != nil {
		slog.Warn("getImageBlob: digest not recorded", "image", name, "host", host, "export", ev.Name, "err", err)
	}
}

// idleWriter pushes the connection's write deadline forward before every
// write, so only a stalled client times out.
type idleWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (x *idleWriter) Write(p []byte) (int, error) {
	_ = x.rc.SetWriteDeadline(time.Now().Add(blobIdle))
	return x.w.Write(p)
}

// forwardBlob serves getImageBlob on a workstation for a listed host: the
// listed host's daemon is asked and its bytes are relayed with the same
// Content-Length and X-Image-SHA256.
func (h *handlers) forwardBlob(w http.ResponseWriter, r *http.Request, host, name string) {
	if !vmtool.ValidImageName(name) {
		writeBlobStatus(w, http.StatusBadRequest, badImageName(name))
		return
	}
	m, done, err := h.hosts.WithToken(requestToken(r.Context())).Resolve(host)
	if err != nil {
		writeBlobStatus(w, statusFor(err), err.Error())
		return
	}
	defer done()
	src, ok := m.(app.ImageBlobSource)
	if !ok {
		writeBlobStatus(w, http.StatusInternalServerError, fmt.Sprintf("host %s keeps no image cache", host))
		return
	}
	rc, info, err := src.OpenImageBlob(r.Context(), name)
	if err != nil {
		writeBlobStatus(w, statusFor(err), err.Error())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	// The digest is relayed where the daemon put it: in the header (with
	// Content-Length), or as the trailer after the last byte (with X-Image-Size).
	trailer := info.SHA256 == "" && info.Trailer != nil
	if trailer {
		w.Header().Set("X-Image-Size", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Trailer", "X-Image-SHA256")
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("X-Image-SHA256", info.SHA256)
	}
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(&idleWriter{w: w, rc: http.NewResponseController(w)}, io.LimitReader(rc, info.Size))
	if err != nil || n != info.Size {
		slog.Warn("getImageBlob forward", "image", name, "host", host, "outcome", "interrupted")
		return
	}
	if trailer {
		// The upstream trailer is there once its body is read to the end; no
		// trailer (or more bytes than stated) means none is sent, and the
		// client keeps nothing.
		var extra [1]byte
		if k, rerr := rc.Read(extra[:]); k == 0 && errors.Is(rerr, io.EOF) {
			if d := info.Trailer(); d != "" {
				w.Header().Set("X-Image-SHA256", d)
			}
		}
	}
}
