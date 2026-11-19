package hosts

import (
	"fmt"
	"regexp"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Export a template as an image: the volume side. The image cache, the
// manifest and the one-at-a-time registry are the daemon's HTTP handler's
// (internal/server, exportTemplate); this is what names a volume.
//
// The rule that keeps it safe: the only volume the helper's export is ever
// given is one the report, read again under the (VG, template LV) lock that
// builds and deletes hold, lists as a template of the name and LV the caller
// looked up. The destination is a path the handler made; the helper itself
// accepts only a `.part` name in the image directory that does not exist. The
// tag change names a template of that same check.

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

// ExportTemplateVolume implements app.TemplateVolumeExporter: the helper's
// export of the template's volume to dest, under the template's lock (a
// delete or rebuild waits for it).
func (t ThinPools) ExportTemplateVolume(tpl app.TemplateInfo, dest string) error {
	const op = "export template"
	if t.Thin == nil {
		return app.ErrNoThinPools
	}
	cur, _, unlock, err := t.lockTemplate(tpl, op)
	if err != nil {
		return err
	}
	defer unlock()
	// cur is a vmtool_template volume the report lists, of the looked-up name.
	return t.Thin.Export(cur.VG, cur.Name, dest)
}

// RecordTemplateSHA implements app.TemplateVolumeExporter: the template's
// vmtool_sha256 tag becomes sha (one tag call: the old digest, if the template
// has a different one, is removed in the same call).
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
	if have.SHA256 == sha {
		return nil
	}
	var changes []string
	if have.SHA256 != "" {
		changes = append(changes, "-"+tagSHA256+have.SHA256)
	}
	changes = append(changes, "+"+tagSHA256+sha)
	if err := t.Thin.Tag(cur.VG, cur.Name, changes...); err != nil {
		return fmt.Errorf("record the SHA-256 on template %s: %w", cur.Name, err)
	}
	return nil
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

func (h hypervisorPools) ExportTemplateVolume(t app.TemplateInfo, dest string) error {
	if x, ok := volumeExporterOf(h.Manager); ok {
		return x.ExportTemplateVolume(t, dest)
	}
	return app.ErrNoThinPools
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

func (d defaultPools) ExportTemplateVolume(t app.TemplateInfo, dest string) error {
	if x, ok := volumeExporterOf(d.Manager); ok {
		return x.ExportTemplateVolume(t, dest)
	}
	return app.ErrNoThinPools
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
