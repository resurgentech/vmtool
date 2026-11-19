package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/storeclient"
)

const (
	ctTicket = "PLACEHOLDER-ticket-not-a-secret"
	ctSHA    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// A store double (whoami, image metadata, tickets) and a daemon double
// (manifest, fetchImage) over TLS on loopback. create against the real
// remote.Manager: the ticket reaches the daemon in the fetchImage request body
// and nowhere else; a failing fetch leaves it out of every progress event and
// the error; the image's bytes are never requested from the store.
func TestCreateFetchTicketOnlyInRequestBody(t *testing.T) {
	var mu sync.Mutex
	var daemonReqs []string // method path query and header values, no body
	var fetchBody api.FetchImageRequest
	blobs := 0

	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.URL.Path == "/v1/whoami":
			out(200, map[string]string{"name": "jared"})
		case r.URL.Path == "/v1/images/u.qcow2":
			out(200, storeapi.Image{Name: "u.qcow2", Sha256: ctSHA, Size: 2048, Updated: time.Now()})
		case r.URL.Path == "/v1/images/u.qcow2/tickets":
			out(201, storeapi.Ticket{Ticket: ctTicket, ExpiresAt: time.Now().Add(time.Minute), Image: "u.qcow2", Sha256: ctSHA, Size: 2048})
		case strings.HasSuffix(r.URL.Path, "/blob"):
			mu.Lock()
			blobs++
			mu.Unlock()
			out(200, map[string]string{})
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	defer store.Close()
	storePool := store.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		daemonReqs = append(daemonReqs, fmt.Sprintf("%s %s?%s %v", r.Method, r.URL.Path, r.URL.RawQuery, r.Header))
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest"):
			jsonReply(w, 200, api.ImageManifestEntry{Name: "u.qcow2"})
		case strings.HasSuffix(r.URL.Path, "/fetch"):
			_ = json.NewDecoder(r.Body).Decode(&fetchBody)
			ndjson(w,
				api.FetchImageEvent{Event: api.FetchImageEventEventProgress, Received: ptr(int64(1024)), Total: ptr(int64(2048))},
				api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: ptr("disk full on veritech01"), Reason: nil})
		default:
			http.NotFound(w, r)
		}
	})
	m := f.manager(t)

	t.Chdir(t.TempDir())
	var notes []string
	svc := &app.Service{
		Store: app.OpenStoreSession(store.URL, func() (string, error) { return "PLACEHOLDER-not-a-secret-1", nil },
			[]storeclient.Option{storeclient.WithRootCAs(storePool)}, func(l string) { notes = append(notes, l) }),
	}
	var events []app.ProgressEvent
	_, err := svc.CreateVM(context.Background(), m, app.CreateVMInput{
		Host: "veritech01", Name: "web", Image: "u.qcow2",
		OnProgress: func(ev app.ProgressEvent) { events = append(events, ev) },
	})
	if err == nil {
		t.Fatal("create succeeded against a failing fetch")
	}
	if fetchBody.Ticket != ctTicket || fetchBody.Sha256 != ctSHA || fetchBody.Size != 2048 {
		t.Fatalf("daemon was sent %+v", fetchBody)
	}
	all := err.Error() + fmt.Sprintf("%+v", events) + strings.Join(notes, "\n")
	if strings.Contains(all, ctTicket) {
		t.Fatalf("the ticket leaked into an event, a note or the error:\n%s", all)
	}
	for _, r := range daemonReqs {
		if strings.Contains(r, ctTicket) {
			t.Errorf("the ticket is in a daemon request's path, query or headers: %s", r)
		}
	}
	if blobs != 0 {
		t.Errorf("the client read %d image blobs from the store", blobs)
	}
	var progress *app.ProgressEvent
	for i := range events {
		if events[i].Stage == "upload" && events[i].Status == "progress" {
			progress = &events[i]
		}
	}
	if progress == nil || progress.Sent != 1024 || progress.Total != 2048 {
		t.Errorf("daemon progress was not mapped to an upload event: %+v", events)
	}
	last := events[len(events)-1]
	if last.Stage != "upload" || last.Status != "error" || !strings.Contains(last.Detail, "disk full") {
		t.Errorf("last event %+v", last)
	}
}
