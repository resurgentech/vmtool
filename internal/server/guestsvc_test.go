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
	r := httptest.NewRequest("POST", "/v1/hosts/jetfire/vms/web/cmd", nil)
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
