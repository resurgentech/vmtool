package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func TestGuestSvcUsesRequestBearer(t *testing.T) {
	h := &handlers{svc: &app.Service{PlaybookDir: "pb"}, tunnelExe: "/opt/vmtool/vmtool"}
	r := httptest.NewRequest("POST", "/v1/hosts/jetfire/vms/web/exec", nil)
	r.Header.Set("Authorization", "Bearer req-token-1")
	ctx := context.WithValue(r.Context(), requestKey, r)
	got := h.guestSvc(ctx)
	if want := (vmtool.Tunnel{Exe: "/opt/vmtool/vmtool", Token: "req-token-1"}); got.Tunnel != want || got.PlaybookDir != "pb" {
		t.Fatalf("guestSvc = %+v", got)
	}
	if h.svc.Tunnel != (vmtool.Tunnel{}) {
		t.Fatal("shared service was mutated")
	}
	// No request in ctx: no token, never a panic.
	if got := h.guestSvc(context.Background()); got.Tunnel.Token != "" {
		t.Fatalf("token without a request: %+v", got.Tunnel)
	}
}

// Without tunnelExe the server would use its own executable, which under
// go test is server.test: CurrentExe refuses it, so guestSvc gets no Exe and
// every guest-SSH path fails before any process starts.
func TestGuestSvcNeverUsesTestBinaryAsTunnel(t *testing.T) {
	t.Chdir(t.TempDir())
	h := &handlers{svc: &app.Service{PlaybookDir: "pb"}}
	got := h.guestSvc(context.Background())
	if got.Tunnel.Exe != "" {
		t.Fatalf("guestSvc tunnel Exe = %q under go test, want empty", got.Tunnel.Exe)
	}
	if _, err := got.RemoteCmd(app.VMRef{Host: "box", Name: "web"}, []string{"true"}); err == nil {
		t.Fatal("RemoteCmd without a tunnel executable succeeded")
	}
}
