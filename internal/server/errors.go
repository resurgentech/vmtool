package server

import (
	"encoding/json"
	"net/http"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
)

// The error body's machine-readable code. Every error body the server sends
// takes its code from here: codeForKind for an error from internal/app,
// codeForStatus for a refusal that has only a status. Clients branch on the
// code, never on the text.

// codeForKind is the code of an app error kind. A kind with no more specific
// word is internal.
func codeForKind(k app.Kind) api.ErrorCode {
	switch k {
	case app.KindInvalid:
		return api.ErrorCodeBadRequest
	case app.KindNotFound:
		return api.ErrorCodeNotFound
	case app.KindConflict:
		return api.ErrorCodeConflict
	case app.KindBadGateway:
		return api.ErrorCodeBadGateway
	case app.KindUnavailable:
		return api.ErrorCodeStoreUnavailable
	case app.KindHostUnreachable:
		return api.ErrorCodeHostUnreachable
	case app.KindHostTimeout:
		return api.ErrorCodeHostTimeout
	}
	return api.ErrorCodeInternal
}

// codeForStatus is the code of a refusal the server words itself. 403 is the
// refusal of a daemon that serves hypervisor operations only, asked for one it
// does not serve (the workstation's own 403 sets workstation_only where it is
// written); to the caller that request is wrong for this server.
func codeForStatus(status int) api.ErrorCode {
	switch status {
	case http.StatusNotFound:
		return api.ErrorCodeNotFound
	case http.StatusConflict:
		return api.ErrorCodeConflict
	case http.StatusUnauthorized:
		return api.ErrorCodeUnauthorized
	case http.StatusBadGateway:
		return api.ErrorCodeBadGateway
	case http.StatusServiceUnavailable:
		return api.ErrorCodeStoreUnavailable
	case http.StatusGatewayTimeout:
		return api.ErrorCodeHostTimeout
	}
	if status >= 400 && status < 500 {
		return api.ErrorCodeBadRequest
	}
	return api.ErrorCodeInternal
}

// errBody is an error body for a refusal the server words itself.
func errBody(status int, msg string) api.Error {
	return withCode(api.Error{Error: msg}, codeForStatus(status))
}

func withCode(e api.Error, c api.ErrorCode) api.Error {
	e.Code = &c
	return e
}

// hostFailure reports whether err is a configured host that did not answer,
// and the status it is answered with: 502 for a refused or broken connection,
// 504 for no answer in time. Anything else is 0.
func hostFailure(err error) int {
	switch kindOf(err) {
	case app.KindHostUnreachable:
		return http.StatusBadGateway
	case app.KindHostTimeout:
		return http.StatusGatewayTimeout
	}
	return 0
}

// unhandled is the answer to an error a handler has no status of its own
// for: a host that did not answer is 502 or 504, anything else is the 500 the
// operation documents. Handlers return `nil, err` for it and the strict
// server's response error handler (see handlerOptions) writes it, so one
// function maps the error of every operation.
func unhandled(err error) (int, api.Error) {
	e := apiErr(err)
	if st := hostFailure(err); st != 0 {
		return st, e
	}
	return http.StatusInternalServerError, withCode(e, api.ErrorCodeInternal)
}

// writeUnhandled writes unhandled(err) as JSON.
func writeUnhandled(w http.ResponseWriter, _ *http.Request, err error) {
	st, e := unhandled(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_ = json.NewEncoder(w).Encode(e)
}

// handlerOptions is the strict server's: its response error handler is
// writeUnhandled; a request it cannot decode keeps the generated plain 400.
var handlerOptions = api.StrictHTTPServerOptions{
	RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusBadRequest)
	},
	ResponseErrorHandlerFunc: writeUnhandled,
}

// statusCode is the code of a response the handler words with its status.
func statusCode(status int) *api.ErrorCode {
	c := codeForStatus(status)
	return &c
}
