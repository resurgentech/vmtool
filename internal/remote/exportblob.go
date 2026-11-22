package remote

// exportTemplate and getImageBlob, the client side (API 2.4.0): a hypervisor
// daemon writes a template into its image cache as a qcow2 image and streams
// NDJSON progress; a cached image the daemon created is downloaded with its
// Content-Length and X-Image-SHA256 (or X-Image-Size and an X-Image-SHA256 trailer) for the caller to check. Neither request
// nor response bodies are logged here (nothing in this package logs).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// blobIdle is how long a download may deliver nothing before it fails.
const blobIdle = 2 * time.Minute

var blobDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ExportTemplate asks the daemon to export the template name (its name or LV
// name; pool limits the match) onto its array as a temporary volume (hash: and
// to read it once for its digest) and calls on with every
// progress line until the done or error line. It returns the cache entry after
// done. An error line is an error after on has seen the lines before it; a
// refusal before the stream is the daemon's status as an *app.Error.
func (m *Manager) ExportTemplate(ctx context.Context, name, pool string, hash bool, on func(app.ExportEvent)) (app.ExportedImage, error) {
	ctx, cancel := context.WithTimeout(ctx, m.long)
	defer cancel()
	resp, err := m.client.ExportTemplate(ctx, m.name, name, &api.ExportTemplateParams{Pool: nonEmpty(pool), Hash: hashParam(hash)})
	if err != nil {
		return app.ExportedImage{}, m.transportErr("exportTemplate", m.long, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return app.ExportedImage{}, m.transportErr("exportTemplate", m.long, err)
		}
		var e api.Error
		if resp.StatusCode == http.StatusNotFound && json.Unmarshal(body, &e) != nil {
			// An older daemon has no such route: Go's mux answers plain text.
			return app.ExportedImage{}, fail(app.KindNotFound, fmt.Errorf("%s: the daemon does not support exportTemplate (it predates API 2.4.0): %w", m.name, vmtool.ErrNotFound))
		}
		return app.ExportedImage{}, m.statusErr(resp.StatusCode, body)
	}
	br := bufio.NewReaderSize(resp.Body, 4096)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			var ev api.ExportTemplateEvent
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				return app.ExportedImage{}, fail(app.KindBadGateway, fmt.Errorf("%s: exportTemplate: a line of the stream is not JSON", m.name))
			}
			switch ev.Event {
			case api.ExportTemplateEventEventProgress:
				if on != nil {
					e := app.ExportEvent{Stage: "export"}
					if ev.Stage != nil {
						e.Stage = string(*ev.Stage)
					}
					if ev.Received != nil {
						e.Received = *ev.Received
					}
					if ev.Total != nil {
						e.Total = *ev.Total
					}
					on(e)
				}
			case api.ExportTemplateEventEventDone:
				if ev.Image == nil || ev.Image.Size == nil {
					return app.ExportedImage{}, fail(app.KindBadGateway, fmt.Errorf("%s: exportTemplate: the done line has no image entry", m.name))
				}
				out := app.ExportedImage{Name: ev.Image.Name, Size: *ev.Image.Size}
				if ev.Image.Sha256 != nil {
					out.SHA256 = *ev.Image.Sha256
				}
				if ev.Image.Path != nil {
					out.Path = *ev.Image.Path
				}
				return out, nil
			case api.ExportTemplateEventEventError:
				return app.ExportedImage{}, fail(app.KindBadGateway, fmt.Errorf("%s: %s", m.name, m.clean(derefString(ev.Error))))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return app.ExportedImage{}, fail(app.KindBadGateway, fmt.Errorf("%s: exportTemplate: the stream ended without a done or error line", m.name))
			}
			return app.ExportedImage{}, m.transportErr("exportTemplate", m.long, err)
		}
	}
}

var _ app.TemplateExporter = (*Manager)(nil)

// OpenImageBlob asks the daemon for the bytes of the cached image name (one
// it created: the daemon refuses any other) and returns the stream with the
// size (Content-Length) and SHA-256 (X-Image-SHA256) the daemon states. A
// response without either is refused. The caller reads the stream, checks it
// against the info and closes it; a download that delivers nothing for two
// minutes fails.
func (m *Manager) OpenImageBlob(ctx context.Context, name string) (io.ReadCloser, app.ImageBlobInfo, error) {
	ctx, cancel := context.WithCancel(ctx)
	// Response headers within the ordinary timeout.
	headerTimer := time.AfterFunc(m.timeout, cancel)
	resp, err := m.client.GetImageBlob(ctx, m.name, name)
	late := !headerTimer.Stop() // the timer fired: no headers in time
	if err != nil {
		if late {
			// cancel was the header timer's: report the timeout, not "context canceled"
			err = context.DeadlineExceeded
		}
		cancel()
		return nil, app.ImageBlobInfo{}, m.transportErr("getImageBlob", m.timeout, err)
	}
	refuse := func(err error) (io.ReadCloser, app.ImageBlobInfo, error) {
		resp.Body.Close()
		cancel()
		return nil, app.ImageBlobInfo{}, err
	}
	if resp.StatusCode != http.StatusOK {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if rerr != nil {
			return refuse(m.transportErr("getImageBlob", m.timeout, rerr))
		}
		return refuse(m.statusErr(resp.StatusCode, body))
	}
	// The size is Content-Length, or X-Image-Size
	// (a chunked body whose digest follows as a trailer).
	size := resp.ContentLength
	sum := strings.ToLower(resp.Header.Get("X-Image-SHA256"))
	info := app.ImageBlobInfo{}
	switch {
	case blobDigestRE.MatchString(sum):
		info.SHA256 = sum
	case declaresTrailer(resp):
		// The digest is sent after the last byte; it is read at the end.
		info.Trailer = func() string { return strings.ToLower(resp.Trailer.Get("X-Image-SHA256")) }
	default:
		if size < 0 {
			return refuse(fail(app.KindBadGateway, fmt.Errorf("%s: getImageBlob: the daemon did not state the image's size", m.name)))
		}
		return refuse(fail(app.KindBadGateway, fmt.Errorf("%s: getImageBlob: the daemon did not send a SHA-256 digest for the image", m.name)))
	}
	if size < 0 {
		n, err := strconv.ParseInt(resp.Header.Get("X-Image-Size"), 10, 64)
		if err != nil || n < 0 {
			return refuse(fail(app.KindBadGateway, fmt.Errorf("%s: getImageBlob: the daemon did not state the image's size", m.name)))
		}
		size = n
	}
	info.Size = size
	body := &blobBody{rc: resp.Body, idle: blobIdle, cancel: cancel}
	body.timer = time.AfterFunc(blobIdle, cancel)
	return body, info, nil
}

// declaresTrailer reports whether the response announced X-Image-SHA256 as a
// trailer (Go's client lists the announced keys in resp.Trailer).
func declaresTrailer(resp *http.Response) bool {
	for k := range resp.Trailer {
		if http.CanonicalHeaderKey(k) == "X-Image-Sha256" {
			return true
		}
	}
	return false
}

func hashParam(hash bool) *bool {
	if !hash {
		return nil
	}
	return &hash
}

// blobBody fails a read that has waited longer than the idle time (a timer
// pushed forward on every read cancels the request) and releases the request
// on Close.
type blobBody struct {
	rc     io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelFunc
	once   sync.Once
}

func (b *blobBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	return b.rc.Read(p)
}

func (b *blobBody) Close() error {
	var err error
	b.once.Do(func() {
		b.timer.Stop()
		err = b.rc.Close()
		b.cancel()
	})
	return err
}

var _ app.ImageBlobSource = (*Manager)(nil)
