package hosts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Save a VM as a project template.
//
// The save is one helper write: a thin snapshot (-kn) of the VM's boot LV,
// made directly under its final name vmtpl-<12 random hex> with the tags
// vmtool_template, vmtool_origin=saved and vmtool_name=<name>. vmtool does not
// boot, mount, inspect or change the disk. The source LV is only ever the
// origin argument of that snapshot: no create, extend, remove, rename, tag,
// import or export names it, and the VM and its extra disks are not touched.
//
// With replace the old saved template is removed after the new one exists,
// by its LV, under the (VG, LV) lock the template builds and deletes hold and
// after reading the report again; only a volume that is, at that moment, a
// saved template of that name is removed. An image template or a cached image
// of the name is never replaced.

const notThinMsg = "templates are saved from VMs on a thin pool"

// SaveTemplate implements app.TemplateSaver.
func (t ThinPools) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	const op = "save template"
	if !vmtool.ValidMachineName(vm) {
		return app.SavedTemplate{}, invalidErr(op, "VM name %q must be a plain name", vm)
	}
	if !vmtool.ValidImageName(name) {
		return app.SavedTemplate{}, invalidErr(op, "template name %q must be a plain name (letters, digits, '.', '_', '-')", name)
	}
	dm, hasDom := t.Manager.(thinDomain)
	if t.Thin == nil || !hasDom {
		return app.SavedTemplate{}, invalidErr(op, "%s", notThinMsg)
	}
	// One save of a name at a time on this daemon.
	unlockName := t.Builds.lock("save/" + name)
	defer unlockName()

	info, err := t.Manager.Info(vm)
	if err != nil {
		return app.SavedTemplate{}, err
	}
	if info.State != vmtool.StateShutoff {
		return app.SavedTemplate{}, &app.Error{Kind: app.KindConflict, Op: op,
			Err: fmt.Errorf("VM %q is %s: shut it down first: `vmtool stop <host>/%s`", vm, info.State, vm)}
	}
	dom, err := dm.DomainDisks(vm)
	if err != nil {
		return app.SavedTemplate{}, err
	}
	pools, vols, err := t.Thin.Report()
	if err != nil {
		return app.SavedTemplate{}, err
	}
	pool, boot, ok := bootVolume(dom, pools, vols)
	if !ok {
		return app.SavedTemplate{}, invalidErr(op, "%s: the boot disk of %q is not a thin volume vmtool made", notThinMsg, vm)
	}

	old, err := t.checkSaveName(name, replace, vols)
	if err != nil {
		return app.SavedTemplate{}, err
	}
	if err := vmtool.CheckThinRoom(pool, boot.SizeBytes); err != nil {
		return app.SavedTemplate{}, noRoomOp("save_template", err)
	}

	lv, err := newSavedLV(pool.VG, vols)
	if err != nil {
		return app.SavedTemplate{}, err
	}
	tags, err := vmtool.TemplateTags(name, "", vmtool.TemplateOriginSaved)
	if err != nil {
		return app.SavedTemplate{}, invalidErr(op, "%v", err)
	}
	// The one write that names the source: it is the snapshot's origin.
	if err := t.Thin.Snapshot(pool.VG, boot.Name, lv, tags...); err != nil {
		return app.SavedTemplate{}, err // nothing of ours exists yet: remove nothing
	}

	out := app.SavedTemplate{TemplateInfo: app.TemplateInfo{Template: vmtool.Template{
		Name: name, Origin: vmtool.TemplateOriginSaved, Pool: pool.Name, LV: lv, SizeBytes: boot.SizeBytes, Created: time.Now().UTC(),
	}}}
	for _, d := range dom.Used {
		if d != "vda" {
			out.ExtraDisks = append(out.ExtraDisks, d)
		}
	}

	if old != nil {
		if err := t.removeOldSaved(*old, name, lv); err != nil {
			return app.SavedTemplate{}, fmt.Errorf("template %q is saved as %s, but removing the old saved template %s failed: %w; remove it by its LV name (vmtool templates delete <host>/%s)",
				name, lv, old.LV, err, old.LV)
		}
	}
	// The answer is the report's view of the new template when it has it.
	if _, vols2, err := t.Thin.Report(); err == nil {
		if v, found := findVol(vols2, pool.VG, lv); found && v.Kind == vmtool.ThinKindTemplate {
			if tpl, err := vmtool.TemplateFromVolume(v, vols2); err == nil {
				out.Template = tpl
			}
		}
	}
	return out, nil
}

// bootVolume is the boot disk's thin volume and its pool, when vda is a
// vmtool_vol LV of a vmtool thin pool in the report (the same rule as thinBoot,
// on a report already read).
func bootVolume(dom *vmtool.DomainDisks, pools []vmtool.ThinPool, vols []vmtool.ThinVolume) (vmtool.ThinPool, vmtool.ThinVolume, bool) {
	if dom.BootKind != vmtool.DiskBackingBlock {
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	vg, lv, ok := vmtool.ParseThinDevPath(dom.BootPath)
	if !ok {
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	v, found := findVol(vols, vg, lv)
	if !found || v.Kind != vmtool.ThinKindVol {
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	for _, p := range pools {
		if p.VG == v.VG && p.Name == v.Pool {
			return p, v, true
		}
	}
	return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
}

// checkSaveName refuses a name that a template (of any origin, in any pool)
// or a cached image already has, except the one saved template that replace
// may replace, which it returns.
func (t ThinPools) checkSaveName(name string, replace bool, vols []vmtool.ThinVolume) (*vmtool.Template, error) {
	var saved []vmtool.Template
	for _, v := range vols {
		if v.Kind != vmtool.ThinKindTemplate {
			continue
		}
		tpl, err := vmtool.TemplateFromVolume(v, vols)
		if err != nil {
			if v.Name == name { // broken tags: the LV name is still taken
				return nil, conflictErr("save template", "the name %q is in use by the template volume %s", name, v.Name)
			}
			continue
		}
		switch {
		case tpl.Name != name && v.Name != name:
			continue
		case v.Name == name && tpl.Name != name:
			return nil, conflictErr("save template", "the name %q is in use: it is the LV name of the template %q; choose another name", name, tpl.Name)
		case tpl.Origin == vmtool.TemplateOriginImage:
			return nil, conflictErr("save template", "the name %q is in use by an image template (%s in pool %s); a saved template never replaces one, choose another name", name, tpl.LV, tpl.Pool)
		}
		saved = append(saved, tpl)
	}
	if _, err := t.Manager.ImagePath(name); err == nil {
		return nil, conflictErr("save template", "the name %q is in use by a cached image on this host; a saved template never replaces one, choose another name", name)
	} else if !vmtool.IsNotFound(err) {
		return nil, err
	}
	switch {
	case len(saved) == 0:
		return nil, nil
	case !replace:
		return nil, conflictErr("save template", "a template named %q already exists (%s in pool %s); use --replace to replace it, or choose another name", name, saved[0].LV, saved[0].Pool)
	case len(saved) > 1:
		return nil, conflictErr("save template", "%d saved templates are named %q; remove all but one by their LV names first", len(saved), name)
	}
	return &saved[0], nil
}

func conflictErr(op string, format string, a ...any) error {
	return &app.Error{Kind: app.KindConflict, Op: op, Err: fmt.Errorf(format, a...)}
}

// newSavedLV is a fresh vmtpl-<12 random hex> name that no volume of the VG has.
func newSavedLV(vg string, vols []vmtool.ThinVolume) (string, error) {
	for range 8 {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		lv, err := vmtool.TemplateLVName(hex.EncodeToString(b[:]))
		if err != nil {
			return "", err
		}
		if _, taken := findVol(vols, vg, lv); !taken {
			return lv, nil
		}
	}
	return "", errors.New("could not find an unused template LV name")
}

// removeOldSaved removes old, the saved template that replace names, after the
// new template newLV exists. It takes old's (VG, LV) lock, reads the report
// again and removes it only if it is still a saved template of that name (and
// not newLV); one that has changed or gone is left, and that is no error.
func (t ThinPools) removeOldSaved(old vmtool.Template, name, newLV string) error {
	_, vols, err := t.Thin.Report()
	if err != nil {
		return err
	}
	var vg string
	for _, v := range vols {
		if v.Name == old.LV && v.Pool == old.Pool {
			vg = v.VG
		}
	}
	if vg == "" {
		return nil // already gone
	}
	unlock := t.Builds.lock(vg + "/" + old.LV)
	defer unlock()
	_, vols, err = t.Thin.Report()
	if err != nil {
		return err
	}
	cur, ok := findVol(vols, vg, old.LV)
	if !ok || cur.Kind != vmtool.ThinKindTemplate || cur.Name == newLV {
		return nil
	}
	tpl, err := vmtool.TemplateFromVolume(cur, vols)
	if err != nil || tpl.Origin != vmtool.TemplateOriginSaved || tpl.Name != name {
		return nil
	}
	// cur is a vmtool_template volume the report lists, saved, of that name.
	return t.Thin.Remove(cur.VG, cur.Name)
}

var _ app.TemplateSaver = ThinPools{}

func (h hypervisorPools) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	if ts, ok := h.Manager.(app.TemplateSaver); ok {
		return ts.SaveTemplate(vm, name, replace)
	}
	return app.SavedTemplate{}, app.ErrNoThinPools
}

func (d defaultPools) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	if ts, ok := d.Manager.(app.TemplateSaver); ok {
		return ts.SaveTemplate(vm, name, replace)
	}
	return app.SavedTemplate{}, app.ErrNoThinPools
}

var (
	_ app.TemplateSaver = hypervisorPools{}
	_ app.TemplateSaver = defaultPools{}
)
