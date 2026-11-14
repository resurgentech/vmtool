package server

// Daemon primitives: the libvirt-only operations a remote hypervisor serves
// so a workstation's remote.Manager can implement every app.Manager method
// over HTTP. Each handler calls the Manager method directly (no app.Service
// workflow): these are primitives, and the workflow layer stays on the
// workstation. The handler-to-method map is in spec/README.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
	"github.com/jaredeh/vmtool/spec"
)

// errOutsidePool marks a caller-supplied path that does not resolve inside a
// libvirt storage pool. It is the caller's mistake: 400.
var errOutsidePool = errors.New("path is not inside a libvirt storage pool")

// plainName is what a volume or VM name from a request may look like. These
// names end up in libvirt XML and in file names, so no separators or markup.
var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	defaultWaitIP = 120
	maxWaitIP     = 3600
)

// inPool reports nil if p, once cleaned and with symlinks resolved, lies
// strictly inside the path of one of m's storage pools. p must be absolute.
// The parts of p that do not exist yet are kept as written (they are plain
// names after Clean), so a volume that is about to be created can be checked.
// A pool with no path is skipped. Failing to list pools is not an
// errOutsidePool. The check is a point-in-time one: a symlink created between
// the check and the libvirt call is not caught.
func inPool(m app.Manager, p string) error {
	if p == "" || !filepath.IsAbs(p) {
		return fmt.Errorf("%w: %q must be an absolute path", errOutsidePool, p)
	}
	p = filepath.Clean(p)
	pools, err := m.ListPools()
	if err != nil {
		return fmt.Errorf("list storage pools: %w", err)
	}
	rp, err := resolveExisting(p)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", errOutsidePool, p, err)
	}
	for _, pool := range pools {
		if pool.Path == "" {
			continue
		}
		pp, err := resolveExisting(filepath.Clean(pool.Path))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(pp, rp)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return nil
	}
	return fmt.Errorf("%w: %q", errOutsidePool, p)
}

// poolPath validates p with inPool and returns the cleaned form, which is
// what must be handed to libvirt so the checked and the used path agree.
func poolPath(m app.Manager, p string) (string, error) {
	if err := inPool(m, p); err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}

// resolveExisting evaluates symlinks in the longest existing prefix of the
// clean absolute path p and appends the rest. A symlink that exists but does
// not resolve (dangling or looping) is an error.
func resolveExisting(p string) (string, error) {
	rest := ""
	cur := p
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("cannot resolve %q: %v", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// primKind classifies a primitive's error: a bad path is the caller's (400),
// everything else as kindOf does.
func primKind(err error) app.Kind {
	if errors.Is(err, errOutsidePool) {
		return app.KindInvalid
	}
	return kindOf(err)
}

func badName(what, s string) error {
	return fmt.Errorf("%s %q must be a plain name (letters, digits, '.', '_', '-')", what, s)
}

func xmlSafe(s string) bool { return !strings.ContainsAny(s, "<>&'\"\x00\n\r") }

func (h *handlers) GetImagePath(_ context.Context, req api.GetImagePathRequestObject) (api.GetImagePathResponseObject, error) {
	v, err := h.withManager(req.Host, func(m app.Manager) (any, error) { return m.ImagePath(req.Name) })
	if err != nil {
		if primKind(err) == app.KindNotFound {
			return api.GetImagePath404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.GetImagePath500JSONResponse(apiErr(err)), nil
	}
	return api.GetImagePath200JSONResponse{Path: v.(string)}, nil
}

func (h *handlers) CreateVolume(_ context.Context, req api.CreateVolumeRequestObject) (api.CreateVolumeResponseObject, error) {
	b := req.Body
	var bad error
	switch {
	case b == nil:
		bad = errors.New("request body required")
	case b.SizeGb < 1:
		bad = errors.New("size_gb must be > 0")
	case !plainName.MatchString(b.Name):
		bad = badName("name", b.Name)
	case !xmlSafe(derefStr(b.Pool)):
		bad = errors.New("pool contains invalid characters")
	}
	if bad != nil {
		return api.CreateVolume400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: bad.Error()}}, nil
	}
	v, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		return m.CreateVolume(derefStr(b.Pool), b.Name, uint(b.SizeGb))
	})
	if err != nil {
		switch primKind(err) {
		case app.KindNotFound:
			return api.CreateVolume404JSONResponse(apiErr(err)), nil
		default:
			return api.CreateVolume500JSONResponse(apiErr(err)), nil
		}
	}
	return api.CreateVolume201JSONResponse{Path: v.(string)}, nil
}

func (h *handlers) CloneVolume(_ context.Context, req api.CloneVolumeRequestObject) (api.CloneVolumeResponseObject, error) {
	b := req.Body
	var bad error
	switch {
	case b == nil:
		bad = errors.New("request body required")
	case b.BasePath == "":
		bad = errors.New("base_path is required")
	case !plainName.MatchString(b.NewName):
		bad = badName("new_name", b.NewName)
	case !xmlSafe(derefStr(b.TargetPool)):
		bad = errors.New("target_pool contains invalid characters")
	}
	if bad != nil {
		return api.CloneVolume400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: bad.Error()}}, nil
	}
	v, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		base, err := poolPath(m, b.BasePath)
		if err != nil {
			return nil, err
		}
		return m.CloneImage(base, b.NewName, derefStr(b.TargetPool))
	})
	if err != nil {
		switch primKind(err) {
		case app.KindInvalid:
			return api.CloneVolume400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.CloneVolume404JSONResponse(apiErr(err)), nil
		default:
			return api.CloneVolume500JSONResponse(apiErr(err)), nil
		}
	}
	return api.CloneVolume201JSONResponse{Path: v.(string)}, nil
}

func (h *handlers) ResizeVolume(_ context.Context, req api.ResizeVolumeRequestObject) (api.ResizeVolumeResponseObject, error) {
	b := req.Body
	var bad error
	switch {
	case b == nil:
		bad = errors.New("request body required")
	case b.Path == "":
		bad = errors.New("path is required")
	case b.SizeBytes < 1:
		bad = errors.New("size_bytes must be > 0")
	}
	if bad != nil {
		return api.ResizeVolume400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: bad.Error()}}, nil
	}
	_, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		p, err := poolPath(m, b.Path)
		if err != nil {
			return nil, err
		}
		return nil, m.ResizeVolume(p, uint64(b.SizeBytes))
	})
	if err != nil {
		switch primKind(err) {
		case app.KindInvalid:
			return api.ResizeVolume400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.ResizeVolume404JSONResponse(apiErr(err)), nil
		default:
			return api.ResizeVolume500JSONResponse(apiErr(err)), nil
		}
	}
	return api.ResizeVolume204Response{}, nil
}

// vmConfigFromDefine builds the Manager's config from the request. It
// validates what does not need libvirt: names, enums and markup-free strings.
func vmConfigFromDefine(name string, b *api.DefineVMRequest) (vmtool.VMConfig, error) {
	var zero vmtool.VMConfig
	if b == nil {
		return zero, errors.New("request body required")
	}
	if !plainName.MatchString(name) {
		return zero, badName("VM name", name)
	}
	if b.Vcpus < 1 || b.MemoryGib < 1 {
		return zero, errors.New("vcpus and memory_gib must be > 0")
	}
	if b.DiskPath == "" {
		return zero, errors.New("disk_path is required")
	}
	if derefInt0(b.DiskSizeGb) < 0 || derefInt0(b.ExtraDiskSizeGb) < 0 {
		return zero, errors.New("disk sizes must not be negative")
	}
	cfg := vmtool.VMConfig{
		Name:            name,
		VCPUs:           uint(b.Vcpus),
		Memory:          uint(b.MemoryGib),
		DiskPath:        b.DiskPath,
		DiskSizeGB:      uint(derefInt0(b.DiskSizeGb)),
		Pool:            derefStr(b.Pool),
		Noclone:         derefBool(b.Noclone),
		ExtraDiskSizeGB: uint(derefInt0(b.ExtraDiskSizeGb)),
		ExtraDiskPool:   derefStr(b.ExtraDiskPool),
	}
	n := b.Network
	switch n.Type {
	case api.NetworkConfigTypeNat, api.NetworkConfigTypeBridge, api.NetworkConfigTypeDirect:
	default:
		return zero, fmt.Errorf("network.type %q must be nat, bridge or direct", n.Type)
	}
	cfg.Network = vmtool.NetworkConfig{Type: vmtool.NetworkType(n.Type), Source: derefStr(n.Source)}
	if n.MacvtapMode != nil {
		switch *n.MacvtapMode {
		case api.NetworkConfigMacvtapModeBridge, api.NetworkConfigMacvtapModeVepa,
			api.NetworkConfigMacvtapModePrivate, api.NetworkConfigMacvtapModePassthrough:
		default:
			return zero, fmt.Errorf("network.macvtap_mode %q is not valid", *n.MacvtapMode)
		}
		cfg.Network.MacvtapMode = vmtool.MacvtapMode(*n.MacvtapMode)
	}
	for _, s := range []string{cfg.Network.Source, cfg.Pool, cfg.ExtraDiskPool} {
		if !xmlSafe(s) {
			return zero, errors.New("network.source and pool names must not contain markup characters")
		}
	}
	if b.ExtraDisks != nil {
		for _, d := range *b.ExtraDisks {
			if d.Path == "" || d.Target == "" || !plainName.MatchString(d.Target) {
				return zero, errors.New("extra_disks entries need a path and a plain target name")
			}
			cfg.ExtraDisks = append(cfg.ExtraDisks, vmtool.DiskDevice{Path: d.Path, Target: d.Target})
		}
	}
	return cfg, nil
}

func derefInt0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (h *handlers) DefineVM(_ context.Context, req api.DefineVMRequestObject) (api.DefineVMResponseObject, error) {
	cfg, bad := vmConfigFromDefine(req.Name, req.Body)
	if bad != nil {
		return api.DefineVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: bad.Error()}}, nil
	}
	_, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		var err error
		if cfg.DiskPath, err = poolPath(m, cfg.DiskPath); err != nil {
			return nil, fmt.Errorf("disk_path: %w", err)
		}
		for i, d := range cfg.ExtraDisks {
			if cfg.ExtraDisks[i].Path, err = poolPath(m, d.Path); err != nil {
				return nil, fmt.Errorf("extra_disks[%d].path: %w", i, err)
			}
		}
		return nil, m.Define(cfg)
	})
	if err != nil {
		switch primKind(err) {
		case app.KindInvalid:
			return api.DefineVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.DefineVM404JSONResponse(apiErr(err)), nil
		default:
			return api.DefineVM500JSONResponse(apiErr(err)), nil
		}
	}
	return api.DefineVM204Response{}, nil
}

func (h *handlers) DeleteVMDefinition(_ context.Context, req api.DeleteVMDefinitionRequestObject) (api.DeleteVMDefinitionResponseObject, error) {
	noclone := derefBool(req.Params.Noclone)
	_, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		return nil, m.Delete(req.Name, noclone)
	})
	if err != nil {
		if primKind(err) == app.KindNotFound {
			return api.DeleteVMDefinition404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.DeleteVMDefinition500JSONResponse(apiErr(err)), nil
	}
	return api.DeleteVMDefinition204Response{}, nil
}

func (h *handlers) WaitVMIP(_ context.Context, req api.WaitVMIPRequestObject) (api.WaitVMIPResponseObject, error) {
	secs := defaultWaitIP
	if req.Body != nil && req.Body.TimeoutSeconds != nil {
		secs = *req.Body.TimeoutSeconds
	}
	if secs < 1 || secs > maxWaitIP {
		return api.WaitVMIP400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{
			Error: fmt.Sprintf("timeout_seconds must be between 1 and %d", maxWaitIP)}}, nil
	}
	v, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		return m.WaitForIP(req.Name, time.Duration(secs)*time.Second)
	})
	if err != nil {
		if primKind(err) == app.KindNotFound {
			return api.WaitVMIP404JSONResponse(apiErr(err)), nil
		}
		return api.WaitVMIP500JSONResponse(apiErr(err)), nil
	}
	return api.WaitVMIP200JSONResponse{Ip: v.(string)}, nil
}

func (h *handlers) ResizeVMVolume(_ context.Context, req api.ResizeVMVolumeRequestObject) (api.ResizeVMVolumeResponseObject, error) {
	if req.Body == nil || req.Body.SizeGb < 1 {
		return api.ResizeVMVolume400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "size_gb must be > 0"}}, nil
	}
	_, err := h.withManager(req.Host, func(m app.Manager) (any, error) {
		return nil, m.ResizeDisk(req.Name, uint(req.Body.SizeGb))
	})
	if err != nil {
		if primKind(err) == app.KindNotFound {
			return api.ResizeVMVolume404JSONResponse(apiErr(err)), nil
		}
		return api.ResizeVMVolume500JSONResponse(apiErr(err)), nil
	}
	return api.ResizeVMVolume204Response{}, nil
}

// workstationOnlyExt marks, in the spec, the operations a hypervisor refuses.
const workstationOnlyExt = "x-vmtool-workstation-only"

// gateWorkstationOnly returns next unchanged for a workstation. For a hypervisor
// it wraps next so that every operation the spec marks
// x-vmtool-workstation-only answers 403 before the body is read and before
// any libvirt work. The set is read from the embedded spec, so it cannot
// drift from the contract. If the spec cannot be loaded the gate refuses
// every request rather than serve workflows it cannot identify.
func gateWorkstationOnly(role, host string, next http.Handler) http.Handler {
	if role != config.RoleHypervisor {
		return next
	}
	routes := http.NewServeMux()
	ids := map[string]string{}
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		slog.Error("load embedded spec; hypervisor refuses every request", "err", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeWorkstationOnly(w, "this operation", host)
		})
	}
	noop := func(http.ResponseWriter, *http.Request) {}
	for p, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !isWorkstationOnly(op) {
				continue
			}
			pattern := strings.ToUpper(method) + " " + p
			routes.HandleFunc(pattern, noop)
			ids[pattern] = op.OperationID
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An unclean path is redirected by the mux behind us; the clean
		// request that follows is gated.
		if p := r.URL.Path; path.Clean(p) == p {
			if _, pattern := routes.Handler(r); pattern != "" {
				writeWorkstationOnly(w, ids[pattern], host)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isWorkstationOnly(op *openapi3.Operation) bool {
	v, ok := op.Extensions[workstationOnlyExt]
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case json.RawMessage:
		return string(x) == "true"
	}
	return fmt.Sprint(v) == "true"
}

func writeWorkstationOnly(w http.ResponseWriter, opID, host string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(api.Error{Error: fmt.Sprintf("%s is served by the workstation, not by hypervisor %s", opID, host)})
}
