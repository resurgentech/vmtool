package hosts

import (
	"fmt"
	"strings"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Template management on a thin-pool host: list, build ahead, delete.
//
// The rule that keeps delete safe: the only volume it ever removes is one the
// helper's report lists as a template (the vmtool_template tag, Kind ==
// vmtool.ThinKindTemplate) that the caller's name or LV name matches. Never a
// vmtool_vol volume, a vmtool_building one, an untagged or thick LV, and never
// a clone: a clone is a thin snapshot, independent of its origin, and is not
// named to the helper. No caller names a VG or a path.

// ListTemplates implements app.TemplateManager: every template in every thin
// pool the report shows, with its stale mark against this daemon's image
// manifest. A helper that cannot run is an error: the caller asked.
func (t ThinPools) ListTemplates() ([]app.TemplateInfo, error) {
	if t.Thin == nil {
		return nil, nil
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		return nil, err
	}
	return t.annotate(vmtool.ThinTemplates(vols))
}

// annotate adds the stale mark to templates, from the daemon's current image
// manifest. An unreadable manifest is an error, never "nothing is stale".
func (t ThinPools) annotate(templates []vmtool.Template) ([]app.TemplateInfo, error) {
	entries, err := t.manifest().Load()
	if err != nil {
		return nil, fmt.Errorf("read the image manifest: %w", err)
	}
	stale := map[string]bool{}
	for _, s := range vmtool.StaleTemplates(templates, vmtool.ImageSHAs(entries)) {
		stale[s.Pool+"/"+s.LV] = true
	}
	out := make([]app.TemplateInfo, len(templates))
	for i, tp := range templates {
		out[i] = app.TemplateInfo{Template: tp, Stale: stale[tp.Pool+"/"+tp.LV]}
	}
	return out, nil
}

// PrepareTemplate implements app.TemplateManager: the image template of the
// cached image in pool, built as the first create into that pool would build
// it (ensureTemplate: room check, building volume, import, tag, rename, under
// the (VG, template LV) lock), or the one that is already there, with no
// write. A pool that is not a thin pool of this host is invalid; an image this
// daemon does not hold is not found.
func (t ThinPools) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	if !vmtool.ValidImageName(image) {
		return app.TemplateInfo{}, invalidErr("prepare template", "image name %q must be a plain name", image)
	}
	tp, vols, ok := t.thinTarget(pool)
	if !ok {
		if pool == "" {
			return app.TemplateInfo{}, invalidErr("prepare template", "no thin pool named: this host has no default thin pool; name one with its pool")
		}
		return app.TemplateInfo{}, invalidErr("prepare template", "%q is not a thin pool on this host", pool)
	}
	path, err := t.Manager.ImagePath(image)
	if err != nil {
		return app.TemplateInfo{}, err // not found when no pool holds it
	}
	src, err := t.resolveSource(tp, vols, image, path)
	if err != nil {
		return app.TemplateInfo{}, err
	}
	if src.template != nil || src.cache == "" {
		return app.TemplateInfo{}, invalidErr("prepare template", "%q is a template, not an image", image)
	}
	if found, ok := imageTemplate(vols, tp, src.image, src.sha); ok {
		return app.TemplateInfo{Template: found}, nil
	}
	built, err := t.ensureTemplate(tp, src, noClone)
	if err != nil {
		return app.TemplateInfo{}, err
	}
	// Built from the manifest's current entry: not stale.
	return app.TemplateInfo{Template: built}, nil
}

// DeleteTemplate implements app.TemplateManager. name is a template's name
// (its vmtool_name tag) or its LV name; pool, when set, limits the match to
// that thin pool. No match is not found; more than one is a conflict that
// lists them (repeat with the LV name and/or the pool), and nothing is
// removed. The removal holds the (VG, template LV) lock the builds hold, and
// reads the report again under it: a template another call has just replaced
// or removed is not removed twice or by mistake.
func (t ThinPools) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	if !vmtool.ValidImageName(name) {
		return app.TemplateInfo{}, invalidErr("delete template", "template name %q must be a plain name", name)
	}
	if t.Thin == nil {
		return app.TemplateInfo{}, app.ErrNoThinPools
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		return app.TemplateInfo{}, err
	}
	target, err := pickTemplate(vols, name, pool, "delete template")
	if err != nil {
		return app.TemplateInfo{}, err
	}

	unlock := t.Builds.lock(target.VG + "/" + target.Name)
	defer unlock()
	_, vols, err = t.Thin.Report()
	if err != nil {
		return app.TemplateInfo{}, err
	}
	// the same volume, still a template, still the one asked for
	cur, ok := findVol(vols, target.VG, target.Name)
	if !ok || cur.Kind != vmtool.ThinKindTemplate || len(matchTemplates([]vmtool.ThinVolume{cur}, name, pool)) != 1 {
		return app.TemplateInfo{}, notFoundTemplate(name, pool)
	}
	tpl, err := vmtool.TemplateFromVolume(cur, vols)
	if err != nil {
		return app.TemplateInfo{}, invalidErr("delete template", "%v", err)
	}
	infos, err := t.annotate([]vmtool.Template{tpl})
	if err != nil {
		return app.TemplateInfo{}, err
	}
	// cur is a vmtool_template volume the report lists: the one removal.
	if err := t.Thin.Remove(cur.VG, cur.Name); err != nil {
		return app.TemplateInfo{}, err
	}
	return infos[0], nil
}

// matchTemplates is the template volumes among vols whose name tag or LV name
// is name (in pool, when set). Only Kind == ThinKindTemplate volumes with
// usable tags are candidates.
func matchTemplates(vols []vmtool.ThinVolume, name, pool string) []vmtool.ThinVolume {
	var out []vmtool.ThinVolume
	for _, v := range vols {
		if v.Kind != vmtool.ThinKindTemplate || (pool != "" && v.Pool != pool) {
			continue
		}
		tpl, err := vmtool.TemplateFromVolume(v, vols)
		if err != nil {
			continue
		}
		if tpl.Name == name || v.Name == name {
			out = append(out, v)
		}
	}
	return out
}

// pickTemplate is the one template volume among vols that name (and pool,
// when set) matches: not found when none does, a conflict listing them when
// several do. op names the operation in the error.
func pickTemplate(vols []vmtool.ThinVolume, name, pool, op string) (vmtool.ThinVolume, error) {
	matches := matchTemplates(vols, name, pool)
	switch len(matches) {
	case 0:
		e := notFoundTemplate(name, pool).(*app.Error)
		e.Op = op
		return vmtool.ThinVolume{}, e
	case 1:
		return matches[0], nil
	}
	var which []string
	for _, v := range matches {
		which = append(which, v.Pool+"/"+v.Name)
	}
	return vmtool.ThinVolume{}, &app.Error{Kind: app.KindConflict, Op: op,
		Err: fmt.Errorf("%q matches %d templates (%s); name one by its LV name, and by --pool if the LV name is in two pools", name, len(matches), strings.Join(which, ", "))}
}

func notFoundTemplate(name, pool string) error {
	in := ""
	if pool != "" {
		in = " in thin pool " + pool
	}
	return &app.Error{Kind: app.KindNotFound, Op: "delete template",
		Err: fmt.Errorf("%q is not a template%s: %w", name, in, vmtool.ErrNotFound)}
}

var _ app.TemplateManager = ThinPools{}

// ---- wrappers ----

// templatesOf is m's TemplateManager; a Manager without one has no thin pools.
func templatesOf(m app.Manager) (app.TemplateManager, bool) {
	tm, ok := m.(app.TemplateManager)
	return tm, ok
}

func (h hypervisorPools) ListTemplates() ([]app.TemplateInfo, error) {
	if tm, ok := templatesOf(h.Manager); ok {
		return tm.ListTemplates()
	}
	return nil, app.ErrNoThinPools
}

func (h hypervisorPools) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	if tm, ok := templatesOf(h.Manager); ok {
		return tm.PrepareTemplate(image, hvPool(pool))
	}
	return app.TemplateInfo{}, app.ErrNoThinPools
}

func (h hypervisorPools) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	if tm, ok := templatesOf(h.Manager); ok {
		return tm.DeleteTemplate(name, pool)
	}
	return app.TemplateInfo{}, app.ErrNoThinPools
}

func (d defaultPools) ListTemplates() ([]app.TemplateInfo, error) {
	if tm, ok := templatesOf(d.Manager); ok {
		return tm.ListTemplates()
	}
	return nil, app.ErrNoThinPools
}

// PrepareTemplate applies the host's default_pool to an empty pool, as a
// create does.
func (d defaultPools) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	if tm, ok := templatesOf(d.Manager); ok {
		return tm.PrepareTemplate(image, d.or(pool))
	}
	return app.TemplateInfo{}, app.ErrNoThinPools
}

func (d defaultPools) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	if tm, ok := templatesOf(d.Manager); ok {
		return tm.DeleteTemplate(name, pool)
	}
	return app.TemplateInfo{}, app.ErrNoThinPools
}

var (
	_ app.TemplateManager = hypervisorPools{}
	_ app.TemplateManager = defaultPools{}
)
