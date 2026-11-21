package hosts

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Export a template: the volume side. The template is converted into a thick
// export volume (vmexp-<12 hex>, tagged vmtool_export) in the template's volume
// group; the daemon's HTTP handler (internal/server, exportTemplate) streams
// it. This is what names, reuses, reads and removes those volumes.
//
// The rule that keeps it safe: the only template the helper's export is ever
// given is one the report, read again under the (VG, template LV) lock that
// builds and deletes hold, lists as a template of the name and LV the caller
// looked up. Only vmexp- volumes the report lists as export volumes are ever
// read, tagged or removed. Nothing here takes a path.

// tagSHA256 is the helper tag that carries a template's SHA-256.
const tagSHA256 = "vmtool_sha256="

var sha256Hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LookupTemplate implements app.TemplateVolumeExporter.
func (t ThinPools) LookupTemplate(name, pool string) (app.TemplateInfo, error) {
	const op = "export template"
	if !vmtool.ValidImageName(name) {
		return app.TemplateInfo{}, invalidErr(op, "template name %q must be a plain name", name)
	}
	if t.Thin == nil {
		return app.TemplateInfo{}, app.ErrNoThinPools
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		return app.TemplateInfo{}, err
	}
	v, err := pickTemplate(vols, name, pool, op)
	if err != nil {
		return app.TemplateInfo{}, err
	}
	tpl, err := vmtool.TemplateFromVolume(v, vols)
	if err != nil {
		return app.TemplateInfo{}, invalidErr(op, "%v", err)
	}
	return app.TemplateInfo{Template: tpl}, nil
}

// stillTemplate finds, in a fresh report, the volume of template want: the
// same pool and LV, still a template, still of that name. The caller holds the
// (VG, LV) lock; vg is the volume group the report puts it in.
func (t ThinPools) lockTemplate(want app.TemplateInfo, op string) (cur vmtool.ThinVolume, vols []vmtool.ThinVolume, unlock func(), err error) {
	_, vols, err = t.Thin.Report()
	if err != nil {
		return cur, nil, nil, err
	}
	var vg string
	for _, v := range vols {
		if v.Name == want.LV && v.Pool == want.Pool && v.Kind == vmtool.ThinKindTemplate {
			vg = v.VG
		}
	}
	if vg == "" {
		return cur, nil, nil, notFoundTemplateOp(op, want.LV, want.Pool)
	}
	unlock = t.Builds.lock(vg + "/" + want.LV)
	_, vols, err = t.Thin.Report()
	if err != nil {
		unlock()
		return cur, nil, nil, err
	}
	cur, ok := findVol(vols, vg, want.LV)
	if !ok || cur.Kind != vmtool.ThinKindTemplate || cur.Pool != want.Pool {
		unlock()
		return cur, nil, nil, notFoundTemplateOp(op, want.LV, want.Pool)
	}
	tpl, terr := vmtool.TemplateFromVolume(cur, vols)
	if terr != nil || tpl.Name != want.Name {
		unlock()
		return cur, nil, nil, notFoundTemplateOp(op, want.Name, want.Pool)
	}
	return cur, vols, unlock, nil
}

func notFoundTemplateOp(op, name, pool string) error {
	e := notFoundTemplate(name, pool).(*app.Error)
	e.Op = op
	return e
}

// exportsOf are the export volumes of vols that were made from the template
// LV lv of volume group vg.
func exportsOf(vols []vmtool.ThinVolume, vg, lv string) []vmtool.ThinExport {
	var out []vmtool.ThinExport
	for _, v := range vols {
		if v.Kind == vmtool.ThinKindExport && v.VG == vg {
			if e := vmtool.ExportFromVolume(v); e.From == lv {
				out = append(out, e)
			}
		}
	}
	return out
}

// completeExport is the first finished export of the template LV, if any.
func completeExport(vols []vmtool.ThinVolume, vg, lv string) (vmtool.ThinExport, bool) {
	for _, e := range exportsOf(vols, vg, lv) {
		if e.Complete() {
			return e, true
		}
	}
	return vmtool.ThinExport{}, false
}

// newExportLV is a fresh vmexp-<12 random hex> name that no volume of the VG has.
func newExportLV(vg string, vols []vmtool.ThinVolume) (string, error) {
	for range 8 {
		name, err := vmtool.NewExportLVName(rand.Reader)
		if err != nil {
			return "", err
		}
		if _, taken := findVol(vols, vg, name); !taken {
			return name, nil
		}
	}
	return "", errors.New("could not find an unused export volume name")
}

// ExportTemplateVolume implements app.TemplateVolumeExporter: the template's
// export volume, under the template's lock (a delete or rebuild waits for it).
//
// A finished export of this template LV is reused. An unfinished one (a crash
// left it: no vmtool_bytes) is removed and the export redone. An export of a
// template of the same name whose LV is no longer the one asked for (the
// template was replaced) is removed first; if that removal fails it is logged
// and left for the hourly expiry.
func (t ThinPools) ExportTemplateVolume(tpl app.TemplateInfo, on func(pct float64)) (app.ExportVolume, error) {
	const op = "export template"
	if t.Thin == nil {
		return app.ExportVolume{}, app.ErrNoThinPools
	}
	cur, vols, unlock, err := t.lockTemplate(tpl, op)
	if err != nil {
		return app.ExportVolume{}, err
	}
	defer unlock()

	var have *vmtool.ThinExport
	for _, v := range vols {
		if v.Kind != vmtool.ThinKindExport || v.VG != cur.VG {
			continue
		}
		e := vmtool.ExportFromVolume(v)
		switch {
		case e.From == cur.Name && e.Complete() && have == nil:
			have = &e
		case e.From == cur.Name && !e.Complete():
			// Ours, and not usable: it must go before the export is redone.
			if err := t.Thin.Remove(e.VG, e.Name); err != nil {
				return app.ExportVolume{}, fmt.Errorf("remove the unfinished export %s of template %s: %w", e.Name, tpl.Name, err)
			}
		case e.From == cur.Name, e.TemplateName == tpl.Name:
			// A second finished export of this template, or one of a template
			// of this name that was replaced.
			if err := t.Thin.Remove(e.VG, e.Name); err != nil {
				t.log().Warn("old export volume not removed", "vg", e.VG, "lv", e.Name, "err", err)
			}
		}
	}
	if have != nil {
		return app.ExportVolume{ThinExport: *have, Reused: true}, nil
	}

	name, err := newExportLV(cur.VG, vols)
	if err != nil {
		return app.ExportVolume{}, err
	}
	// cur is a vmtool_template volume the report lists, of the looked-up name.
	if err := t.Thin.ExportThick(cur.VG, cur.Name, name, vmtool.ExportTags(tpl.Name, cur.Name), on); err != nil {
		var room *vmtool.ExportRoomError
		if errors.As(err, &room) {
			room.Template = tpl.Name
			return app.ExportVolume{}, &app.Error{Kind: app.KindConflict, Op: op, Err: room}
		}
		return app.ExportVolume{}, err
	}
	_, vols, err = t.Thin.Report()
	if err != nil {
		return app.ExportVolume{}, err
	}
	v, ok := findVol(vols, cur.VG, name)
	if !ok || v.Kind != vmtool.ThinKindExport {
		return app.ExportVolume{}, fmt.Errorf("export of template %s: the helper finished but the report does not list %s as an export volume", tpl.Name, name)
	}
	e := vmtool.ExportFromVolume(v)
	if !e.Complete() {
		return app.ExportVolume{}, fmt.Errorf("export of template %s: %s has no vmtool_bytes tag", tpl.Name, name)
	}
	return app.ExportVolume{ThinExport: e}, nil
}

// OpenExport implements app.TemplateVolumeExporter: the helper's send of the
// finished export volume name, as a reader of exactly its vmtool_bytes bytes.
func (t ThinPools) OpenExport(name string) (io.ReadCloser, app.ExportVolume, error) {
	const op = "open export"
	if !vmtool.ValidExportLVName(name) {
		return nil, app.ExportVolume{}, invalidErr(op, "%q is not an export volume name (vmexp- and 12 hex characters)", name)
	}
	if t.Thin == nil {
		return nil, app.ExportVolume{}, app.ErrNoThinPools
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		return nil, app.ExportVolume{}, err
	}
	for _, v := range vols {
		if v.Kind != vmtool.ThinKindExport || v.Name != name {
			continue
		}
		e := vmtool.ExportFromVolume(v)
		if !e.Complete() {
			return nil, app.ExportVolume{}, conflictErr(op, "export volume %s is not finished (it has no vmtool_bytes tag)", name)
		}
		r, err := t.Thin.Send(e.VG, e.Name, e.Bytes)
		if err != nil {
			return nil, app.ExportVolume{}, err
		}
		return r, app.ExportVolume{ThinExport: e}, nil
	}
	return nil, app.ExportVolume{}, &app.Error{Kind: app.KindNotFound, Op: op,
		Err: fmt.Errorf("no export volume %q: %w", name, vmtool.ErrNotFound)}
}

// removeExports removes the export volumes of the template LV lv, as the
// template goes. A failed removal is logged: the volume has no template now,
// and the hourly expiry takes it.
func (t ThinPools) removeExports(vols []vmtool.ThinVolume, vg, lv string) {
	for _, e := range exportsOf(vols, vg, lv) {
		if err := t.Thin.Remove(e.VG, e.Name); err != nil {
			t.log().Warn("export volume of a removed template not removed", "vg", e.VG, "lv", e.Name, "err", err)
		}
	}
}

// ExpireExports removes every export volume older than vmtool.ExportTTL at
// now (its lv_time; one with no time is left alone) and returns how many it
// removed. It is quiet unless something fails: a helper that cannot report is
// one log line, a removal that fails is one line and is tried again at the next
// call. Each removal holds the lock of the volume's template, so an export
// being reused is not removed under it.
func (t ThinPools) ExpireExports(now time.Time) int {
	if t.Thin == nil {
		return 0
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("export volumes not looked for", "err", err)
		return 0
	}
	n := 0
	for _, v := range vols {
		if v.Kind != vmtool.ThinKindExport {
			continue
		}
		e := vmtool.ExportFromVolume(v)
		if e.Created.IsZero() || now.Sub(e.Created) <= vmtool.ExportTTL {
			continue
		}
		key := e.From
		if key == "" {
			key = e.Name
		}
		unlock := t.Builds.lock(e.VG + "/" + key)
		err := t.Thin.Remove(e.VG, e.Name)
		unlock()
		if err != nil {
			t.log().Warn("expired export volume not removed", "vg", e.VG, "lv", e.Name, "err", err)
			continue
		}
		n++
	}
	return n
}

// RecordTemplateSHA implements app.TemplateVolumeExporter: the template's
// vmtool_sha256 tag, and that of its finished export volume if it has one,
// becomes sha (one tag call each: the old digest, if there is a different
// one, is removed in the same call).
func (t ThinPools) RecordTemplateSHA(tpl app.TemplateInfo, sha string) error {
	const op = "export template"
	if !sha256Hex64.MatchString(sha) {
		return invalidErr(op, "SHA-256 %q is not 64 lowercase hex characters", sha)
	}
	if t.Thin == nil {
		return app.ErrNoThinPools
	}
	cur, vols, unlock, err := t.lockTemplate(tpl, op)
	if err != nil {
		return err
	}
	defer unlock()
	have, err := vmtool.TemplateFromVolume(cur, vols)
	if err != nil {
		return invalidErr(op, "%v", err)
	}
	if have.SHA256 != sha {
		if err := t.Thin.Tag(cur.VG, cur.Name, shaChanges(have.SHA256, sha)...); err != nil {
			return fmt.Errorf("record the SHA-256 on template %s: %w", cur.Name, err)
		}
	}
	if e, ok := completeExport(vols, cur.VG, cur.Name); ok && e.SHA256 != sha {
		if err := t.Thin.Tag(e.VG, e.Name, shaChanges(e.SHA256, sha)...); err != nil {
			return fmt.Errorf("record the SHA-256 on export volume %s: %w", e.Name, err)
		}
	}
	return nil
}

// shaChanges is the tag call that replaces the digest old (if any) with sha.
func shaChanges(old, sha string) []string {
	var changes []string
	if old != "" {
		changes = append(changes, "-"+tagSHA256+old)
	}
	return append(changes, "+"+tagSHA256+sha)
}

var _ app.TemplateVolumeExporter = ThinPools{}

func volumeExporterOf(m app.Manager) (app.TemplateVolumeExporter, bool) {
	x, ok := m.(app.TemplateVolumeExporter)
	return x, ok
}

func (h hypervisorPools) LookupTemplate(name, pool string) (app.TemplateInfo, error) {
	if x, ok := volumeExporterOf(h.Manager); ok {
		return x.LookupTemplate(name, pool)
	}
	return app.TemplateInfo{}, app.ErrNoThinPools
}

func (h hypervisorPools) ExportTemplateVolume(t app.TemplateInfo, on func(float64)) (app.ExportVolume, error) {
	if x, ok := volumeExporterOf(h.Manager); ok {
		return x.ExportTemplateVolume(t, on)
	}
	return app.ExportVolume{}, app.ErrNoThinPools
}

func (h hypervisorPools) OpenExport(name string) (io.ReadCloser, app.ExportVolume, error) {
	if x, ok := volumeExporterOf(h.Manager); ok {
		return x.OpenExport(name)
	}
	return nil, app.ExportVolume{}, app.ErrNoThinPools
}

func (h hypervisorPools) RecordTemplateSHA(t app.TemplateInfo, sha string) error {
	if x, ok := volumeExporterOf(h.Manager); ok {
		return x.RecordTemplateSHA(t, sha)
	}
	return app.ErrNoThinPools
}

func (d defaultPools) LookupTemplate(name, pool string) (app.TemplateInfo, error) {
	if x, ok := volumeExporterOf(d.Manager); ok {
		return x.LookupTemplate(name, pool)
	}
	return app.TemplateInfo{}, app.ErrNoThinPools
}

func (d defaultPools) ExportTemplateVolume(t app.TemplateInfo, on func(float64)) (app.ExportVolume, error) {
	if x, ok := volumeExporterOf(d.Manager); ok {
		return x.ExportTemplateVolume(t, on)
	}
	return app.ExportVolume{}, app.ErrNoThinPools
}

func (d defaultPools) OpenExport(name string) (io.ReadCloser, app.ExportVolume, error) {
	if x, ok := volumeExporterOf(d.Manager); ok {
		return x.OpenExport(name)
	}
	return nil, app.ExportVolume{}, app.ErrNoThinPools
}

func (d defaultPools) RecordTemplateSHA(t app.TemplateInfo, sha string) error {
	if x, ok := volumeExporterOf(d.Manager); ok {
		return x.RecordTemplateSHA(t, sha)
	}
	return app.ErrNoThinPools
}

var (
	_ app.TemplateVolumeExporter = hypervisorPools{}
	_ app.TemplateVolumeExporter = defaultPools{}
)
