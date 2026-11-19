package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
)

// warningsHeader carries the request's warnings: today only the store
// fallback line. One header value per warning, each one line of text. It is
// documented in spec/openapi/vmtool.yaml (info.description).
const warningsHeader = "X-Vmtool-Warnings"

// storeOperations are the operations that use guest credentials, machine
// records or base images. Each decides the store mode at its start, once, for
// that request; no other operation pays for the probe. Keyed by operationId.
var storeOperations = map[string]bool{
	"createVM": true, "deleteVM": true, "execVM": true, "runPlaybook": true, "resizeDisk": true,
	"listMachines": true, "addMachine": true, "getMachine": true, "removeMachine": true,
	"rebootMachine": true, "execMachine": true, "runMachinePlaybook": true,
}

type storeKeyType struct{}

var storeKey storeKeyType

// storeSessionOf is the request's store session, or nil (no server: block,
// or an operation that does not use the store).
func storeSessionOf(ctx context.Context) *app.StoreSession {
	s, _ := ctx.Value(storeKey).(*app.StoreSession)
	return s
}

// decideStore is the strict handler's middleware for store operations: it
// decides this request's mode before the handler runs (so a streaming handler
// has not yet written its headers), puts the fallback line in the response's
// warnings, and hands the session to the handler through the context.
func (h *handlers) decideStore(f api.StrictHandlerFunc, operationID string) api.StrictHandlerFunc {
	if h.storeURL == "" || !storeOperations[strings.ToLower(operationID[:1])+operationID[1:]] {
		return f
	}
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		// A session per request: the mode is decided per request and not
		// shared between concurrent ones.
		sess := app.OpenStoreSession(h.storeURL, h.storeToken, h.storeOpts, nil)
		sess.Mode(ctx)
		if n := sess.Notice(); n != "" {
			w.Header().Add(warningsHeader, strings.Join(strings.Fields(n), " "))
		}
		return f(context.WithValue(ctx, storeKey, sess), w, r, request)
	}
}
