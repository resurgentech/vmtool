package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
)

// F1: the client removes its own token from a daemon's text.

const redactTok = "PLACEHOLDER-not-a-secret-1"

// echoes are the shapes a daemon that echoed request headers would send.
var echoes = []string{
	redactTok,
	"Bearer " + redactTok,
	"bearer " + redactTok,
	"BEARER " + redactTok,
}

// captureLogs collects whatever the log and slog defaults print during the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldLog, oldSlog := log.Writer(), slog.Default()
	log.SetOutput(&buf)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { log.SetOutput(oldLog); slog.SetDefault(oldSlog) })
	return &buf
}

func noToken(t *testing.T, what, got string, logs *bytes.Buffer) {
	t.Helper()
	if strings.Contains(got, redactTok) {
		t.Errorf("%s contains the token: %q", what, got)
	}
	if logs != nil && strings.Contains(logs.String(), redactTok) {
		t.Errorf("%s: a log line contains the token: %q", what, logs.String())
	}
}

func redactManager(t *testing.T, f *fakeAPI) *Manager {
	t.Helper()
	m, err := New("veritech01", f.srv.URL, redactTok, WithRootCAs(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestCleanText(t *testing.T) {
	for _, e := range echoes {
		got := cleanText("a "+e+" b", redactTok)
		if got != "a [redacted] b" {
			t.Errorf("%q -> %q", e, got)
		}
	}
	if got := cleanText("x "+redactTok+" y "+redactTok, redactTok); got != "x [redacted] y [redacted]" {
		t.Errorf("every occurrence: %q", got)
	}
	// Redaction comes before truncation: a token straddling the 512 cut leaves
	// no half of itself.
	long := strings.Repeat("x", 500) + redactTok + strings.Repeat("y", 100)
	got := cleanText(long, redactTok)
	if strings.Contains(got, redactTok[:6]) {
		t.Errorf("a piece of the token survived truncation: %q", got[480:])
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("no marker: %q", got[480:])
	}
	// Control characters are still cleaned and length still bounded.
	if got := cleanText("a\nb\x00c", redactTok); got != "a b c" {
		t.Errorf("controls: %q", got)
	}
	if got := cleanText(strings.Repeat("z", 2000), redactTok); len(got) > 515 {
		t.Errorf("not truncated: %d", len(got))
	}
	// An empty token redacts nothing.
	if got := cleanText("Bearer  x [redacted] y", ""); got != "Bearer  x [redacted] y" {
		t.Errorf("empty token changed the text: %q", got)
	}
}

func TestStatusErrRedactsToken(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), nil)
	m := redactManager(t, f)
	logs := captureLogs(t)
	for _, e := range echoes {
		body, _ := json.Marshal(api.Error{Error: "bad header Authorization: " + e})
		err := m.statusErr(400, body)
		noToken(t, "JSON error "+e, fmt.Sprintf("%v %+v", err, err), logs)
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("%q: no marker in %q", e, err)
		}
		// A plain-text body is never quoted.
		err = m.statusErr(502, []byte("upstream said "+e))
		noToken(t, "plain body "+e, fmt.Sprintf("%v %+v", err, err), logs)
	}
}

func TestFetchRedactsToken(t *testing.T) {
	for _, e := range echoes {
		// Non-200 JSON refusal (the NoStore path keeps its text).
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("x") == "" && strings.HasSuffix(r.URL.Path, "/fetch") {
				jsonReply(w, 409, api.Error{Error: NoStorePrefix + " echo " + e})
			}
		})
		logs := captureLogs(t)
		err := redactManager(t, f).FetchImage(context.Background(), "base.qcow2", fetchReq, nil)
		if err == nil {
			t.Fatal("no error")
		}
		noToken(t, "fetch status "+e, fmt.Sprintf("%v %+v", err, err), logs)

		// Another status.
		f = newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 400, api.Error{Error: "echo " + e})
		})
		err = redactManager(t, f).FetchImage(context.Background(), "base.qcow2", fetchReq, nil)
		noToken(t, "fetch 400 "+e, fmt.Sprintf("%v %+v", err, err), logs)

		// NDJSON error line: the error and the event the caller sees.
		f = newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			ndjson(w, api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: ptr("echo " + e)})
		})
		var evs []string
		err = redactManager(t, f).FetchImage(context.Background(), "base.qcow2", fetchReq, func(ev api.FetchImageEvent) {
			evs = append(evs, fmt.Sprintf("%+v %s", ev, derefString(ev.Error)))
		})
		if err == nil {
			t.Fatal("no error")
		}
		noToken(t, "fetch NDJSON error "+e, fmt.Sprintf("%v %+v", err, err), logs)
		noToken(t, "fetch event "+e, strings.Join(evs, "|"), nil)
		if len(evs) != 1 {
			t.Errorf("events %v", evs)
		}
	}
}

func TestExportRedactsToken(t *testing.T) {
	for _, e := range echoes {
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			exportND(w, api.ExportTemplateEvent{Event: api.ExportTemplateEventEventError, Error: ptr("echo " + e)})
		})
		logs := captureLogs(t)
		_, err := redactManager(t, f).ExportTemplate(context.Background(), "proj", "", false, nil)
		if err == nil {
			t.Fatal("no error")
		}
		noToken(t, "export NDJSON error "+e, fmt.Sprintf("%v %+v", err, err), logs)

		f = newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 500, api.Error{Error: "echo " + e})
		})
		_, err = redactManager(t, f).ExportTemplate(context.Background(), "proj", "", false, nil)
		noToken(t, "export status "+e, fmt.Sprintf("%v %+v", err, err), logs)
	}
}

func TestTunnelRedactsToken(t *testing.T) {
	for _, e := range echoes {
		body, _ := json.Marshal(api.Error{Error: "echo " + e})
		srv, pool, _ := fakeDaemon(t, false, 400, string(body))
		logs := captureLogs(t)
		conn, err := Dial(context.Background(), srv.URL, "box", "web", redactTok, &tls.Config{RootCAs: pool})
		if err == nil {
			conn.Close()
			t.Fatal("no error")
		}
		noToken(t, "tunnel "+e, fmt.Sprintf("%v %+v", err, err), logs)
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("no marker in %q", err)
		}
	}
}

func TestProbeVersionRedactsToken(t *testing.T) {
	for _, e := range echoes {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 200, api.VersionInfo{ApiVersion: "echo " + e, BuildVersion: "test", Host: "veritech01"})
		}))
		t.Cleanup(srv.Close)
		m, err := New("veritech01", srv.URL, redactTok, WithRootCAs(srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		logs := captureLogs(t)
		st := m.Probe(context.Background())
		noToken(t, "probe version "+e, fmt.Sprintf("%+v", st), logs)
		if !strings.Contains(st.APIVersion, "[redacted]") {
			t.Errorf("api version %q", st.APIVersion)
		}
	}
}
