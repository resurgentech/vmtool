package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	fetchSHA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fetchTicket = "PLACEHOLDER-not-a-secret-1"
)

var fetchReq = api.FetchImageRequest{Sha256: fetchSHA, Size: 10, Ticket: fetchTicket}

func ndjson(w http.ResponseWriter, evs ...api.FetchImageEvent) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	for _, e := range evs {
		_ = json.NewEncoder(w).Encode(e)
	}
}

func reasonOf(r api.FetchImageEventReason) *api.FetchImageEventReason { return &r }

// The client posts the request to the operation's path with the bearer, reads
// the NDJSON stream line by line and returns nil after the done line.
func TestFetchImageStreams(t *testing.T) {
	var body api.FetchImageRequest
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/hosts/veritech01/images/base.qcow2/fetch" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sz := int64(10)
		ndjson(w,
			api.FetchImageEvent{Event: api.FetchImageEventEventProgress, Received: ptr(int64(5)), Total: ptr(int64(10))},
			api.FetchImageEvent{Event: api.FetchImageEventEventDone, Image: &api.ImageManifestEntry{Name: "base.qcow2", Size: &sz}})
	})
	m := f.manager(t)
	var got []string
	err := m.FetchImage(context.Background(), "base.qcow2", fetchReq, func(e api.FetchImageEvent) { got = append(got, string(e.Event)) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "progress,done" {
		t.Errorf("events %v", got)
	}
	if body != fetchReq {
		t.Errorf("daemon got %+v", body)
	}
	if _, auths := f.seen(); len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Errorf("auth %v", auths)
	}
}

// S11 tells the three "cannot fetch" cases apart by type and kind.
func TestFetchImageCannotFetchKinds(t *testing.T) {
	cases := []struct {
		name   string
		h      http.HandlerFunc
		kind   FetchFailure
		appK   app.Kind
		notNil bool
	}{
		{"old daemon: plain-text 404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, FetchDaemonTooOld, app.KindNotFound, true},
		{"no store_url: 409", func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, 409, api.Error{Error: "no store configured on veritech01; redeploy with a store"})
		}, FetchNoStore, app.KindConflict, true},
		{"store unreachable: error line", func(w http.ResponseWriter, _ *http.Request) {
			ndjson(w, api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: ptr("store unreachable from veritech01: boom"), Reason: reasonOf(api.StoreUnreachable)})
		}, FetchStoreUnreachable, app.KindBadGateway, true},
		{"mismatch: error line", func(w http.ResponseWriter, _ *http.Request) {
			ndjson(w, api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: ptr("digest"), Reason: reasonOf(api.Mismatch)})
		}, FetchMismatch, app.KindBadGateway, true},
		{"store refused: error line", func(w http.ResponseWriter, _ *http.Request) {
			ndjson(w, api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: ptr("401"), Reason: reasonOf(api.StoreRefused)})
		}, FetchStoreRefused, app.KindBadGateway, true},
		{"other error line", func(w http.ResponseWriter, _ *http.Request) {
			ndjson(w, api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: ptr("disk full")})
		}, FetchOther, app.KindBadGateway, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeAPI(t, specVersion(t), c.h)
			err := f.manager(t).FetchImage(context.Background(), "base.qcow2", fetchReq, nil)
			var fe *FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("err %v is not a *FetchError", err)
			}
			if fe.Kind != c.kind {
				t.Errorf("kind %v, want %v (%v)", fe.Kind, c.kind, err)
			}
			if k := kindOfErr(err); k != c.appK {
				t.Errorf("app kind %v, want %v", k, c.appK)
			}
			if strings.Contains(err.Error(), fetchTicket) {
				t.Error("ticket in error")
			}
			if c.kind == FetchDaemonTooOld && !IsFetchUnsupported(err) {
				t.Error("IsFetchUnsupported false")
			}
		})
	}
}

func kindOfErr(err error) app.Kind {
	var ae *app.Error
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return -1
}

// Another refusal (a foreign image: 409 with another message; a JSON 404) is
// an ordinary status error, not one of the "cannot fetch" kinds.
func TestFetchImageOtherRefusalsAreStatusErrors(t *testing.T) {
	for _, c := range []struct {
		status int
		msg    string
	}{{409, `image "base.qcow2" already exists on veritech01 at /x; vmtool did not create it`}, {404, "host not found"}} {
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, c.status, api.Error{Error: c.msg}) })
		err := f.manager(t).FetchImage(context.Background(), "base.qcow2", fetchReq, nil)
		var fe *FetchError
		if errors.As(err, &fe) {
			t.Errorf("%d: %v is a FetchError", c.status, err)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.Status != c.status {
			t.Errorf("%d: err %v", c.status, err)
		}
		if c.status == 404 && !errors.Is(err, vmtool.ErrNotFound) {
			t.Error("404 JSON should still match ErrNotFound")
		}
	}
}

// A stream that ends without done or error is a bad gateway, not success.
func TestFetchImageTruncatedStream(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, _ *http.Request) {
		ndjson(w, api.FetchImageEvent{Event: api.FetchImageEventEventProgress, Received: ptr(int64(1)), Total: ptr(int64(10))})
	})
	err := f.manager(t).FetchImage(context.Background(), "base.qcow2", fetchReq, nil)
	if err == nil || kindOfErr(err) != app.KindBadGateway {
		t.Errorf("err %v", err)
	}
}
