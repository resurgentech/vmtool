package server

import (
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/spec"
)

type recMux struct {
	routes map[string]bool
}

func (m *recMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if m.routes == nil {
		m.routes = map[string]bool{}
	}
	m.routes[normalizePattern(pattern)] = true
}

func (m *recMux) ServeHTTP(http.ResponseWriter, *http.Request) {}

func normalizePattern(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "/{$}", "/")
	return p
}

func TestSpecParity(t *testing.T) {
	var _ api.ServeMux = (*recMux)(nil)
	rec := &recMux{}
	api.HandlerFromMux(api.NewStrictHandler(&handlers{}, nil), rec)

	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	specOps := map[string]bool{}
	for rawPath, item := range doc.Paths.Map() {
		full := path.Clean("/" + rawPath)
		if full != "/" {
			full = strings.TrimSuffix(full, "/")
		}
		for method := range item.Operations() {
			specOps[strings.ToUpper(method)+" "+full] = true
		}
	}

	var missing, extra []string
	for k := range rec.routes {
		if !specOps[k] {
			extra = append(extra, k)
		}
	}
	for k := range specOps {
		if !rec.routes[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("parity mismatch\nmissing from mux: %v\nextra on mux: %v\nmux=%v\nspec=%v", missing, extra, keys(rec.routes), keys(specOps))
	}
}

func TestNoConsoleInSpec(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	for p, item := range doc.Paths.Map() {
		if strings.Contains(strings.ToLower(p), "console") {
			t.Errorf("spec path %s mentions console", p)
		}
		for m, op := range item.Operations() {
			if strings.Contains(strings.ToLower(op.OperationID), "console") {
				t.Errorf("%s %s operationId %s mentions console", m, p, op.OperationID)
			}
		}
	}
	if strings.Contains(strings.ToLower(doc.Info.Description), "console") {
		t.Errorf("info.description mentions console")
	}
}

func TestConsoleRoutesGone(t *testing.T) {
	h := handlerFor(&handlers{})
	for _, p := range []string{"/v1/hosts/x/vms/x/console", "/v1/metal/x/console"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
