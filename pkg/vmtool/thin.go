package vmtool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Thin primitives: the client of the root-owned LVM helper
// (deploy/vmtool-thin) and the pure decisions around it (parsing its report,
// template tags, the overcommit rule, stale templates). Nothing here talks to
// libvirt or decides policy beyond those rules; internal/app wires it up.

// DefaultThinHelper is where `just deploy` installs the helper.
const DefaultThinHelper = "/usr/local/libexec/vmtool-thin"

// Tags the helper knows (it acts only on LVs carrying one of the three kinds).
const (
	ThinTagPool     = "vmtool_thinpool"
	ThinTagVol      = "vmtool_vol"
	ThinTagTemplate = "vmtool_template"
	ThinTagBuilding = "vmtool_building"

	thinTagNamePrefix   = "vmtool_name="
	thinTagSHA256Prefix = "vmtool_sha256="
	thinTagOriginPrefix = "vmtool_origin="
)

// ThinVolume kinds.
const (
	ThinKindVol      = "vol"
	ThinKindTemplate = "template"
	ThinKindBuilding = "building"
)

// Template origins.
const (
	TemplateOriginImage = "image"
	TemplateOriginSaved = "saved"
)

// Overcommit limits (locked decision "Thin pools and templates", 3).
const (
	thinUsePercentLimit = 80.0
	thinOvercommitX     = 4
)

// ThinPool is a vmtool thin pool: a thin-pool LV tagged vmtool_thinpool.
// VirtualBytes is the sum of the sizes of every thin LV in the pool, vmtool's
// or not: what the pool has promised.
type ThinPool struct {
	Name         string
	VG           string
	SizeBytes    int64
	DataPercent  float64
	MetaPercent  float64
	VirtualBytes int64
}

// ThinVolume is a thin LV in a vmtool pool carrying a vmtool tag.
type ThinVolume struct {
	Name        string
	VG          string
	Pool        string
	SizeBytes   int64
	DataPercent float64 // share of the volume's size in use (0 when lvs gave none)
	Origin      string  // the LV this is a snapshot of, if any
	Tags        []string
	Kind        string // ThinKindVol, ThinKindTemplate or ThinKindBuilding
	Created     time.Time
}

// Template is a template as the daemon lists it. For an image template Name
// and Image are both the image name; for a saved one Name is the user's name
// and Image and SHA256 are empty.
type Template struct {
	Name      string
	Image     string
	SHA256    string
	Origin    string
	Pool      string
	LV        string
	SizeBytes int64
	UsedBytes int64
	Clones    int
	Created   time.Time
}

// ---- report parsing ----

type lvsReport struct {
	Report []struct {
		LV []lvsRow `json:"lv"`
	} `json:"report"`
}

type lvsRow struct {
	Name   string `json:"lv_name"`
	VG     string `json:"vg_name"`
	Attr   string `json:"lv_attr"`
	Size   string `json:"lv_size"`
	Pool   string `json:"pool_lv"`
	Origin string `json:"origin"`
	Data   string `json:"data_percent"`
	Meta   string `json:"metadata_percent"`
	Tags   string `json:"lv_tags"`
	LVTime string `json:"lv_time"`
}

func splitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func parseLVBytes(name, s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("volume %s: size %q is not a byte count", name, s)
	}
	return n, nil
}

// parseLVPercent: lvs leaves the percent empty for LVs that have none.
func parseLVPercent(name, s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("volume %s: usage %q is not a percentage", name, s)
	}
	return f, nil
}

// ParseThinReport parses the helper's `report` output (lvs JSON, or
// {"report": [{"lv": []}]} when no VG holds a pool).
//
// A pool is an LV whose attributes start with t and that carries the exact tag
// vmtool_thinpool (the helper's rule). A volume is a thin LV (attributes start
// with V) in such a pool, in the same VG, carrying vmtool_vol, vmtool_template
// or vmtool_building; its Kind is building if that tag is present, else
// template, else vol (a half-built template must never be taken for a usable
// one). Everything else (thick LVs, foreign thin LVs, hidden [..] LVs) is
// left out, except that every thin LV in a vmtool pool counts toward that
// pool's VirtualBytes. Unknown fields are ignored and empty percentages are 0;
// malformed JSON, or a size that is not an integer on a pool or a thin LV in a
// pool, is an error.
func ParseThinReport(data []byte) ([]ThinPool, []ThinVolume, error) {
	var rep lvsReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, nil, fmt.Errorf("parse thin report: %w", err)
	}
	var rows []lvsRow
	for _, r := range rep.Report {
		rows = append(rows, r.LV...)
	}

	var pools []ThinPool
	poolIdx := map[[2]string]int{} // (vg, name) -> index in pools
	for _, r := range rows {
		if strings.HasPrefix(r.Name, "[") || !strings.HasPrefix(r.Attr, "t") || !hasTag(splitTags(r.Tags), ThinTagPool) {
			continue
		}
		size, err := parseLVBytes(r.Name, r.Size)
		if err != nil {
			return nil, nil, err
		}
		data, err := parseLVPercent(r.Name, r.Data)
		if err != nil {
			return nil, nil, err
		}
		meta, err := parseLVPercent(r.Name, r.Meta)
		if err != nil {
			return nil, nil, err
		}
		poolIdx[[2]string{r.VG, r.Name}] = len(pools)
		pools = append(pools, ThinPool{Name: r.Name, VG: r.VG, SizeBytes: size, DataPercent: data, MetaPercent: meta})
	}

	var vols []ThinVolume
	for _, r := range rows {
		if strings.HasPrefix(r.Name, "[") || !strings.HasPrefix(r.Attr, "V") {
			continue
		}
		pi, ok := poolIdx[[2]string{r.VG, r.Pool}]
		if !ok {
			continue
		}
		size, err := parseLVBytes(r.Name, r.Size)
		if err != nil {
			return nil, nil, err
		}
		pools[pi].VirtualBytes += size

		tags := splitTags(r.Tags)
		kind := ""
		switch {
		case hasTag(tags, ThinTagBuilding):
			kind = ThinKindBuilding
		case hasTag(tags, ThinTagTemplate):
			kind = ThinKindTemplate
		case hasTag(tags, ThinTagVol):
			kind = ThinKindVol
		default:
			continue
		}
		data, err := parseLVPercent(r.Name, r.Data)
		if err != nil {
			return nil, nil, err
		}
		v := ThinVolume{Name: r.Name, VG: r.VG, Pool: r.Pool, SizeBytes: size, DataPercent: data,
			Origin: r.Origin, Tags: tags, Kind: kind}
		if t, err := time.Parse("2006-01-02 15:04:05 -0700", strings.TrimSpace(r.LVTime)); err == nil {
			v.Created = t
		}
		vols = append(vols, v)
	}
	return pools, vols, nil
}

// ---- overcommit ----

// ErrThinNoRoom is what CheckThinRoom's error matches with errors.Is; callers
// map it to a conflict (409).
var ErrThinNoRoom = errors.New("thin pool has no room")

// ThinRoomError is the refusal; its text gives the numbers.
type ThinRoomError struct{ Msg string }

func (e *ThinRoomError) Error() string        { return e.Msg }
func (e *ThinRoomError) Is(target error) bool { return target == ErrThinNoRoom }

func gibText(n int64) string {
	return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
}

// CheckThinRoom refuses an operation that adds addBytes of thin volume (a
// clone, a template build, an extra disk, or the growth of a resize) when the
// pool's data or metadata use is 80 % or more, or when the sizes it has
// promised plus addBytes would exceed 4 times its size. The error matches
// ErrThinNoRoom.
func CheckThinRoom(pool ThinPool, addBytes int64) error {
	if addBytes < 0 {
		return fmt.Errorf("thin pool %s: cannot add a negative size (%d)", pool.Name, addBytes)
	}
	who := fmt.Sprintf("thin pool %s (%s) has no room", pool.Name, pool.VG)
	if pool.DataPercent >= thinUsePercentLimit {
		return &ThinRoomError{fmt.Sprintf("%s: data use is %.2f%%, at or above the %.0f%% limit",
			who, pool.DataPercent, thinUsePercentLimit)}
	}
	if pool.MetaPercent >= thinUsePercentLimit {
		return &ThinRoomError{fmt.Sprintf("%s: metadata use is %.2f%%, at or above the %.0f%% limit",
			who, pool.MetaPercent, thinUsePercentLimit)}
	}
	limit := pool.SizeBytes * thinOvercommitX
	if pool.VirtualBytes+addBytes > limit {
		return &ThinRoomError{fmt.Sprintf("%s: %s (%d bytes) already promised plus %s (%d bytes) is %s (%d bytes), over the limit of %dx the pool size %s (%d bytes) = %s (%d bytes)",
			who,
			gibText(pool.VirtualBytes), pool.VirtualBytes,
			gibText(addBytes), addBytes,
			gibText(pool.VirtualBytes+addBytes), pool.VirtualBytes+addBytes,
			thinOvercommitX, gibText(pool.SizeBytes), pool.SizeBytes,
			gibText(limit), limit)}
	}
	return nil
}

// ---- templates ----

var (
	thinNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	// The helper's tag rule.
	thinTagRE = regexp.MustCompile(`^vmtool_[a-z0-9_]+(=[A-Za-z0-9._:-]+)?$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexIDRE   = regexp.MustCompile(`^[0-9a-fA-F]{12,}$`)
)

// TemplateLVName is the LV name of a template: vmtpl- and the first 12
// lowercase hex characters of its SHA-256 (image template) or random id
// (saved template).
func TemplateLVName(shaOrID string) (string, error) {
	if !hexIDRE.MatchString(shaOrID) {
		return "", fmt.Errorf("template id %q is not at least 12 hex characters", shaOrID)
	}
	return "vmtpl-" + strings.ToLower(shaOrID[:12]), nil
}

// TemplateTags is the tag list of a new template LV. An image template needs
// the image's SHA-256 (64 lowercase hex); a saved one may omit it. name is an
// image name or the user's name for a saved template (the same rule as image
// and VM names).
func TemplateTags(name, sha, origin string) ([]string, error) {
	if !ValidImageName(name) || !thinNameRE.MatchString(name) {
		return nil, fmt.Errorf("template name %q is not a valid name", name)
	}
	switch origin {
	case TemplateOriginImage:
		if sha == "" {
			return nil, fmt.Errorf("template %s: an image template needs the image's SHA-256", name)
		}
	case TemplateOriginSaved:
	default:
		return nil, fmt.Errorf("template %s: origin %q is not %s or %s", name, origin, TemplateOriginImage, TemplateOriginSaved)
	}
	if sha != "" && !sha256RE.MatchString(sha) {
		return nil, fmt.Errorf("template %s: SHA-256 %q is not 64 lowercase hex characters", name, sha)
	}
	tags := []string{ThinTagTemplate, thinTagNamePrefix + name}
	if sha != "" {
		tags = append(tags, thinTagSHA256Prefix+sha)
	}
	tags = append(tags, thinTagOriginPrefix+origin)
	for _, t := range tags {
		if !thinTagRE.MatchString(t) {
			return nil, fmt.Errorf("template %s: tag %q is not an allowed tag", name, t)
		}
	}
	return tags, nil
}

func tagValue(tags []string, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return t[len(prefix):]
		}
	}
	return ""
}

// TemplateFromVolume reads a template out of its LV's tags. all is every
// volume of the report: Clones counts the volumes in the same VG whose
// Origin is this LV, and UsedBytes is the volume's usage times its size.
// A volume that is not a template, or whose tags are incomplete or invalid, is
// an error.
func TemplateFromVolume(v ThinVolume, all []ThinVolume) (Template, error) {
	if v.Kind != ThinKindTemplate {
		return Template{}, fmt.Errorf("volume %s is not a template", v.Name)
	}
	name := tagValue(v.Tags, thinTagNamePrefix)
	sha := tagValue(v.Tags, thinTagSHA256Prefix)
	origin := tagValue(v.Tags, thinTagOriginPrefix)
	if _, err := TemplateTags(name, sha, origin); err != nil {
		return Template{}, fmt.Errorf("volume %s: %w", v.Name, err)
	}
	t := Template{
		Name: name, SHA256: sha, Origin: origin, Pool: v.Pool, LV: v.Name,
		SizeBytes: v.SizeBytes, UsedBytes: int64(math.Round(float64(v.SizeBytes) * v.DataPercent / 100)),
		Created: v.Created,
	}
	if origin == TemplateOriginImage {
		t.Image = name
	}
	for _, o := range all {
		if o.VG == v.VG && o.Origin == v.Name && o.Name != v.Name {
			t.Clones++
		}
	}
	return t, nil
}

// ThinTemplates lists the usable templates among the volumes, in report
// order. A template LV whose tags are broken is left out (it can neither be
// found nor cloned); building volumes are not templates yet.
func ThinTemplates(vols []ThinVolume) []Template {
	var out []Template
	for _, v := range vols {
		if v.Kind != ThinKindTemplate {
			continue
		}
		if t, err := TemplateFromVolume(v, vols); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// ImageSHAs is the name to SHA-256 view of an image manifest that
// StaleTemplates takes.
func ImageSHAs(entries map[string]ImageEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for name, e := range entries {
		out[name] = e.SHA256
	}
	return out
}

// StaleTemplates returns the image templates whose (image, SHA-256) is not the
// current manifest entry (name to SHA-256) for that image; an image absent from
// the manifest is stale. Saved templates are never stale.
func StaleTemplates(templates []Template, current map[string]string) []Template {
	var out []Template
	for _, t := range templates {
		if t.Origin != TemplateOriginImage {
			continue
		}
		if sha, ok := current[t.Image]; !ok || sha == "" || sha != t.SHA256 {
			out = append(out, t)
		}
	}
	return out
}

// FindTemplate picks the template to clone for name. With a SHA-256 it is the
// image template of that image and exactly that SHA-256 (never an older or
// newer one). Otherwise, or when no image template matches, it is the saved
// template of that name. An image template never matches an empty sha. The
// first match in list order wins.
func FindTemplate(templates []Template, name, sha string) (Template, bool) {
	if name == "" {
		return Template{}, false
	}
	if sha != "" {
		for _, t := range templates {
			if t.Origin == TemplateOriginImage && t.Image == name && t.SHA256 == sha {
				return t, true
			}
		}
	}
	for _, t := range templates {
		if t.Origin == TemplateOriginSaved && t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// ---- helper client ----

// Thin runs the root-owned helper through sudo -n. The zero value uses
// DefaultThinHelper and the sudo on PATH. It carries no password and no stdin.
type Thin struct {
	Helper string // helper path; default DefaultThinHelper
}

func (t *Thin) helper() string {
	if t == nil || t.Helper == "" {
		return DefaultThinHelper
	}
	return t.Helper
}

// run executes `sudo -n <helper> <verb> <args...>` and returns stdout. On
// failure the error is the verb and the helper's stderr on one line.
func (t *Thin) run(verb string, args ...string) ([]byte, error) {
	cmd, err := command("sudo", append([]string{"-n", t.helper(), verb}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("vmtool-thin %s: %w", verb, err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		msg = strings.TrimPrefix(msg, "vmtool-thin: ")
		msg = strings.Join(strings.Fields(strings.ReplaceAll(msg, "\n", " ; ")), " ")
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("vmtool-thin %s: %s", verb, msg)
	}
	return stdout.Bytes(), nil
}

func thinCheckName(what, s string) error {
	if !thinNameRE.MatchString(s) {
		return fmt.Errorf("invalid %s %q", what, s)
	}
	return nil
}

func thinCheckBytes(n int64) error {
	if n <= 0 {
		return fmt.Errorf("size must be a positive number of bytes, got %d", n)
	}
	return nil
}

func thinCheckPath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\n\x00") {
		return fmt.Errorf("invalid path %q", p)
	}
	return nil
}

func thinCheckTags(tags []string) error {
	for _, tg := range tags {
		if !thinTagRE.MatchString(tg) {
			return fmt.Errorf("invalid tag %q", tg)
		}
	}
	return nil
}

// Report lists the pools and tagged volumes of every VG holding a pool.
func (t *Thin) Report() ([]ThinPool, []ThinVolume, error) {
	out, err := t.run("report")
	if err != nil {
		return nil, nil, err
	}
	pools, vols, err := ParseThinReport(out)
	if err != nil {
		return nil, nil, fmt.Errorf("vmtool-thin report: %w", err)
	}
	return pools, vols, nil
}

// Create makes a thin volume of bytes in the pool; tags must include one of
// vmtool_vol, vmtool_template or vmtool_building.
func (t *Thin) Create(vg, pool, name string, bytes int64, tags ...string) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("pool", pool),
		thinCheckName("volume", name), thinCheckBytes(bytes), thinCheckTags(tags)); err != nil {
		return fmt.Errorf("vmtool-thin create: %w", err)
	}
	_, err := t.run("create", append([]string{vg, pool, name, strconv.FormatInt(bytes, 10)}, tags...)...)
	return err
}

// Snapshot makes a thin snapshot of src, activated and started at boot.
func (t *Thin) Snapshot(vg, src, name string, tags ...string) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", src),
		thinCheckName("volume", name), thinCheckTags(tags)); err != nil {
		return fmt.Errorf("vmtool-thin snapshot: %w", err)
	}
	_, err := t.run("snapshot", append([]string{vg, src, name}, tags...)...)
	return err
}

// Extend grows a volume to bytes (never shrinks).
func (t *Thin) Extend(vg, name string, bytes int64) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", name), thinCheckBytes(bytes)); err != nil {
		return fmt.Errorf("vmtool-thin extend: %w", err)
	}
	_, err := t.run("extend", vg, name, strconv.FormatInt(bytes, 10))
	return err
}

// Remove deletes a tagged volume.
func (t *Thin) Remove(vg, name string) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", name)); err != nil {
		return fmt.Errorf("vmtool-thin remove: %w", err)
	}
	_, err := t.run("remove", vg, name)
	return err
}

// Rename renames a tagged volume.
func (t *Thin) Rename(vg, oldName, newName string) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", oldName),
		thinCheckName("volume", newName)); err != nil {
		return fmt.Errorf("vmtool-thin rename: %w", err)
	}
	_, err := t.run("rename", vg, oldName, newName)
	return err
}

// Tag adds and removes tags; each change is "+TAG" or "-TAG".
func (t *Thin) Tag(vg, name string, changes ...string) error {
	err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", name))
	if err == nil && len(changes) == 0 {
		err = errors.New("no tag changes")
	}
	for _, c := range changes {
		if err == nil && (len(c) < 2 || (c[0] != '+' && c[0] != '-') || !thinTagRE.MatchString(c[1:])) {
			err = fmt.Errorf("invalid tag change %q", c)
		}
	}
	if err != nil {
		return fmt.Errorf("vmtool-thin tag: %w", err)
	}
	_, err = t.run("tag", append([]string{vg, name}, changes...)...)
	return err
}

// Import writes the qcow2 file src (in the image directory) into the volume.
func (t *Thin) Import(vg, name, src string) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", name), thinCheckPath(src)); err != nil {
		return fmt.Errorf("vmtool-thin import: %w", err)
	}
	_, err := t.run("import", vg, name, src)
	return err
}

// Export writes the volume to the qcow2 file dest (a .part file in the image
// directory).
func (t *Thin) Export(vg, name, dest string) error {
	if err := firstErr(thinCheckName("volume group", vg), thinCheckName("volume", name), thinCheckPath(dest)); err != nil {
		return fmt.Errorf("vmtool-thin export: %w", err)
	}
	_, err := t.run("export", vg, name, dest)
	return err
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
