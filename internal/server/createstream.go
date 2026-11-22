package server

// createVM over REST streams its progress when the request asks for it.
//
//   - A request whose Accept header names application/x-ndjson gets a 200
//     stream of api.CreateVMEvent lines (progress, then one done or one error),
//     each flushed as written. The route stays the strict handler's: tunnelMux
//     only marks the request's context and calls the strict adapter, so body
//     decoding, the store session and the operation note run as for the JSON
//     call, and the hypervisor's 403 (gateWorkstationOnly) is unchanged.
//   - The stream starts with the first workflow event. A refusal before any
//     event (validation, name taken, unknown host, image not found) is the same
//     JSON status as without the header.
//   - No event line is logged, like no request body (an event's detail can hold
//     an address, and its output a playbook's output).

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

type createStreamKeyType struct{}

var createStreamKey createStreamKeyType

// CreateVM on tunnelMux marks a request that asks for the stream and hands
// every request to the strict handler.
func (t tunnelMux) CreateVM(w http.ResponseWriter, r *http.Request, host api.HostName) {
	if acceptsNDJSON(r.Header.Values("Accept")) {
		cs := &createStream{st: newNDJSON(w)}
		r = r.WithContext(context.WithValue(r.Context(), createStreamKey, cs))
	}
	t.ServerInterface.CreateVM(w, r, host)
}

// acceptsNDJSON reports whether an Accept header names application/x-ndjson
// with a quality above 0. A wildcard does not: the stream is opt-in.
func acceptsNDJSON(values []string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil || mt != ndjsonMediaType {
				continue
			}
			if q, ok := params["q"]; ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f <= 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}

const ndjsonMediaType = "application/x-ndjson"

func createStreamOf(ctx context.Context) *createStream {
	cs, _ := ctx.Value(createStreamKey).(*createStream)
	return cs
}

// createStream writes a create's events as they are emitted and keeps the
// timings. started is true once a line was written.
type createStream struct {
	mu    sync.Mutex
	st    *ndjsonStream
	timer app.CreateTimer
}

func (c *createStream) started() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.started
}

func (c *createStream) progress(ev app.ProgressEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer.Add(ev)
	elapsed := ev.ElapsedMS
	p := api.ProgressEvent{
		Stage: ev.Stage, Status: ev.Status, ElapsedMs: &elapsed,
		Detail: ptrToNonEmpty(ev.Detail), Output: ptrToNonEmpty(ev.Output),
	}
	if ev.Sent != 0 {
		p.Sent = &ev.Sent
	}
	if ev.Total != 0 {
		p.Total = &ev.Total
	}
	c.st.send(api.CreateVMEvent{Event: api.CreateVMEventEventProgress, Progress: &p})
}

func (c *createStream) timings() *api.CreateTimings {
	var out api.CreateTimings
	b, err := json.Marshal(c.timer.Timings())
	if err != nil || json.Unmarshal(b, &out) != nil {
		return nil
	}
	return &out
}

func (c *createStream) done(info *vmtool.VMInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := toAPI(info)
	c.st.send(api.CreateVMEvent{Event: api.CreateVMEventEventDone, Vm: &v, Timings: c.timings()})
}

func (c *createStream) fail(err error, info *vmtool.VMInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := apiErr(err)
	kind := api.CreateVMEventKind("internal")
	switch kindOf(err) {
	case app.KindInvalid:
		kind = api.CreateVMEventKind("invalid")
	case app.KindNotFound:
		kind = api.CreateVMEventKind("not_found")
	case app.KindConflict:
		kind = api.CreateVMEventKind("conflict")
	}
	ev := api.CreateVMEvent{Event: api.CreateVMEventEventError, Error: &e.Error, Kind: &kind, Output: e.Output, Timings: c.timings()}
	if info != nil {
		v := toAPI(info)
		ev.Vm = &v
	}
	c.st.send(ev)
}

// streamed is the response of a create whose answer went out as a stream:
// there is nothing left to write.
type streamed struct{}

func (streamed) VisitCreateVMResponse(http.ResponseWriter) error { return nil }
