package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/store/blob"
	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/tokenhash"
)

// TicketLifetime is how long a download ticket stays valid.
const TicketLifetime = 15 * time.Minute

const unauthorizedMsg = "unauthorized: missing or invalid bearer token"

var (
	imageNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

	internalErr = storeapi.InternalErrorJSONResponse{Error: "internal error"}
	badName     = storeapi.BadRequestJSONResponse{Error: "invalid name"}
	notFound    = storeapi.NotFoundJSONResponse{Error: "not found"}
)

func validImageName(n string) bool {
	return imageNameRE.MatchString(n) && !strings.Contains(n, "..")
}

// objectKey is where an image's bytes live. It includes the digest, so a
// push of new bytes never writes over the object that is being served.
func objectKey(name, sha string) string { return "images/" + name + "/" + sha }

// images holds what the image routes and ticket redemption share.
type images struct {
	blob blob.BlobStore
	db   *db.DB
	now  func() time.Time
	log  *slog.Logger

	tickets *ticketStore

	mu   sync.Mutex
	busy map[string]bool // names with a push (or delete) in flight
}

func newImages(log *slog.Logger, d Deps) *images {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &images{blob: d.Blob, db: d.DB, now: now, log: log, tickets: newTicketStore(), busy: map[string]bool{}}
}

func (i *images) ready() bool { return i != nil && i.blob != nil && i.db != nil }

// lock claims name without waiting; false means someone else holds it.
func (i *images) lock(name string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.busy[name] {
		return false
	}
	i.busy[name] = true
	return true
}

func (i *images) unlock(name string) {
	i.mu.Lock()
	delete(i.busy, name)
	i.mu.Unlock()
}

// fail logs a backend error without any request data and returns nothing; the
// caller answers with fixed text.
func (i *images) fail(op string, err error) {
	i.log.Error("backend error", "op", op, "err", err)
}

// cleanup deletes an object even when the request's context is gone.
func (i *images) cleanup(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := i.blob.Delete(ctx, key); err != nil {
		i.fail("blob delete", err)
	}
}

func toImage(m db.Image) storeapi.Image {
	return storeapi.Image{Name: m.Name, Sha256: m.SHA256, Size: m.Size, Updated: m.Updated}
}

// ---- tickets --------------------------------------------------------------

type ticketEntry struct {
	image, sha, issuer string
	expires            time.Time
}

// ticketStore keeps only SHA-256(ticket), in memory. A restart forgets all.
type ticketStore struct {
	mu sync.Mutex
	m  map[string]ticketEntry
}

func newTicketStore() *ticketStore { return &ticketStore{m: map[string]ticketEntry{}} }

func hashTicket(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// pruneLocked drops expired entries.
func (s *ticketStore) pruneLocked(now time.Time) {
	for k, e := range s.m {
		if !now.Before(e.expires) {
			delete(s.m, k)
		}
	}
}

// issue returns a new ticket (shown once, never stored) and its expiry.
func (s *ticketStore) issue(image, sha, issuer string, now time.Time) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	t := base64.RawURLEncoding.EncodeToString(raw[:])
	exp := now.Add(TicketLifetime)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	s.m[hashTicket(t)] = ticketEntry{image: image, sha: sha, issuer: issuer, expires: exp}
	return t, exp, nil
}

// redeem spends t if it is live and was issued for exactly this image at
// this digest. A ticket that fails a check is left as it was.
func (s *ticketStore) redeem(t, image, sha string, now time.Time) (string, bool) {
	k := hashTicket(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	e, ok := s.m[k]
	if !ok || e.image != image || e.sha != sha || !now.Before(e.expires) {
		return "", false
	}
	delete(s.m, k)
	return e.issuer, true
}

// ticketCaller authenticates `Authorization: Ticket <cred>`. It succeeds only
// for GET of a clean, unencoded /v1/images/{name}/blob whose image is indexed
// at the digest the ticket was issued for; the ticket is spent on success
// only. Every failure looks the same to the client.
func (s *Server) ticketCaller(r *http.Request, pat, cred string) (string, bool) {
	if !s.img.ready() || r.Method != http.MethodGet || pat != "GET /v1/images/{name}/blob" || r.URL.RawPath != "" {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/images/"), "/blob")
	if !validImageName(name) {
		return "", false
	}
	img, err := s.img.db.GetImage(r.Context(), name)
	if err != nil {
		return "", false
	}
	issuer, ok := s.img.tickets.redeem(cred, name, img.SHA256, s.img.now())
	if !ok {
		return "", false
	}
	infoFrom(r.Context()).ticketSHA = img.SHA256
	return "ticket:" + issuer, true
}

// ---- handlers -------------------------------------------------------------

func (h *handlers) ListImages(ctx context.Context, _ storeapi.ListImagesRequestObject) (storeapi.ListImagesResponseObject, error) {
	if !h.img.ready() {
		return storeapi.ListImages500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	rows, err := h.img.db.ListImages(ctx)
	if err != nil {
		h.img.fail("list images", err)
		return storeapi.ListImages500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.ImageList{Images: make([]storeapi.Image, 0, len(rows))}
	for _, m := range rows {
		out.Images = append(out.Images, toImage(m))
	}
	return storeapi.ListImages200JSONResponse(out), nil
}

func (h *handlers) GetImage(ctx context.Context, req storeapi.GetImageRequestObject) (storeapi.GetImageResponseObject, error) {
	if !validImageName(req.Name) {
		return storeapi.GetImage400JSONResponse{BadRequestJSONResponse: badName}, nil
	}
	if !h.img.ready() {
		return storeapi.GetImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	m, err := h.img.db.GetImage(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.GetImage404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("get image", err)
		return storeapi.GetImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.GetImage200JSONResponse(toImage(m)), nil
}

// bodyWatch notes whether reading the request body failed, so a client that
// hung up mid-push (a 400) is told from a storage failure (a 500).
type bodyWatch struct {
	r      io.Reader
	failed bool
}

func (b *bodyWatch) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		b.failed = true
	}
	return n, err
}

// hashCount feeds a hash and counts what passes through.
type hashCount struct {
	r io.Reader
	h io.Writer
	n int64
}

func (c *hashCount) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		_, _ = c.h.Write(p[:n])
	}
	return n, err
}

func (h *handlers) PutImage(ctx context.Context, req storeapi.PutImageRequestObject) (storeapi.PutImageResponseObject, error) {
	if !validImageName(req.Name) {
		return storeapi.PutImage400JSONResponse{BadRequestJSONResponse: badName}, nil
	}
	sha := req.Params.XImageSHA256
	if !tokenhash.Valid(sha) {
		return storeapi.PutImage400JSONResponse{BadRequestJSONResponse: storeapi.BadRequestJSONResponse{Error: "invalid X-Image-SHA256"}}, nil
	}
	size := infoFrom(ctx).contentLength
	if size < 0 {
		return storeapi.PutImage411JSONResponse{Error: "Content-Length is required"}, nil
	}
	im := h.img
	if !im.ready() {
		return storeapi.PutImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if !im.lock(req.Name) {
		return storeapi.PutImage409JSONResponse{Error: "another push of this image is in progress"}, nil
	}
	defer im.unlock(req.Name)

	prev, err := im.db.GetImage(ctx, req.Name)
	havePrev := err == nil
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		im.fail("get image", err)
		return storeapi.PutImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	key := objectKey(req.Name, sha)
	if havePrev && prev.SHA256 == sha {
		// The digest being pushed is the one being served. A different
		// length cannot be the same bytes; the same length needs no rewrite
		// (and never a write over the live object) if the object is intact.
		if prev.Size != size {
			return storeapi.PutImage400JSONResponse{BadRequestJSONResponse: storeapi.BadRequestJSONResponse{Error: "body does not match X-Image-SHA256"}}, nil
		}
		if st, err := im.blob.Stat(ctx, key); err == nil && st.Size == size {
			return storeapi.PutImage200JSONResponse(toImage(prev)), nil
		}
	}

	hasher := sha256.New()
	watch := &bodyWatch{r: req.Body}
	hc := &hashCount{r: watch, h: hasher}
	if err := im.blob.Put(ctx, key, hc, size); err != nil {
		im.cleanup(ctx, key)
		if errors.Is(err, blob.ErrShortBody) || watch.failed {
			return storeapi.PutImage400JSONResponse{BadRequestJSONResponse: storeapi.BadRequestJSONResponse{Error: "request body shorter than Content-Length"}}, nil
		}
		im.fail("blob put", err)
		return storeapi.PutImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if hc.n != size || hex.EncodeToString(hasher.Sum(nil)) != sha {
		im.cleanup(ctx, key)
		return storeapi.PutImage400JSONResponse{BadRequestJSONResponse: storeapi.BadRequestJSONResponse{Error: "body does not match X-Image-SHA256"}}, nil
	}
	if err := im.db.PutImage(ctx, db.Image{Name: req.Name, SHA256: sha, Size: size}); err != nil {
		im.cleanup(ctx, key)
		im.fail("put image", err)
		return storeapi.PutImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if havePrev && prev.SHA256 != sha {
		im.cleanup(ctx, objectKey(req.Name, prev.SHA256))
	}
	m, err := im.db.GetImage(ctx, req.Name)
	if err != nil {
		im.fail("get image", err)
		return storeapi.PutImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutImage201JSONResponse(toImage(m)), nil
}

func (h *handlers) DeleteImage(ctx context.Context, req storeapi.DeleteImageRequestObject) (storeapi.DeleteImageResponseObject, error) {
	if !validImageName(req.Name) {
		return storeapi.DeleteImage400JSONResponse{BadRequestJSONResponse: badName}, nil
	}
	im := h.img
	if !im.ready() {
		return storeapi.DeleteImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if !im.lock(req.Name) {
		return storeapi.DeleteImage409JSONResponse{Error: "a push of this image is in progress"}, nil
	}
	defer im.unlock(req.Name)
	m, err := im.db.GetImage(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteImage404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		im.fail("get image", err)
		return storeapi.DeleteImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if err := im.db.DeleteImage(ctx, req.Name); err != nil && !errors.Is(err, db.ErrNotFound) {
		im.fail("delete image", err)
		return storeapi.DeleteImage500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	im.cleanup(ctx, objectKey(m.Name, m.SHA256))
	return storeapi.DeleteImage204Response{}, nil
}

func (h *handlers) GetImageBlob(ctx context.Context, req storeapi.GetImageBlobRequestObject) (storeapi.GetImageBlobResponseObject, error) {
	if !validImageName(req.Name) {
		return storeapi.GetImageBlob400JSONResponse{BadRequestJSONResponse: badName}, nil
	}
	im := h.img
	if !im.ready() {
		return storeapi.GetImageBlob500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	m, err := im.db.GetImage(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.GetImageBlob404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		im.fail("get image", err)
		return storeapi.GetImageBlob500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	// A ticket was checked against the digest at the time of the request;
	// if the image was replaced since, never serve the other bytes.
	if want := infoFrom(ctx).ticketSHA; want != "" && want != m.SHA256 {
		return storeapi.GetImageBlob401JSONResponse{UnauthorizedJSONResponse: storeapi.UnauthorizedJSONResponse{Error: unauthorizedMsg}}, nil
	}
	rc, st, err := im.blob.Get(ctx, objectKey(m.Name, m.SHA256))
	if err != nil {
		im.fail("blob get", err)
		return storeapi.GetImageBlob500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if st.Size != m.Size {
		_ = rc.Close()
		im.fail("blob get", errors.New("object size differs from the index"))
		return storeapi.GetImageBlob500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.GetImageBlob200ApplicationoctetStreamResponse{
		Body:          rc,
		ContentLength: m.Size,
		Headers:       storeapi.GetImageBlob200ResponseHeaders{XImageSHA256: m.SHA256},
	}, nil
}

func (h *handlers) CreateImageTicket(ctx context.Context, req storeapi.CreateImageTicketRequestObject) (storeapi.CreateImageTicketResponseObject, error) {
	if !validImageName(req.Name) {
		return storeapi.CreateImageTicket400JSONResponse{BadRequestJSONResponse: badName}, nil
	}
	im := h.img
	if !im.ready() {
		return storeapi.CreateImageTicket500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	m, err := im.db.GetImage(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.CreateImageTicket404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		im.fail("get image", err)
		return storeapi.CreateImageTicket500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	t, exp, err := im.tickets.issue(m.Name, m.SHA256, callerFrom(ctx), im.now())
	if err != nil {
		im.fail("ticket", err)
		return storeapi.CreateImageTicket500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.CreateImageTicket201JSONResponse{Ticket: t, ExpiresAt: exp, Image: m.Name, Sha256: m.SHA256, Size: m.Size}, nil
}
