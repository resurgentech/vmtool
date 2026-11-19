package hosts

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Thin-pool clone, define, delete and resize: ThinPools' overrides. A request
// whose pool is a libvirt pool (or whose path is not an LV of a vmtool thin
// pool) goes to the inner Manager exactly as before; only a thin pool's own
// volumes are handled here, through the root helper (vmtool.Thin).
//
// The rule that keeps this safe: the only volumes ever removed are
//   - a volume tagged vmtool_vol, named for the VM being deleted (Delete);
//   - the volume this same call has just made and failed to finish (a failed
//     template build, a failed extend);
//   - the vmtool_building volumes a restarted daemon finds (CleanThinBuilding).
// Anything else, foreign or vmtool's, is left alone.

// ThinBuilds serializes template builds: one holder per (VG, template LV) at a
// time. It is process-wide state, so the resolver holds one and every
// ThinPools it makes shares it. The zero value is ready; a nil *ThinBuilds
// is the package's own table.
type ThinBuilds struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var processBuilds ThinBuilds

func (b *ThinBuilds) lock(key string) (unlock func()) {
	if b == nil {
		b = &processBuilds
	}
	b.mu.Lock()
	if b.locks == nil {
		b.locks = map[string]*sync.Mutex{}
	}
	m := b.locks[key]
	if m == nil {
		m = &sync.Mutex{}
		b.locks[key] = m
	}
	b.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (t ThinPools) manifest() *vmtool.ImageManifest {
	p := t.ManifestPath
	if p == "" {
		p = vmtool.ImageManifestFile
	}
	return vmtool.OpenImageManifest(p)
}

func invalidErr(op string, format string, a ...any) error {
	return &app.Error{Kind: app.KindInvalid, Op: op, Err: fmt.Errorf(format, a...)}
}

// thinTarget finds the thin pool named pool, with the report's volumes. A
// libvirt pool of that name wins (ListPools leaves a clashing thin pool out
// the same way), and a helper that cannot run or a pool the helper does not
// have is "not thin": ok false, and the caller goes on as before.
func (t ThinPools) thinTarget(pool string) (vmtool.ThinPool, []vmtool.ThinVolume, bool) {
	if t.Thin == nil || pool == "" {
		return vmtool.ThinPool{}, nil, false
	}
	libvirt, err := t.Manager.ListPools()
	if err != nil {
		return vmtool.ThinPool{}, nil, false
	}
	for _, p := range libvirt {
		if p.Name == pool {
			return vmtool.ThinPool{}, nil, false
		}
	}
	pools, vols, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("thin pools not read", "pool", pool, "err", err)
		return vmtool.ThinPool{}, nil, false
	}
	if tp, ok := findPool(pools, pool); ok {
		return tp, vols, true
	}
	return vmtool.ThinPool{}, nil, false
}

func findPool(pools []vmtool.ThinPool, name string) (vmtool.ThinPool, bool) {
	for _, p := range pools {
		if p.Name == name {
			return p, true
		}
	}
	return vmtool.ThinPool{}, false
}

func findVol(vols []vmtool.ThinVolume, vg, lv string) (vmtool.ThinVolume, bool) {
	for _, v := range vols {
		if v.VG == vg && v.Name == lv {
			return v, true
		}
	}
	return vmtool.ThinVolume{}, false
}

// poolVols are the volumes of one pool.
func poolVols(vols []vmtool.ThinVolume, pool vmtool.ThinPool) []vmtool.ThinVolume {
	var out []vmtool.ThinVolume
	for _, v := range vols {
		if v.VG == pool.VG && v.Pool == pool.Name {
			out = append(out, v)
		}
	}
	return out
}

// imageTemplate is the image template of exactly (image, sha) in the pool. A
// saved template of the same name is never an answer here: a cached image is
// not shadowed (T3's A9, the conservative reading).
func imageTemplate(vols []vmtool.ThinVolume, pool vmtool.ThinPool, image, sha string) (vmtool.Template, bool) {
	for _, tp := range vmtool.ThinTemplates(poolVols(vols, pool)) {
		if tp.Origin == vmtool.TemplateOriginImage && tp.Image == image && tp.SHA256 == sha {
			return tp, true
		}
	}
	return vmtool.Template{}, false
}

func noRoom(err error) error {
	if errors.Is(err, vmtool.ErrThinNoRoom) {
		return &app.Error{Kind: app.KindConflict, Op: "clone", Err: err}
	}
	return err
}

// CloneImage is CloneImageFrom with no image name: for a cached image the
// name is the base name of basePath.
func (t ThinPools) CloneImage(basePath, newName, targetPool string, sizeBytes uint64) (string, error) {
	return t.CloneImageFrom("", basePath, newName, targetPool, sizeBytes)
}

// CloneImageFrom clones into targetPool. Into a libvirt pool (or a pool that
// is not a thin pool) it is the inner Manager's clone, unchanged. Into a thin
// pool the clone is a snapshot of the image's template, built first if the
// pool has none (see cloneThin).
func (t ThinPools) CloneImageFrom(image, basePath, newName, targetPool string, sizeBytes uint64) (string, error) {
	pool, vols, ok := t.thinTarget(targetPool)
	if !ok {
		if tc, ok := t.Manager.(app.TemplateCloner); ok {
			return tc.CloneImageFrom(image, basePath, newName, targetPool, sizeBytes)
		}
		return t.Manager.CloneImage(basePath, newName, targetPool, sizeBytes)
	}
	return t.cloneThin(pool, vols, image, basePath, newName, sizeBytes)
}

// cloneSource is what a thin clone comes from.
type cloneSource struct {
	template *vmtool.Template // set when basePath is a template's LV
	image    string
	sha      string
	cache    string // the cached qcow2, for a build
}

func (t ThinPools) resolveSource(pool vmtool.ThinPool, vols []vmtool.ThinVolume, image, basePath string) (cloneSource, error) {
	if vg, lv, ok := vmtool.ParseThinDevPath(basePath); ok {
		v, found := findVol(vols, vg, lv)
		if !found || v.Kind != vmtool.ThinKindTemplate {
			return cloneSource{}, invalidErr("clone", "%s is not a template of a vmtool thin pool", basePath)
		}
		if v.VG != pool.VG || v.Pool != pool.Name {
			return cloneSource{}, invalidErr("clone", "template %s is in thin pool %s, not %s", basePath, v.Pool, pool.Name)
		}
		tpl, err := vmtool.TemplateFromVolume(v, vols)
		if err != nil {
			return cloneSource{}, invalidErr("clone", "%v", err)
		}
		return cloneSource{template: &tpl, image: tpl.Name}, nil
	}
	// The cached file is what is cloned, so its name (the manifest's key)
	// names the template. The request's image name may differ from it by
	// lookup alone (e.g. "img1" found as "img1.qcow2"), which must not fail a
	// thin create.
	name := filepath.Base(basePath)
	entry, listed, err := t.manifest().Get(name)
	if err != nil {
		return cloneSource{}, fmt.Errorf("read the image manifest: %w", err)
	}
	if !listed {
		return cloneSource{}, invalidErr("clone", "image %q is not in this daemon's image manifest; only images uploaded to it can be cloned into a thin pool", name)
	}
	if !vmtool.ValidImageName(name) || len(entry.SHA256) != 64 {
		return cloneSource{}, invalidErr("clone", "image %q has no usable SHA-256 in the manifest", name)
	}
	st, err := os.Lstat(basePath)
	switch {
	case err != nil:
		return cloneSource{}, invalidErr("clone", "cached image %q: %v", name, err)
	case !st.Mode().IsRegular():
		return cloneSource{}, invalidErr("clone", "cached image %q is not a regular file", name)
	case st.Size() != entry.Size:
		return cloneSource{}, invalidErr("clone", "cached image %q is %d bytes but the manifest says %d; it must be uploaded again", name, st.Size(), entry.Size)
	}
	return cloneSource{image: name, sha: entry.SHA256, cache: basePath}, nil
}

// cloneThin makes newName in the pool as a thin snapshot, and returns its
// device path. Before any helper write the room check has passed for the bytes
// this call will promise.
func (t ThinPools) cloneThin(pool vmtool.ThinPool, vols []vmtool.ThinVolume, image, basePath, newName string, sizeBytes uint64) (string, error) {
	if strings.HasPrefix(newName, "vmtpl-") {
		return "", invalidErr("clone", "a VM on a thin pool cannot be named vmtpl-...: that prefix is for templates")
	}
	if !vmtool.ValidThinLVName(newName) {
		return "", invalidErr("clone", "%q is not usable as a thin volume name", newName)
	}
	if sizeBytes > math.MaxInt64 {
		return "", invalidErr("clone", "size %d bytes is too large", sizeBytes)
	}
	size := int64(sizeBytes)
	src, err := t.resolveSource(pool, vols, image, basePath)
	if err != nil {
		return "", err
	}

	var tpl vmtool.Template
	switch {
	case src.template != nil:
		tpl = *src.template
		if err := vmtool.CheckThinRoom(pool, max(tpl.SizeBytes, size)); err != nil {
			return "", noRoom(err)
		}
	default:
		if found, ok := imageTemplate(vols, pool, src.image, src.sha); ok {
			tpl = found
			if err := vmtool.CheckThinRoom(pool, max(tpl.SizeBytes, size)); err != nil {
				return "", noRoom(err)
			}
		} else if tpl, err = t.ensureTemplate(pool, src, size); err != nil {
			return "", err
		}
	}

	if err := t.Thin.Snapshot(pool.VG, tpl.LV, newName, vmtool.ThinTagVol); err != nil {
		return "", err // nothing of ours exists yet: remove nothing
	}
	if size > tpl.SizeBytes {
		if err := t.Thin.Extend(pool.VG, newName, size); err != nil {
			// newName is the volume this call has just made, tagged vmtool_vol.
			if rerr := t.Thin.Remove(pool.VG, newName); rerr != nil {
				return "", fmt.Errorf("%w (and removing the new volume %s failed: %v)", err, newName, rerr)
			}
			return "", err
		}
	}
	return vmtool.ThinDevPath(pool.VG, newName), nil
}

// ensureTemplate returns the image template of src in pool, building it if no
// one has. One build per (VG, template LV) at a time; a second caller waits,
// reads the report again and finds the finished template. The room check for
// what the build and the clone promise runs before the first write.
func (t ThinPools) ensureTemplate(pool vmtool.ThinPool, src cloneSource, size int64) (vmtool.Template, error) {
	final, err := vmtool.TemplateLVName(src.sha)
	if err != nil {
		return vmtool.Template{}, invalidErr("clone", "%v", err)
	}
	building, err := vmtool.TemplateBuildLVName(src.sha)
	if err != nil {
		return vmtool.Template{}, invalidErr("clone", "%v", err)
	}
	tags, err := vmtool.TemplateTags(src.image, src.sha, vmtool.TemplateOriginImage)
	if err != nil {
		return vmtool.Template{}, invalidErr("clone", "%v", err)
	}

	unlock := t.Builds.lock(pool.VG + "/" + final)
	pool2, vols, err := t.reread(pool)
	if err != nil {
		unlock()
		return vmtool.Template{}, err
	}
	if tpl, ok := imageTemplate(vols, pool2, src.image, src.sha); ok {
		// another clone built it while this one waited
		unlock()
		if err := vmtool.CheckThinRoom(pool2, max(tpl.SizeBytes, size)); err != nil {
			return vmtool.Template{}, noRoom(err)
		}
		return tpl, nil
	}
	defer unlock()

	virtual, err := vmtool.Qcow2VirtualSize(src.cache)
	if err != nil {
		return vmtool.Template{}, invalidErr("clone", "%v", err)
	}
	// What this call promises: the template, and the clone made from it.
	if err := vmtool.CheckThinRoom(pool2, virtual+max(virtual, size)); err != nil {
		return vmtool.Template{}, noRoom(err)
	}
	if err := t.buildTemplate(pool2, building, final, src.cache, virtual, tags); err != nil {
		return vmtool.Template{}, err
	}
	// lvcreate rounds the template up to whole extents: the size to compare
	// with is the volume's own.
	pool3, vols, err := t.reread(pool2)
	if err != nil {
		return vmtool.Template{}, fmt.Errorf("template %s was built but the report could not be read: %w", final, err)
	}
	tpl, ok := imageTemplate(vols, pool3, src.image, src.sha)
	if !ok {
		return vmtool.Template{}, fmt.Errorf("template %s was built but is not in the report", final)
	}
	return tpl, nil
}

func (t ThinPools) reread(pool vmtool.ThinPool) (vmtool.ThinPool, []vmtool.ThinVolume, error) {
	pools, vols, err := t.Thin.Report()
	if err != nil {
		return vmtool.ThinPool{}, nil, err
	}
	p, ok := findPool(pools, pool.Name)
	if !ok || p.VG != pool.VG {
		return vmtool.ThinPool{}, nil, fmt.Errorf("thin pool %s is gone", pool.Name)
	}
	return p, vols, nil
}

// buildTemplate makes the template under its building name and puts it in
// place: create, import, one tag change (add the template tags, drop
// vmtool_building: atomic), rename. Once create has succeeded, any failure
// removes the building volume and returns the original error. Nothing else is
// ever removed here.
func (t ThinPools) buildTemplate(pool vmtool.ThinPool, building, final, cache string, virtual int64, tags []string) error {
	if err := t.Thin.Create(pool.VG, pool.Name, building, virtual, vmtool.ThinTagBuilding); err != nil {
		return err // nothing was made
	}
	fail := func(err error) error {
		// building is the volume this call has just created.
		if rerr := t.Thin.Remove(pool.VG, building); rerr != nil {
			return fmt.Errorf("%w (and removing the half-built volume %s failed: %v)", err, building, rerr)
		}
		return err
	}
	if err := t.Thin.Import(pool.VG, building, cache); err != nil {
		return fail(err)
	}
	changes := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		changes = append(changes, "+"+tag)
	}
	changes = append(changes, "-"+vmtool.ThinTagBuilding)
	if err := t.Thin.Tag(pool.VG, building, changes...); err != nil {
		return fail(err)
	}
	if err := t.Thin.Rename(pool.VG, building, final); err != nil {
		return fail(err)
	}
	return nil
}

// ---- define ----

// Define marks every disk that is a thin vmtool_vol volume as a block disk
// before the inner Define, so libvirt is never asked about a volume it does
// not know. A disk that is a template or a building volume is refused (the
// VM would boot the template in place). A disk that is not an LV of a thin
// pool is untouched; a helper that cannot run leaves everything as before.
func (t ThinPools) Define(cfg vmtool.VMConfig) error {
	paths := []string{cfg.DiskPath}
	for _, d := range cfg.ExtraDisks {
		paths = append(paths, d.Path)
	}
	candidate := false
	for _, p := range paths {
		if _, _, ok := vmtool.ParseThinDevPath(p); ok {
			candidate = true
		}
	}
	if !candidate || t.Thin == nil {
		return t.Manager.Define(cfg)
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("thin volumes not read; disks defined as before", "err", err)
		return t.Manager.Define(cfg)
	}
	kindOf := func(path string) (string, error) {
		vg, lv, ok := vmtool.ParseThinDevPath(path)
		if !ok {
			return "", nil
		}
		v, found := findVol(vols, vg, lv)
		switch {
		case !found:
			return "", nil
		case v.Kind == vmtool.ThinKindVol:
			return vmtool.DiskBackingBlock, nil
		default:
			return "", invalidErr("define", "disk %s is a %s volume: a VM cannot boot from a template or a volume still being built; clone it", path, v.Kind)
		}
	}
	k, err := kindOf(cfg.DiskPath)
	if err != nil {
		return err
	}
	if k != "" {
		cfg.DiskKind = k
	}
	extras := make([]vmtool.DiskDevice, len(cfg.ExtraDisks))
	copy(extras, cfg.ExtraDisks)
	for i := range extras {
		k, err := kindOf(extras[i].Path)
		if err != nil {
			return err
		}
		if k != "" {
			extras[i].Kind = k
		}
	}
	cfg.ExtraDisks = extras
	return t.Manager.Define(cfg)
}

// ---- delete ----

// blockDiskPather is the inner Manager's read of a domain's block disks.
type blockDiskPather interface {
	BlockDiskPaths(name string) ([]string, error)
}

// ownedBy reports whether LV lv is named for VM name: the boot disk <vm>, or
// an extra disk <vm>-N.
func ownedBy(lv, name string) bool {
	if lv == name {
		return true
	}
	rest, ok := strings.CutPrefix(lv, name+"-")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Delete is the inner Delete, then (unless noclone) the removal of the VM's
// thin disks: those of its block disks that are vmtool_vol volumes of a vmtool
// thin pool and named for the VM. libvirt does not know them, so its Delete
// leaves them. A template, a building volume, an untagged or thick LV, or a
// volume named for another VM is left, silently, as "not ours".
func (t ThinPools) Delete(name string, noclone bool) error {
	bp, ok := t.Manager.(blockDiskPather)
	if noclone || t.Thin == nil || !ok {
		return t.Manager.Delete(name, noclone)
	}
	paths, perr := bp.BlockDiskPaths(name)
	if err := t.Manager.Delete(name, noclone); err != nil {
		return err
	}
	if perr != nil {
		t.log().Warn("thin disks of a deleted VM not looked for", "vm", name, "err", perr)
		return nil
	}
	var mine []string
	for _, p := range paths {
		if vg, lv, ok := vmtool.ParseThinDevPath(p); ok && ownedBy(lv, name) {
			mine = append(mine, vg+"/"+lv)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		// The domain is gone and its disks may be thick LVs of a logical pool,
		// whose delete must not turn into an error because the helper is
		// missing: leave the volumes (the safe direction) and say so.
		t.log().Warn("VM deleted; its block volumes could not be checked for thin volumes and were left", "vm", name, "volumes", strings.Join(mine, ","), "err", err)
		return nil
	}
	var failed []string
	for _, m := range mine {
		vg, lv, _ := strings.Cut(m, "/")
		v, found := findVol(vols, vg, lv)
		if !found || v.Kind != vmtool.ThinKindVol { // exactly the vmtool_vol tag
			continue
		}
		if err := t.Thin.Remove(vg, lv); err != nil {
			failed = append(failed, fmt.Sprintf("%s/%s: %v", vg, lv, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("VM %s is already deleted, but removing its thin volume failed: %s", name, strings.Join(failed, "; "))
	}
	return nil
}

// ---- resize and create volume ----

// ResizeVolume on a thin volume: nothing to do when it is already at least
// that size (a clone has been extended at clone time); growing one is the
// room check and an extend (no libvirt call: create's disk is not defined
// yet). Any other path is the inner Manager's.
func (t ThinPools) ResizeVolume(volPath string, sizeBytes uint64) error {
	vg, lv, ok := vmtool.ParseThinDevPath(volPath)
	if !ok || t.Thin == nil {
		return t.Manager.ResizeVolume(volPath, sizeBytes)
	}
	pools, vols, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("thin volumes not read", "err", err)
		return t.Manager.ResizeVolume(volPath, sizeBytes)
	}
	v, found := findVol(vols, vg, lv)
	if !found {
		return t.Manager.ResizeVolume(volPath, sizeBytes)
	}
	if v.Kind != vmtool.ThinKindVol {
		return invalidErr("resize", "%s is a %s volume, not a VM disk", volPath, v.Kind)
	}
	if sizeBytes <= uint64(v.SizeBytes) {
		return nil
	}
	if sizeBytes > math.MaxInt64 {
		return invalidErr("resize", "size %d bytes is too large", sizeBytes)
	}
	pool, ok := findPool(pools, v.Pool)
	if !ok || pool.VG != v.VG {
		return invalidErr("resize", "thin volume %s is in no thin pool the helper reports", volPath)
	}
	return noRoomOp("resize", t.growThin(pool, v, int64(sizeBytes)))
}

// CreateVolume into a thin pool is not supported yet; into any other pool it
// is the inner Manager's.
func (t ThinPools) CreateVolume(pool, name string, sizeGB uint) (string, error) {
	if _, _, ok := t.thinTarget(pool); ok {
		return "", invalidErr("create volume", "extra disks on a thin pool come with add-disk support for thin pools; pool %s is a thin pool", pool)
	}
	return t.Manager.CreateVolume(pool, name, sizeGB)
}

// ---- lookups for the server ----

// ThinVolume implements app.ThinHost.
func (t ThinPools) ThinVolume(path string) (string, bool, error) {
	vg, lv, ok := vmtool.ParseThinDevPath(path)
	if !ok || t.Thin == nil {
		return "", false, nil
	}
	_, vols, err := t.Thin.Report()
	if err != nil {
		return "", false, err
	}
	if v, found := findVol(vols, vg, lv); found {
		return v.Kind, true, nil
	}
	return "", false, nil
}

// SavedTemplatePath implements app.ThinHost: the device path of the saved
// template called name, in the first pool (report order) that has one. An
// image template (whose image has left the cache, or it would have been found
// there) is stale and not an answer. A helper that cannot run is "not found".
func (t ThinPools) SavedTemplatePath(name string) (string, error) {
	notFound := fmt.Errorf("template %q not found in any thin pool: %w", name, vmtool.ErrNotFound)
	if t.Thin == nil {
		return "", notFound
	}
	pools, vols, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("saved templates not looked up", "err", err)
		return "", notFound
	}
	for _, p := range pools {
		if tpl, ok := vmtool.FindTemplate(vmtool.ThinTemplates(poolVols(vols, p)), name, ""); ok && tpl.Origin == vmtool.TemplateOriginSaved {
			return vmtool.ThinDevPath(p.VG, tpl.LV), nil
		}
	}
	return "", notFound
}

// ---- start-up ----

// CleanThinBuilding removes every vmtool_building volume the helper reports:
// builds a stopped daemon left half-made. One log line each; a helper that
// cannot run is one line and never fatal. It returns how many it removed. Run
// it only when no other daemon can be building (after binding the port).
func CleanThinBuilding(th *vmtool.Thin, log *slog.Logger) int {
	if th == nil {
		return 0
	}
	if log == nil {
		log = slog.Default()
	}
	_, vols, err := th.Report()
	if err != nil {
		log.Warn("thin building volumes not looked for at start", "err", err)
		return 0
	}
	n := 0
	for _, v := range vols {
		if v.Kind != vmtool.ThinKindBuilding {
			continue
		}
		if err := th.Remove(v.VG, v.Name); err != nil {
			log.Warn("half-built template not removed at start", "vg", v.VG, "lv", v.Name, "err", err)
			continue
		}
		log.Info("removed a half-built template at start", "vg", v.VG, "lv", v.Name)
		n++
	}
	return n
}

var (
	_ app.TemplateCloner = ThinPools{}
	_ app.ThinHost       = ThinPools{}
)
