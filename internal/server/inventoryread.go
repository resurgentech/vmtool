package server

import (
	"context"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/storeclient"
)

// GetInventory serves the store's inventory through app.Service.Inventory,
// the call the CLI and the TUI use. The store session (store.go) carries no
// probe: Inventory decides the mode itself and answers KindUnavailable, which
// is 503, so no "using local" warning rides on a request that has no local
// mode.
func (h *handlers) GetInventory(ctx context.Context, req api.GetInventoryRequestObject) (api.GetInventoryResponseObject, error) {
	host := ""
	if req.Params.Host != nil {
		host = *req.Params.Host
	}
	gone := req.Params.Gone != nil && *req.Params.Gone
	inv, err := h.svcFor(ctx).Inventory(ctx, host, gone)
	if err != nil {
		if kindOf(err) == app.KindUnavailable {
			return api.GetInventory503JSONResponse(apiErr(err)), nil
		}
		return api.GetInventory500JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
	}
	return api.GetInventory200JSONResponse(toAPIInventory(inv)), nil
}

func toAPIInventory(in *storeclient.Inventory) api.Inventory {
	out := api.Inventory{
		Hosts:    make([]api.InventoryHost, len(in.Hosts)),
		Vms:      make([]api.InventoryVM, len(in.Vms)),
		Machines: make([]api.InventoryMachine, len(in.Machines)),
	}
	for i, h := range in.Hosts {
		out.Hosts[i] = api.InventoryHost{Name: h.Name, LastReport: h.LastReport, DaemonVersion: h.DaemonVersion, Live: h.Live, Gone: h.Gone}
	}
	for i, v := range in.Vms {
		o := api.InventoryVM{
			Host: v.Host, Name: v.Name, Uuid: v.Uuid, State: v.State, Vcpus: v.Vcpus, Memory: v.Memory,
			Ip: v.Ip, Autostart: v.Autostart, Firmware: v.Firmware, Comment: v.Comment,
			FirstSeen: v.FirstSeen, LastSeen: v.LastSeen, GoneSince: v.GoneSince,
			CredentialsKeptUntil: v.CredentialsKeptUntil, HasCredentials: v.HasCredentials,
			DisksUnreadable: v.DisksUnreadable,
			Disks:           make([]api.InventoryDisk, len(v.Disks)),
		}
		for j, d := range v.Disks {
			o.Disks[j] = api.InventoryDisk{Target: d.Target, Pool: d.Pool, CapacityBytes: d.CapacityBytes, Kind: api.InventoryDiskKind(d.Kind)}
		}
		if v.Origin != nil {
			o.Origin = &api.InventoryOrigin{Image: v.Origin.Image, ImageSha256: v.Origin.ImageSha256,
				Template: v.Origin.Template, Clone: v.Origin.Clone, Pool: v.Origin.Pool, Created: v.Origin.Created}
		}
		out.Vms[i] = o
	}
	for i, m := range in.Machines {
		out.Machines[i] = api.InventoryMachine{Name: m.Name, Kind: m.Kind}
	}
	return out
}
