package server

import (
	"net/http"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/spec"
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

// The console is its own operation on its own path: GET only, workstation
// only. The exec paths stay "run a command, return the result": POST only.
func TestConsoleOperationsInSpec(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	want := map[string]string{
		"/v1/hosts/{host}/vms/{name}/console": "openVMConsole",
		"/v1/metal/{name}/console":            "openMachineConsole",
	}
	for p, id := range want {
		item := doc.Paths.Find(p)
		if item == nil {
			t.Errorf("spec has no path %s", p)
			continue
		}
		ops := item.Operations()
		if len(ops) != 1 || ops["GET"] == nil || ops["GET"].OperationID != id {
			t.Errorf("%s: want exactly GET %s, got %v", p, id, ops)
		}
	}
	for _, p := range []string{"/v1/hosts/{host}/vms/{name}/exec", "/v1/metal/{name}/exec"} {
		item := doc.Paths.Find(p)
		if item == nil || len(item.Operations()) != 1 || item.Operations()["POST"] == nil {
			t.Errorf("%s must offer POST only (no GET or Upgrade)", p)
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
