package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Template management (API 2.4.0): listTemplates, createTemplate and
// deleteTemplate. Each runs on the host's Manager, so a workstation forwards
// them for a listed host exactly as it does the other host routes (the
// remote client is an app.TemplateManager), and a hypervisor daemon answers
// from its thin pools. The workflow is internal/app's.

func toAPITemplate(t app.TemplateInfo) api.TemplateInfo {
	out := api.TemplateInfo{
		Name: t.Name, Origin: api.TemplateInfoOrigin(t.Origin), Pool: t.Pool, Lv: t.LV,
		SizeBytes: t.SizeBytes, UsedBytes: t.UsedBytes, Clones: t.Clones, Stale: t.Stale,
		Image: nonEmptyPtr(t.Image), Sha256: nonEmptyPtr(t.SHA256),
	}
	if !t.Created.IsZero() {
		c := t.Created
		out.Created = &c
	}
	if x := t.Export; x != nil {
		e := &api.TemplateExport{Bytes: x.Bytes, Sha256: nonEmptyPtr(x.SHA256)}
		if exp := x.Expires(); !exp.IsZero() {
			e.Expires = &exp
		}
		out.Export = e
	}
	return out
}

func (h *handlers) ListTemplates(ctx context.Context, req api.ListTemplatesRequestObject) (api.ListTemplatesResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.ListTemplates(ctx, m)
	})
	if err != nil {
		if primKind(err) == app.KindNotFound {
			return api.ListTemplates404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return api.ListTemplates500JSONResponse(apiErr(err)), nil
	}
	ts := v.([]app.TemplateInfo)
	out := make([]api.TemplateInfo, len(ts))
	for i, t := range ts {
		out[i] = toAPITemplate(t)
	}
	return api.ListTemplates200JSONResponse(out), nil
}

func (h *handlers) CreateTemplate(ctx context.Context, req api.CreateTemplateRequestObject) (api.CreateTemplateResponseObject, error) {
	if e := h.networkHomeRefusal(); e != nil {
		return api.CreateTemplate409JSONResponse(*e), nil
	}
	b := req.Body
	var bad error
	switch {
	case b == nil:
		bad = errors.New("request body required")
	case derefStr(b.FromVm) != "" && derefStr(b.Image) != "":
		bad = errors.New("image and from_vm are exclusive: build an image template or save a VM, not both")
	case derefStr(b.FromVm) != "":
		bad = checkSaveRequest(b)
	case derefStr(b.Image) == "":
		bad = errors.New("image or from_vm is required")
	case !vmtool.ValidImageName(derefStr(b.Image)):
		bad = badName("image", derefStr(b.Image))
	case !xmlSafe(derefStr(b.Pool)):
		bad = errors.New("pool contains invalid characters")
	case derefStr(b.Name) != "" || (b.Replace != nil && *b.Replace):
		bad = errors.New("name and replace go with from_vm")
	}
	if bad != nil {
		return api.CreateTemplate400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: bad.Error()}}, nil
	}
	save := derefStr(b.FromVm) != ""
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		if save {
			return h.svc.SaveTemplate(ctx, m, app.SaveTemplateInput{Host: req.Host, VM: derefStr(b.FromVm), Name: derefStr(b.Name),
				Replace: b.Replace != nil && *b.Replace})
		}
		return h.guestSvc(ctx).PrepareTemplate(ctx, m, app.PrepareTemplateInput{Host: req.Host, Image: derefStr(b.Image), Pool: derefStr(b.Pool)})
	})
	if err != nil {
		switch primKind(err) {
		case app.KindInvalid:
			return api.CreateTemplate400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.CreateTemplate404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.CreateTemplate409JSONResponse(apiErr(err)), nil
		default:
			return api.CreateTemplate500JSONResponse(apiErr(err)), nil
		}
	}
	if st, ok := v.(app.SavedTemplate); ok {
		out := toAPITemplate(st.TemplateInfo)
		if len(st.ExtraDisks) > 0 {
			out.ExtraDisks = &st.ExtraDisks
		}
		return api.CreateTemplate201JSONResponse(out), nil
	}
	return api.CreateTemplate201JSONResponse(toAPITemplate(v.(app.TemplateInfo))), nil
}

// checkSaveRequest is the input check of a save: a plain VM name and template
// name, and no pool (the template is made in the VM's own pool).
func checkSaveRequest(b *api.CreateTemplateRequest) error {
	switch {
	case !vmtool.ValidMachineName(derefStr(b.FromVm)):
		return badName("from_vm", derefStr(b.FromVm))
	case derefStr(b.Name) == "":
		return errors.New("name is required with from_vm")
	case !vmtool.ValidImageName(derefStr(b.Name)):
		return badName("name", derefStr(b.Name))
	case derefStr(b.Pool) != "":
		return errors.New("pool does not go with from_vm: the template is made in the VM's own pool")
	}
	return nil
}

func (h *handlers) DeleteTemplate(ctx context.Context, req api.DeleteTemplateRequestObject) (api.DeleteTemplateResponseObject, error) {
	pool := derefStr(req.Params.Pool)
	var bad error
	switch {
	case !vmtool.ValidImageName(req.Name):
		bad = badName("name", req.Name)
	case !xmlSafe(pool):
		bad = fmt.Errorf("pool contains invalid characters")
	}
	if bad != nil {
		return api.DeleteTemplate400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: bad.Error()}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.DeleteTemplate(ctx, m, req.Name, pool)
	})
	if err != nil {
		switch primKind(err) {
		case app.KindInvalid:
			return api.DeleteTemplate400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.DeleteTemplate404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.DeleteTemplate409JSONResponse(apiErr(err)), nil
		default:
			return api.DeleteTemplate500JSONResponse(apiErr(err)), nil
		}
	}
	return api.DeleteTemplate200JSONResponse(toAPITemplate(v.(app.TemplateInfo))), nil
}
