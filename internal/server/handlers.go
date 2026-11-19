package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/buildinfo"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

type handlers struct {
	svc    *app.Service
	listen string
	// hosts maps {host} to a Manager; chain fills it when nil.
	hosts *hosts.Resolver
	// role is the server's config.yml role. config.RoleHypervisor makes chain
	// refuse the workstation-only operations; empty means workstation.
	role string
	// vmIP and dialGuest are the tunnel's VM lookup and guest dial (tunnel.go).
	// nil means the resolver's Manager and a 5 s TCP dial; tests inject fakes.
	vmIP      func(ctx context.Context, host, name string) (string, error)
	dialGuest func(ctx context.Context, addr string) (net.Conn, error)
	// tunnelExe is the vmtool path guest SSH uses as its ProxyCommand; empty
	// means the running executable. Under go test that is refused
	// (vmtool.CurrentExe), so tests that need a tunnel set a stub path here.
	tunnelExe string
	// imageManifest is the daemon's image-manifest.yml; empty means
	// vmtool.ImageManifestFile in the working directory (images.go).
	imageManifest string
	// beforeCommit, when set, runs in uploadImage after the body is verified
	// and before the new file is put in place (tests inject races here).
	beforeCommit func(name string)
	// slowAfter is the duration past which a request is logged as slow; zero
	// means slowRequest (server.go). Tests shorten it.
	slowAfter time.Duration
	// poolDir is the vmtool pool's directory, resolved once and kept
	// (poolDirOf in images.go); poolMu guards it.
	poolMu  sync.Mutex
	poolDir string
	// console is the open consoles and their limits (console.go).
	console consoles
}

// guestSvc is h.svc with a Tunnel for this request: the running vmtool as
// ProxyCommand and the request's own bearer token (requireBearer already
// checked it) as VMTOOL_TOKEN for the ssh and ansible children. The shared
// h.svc is never given a token. ssh runs the ProxyCommand in the service's
// working directory, where config.yml lives.
func (h *handlers) guestSvc(ctx context.Context) *app.Service {
	t := vmtool.Tunnel{Exe: h.tunnelExe}
	if t.Exe == "" {
		t.Exe, _ = vmtool.CurrentExe()
	}
	t.Token = requestToken(ctx)
	return h.svc.WithTunnel(t)
}

// requestToken is the bearer of the request ctx carries (requireBearer has
// already checked it), or "" when there is none.
func requestToken(ctx context.Context) string {
	if r, ok := ctx.Value(requestKey).(*http.Request); ok && r != nil {
		tok, _ := bearerToken(r)
		return tok
	}
	return ""
}

// withManager resolves host first, so an unknown host never touches libvirt,
// then runs fn on its Manager and releases it. An unknown host comes back as
// an error that kindOf reports as KindNotFound. A configured remote host is
// reached with the bearer of the request being served (ctx): the server
// holds no token of its own and forwards the caller's.
func (h *handlers) withManager(ctx context.Context, host string, fn func(m app.Manager) (any, error)) (any, error) {
	m, done, err := h.hosts.WithToken(requestToken(ctx)).Resolve(host)
	if err != nil {
		return nil, err
	}
	defer done()
	return fn(m)
}

func toAPI(info *vmtool.VMInfo) api.VMInfo {
	if info == nil {
		return api.VMInfo{}
	}
	out := api.VMInfo{
		Name:      info.Name,
		State:     api.VMInfoState(info.State),
		Vcpus:     int(info.VCPUs),
		Memory:    int(info.Memory),
		Ip:        info.IP,
		Autostart: info.Autostart,
	}
	if info.DiskCache != "" {
		dc := api.VMInfoDiskCache(info.DiskCache)
		out.DiskCache = &dc
	}
	if info.DiskBacking != "" {
		db := api.VMInfoDiskBacking(info.DiskBacking)
		out.DiskBacking = &db
	}
	if info.UUID != "" {
		u := info.UUID
		out.Uuid = &u
	}
	if info.Firmware != "" {
		fw := api.VMInfoFirmware(info.Firmware)
		out.Firmware = &fw
	}
	return out
}

func apiErr(err error) api.Error {
	out := api.Error{Error: err.Error()}
	var ae *app.Error
	if app.AsError(err, &ae) {
		if ae.Output != "" {
			out.Output = &ae.Output
		}
	}
	return out
}

func apiErrVM(err error, info *vmtool.VMInfo) api.Error {
	e := apiErr(err)
	if info != nil {
		v := toAPI(info)
		e.Vm = &v
	}
	return e
}

func kindOf(err error) app.Kind {
	var ae *app.Error
	if app.AsError(err, &ae) {
		return ae.Kind
	}
	if errors.Is(err, hosts.ErrUnknownHost) || vmtool.IsNotFound(err) {
		return app.KindNotFound
	}
	return app.KindInternal
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefBool(p *bool) bool {
	return p != nil && *p
}

func derefInt(p *int) uint {
	if p == nil || *p < 0 {
		return 0
	}
	return uint(*p)
}

func (h *handlers) ListVMs(ctx context.Context, req api.ListVMsRequestObject) (api.ListVMsResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ListVMs(ctx, m)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ListVMs404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ListVMs500JSONResponse(apiErr(err)), nil
	}
	vms := v.([]vmtool.VMInfo)
	out := make([]api.VMInfo, len(vms))
	for i := range vms {
		out[i] = toAPI(&vms[i])
	}
	return api.ListVMs200JSONResponse(out), nil
}

func (h *handlers) GetVM(ctx context.Context, req api.GetVMRequestObject) (api.GetVMResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.GetVM(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.GetVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.GetVM500JSONResponse(apiErr(err)), nil
	}
	return api.GetVM200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) CreateVM(ctx context.Context, req api.CreateVMRequestObject) (api.CreateVMResponseObject, error) {
	if req.Body == nil {
		return api.CreateVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "request body required"}}, nil
	}
	b := req.Body
	in := app.CreateVMInput{
		Host:  req.Host,
		Name:  b.Name,
		Image: b.Image,
	}
	in.VCPUs = derefInt(b.Vcpus)
	in.Memory = derefInt(b.Memory)
	in.DiskSizeGB = derefInt(b.DiskSize)
	in.Pool = derefStr(b.Pool)
	if b.NetType != nil {
		in.NetType = string(*b.NetType)
	}
	in.NetSource = derefStr(b.NetSource)
	if b.MacvtapMode != nil {
		in.MacvtapMode = string(*b.MacvtapMode)
	}
	in.SSHUser = derefStr(b.SshUser)
	in.SSHPass = derefStr(b.SshPass)
	in.Playbook = derefStr(b.Playbook)
	in.Noclone = derefBool(b.Noclone)
	in.NoWait = derefBool(b.NoWait)
	if b.Firmware != nil {
		in.Firmware = string(*b.Firmware)
	}
	if b.DiskCache != nil {
		in.DiskCache = string(*b.DiskCache)
	}
	if b.ExtraDisk != nil {
		if b.ExtraDisk.Size < 1 {
			return api.CreateVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "extra_disk.size must be > 0"}}, nil
		}
		in.ExtraDiskSizeGB = uint(b.ExtraDisk.Size)
		in.ExtraDiskPool = derefStr(b.ExtraDisk.Pool)
	}
	if b.ExtraVars != nil {
		in.ExtraVars = map[string]string(*b.ExtraVars)
	}

	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		info, e := h.guestSvc(ctx).CreateVM(ctx, m, in)
		if e != nil {
			return info, e
		}
		return info, nil
	})
	info, _ := v.(*vmtool.VMInfo)
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.CreateVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.CreateVM404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.CreateVM409JSONResponse(apiErr(err)), nil
		default:
			return api.CreateVM500JSONResponse(apiErrVM(err, info)), nil
		}
	}
	return api.CreateVM201JSONResponse(toAPI(info)), nil
}

func (h *handlers) DeleteVM(ctx context.Context, req api.DeleteVMRequestObject) (api.DeleteVMResponseObject, error) {
	noclone := derefBool(req.Params.Noclone)
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.DeleteVM(ctx, m, app.VMRef{Host: req.Host, Name: req.Name}, noclone)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.DeleteVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.DeleteVM500JSONResponse(apiErr(err)), nil
	}
	return api.DeleteVM204Response{}, nil
}

func (h *handlers) StartVM(ctx context.Context, req api.StartVMRequestObject) (api.StartVMResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.StartVM(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.StartVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.StartVM500JSONResponse(apiErr(err)), nil
	}
	return api.StartVM200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) StopVM(ctx context.Context, req api.StopVMRequestObject) (api.StopVMResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.StopVM(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.StopVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.StopVM500JSONResponse(apiErr(err)), nil
	}
	return api.StopVM200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) ResumeVM(ctx context.Context, req api.ResumeVMRequestObject) (api.ResumeVMResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ResumeVM(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ResumeVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ResumeVM500JSONResponse(apiErr(err)), nil
	}
	return api.ResumeVM200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) PoweroffVM(ctx context.Context, req api.PoweroffVMRequestObject) (api.PoweroffVMResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.PoweroffVM(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.PoweroffVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.PoweroffVM500JSONResponse(apiErr(err)), nil
	}
	return api.PoweroffVM200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) RebootVM(ctx context.Context, req api.RebootVMRequestObject) (api.RebootVMResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.RebootVM(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.RebootVM404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.RebootVM500JSONResponse(apiErr(err)), nil
	}
	return api.RebootVM200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) SetAutostart(ctx context.Context, req api.SetAutostartRequestObject) (api.SetAutostartResponseObject, error) {
	if req.Body == nil {
		return api.SetAutostart400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "request body required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.SetAutostart(ctx, m, req.Name, req.Body.Enabled)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.SetAutostart404JSONResponse(apiErr(err)), nil
		}
		return api.SetAutostart500JSONResponse(apiErr(err)), nil
	}
	return api.SetAutostart200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

// stdinOf is an exec request's stdin text, "" when absent.
func stdinOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *handlers) ExecVM(ctx context.Context, req api.ExecVMRequestObject) (api.ExecVMResponseObject, error) {
	if req.Body == nil || len(req.Body.Command) == 0 {
		return api.ExecVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "command is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.guestSvc(ctx).ExecVM(ctx, m, app.VMRef{Host: req.Host, Name: req.Name}, req.Body.Command, stdinOf(req.Body.Stdin))
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.ExecVM400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.ExecVM404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.ExecVM409JSONResponse(apiErr(err)), nil
		case app.KindBadGateway:
			return api.ExecVM502JSONResponse(apiErr(err)), nil
		default:
			return api.ExecVM500JSONResponse(apiErr(err)), nil
		}
	}
	res := v.(*app.ExecResult)
	return api.ExecVM200JSONResponse{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

func (h *handlers) RunPlaybook(ctx context.Context, req api.RunPlaybookRequestObject) (api.RunPlaybookResponseObject, error) {
	if req.Body == nil || req.Body.Playbook == "" {
		return api.RunPlaybook400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "playbook is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		var extra map[string]string
		if req.Body.ExtraVars != nil {
			extra = map[string]string(*req.Body.ExtraVars)
		}
		return h.guestSvc(ctx).RunPlaybook(ctx, m, app.VMRef{Host: req.Host, Name: req.Name}, req.Body.Playbook, vmtool.DefaultAuth(), extra)
	})
	res, _ := v.(*app.PlaybookResult)
	out := ""
	if res != nil {
		out = res.Output
	}
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.RunPlaybook400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.RunPlaybook404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.RunPlaybook409JSONResponse(apiErr(err)), nil
		default:
			msg := err.Error()
			return api.RunPlaybook500JSONResponse{Output: out, Error: &msg}, nil
		}
	}
	return api.RunPlaybook200JSONResponse{Output: out}, nil
}

func (h *handlers) MigrateDisk(ctx context.Context, req api.MigrateDiskRequestObject) (api.MigrateDiskResponseObject, error) {
	if req.Body == nil || req.Body.Pool == "" {
		return api.MigrateDisk400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "pool is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.MigrateDisk(ctx, m, req.Name, req.Body.Pool)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.MigrateDisk404JSONResponse(apiErr(err)), nil
		}
		if kindOf(err) == app.KindInvalid {
			return api.MigrateDisk400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.MigrateDisk500JSONResponse(apiErr(err)), nil
	}
	return api.MigrateDisk200JSONResponse(toAPI(v.(*vmtool.VMInfo))), nil
}

func (h *handlers) ResizeDisk(ctx context.Context, req api.ResizeDiskRequestObject) (api.ResizeDiskResponseObject, error) {
	if req.Body == nil || req.Body.Size < 1 {
		return api.ResizeDisk400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "size must be > 0"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.guestSvc(ctx).ResizeDisk(ctx, m, app.VMRef{Host: req.Host, Name: req.Name}, uint(req.Body.Size), vmtool.DefaultAuth())
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ResizeDisk404JSONResponse(apiErr(err)), nil
		}
		if kindOf(err) == app.KindInvalid {
			return api.ResizeDisk400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ResizeDisk500JSONResponse(apiErr(err)), nil
	}
	res := v.(*app.ResizeResult)
	out := api.ResizeDiskResult{Vm: toAPI(&res.VM), FilesystemGrown: res.FilesystemGrown}
	if res.GrowError != "" {
		out.GrowError = &res.GrowError
	}
	return api.ResizeDisk200JSONResponse(out), nil
}

func (h *handlers) AddDisk(ctx context.Context, req api.AddDiskRequestObject) (api.AddDiskResponseObject, error) {
	if req.Body == nil || req.Body.Size < 1 {
		return api.AddDisk400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "size must be > 0"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.AddDisk(ctx, m, req.Name, uint(req.Body.Size), derefStr(req.Body.Pool))
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.AddDisk404JSONResponse(apiErr(err)), nil
		}
		if kindOf(err) == app.KindInvalid {
			return api.AddDisk400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.AddDisk500JSONResponse(apiErr(err)), nil
	}
	res := v.(*app.AddDiskResult)
	return api.AddDisk200JSONResponse{Vm: toAPI(&res.VM), Path: res.Path, Target: res.Target}, nil
}

func (h *handlers) ListNetworks(ctx context.Context, req api.ListNetworksRequestObject) (api.ListNetworksResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ListNetworks(ctx, m)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ListNetworks404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ListNetworks500JSONResponse(apiErr(err)), nil
	}
	return api.ListNetworks200JSONResponse(v.([]string)), nil
}

func (h *handlers) ListBridges(ctx context.Context, req api.ListBridgesRequestObject) (api.ListBridgesResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ListBridges(ctx, m)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ListBridges404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ListBridges500JSONResponse(apiErr(err)), nil
	}
	return api.ListBridges200JSONResponse(v.([]string)), nil
}

func (h *handlers) ListImages(ctx context.Context, req api.ListImagesRequestObject) (api.ListImagesResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ListImages(ctx, m)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ListImages404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ListImages500JSONResponse(apiErr(err)), nil
	}
	return api.ListImages200JSONResponse(v.(map[string][]string)), nil
}

func (h *handlers) DeleteImage(ctx context.Context, req api.DeleteImageRequestObject) (api.DeleteImageResponseObject, error) {
	// No pool is the host's default. A hypervisor's is its image cache's pool
	// (named here, so the manifest is kept below); anywhere else the empty
	// pool goes on to the Manager: the local libvirt one uses "default", a
	// remote host's daemon picks its own.
	pool := derefStr(req.Params.Pool)
	if pool == "" && h.role == config.RoleHypervisor {
		pool = cachePool
	}
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		if h.role != config.RoleHypervisor || pool != cachePool {
			return nil, h.svc.DeleteImage(ctx, m, req.Name, pool)
		}
		// A hypervisor deleting from its image cache's pool drops the
		// manifest entry of an image it owned, under the manifest's lock so
		// an upload's commit cannot interleave.
		man := h.manifest()
		man.Lock()
		defer man.Unlock()
		if err := h.svc.DeleteImage(ctx, m, req.Name, pool); err != nil {
			return nil, err
		}
		all, err := man.LoadLocked()
		if err != nil {
			return nil, fmt.Errorf("deleted %q but could not read the image manifest: %w", req.Name, err)
		}
		if _, ok := all[req.Name]; !ok {
			return nil, nil
		}
		delete(all, req.Name)
		if err := man.SaveLocked(all); err != nil {
			return nil, fmt.Errorf("deleted %q but could not drop it from the image manifest: %w", req.Name, err)
		}
		return nil, nil
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.DeleteImage404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.DeleteImage500JSONResponse(apiErr(err)), nil
	}
	return api.DeleteImage204Response{}, nil
}

func (h *handlers) ListPools(ctx context.Context, req api.ListPoolsRequestObject) (api.ListPoolsResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ListPools(ctx, m)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.ListPools404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ListPools500JSONResponse(apiErr(err)), nil
	}
	pools := v.([]vmtool.PoolInfo)
	out := make([]api.PoolInfo, len(pools))
	for i, p := range pools {
		out[i] = api.PoolInfo{Name: p.Name, Path: p.Path, Active: p.Active, Type: nonEmptyPtr(p.Type)}
	}
	return api.ListPools200JSONResponse(out), nil
}

func (h *handlers) CreatePool(ctx context.Context, req api.CreatePoolRequestObject) (api.CreatePoolResponseObject, error) {
	if req.Body == nil || req.Body.Name == "" || req.Body.Path == "" {
		return api.CreatePool400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "name and path are required"}}, nil
	}
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.CreatePool(ctx, m, req.Body.Name, req.Body.Path)
	})
	if err != nil {
		if kindOf(err) == app.KindInvalid {
			return api.CreatePool400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		if kindOf(err) == app.KindNotFound {
			return api.CreatePool404JSONResponse(apiErr(err)), nil
		}
		return api.CreatePool500JSONResponse(apiErr(err)), nil
	}
	return api.CreatePool201JSONResponse{Name: req.Body.Name, Path: req.Body.Path}, nil
}

func (h *handlers) ListPlaybooks(_ context.Context, _ api.ListPlaybooksRequestObject) (api.ListPlaybooksResponseObject, error) {
	pbs, err := h.svc.ListPlaybooks()
	if err != nil {
		return api.ListPlaybooks500JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
	}
	return api.ListPlaybooks200JSONResponse(pbs), nil
}

func (h *handlers) GetVersion(_ context.Context, _ api.GetVersionRequestObject) (api.GetVersionResponseObject, error) {
	sw, err := api.GetSwagger()
	if err != nil {
		return nil, err
	}
	return api.GetVersion200JSONResponse{
		ApiVersion:   sw.Info.Version,
		BuildVersion: buildinfo.Version(),
		Host:         h.hosts.Local(),
	}, nil
}

// ListHosts answers the local row and one probed row per configured host.
// Probes run concurrently under app.ListTimeout with the bearer of this
// request (never a stored token); an unreachable host is a row with its
// error. A hypervisor has no configured hosts, so only its own row answers.
func (h *handlers) ListHosts(ctx context.Context, _ api.ListHostsRequestObject) (api.ListHostsResponseObject, error) {
	now := time.Now()
	status := h.hosts.WithToken(requestToken(ctx)).Status(ctx, app.ListTimeout)
	out := make([]api.HostInfo, len(status))
	for i, s := range status {
		hi := api.HostInfo{
			Name: s.Host, Role: api.HostInfoRole(s.Role), Local: s.Local,
			Reachable: &s.Reachable,
		}
		if s.Address != "" {
			hi.Address = &s.Address
		}
		if s.APIVersion != "" {
			hi.ApiVersion = &s.APIVersion
		}
		if !s.CertNotAfter.IsZero() {
			na := s.CertNotAfter
			hi.CertNotAfter = &na
			if d, ok := s.CertDaysLeft(now); ok {
				hi.CertDaysLeft = &d
			}
		}
		if s.Err != nil {
			msg := s.Err.Error()
			hi.Error = &msg
		}
		out[i] = hi
	}
	return api.ListHosts200JSONResponse(out), nil
}

// nonEmptyPtr is nil for "", so an unknown value is omitted from the JSON.
func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
