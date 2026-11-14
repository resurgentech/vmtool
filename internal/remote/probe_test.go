package remote

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func noOps(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("probe reached %s %s", r.Method, r.URL.Path)
	}
}

func TestProbeReachable(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), noOps(t))
	st := f.manager(t).Probe(context.Background())
	if !st.Reachable || st.Err != nil || st.APIVersion != specVersion(t) {
		t.Fatalf("status %+v", st)
	}
	if st.Host != "veritech01" || st.Address != strings.TrimPrefix(f.srv.URL, "https://") {
		t.Errorf("host %q address %q", st.Host, st.Address)
	}
	if !st.CertNotAfter.Equal(f.srv.Certificate().NotAfter) {
		t.Errorf("CertNotAfter %v, want the leaf's %v", st.CertNotAfter, f.srv.Certificate().NotAfter)
	}
	if d, ok := st.CertDaysLeft(time.Now()); !ok || d < 1 {
		t.Errorf("days left %d %v", d, ok)
	}
}

// A mismatch answers: reachable, with the version and a note.
func TestProbeVersionMismatch(t *testing.T) {
	f := newFakeAPI(t, "0.9.0", noOps(t))
	st := f.manager(t).Probe(context.Background())
	var ve *VersionError
	if !st.Reachable || st.APIVersion != "0.9.0" || !errors.As(st.Err, &ve) {
		t.Fatalf("status %+v", st)
	}
	if st.CertNotAfter.IsZero() {
		t.Error("no certificate recorded for a host that answered")
	}
}

// An untrusted certificate is unreachable, with the TLS error, and no token
// in it.
func TestProbeUntrusted(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), noOps(t))
	m, err := New("veritech01", f.srv.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	st := m.Probe(context.Background())
	if st.Reachable || st.Err == nil || !strings.Contains(st.Err.Error(), "certificate") {
		t.Fatalf("status %+v", st)
	}
	if strings.Contains(st.Err.Error(), testToken) {
		t.Error("error contains the token")
	}
}

func TestProbeRespectsContext(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	m, err := New("veritech01", srv.URL, testToken, WithRootCAs(pool))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	st := m.Probe(ctx)
	if st.Reachable || st.Err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("status %+v after %v", st, time.Since(start))
	}
}
