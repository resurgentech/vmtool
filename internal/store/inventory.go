package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
)

// Report and notes limits. The 1 MiB body limit is maxCredentialBody (the
// same guard). The comment's limit is vmnotes.MaxCommentBytes.
const (
	maxReportVMs   = 4096
	maxDisksPerVM  = 128
	maxShortField  = 128
	maxVersionLen  = 128
	maxOriginField = 512
)

// SweepInterval is the period of the inventory sweep.
const SweepInterval = 24 * time.Hour

var forbidden = storeapi.ForbiddenJSONResponse{Error: "forbidden"}

// plainText reports whether s is at most max bytes of valid UTF-8 with no
// control character (a newline included).
func plainText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// reportVM validates one reported VM and returns its row. The error names the
// field, never a value.
func reportVM(v storeapi.ReportVM) (db.VMRow, string) {
	switch {
	// Any name libvirt allows (no '/'), not only names vmtool would give: a VM
	// made outside vmtool must not cost its host the whole report.
	case v.Name == "" || !plainText(v.Name, maxNameLen) || strings.Contains(v.Name, "/"):
		return db.VMRow{}, "name"
	case !uuidRE.MatchString(v.Uuid):
		return db.VMRow{}, "uuid"
	case v.State == "" || !plainText(v.State, 32):
		return db.VMRow{}, "state"
	case v.Vcpus < 0 || v.Vcpus > 1<<16:
		return db.VMRow{}, "vcpus"
	case v.Memory < 0 || v.Memory > 1<<30:
		return db.VMRow{}, "memory"
	case !plainText(v.Ip, maxShortField):
		return db.VMRow{}, "ip"
	case !plainText(v.Firmware, maxShortField):
		return db.VMRow{}, "firmware"
	case len(v.Disks) > maxDisksPerVM:
		return db.VMRow{}, "disks"
	}
	for _, d := range v.Disks {
		if !plainText(d.Target, maxShortField) || !plainText(d.Pool, maxShortField) || d.CapacityBytes < 0 ||
			(d.Kind != storeapi.File && d.Kind != storeapi.Block && d.Kind != storeapi.Thin) {
			return db.VMRow{}, "disks"
		}
	}
	disks, err := json.Marshal(v.Disks)
	if err != nil || v.Disks == nil {
		disks = []byte("[]")
	}
	row := db.VMRow{
		UUID: strings.ToLower(v.Uuid), Name: v.Name, State: v.State, VCPUs: v.Vcpus, MemoryGiB: v.Memory,
		IP: v.Ip, Autostart: v.Autostart, Firmware: v.Firmware, Disks: disks,
		DisksUnreadable: v.DisksUnreadable != nil && *v.DisksUnreadable,
	}
	return row, ""
}

func (h *handlers) PutHostInventory(ctx context.Context, req storeapi.PutHostInventoryRequestObject) (storeapi.PutHostInventoryResponseObject, error) {
	// Only the host's own token: another host's token and every user token
	// (whose caller never starts with "host/") are refused.
	if callerFrom(ctx) != hostCallerPrefix+req.Host {
		return storeapi.PutHostInventory403JSONResponse{ForbiddenJSONResponse: forbidden}, nil
	}
	bad := func(f string) (storeapi.PutHostInventoryResponseObject, error) {
		return storeapi.PutHostInventory400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	switch {
	case b == nil:
		return bad("body")
	case b.GeneratedAt.IsZero():
		return bad("generated_at")
	case !plainText(b.DaemonVersion, maxVersionLen):
		return bad("daemon_version")
	case len(b.Vms) > maxReportVMs:
		return bad("vms")
	}
	rows := make([]db.VMRow, 0, len(b.Vms))
	for i, v := range b.Vms {
		row, field := reportVM(v)
		if field != "" {
			return bad(fmt.Sprintf("%s (vms[%d])", field, i))
		}
		rows = append(rows, row)
	}
	if !h.img.dbReady() {
		return storeapi.PutHostInventory500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	cnt, err := h.img.db.ApplyReport(ctx, req.Host, b.GeneratedAt, b.DaemonVersion, rows)
	if err != nil {
		h.img.fail("apply report", err)
		return storeapi.PutHostInventory500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutHostInventory200JSONResponse{
		Listed: cnt.Listed, NewlyGone: cnt.NewlyGone, Relisted: cnt.Relisted, Stale: cnt.Stale,
	}, nil
}

func disksOf(raw json.RawMessage) []storeapi.Disk {
	out := []storeapi.Disk{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out) // a row the store wrote; on failure, no disks
	}
	if out == nil {
		out = []storeapi.Disk{}
	}
	return out
}

func toInventoryVM(v db.InventoryVM) storeapi.InventoryVM {
	out := storeapi.InventoryVM{
		Host: v.Host, Name: v.Name, Uuid: v.UUID, State: v.State, Vcpus: v.VCPUs, Memory: v.MemoryGiB,
		Ip: v.IP, Autostart: v.Autostart, Firmware: v.Firmware, Disks: disksOf(v.Disks), Comment: v.Comment,
		FirstSeen: v.FirstSeen, LastSeen: v.LastSeen, HasCredentials: v.HasCredentials,
	}
	if v.DisksUnreadable {
		t := true
		out.DisksUnreadable = &t
	}
	out.Origin = originToAPI(v.Origin)
	if v.GoneSince != nil {
		g := *v.GoneSince
		k := g.Add(db.GracePeriod)
		out.GoneSince, out.CredentialsKeptUntil = &g, &k
	}
	return out
}

func (h *handlers) GetInventory(ctx context.Context, req storeapi.GetInventoryRequestObject) (storeapi.GetInventoryResponseObject, error) {
	host := deref(req.Params.Host)
	if host != "" && !validHostName(host) {
		return storeapi.GetInventory400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.GetInventory500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	withGone := req.Params.Gone != nil && *req.Params.Gone
	inv, err := h.img.db.ListInventory(ctx, host, withGone)
	if err != nil {
		h.img.fail("list inventory", err)
		return storeapi.GetInventory500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	machines, err := h.img.db.ListMachines(ctx)
	if err != nil {
		h.img.fail("list machines", err)
		return storeapi.GetInventory500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.Inventory{
		Hosts:    make([]storeapi.InventoryHost, 0, len(inv.Hosts)),
		Vms:      make([]storeapi.InventoryVM, 0, len(inv.VMs)),
		Machines: make([]storeapi.MachineSummary, 0, len(machines)),
	}
	for _, x := range inv.Hosts {
		out.Hosts = append(out.Hosts, storeapi.InventoryHost{
			Name: x.Name, LastReport: x.LastReport, DaemonVersion: x.DaemonVersion, Live: x.Live, Gone: x.Gone,
		})
	}
	for _, v := range inv.VMs {
		out.Vms = append(out.Vms, toInventoryVM(v))
	}
	for _, m := range machines {
		out.Machines = append(out.Machines, storeapi.MachineSummary{Name: m.Name, Kind: m.Kind})
	}
	return storeapi.GetInventory200JSONResponse(out), nil
}

func (h *handlers) DelistVM(ctx context.Context, req storeapi.DelistVMRequestObject) (storeapi.DelistVMResponseObject, error) {
	switch {
	case !validHostName(req.Host):
		return storeapi.DelistVM400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	case !validMachineName(req.Name):
		return storeapi.DelistVM400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	case !h.img.dbReady():
		return storeapi.DelistVM500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	d, err := h.img.db.Delist(ctx, req.Host, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DelistVM404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delist", err)
		return storeapi.DelistVM500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	// The row is still there (gone, not removed): ask it whether a login of
	// the same UUID exists.
	inv, err := h.img.db.ListInventory(ctx, req.Host, true)
	if err != nil {
		h.img.fail("list inventory", err)
		return storeapi.DelistVM500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	had := false
	for _, v := range inv.VMs {
		if v.Name == req.Name && v.UUID == d.UUID {
			had = v.HasCredentials
		}
	}
	return storeapi.DelistVM200JSONResponse{Uuid: d.UUID, CredentialsKeptUntil: d.KeptUntil, HadCredentials: had}, nil
}

func (h *handlers) DeleteInventoryRow(ctx context.Context, req storeapi.DeleteInventoryRowRequestObject) (storeapi.DeleteInventoryRowResponseObject, error) {
	switch {
	case !validHostName(req.Host):
		return storeapi.DeleteInventoryRow400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	case !validMachineName(req.Name):
		return storeapi.DeleteInventoryRow400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	case !uuidRE.MatchString(req.Params.Uuid):
		return storeapi.DeleteInventoryRow400JSONResponse{BadRequestJSONResponse: fieldErr("uuid")}, nil
	case !h.img.dbReady():
		return storeapi.DeleteInventoryRow500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	err := h.img.db.DeleteVMRow(ctx, req.Host, req.Name, strings.ToLower(req.Params.Uuid))
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteInventoryRow404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delete inventory row", err)
		return storeapi.DeleteInventoryRow500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.DeleteInventoryRow204Response{}, nil
}

// ---- the sweep --------------------------------------------------------------

// SweepDeps are what the sweep needs. Now and After default to the real
// clock; tests inject fakes so no test waits.
type SweepDeps struct {
	DB    *db.DB
	Log   *slog.Logger
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

func (d *SweepDeps) fill() {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.After == nil {
		d.After = time.After
	}
}

// RunSweep removes the rows gone for 7 days or more, and their matching
// logins, as of the store's clock. A failure is logged as a class only (the
// database's error text is not logged) and returned.
func RunSweep(ctx context.Context, d SweepDeps) (db.SweepCounts, error) {
	d.fill()
	n, err := d.DB.Sweep(ctx, d.Now())
	if err != nil {
		d.Log.Error("inventory sweep failed", "op", "sweep", "class", "sweep")
		return n, err
	}
	d.Log.Info("inventory sweep done", "op", "sweep", "rows", n.Rows, "credentials", n.Credentials, "orphan_notes", n.Notes)
	return n, nil
}

// StartSweeps sweeps once at start and then every SweepInterval until ctx
// ends. The returned channel closes when the goroutine has stopped. A failed
// sweep is logged by RunSweep and tried again at the next tick.
func StartSweeps(ctx context.Context, d SweepDeps) <-chan struct{} {
	d.fill()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _ = RunSweep(ctx, d)
			select {
			case <-ctx.Done():
				return
			case <-d.After(SweepInterval):
			}
		}
	}()
	return done
}
