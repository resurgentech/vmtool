package server

import (
	"context"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func toAPIMachine(info *app.MachineInfo) api.MachineInfo {
	if info == nil {
		return api.MachineInfo{}
	}
	out := api.MachineInfo{
		Name:  info.Name,
		Kind:  api.MachineInfoKind(info.Kind),
		Host:  info.Host,
		State: api.MachineInfoState(info.State),
	}
	if info.OS != "" {
		out.Os = &info.OS
	}
	if info.Error != "" {
		out.Error = &info.Error
	}
	return out
}

func (h *handlers) ListMachines(ctx context.Context, _ api.ListMachinesRequestObject) (api.ListMachinesResponseObject, error) {
	ms, err := h.svc.ListMachines(ctx)
	if err != nil {
		return api.ListMachines500JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
	}
	out := make([]api.MachineInfo, len(ms))
	for i := range ms {
		out[i] = toAPIMachine(&ms[i])
	}
	return api.ListMachines200JSONResponse(out), nil
}

func (h *handlers) AddMachine(ctx context.Context, req api.AddMachineRequestObject) (api.AddMachineResponseObject, error) {
	if req.Body == nil {
		return api.AddMachine400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "request body required"}}, nil
	}
	in := app.AddMachineInput{
		Name:       req.Body.Name,
		Host:       req.Body.Host,
		Commission: derefBool(req.Body.Commission),
	}
	if req.Body.Kind != nil {
		in.Kind = string(*req.Body.Kind)
	}
	// Name collisions with libvirt domains are checked when libvirt is up;
	// machines do not otherwise need it.
	var vms app.Manager
	if m, err := vmtool.NewManager(); err == nil {
		defer m.Close()
		vms = m
	}
	info, err := h.svc.AddMachine(ctx, vms, in)
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.AddMachine400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindConflict:
			return api.AddMachine409JSONResponse(apiErr(err)), nil
		case app.KindBadGateway:
			return api.AddMachine502JSONResponse(apiErr(err)), nil
		default:
			return api.AddMachine500JSONResponse(apiErr(err)), nil
		}
	}
	return api.AddMachine201JSONResponse(toAPIMachine(info)), nil
}

func (h *handlers) GetMachine(ctx context.Context, req api.GetMachineRequestObject) (api.GetMachineResponseObject, error) {
	info, err := h.svc.GetMachine(ctx, req.Name)
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.GetMachine404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.GetMachine500JSONResponse(apiErr(err)), nil
	}
	return api.GetMachine200JSONResponse(toAPIMachine(info)), nil
}

func (h *handlers) RemoveMachine(ctx context.Context, req api.RemoveMachineRequestObject) (api.RemoveMachineResponseObject, error) {
	if err := h.svc.RemoveMachine(ctx, req.Name); err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.RemoveMachine404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.RemoveMachine500JSONResponse(apiErr(err)), nil
	}
	return api.RemoveMachine204Response{}, nil
}

func (h *handlers) RebootMachine(ctx context.Context, req api.RebootMachineRequestObject) (api.RebootMachineResponseObject, error) {
	info, err := h.svc.RebootMachine(ctx, req.Name)
	if err != nil {
		switch kindOf(err) {
		case app.KindNotFound:
			return api.RebootMachine404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindBadGateway:
			return api.RebootMachine502JSONResponse(apiErr(err)), nil
		default:
			return api.RebootMachine500JSONResponse(apiErr(err)), nil
		}
	}
	return api.RebootMachine200JSONResponse(toAPIMachine(info)), nil
}

func (h *handlers) ExecMachine(ctx context.Context, req api.ExecMachineRequestObject) (api.ExecMachineResponseObject, error) {
	if req.Body == nil || len(req.Body.Command) == 0 {
		return api.ExecMachine400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "command is required"}}, nil
	}
	res, err := h.svc.ExecMachine(ctx, req.Name, req.Body.Command, stdinOf(req.Body.Stdin))
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.ExecMachine400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.ExecMachine404JSONResponse(apiErr(err)), nil
		case app.KindBadGateway:
			return api.ExecMachine502JSONResponse(apiErr(err)), nil
		default:
			return api.ExecMachine500JSONResponse(apiErr(err)), nil
		}
	}
	return api.ExecMachine200JSONResponse{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

func (h *handlers) RunMachinePlaybook(ctx context.Context, req api.RunMachinePlaybookRequestObject) (api.RunMachinePlaybookResponseObject, error) {
	if req.Body == nil || req.Body.Playbook == "" {
		return api.RunMachinePlaybook400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "playbook is required"}}, nil
	}
	var extra map[string]string
	if req.Body.ExtraVars != nil {
		extra = map[string]string(*req.Body.ExtraVars)
	}
	res, err := h.svc.RunMachinePlaybook(ctx, req.Name, req.Body.Playbook, extra)
	out := ""
	if res != nil {
		out = res.Output
	}
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.RunMachinePlaybook400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.RunMachinePlaybook404JSONResponse(apiErr(err)), nil
		default:
			msg := err.Error()
			return api.RunMachinePlaybook500JSONResponse{Output: out, Error: &msg}, nil
		}
	}
	return api.RunMachinePlaybook200JSONResponse{Output: out}, nil
}
