package server

import (
	"context"

	"github.com/resurgentech/vmtool/internal/api"
)

// GetKeyRequestDiscovery answers the public GET /v1/key-request: where a tool
// files a request for an API key. The address is config.yml's
// personal_keys.request_url, validated at load; the daemon only reports it and
// never dials it. Unset: 404.
func (h *handlers) GetKeyRequestDiscovery(_ context.Context, _ api.GetKeyRequestDiscoveryRequestObject) (api.GetKeyRequestDiscoveryResponseObject, error) {
	if h.requestURL == "" {
		return api.GetKeyRequestDiscovery404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(errBody(404, "no key request URL is configured on this daemon"))}, nil
	}
	return api.GetKeyRequestDiscovery200JSONResponse{RequestUrl: h.requestURL}, nil
}
