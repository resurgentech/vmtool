package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/vmtool/internal/config"
)

func keyRequestServer(t *testing.T, requestURL string) *httptest.Server {
	t.Helper()
	cfg := &config.Config{Name: "hv1", Role: config.RoleHypervisor, TokenSHA256: config.Hash(testToken)}
	cfg.PersonalKeys.RequestURL = requestURL
	ts := httptest.NewServer((&Server{Config: cfg}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// GET /v1/key-request is public: 200 with the configured URL, 404 with the
// usual error body when none is set. No token is sent.
func TestKeyRequestDiscovery(t *testing.T) {
	ts := keyRequestServer(t, "https://keys.example.lab/v1/services/example/requests")
	resp, err := http.Get(ts.URL + "/v1/key-request")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("set: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if got, want := strings.TrimSpace(string(body)), `{"request_url":"https://keys.example.lab/v1/services/example/requests"}`; got != want {
		t.Errorf("body %s, want %s", got, want)
	}

	none := keyRequestServer(t, "")
	resp2, err := http.Get(none.URL + "/v1/key-request")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 404 || !strings.Contains(string(body2), `"error"`) {
		t.Errorf("unset: %d %s", resp2.StatusCode, body2)
	}
}

// request.Ensure's discovery finds the URL through a vmtool daemon and files
// the request at the fake provider.
func TestKeyRequestEnsureThroughDaemon(t *testing.T) {
	p := keyclienttest.NewProvider(t)
	p.SetInterval(1)
	p.Script(keyclienttest.Approved())
	ts := keyRequestServer(t, p.RequestURL())

	tokenPath := filepath.Join(t.TempDir(), "token")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := request.Ensure(ctx, request.Options{
		ServiceURL: ts.URL,
		TokenPath:  tokenPath,
		Role:       "operator",
		NameHint:   "user@ws",
		Host:       "ws",
		Tool:       "vmtool-test/0.0.0",
		Wait:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Filed()) != 1 {
		t.Fatalf("filed %d requests, want 1", len(p.Filed()))
	}
	got, err := os.ReadFile(tokenPath)
	if err != nil || strings.TrimSpace(string(got)) != p.Key() {
		t.Errorf("token file: %v", err)
	}
	_ = res
}

// With no request URL, discovery is a 404 and Ensure files nothing.
func TestKeyRequestEnsureUnset(t *testing.T) {
	p := keyclienttest.NewProvider(t)
	ts := keyRequestServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := request.Ensure(ctx, request.Options{
		ServiceURL: ts.URL, TokenPath: filepath.Join(t.TempDir(), "token"),
		Role: "operator", NameHint: "user@ws", Host: "ws", Tool: "vmtool-test/0.0.0", Wait: true,
	})
	if err == nil {
		t.Fatal("Ensure succeeded with no request URL")
	}
	if len(p.Filed()) != 0 {
		t.Errorf("filed %d requests", len(p.Filed()))
	}
}
