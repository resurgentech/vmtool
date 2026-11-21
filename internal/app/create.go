package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/vmnotes"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// RemoteImage is a host's image-cache entry for one image (getImageManifest).
type RemoteImage struct {
	Name string
	// SHA256 and Size are set when the host's manifest lists the image and
	// the file is in place with the listed size.
	SHA256 string
	Size   int64
	// Foreign: a volume of this name exists that the host did not create;
	// it will not be replaced. Path names it.
	Foreign bool
	Path    string
}

// ImageStore is implemented by a Manager whose host caches base images
// uploaded from this machine (remote.Manager). CreateVM uses it to make sure
// the host has the same bytes as this machine's copy of the image.
type ImageStore interface {
	ImageManifest(name string) (RemoteImage, error)
	// UploadImage streams size bytes from r as name; sha256 is their
	// lowercase hex digest. progress, if set, is called with the bytes sent.
	UploadImage(ctx context.Context, name string, r io.Reader, size int64, sha256 string, progress func(sent int64)) error
}

// ForeignImageHint says how to get past a foreign image.
const ForeignImageHint = "vmtool never replaces a file it did not create; copy your image under another name (e.g. <name>-vmtool.qcow2) and create from that"

// foreignImageErr is the refusal for an image a host did not create.
func foreignImageErr(name, host, path string) error {
	return conflict("create", fmt.Errorf("image %q already exists on %s at %s; vmtool did not create it and will not replace it: %s", name, host, path, ForeignImageHint))
}

// FindLocalImage finds name in m's pools. On the workstation (refresh true)
// a miss refreshes the active pools once and looks again; a hypervisor
// daemon (refresh false) never refreshes pools it does not own.
func FindLocalImage(m Manager, refresh bool, name string) (string, error) {
	var fn func() error
	if r, ok := m.(interface{ RefreshPools() error }); ok && refresh {
		fn = r.RefreshPools
	}
	return vmtool.FindImage(name, m.ImagePath, fn)
}

// uploadStep is how often upload progress is reported: every 1% of the
// image, but at least every 64 MiB.
func uploadStep(total int64) int64 {
	return max(1, min(total/100, 64<<20))
}

// syncImage makes the host behind st hold this machine's copy of image:
// compare digests (the local one cached by size and mtime), upload only when
// the host's differs or is missing, and never when the host has a foreign
// volume of that name.
func (s *Service) syncImage(ctx context.Context, st ImageStore, in CreateVMInput) error {
	rem, err := st.ImageManifest(in.Image)
	if err != nil {
		return wrap("create", err)
	}
	if rem.Foreign {
		emit(in, "upload", "error", fmt.Sprintf("image %s on %s is not vmtool's", in.Image, in.Host), "")
		return foreignImageErr(in.Image, in.Host, rem.Path)
	}
	// Store mode, on a host that can fetch: the host downloads the store's
	// copy itself and this machine reads no image bytes. If the store lacks
	// the image (or cannot be asked) this machine's own copy is used as in
	// local mode.
	lacks := false
	if f, ok := st.(ImageFetcher); ok && s.Store.Mode(ctx) == ModeStore {
		handled, l, err := s.fetchViaStore(ctx, st, f, rem, in)
		if handled || err != nil {
			return err
		}
		lacks = l
	}
	local := ""
	if s.Images != nil {
		p, err := s.Images.FindImage(in.Image)
		switch {
		case err == nil:
			local = p
		case !vmtool.IsNotFound(err):
			return wrap("create", fmt.Errorf("find local image %s: %w", in.Image, err))
		}
	}
	if local == "" {
		if rem.SHA256 != "" {
			emit(in, "upload", "done", fmt.Sprintf("%s: no local copy; using %s's cached copy", in.Image, in.Host), "")
			return nil
		}
		// No local copy and no manifest entry: the host may still have a saved
		// template of that name in a thin pool, which answers with its
		// /dev/<vg>/<lv> (and nothing else does, in this branch). Only a
		// Manager that can ask (TemplateFinder) is asked.
		if tf, ok := st.(TemplateFinder); ok {
			if p, err := tf.TemplatePath(in.Image); err == nil {
				if _, _, isLV := vmtool.ParseThinDevPath(p); isLV {
					emit(in, "upload", "done", fmt.Sprintf("%s: no local copy; using %s's template (%s)", in.Image, in.Host, filepath.Base(p)), "")
					return nil
				}
			}
		}
		where := "this machine's pools or image_dir"
		if sr, ok := s.Images.(imageSearcher); ok {
			where = sr.Searched()
		}
		return &Error{Kind: KindNotFound, Op: "create", Err: fmt.Errorf("image %q is neither in %s nor cached on %s: %w", in.Image, where, in.Host, vmtool.ErrNotFound)}
	}
	cachePath := s.ImageCachePath
	if cachePath == "" {
		cachePath = vmtool.ImageManifestFile
	}
	info, err := vmtool.LocalImageInfo(vmtool.OpenImageManifest(cachePath), in.Image, local, func() {
		in.OnProgress.emitEvent(ProgressEvent{Stage: "upload", Status: "hash", Detail: "hashing " + local})
	})
	if err != nil {
		return wrap("create", fmt.Errorf("hash local image %s: %w", local, err))
	}
	if rem.SHA256 == info.SHA256 && rem.Size == info.Size {
		emit(in, "upload", "done", fmt.Sprintf("%s is up to date on %s", in.Image, in.Host), "")
		return nil
	}

	f, err := os.Open(local)
	if err != nil {
		return wrap("create", err)
	}
	defer f.Close()
	total := info.Size
	if lacks {
		s.noteUpload(in, fmt.Sprintf("%s is not in the store; to share it, run: "+StoreImageHint, in.Image, in.Image))
	}
	emit(in, "upload", "start", fmt.Sprintf("uploading %s to %s (%d bytes)", in.Image, in.Host, total), "")
	step, next := uploadStep(total), int64(0)
	progress := func(sent int64) {
		if sent >= next || sent == total {
			next = sent + step
			in.OnProgress.emitEvent(ProgressEvent{Stage: "upload", Status: "progress",
				Detail: fmt.Sprintf("%d/%d bytes", sent, total), Sent: sent, Total: total})
		}
	}
	if err := st.UploadImage(ctx, in.Image, f, total, info.SHA256, progress); err != nil {
		emit(in, "upload", "error", err.Error(), "")
		var ae *Error
		if errors.As(err, &ae) {
			return wrap("create", err)
		}
		return badGateway("create", err)
	}
	emit(in, "upload", "done", fmt.Sprintf("uploaded %s to %s", in.Image, in.Host), "")
	return nil
}

func (s *Service) CreateVM(ctx context.Context, m Manager, in CreateVMInput) (*vmtool.VMInfo, error) {
	in.OnProgress = s.stamped(in.OnProgress)
	if in.Name == "" || in.Image == "" {
		return nil, invalid("create", fmt.Errorf("name and image are required"))
	}
	ref := VMRef{Host: in.Host, Name: in.Name}
	if err := validRef("create", ref); err != nil {
		return nil, err
	}
	if s.NetworkHome != nil && s.NetworkHome(in.Host) {
		return nil, conflict("create", fmt.Errorf("host %s is a network home (vms: false in config.yml) and runs no VMs; create the VM on another host", in.Host))
	}
	if in.NetType != "" && in.NetType != "nat" && in.NetType != "bridge" && in.NetType != "direct" {
		return nil, invalid("create", fmt.Errorf("invalid net_type %q", in.NetType))
	}
	if !vmtool.ValidFirmware(in.Firmware) {
		return nil, invalid("create", fmt.Errorf("invalid firmware %q: want efi or bios", in.Firmware))
	}
	if !vmtool.ValidDiskCache(in.DiskCache) {
		return nil, invalid("create", fmt.Errorf("invalid disk cache %q: want none, writeback, writethrough or unsafe", in.DiskCache))
	}
	if err := vmtool.CheckExtraVars(in.ExtraVars); err != nil {
		return nil, invalid("create", err)
	}
	if err := vmnotes.ValidateComment(in.Comment); err != nil {
		return nil, invalid("create", err)
	}
	if in.NoWait && in.Playbook != "" {
		return nil, invalid("create", fmt.Errorf("--no-wait cannot be combined with --playbook: a playbook needs a guest vmtool can reach"))
	}
	if vmtool.IsMachine(in.Name) {
		return nil, conflict("create", fmt.Errorf("%q is registered as a metal/alien machine", in.Name))
	}
	// A comment is kept in the store only: with no store in use it is refused
	// here, before anything is created (and before any Manager call).
	if in.Comment != "" {
		if _, err := s.Store.requireStore(ctx); err != nil {
			return nil, commentNeedsStore("create", err)
		}
	}

	// Requested cards on vmtool networks are checked now, before anything is
	// created: a network that does not exist or is not vmtool's, a taken or
	// outside address, a network named twice.
	seenNet := map[string]bool{}
	sites := map[string]*cardSite{}
	defer func() {
		for _, cs := range sites {
			cs.close()
		}
	}()
	for _, r := range in.Networks {
		if seenNet[r.Network] {
			return nil, invalid("create", fmt.Errorf("network %q is named twice; one card per network", r.Network))
		}
		seenNet[r.Network] = true
		cs, err := s.checkNetwork(ctx, "create", m, ref.Host, in.Name, r)
		if err != nil {
			return nil, err
		}
		sites[r.Network] = cs
	}

	// The vmexp- prefix belongs to export volumes in the array's volume group;
	// no pool takes a VM named like one (a libvirt pool over that group would
	// list the volume as a disk of that name).
	if strings.HasPrefix(in.Name, vmtool.ExportVolPrefix) {
		return nil, invalid("create", fmt.Errorf("a VM cannot be named %q: the %s prefix is for template export volumes", in.Name, vmtool.ExportVolPrefix))
	}

	// A thin pool takes neither --noclone (a thin disk is always a clone of a
	// template) nor a VM named like a template. Whether a named pool is thin
	// is only known from the pool list, which is asked here, before anything
	// changes, only when one of those could apply (a create that cannot is
	// not given an extra call). The daemon's Define and clone refuse the same
	// when the pool is the host's default, which is not known here.
	var pools []vmtool.PoolInfo
	listed := false
	if strings.HasPrefix(in.Name, "vmtpl-") || (in.Noclone && in.Pool != "") {
		var err error
		if pools, err = m.ListPools(); err != nil {
			return nil, wrap("create", fmt.Errorf("list storage pools: %w", err))
		}
		listed = true
		if in.Noclone && in.Pool != "" && poolIsThin(pools, in.Pool) {
			return nil, invalid("create", fmt.Errorf("--noclone cannot be used with thin pool %q: a thin disk is always a clone of a template", in.Pool))
		}
		if strings.HasPrefix(in.Name, "vmtpl-") && in.Pool != "" && poolIsThin(pools, in.Pool) {
			return nil, invalid("create", fmt.Errorf("a VM on thin pool %q cannot be named %q: the vmtpl- prefix is for templates", in.Pool, in.Name))
		}
	}

	// A host that caches images (a remote daemon) gets this machine's copy
	// first; a local libvirt host already has its own pools.
	imageSHA := ""
	if st, ok := m.(ImageStore); ok {
		if err := s.syncImage(ctx, st, in); err != nil {
			return nil, err
		}
		imageSHA = hostImageSHA(st, in.Image)
	}

	diskPath, err := m.ImagePath(in.Image)
	if err != nil {
		return nil, wrap("create", err)
	}
	cfg := vmtool.DefaultConfig(in.Name, diskPath)
	if in.VCPUs > 0 {
		cfg.VCPUs = in.VCPUs
	}
	if in.Memory > 0 {
		cfg.Memory = in.Memory
	}
	cfg.DiskSizeGB = in.DiskSizeGB
	// An empty pool stays empty: the host's default, resolved by the host.
	cfg.Pool = in.Pool
	if in.NetType != "" {
		cfg.Network.Type = vmtool.NetworkType(in.NetType)
	}
	if in.NetSource != "" {
		cfg.Network.Source = in.NetSource
	}
	if in.MacvtapMode != "" {
		cfg.Network.MacvtapMode = vmtool.MacvtapMode(in.MacvtapMode)
	}
	if in.SSHUser != "" {
		cfg.SSHUser = in.SSHUser
	}
	if in.SSHPass != "" {
		cfg.SSHPass = in.SSHPass
	}
	cfg.Noclone = in.Noclone
	if in.Firmware != "" {
		cfg.Firmware = in.Firmware // else DefaultConfig's efi
	}
	// Resolved here and sent explicitly to Define, so the host never guesses.
	// unsafe on a --noclone base image would risk the image itself.
	// A clone's default depends on the pool it lands in (cloneCacheDefault);
	// the pools are listed before cloning so a failure leaves no orphan volume.
	cfg.DiskCache = in.DiskCache
	if cfg.DiskCache == "" && !in.Noclone && !listed {
		var err error
		if pools, err = m.ListPools(); err != nil {
			return nil, wrap("create", fmt.Errorf("list storage pools: %w", err))
		}
		listed = true
	}
	cfg.ExtraDiskSizeGB = in.ExtraDiskSizeGB
	cfg.ExtraDiskPool = in.ExtraDiskPool
	// An extra disk on a thin pool is not supported yet: refuse it before the
	// clone makes a volume that would be left behind. (Known only when the
	// pools were listed anyway and the pool is named.)
	if listed && cfg.ExtraDiskSizeGB > 0 {
		for _, p := range []string{cfg.ExtraDiskPool, cfg.Pool} {
			if p != "" && poolIsThin(pools, p) {
				return nil, invalid("create", fmt.Errorf("an extra disk cannot be created on thin pool %q yet: extra disks on a thin pool come with add-disk support", p))
			}
			if p == cfg.ExtraDiskPool && p != "" {
				break // an explicit extra pool decides; Pool is only its default
			}
		}
	}

	if !in.Noclone {
		// A fresh clone only has image-default users; drop creds left by a
		// previous VM of the same name on this host.
		// In store mode the store's record goes too.
		if err := s.forgetCreds(ctx, ref, nil); err != nil {
			return nil, wrap("create", err)
		}
		emit(in, "clone", "start", "cloning image", "")
		var cloned string
		var err error
		size := uint64(cfg.DiskSizeGB) * 1024 * 1024 * 1024
		if tc, ok := m.(TemplateCloner); ok {
			// the image name too: a thin pool finds the image's template by it
			cloned, err = tc.CloneImageFrom(in.Image, cfg.DiskPath, cfg.Name, cfg.Pool, size)
		} else {
			cloned, err = m.CloneImage(cfg.DiskPath, cfg.Name, cfg.Pool, size)
		}
		if err != nil {
			emit(in, "clone", "error", err.Error(), "")
			return nil, wrap("create", err)
		}
		cfg.DiskPath = cloned
		if cfg.DiskCache == "" {
			cfg.DiskCache = cloneCacheDefault(pools, cloned)
		}
		emit(in, "clone", "done", fmt.Sprintf("cloned → %s", filepath.Base(cloned)), "")
	}

	if cfg.DiskSizeGB > 0 {
		emit(in, "resize", "start", fmt.Sprintf("resizing disk to %dGB", cfg.DiskSizeGB), "")
		if err := m.ResizeVolume(cfg.DiskPath, uint64(cfg.DiskSizeGB)*1024*1024*1024); err != nil {
			emit(in, "resize", "error", err.Error(), "")
			return nil, wrap("create", err)
		}
		emit(in, "resize", "done", fmt.Sprintf("disk resized to %dGB", cfg.DiskSizeGB), "")
	}

	if cfg.ExtraDiskSizeGB > 0 {
		emit(in, "extra_disk", "start", fmt.Sprintf("creating extra disk %dGB", cfg.ExtraDiskSizeGB), "")
		pool := cfg.ExtraDiskPool
		if pool == "" {
			pool = cfg.Pool
		}
		path, err := m.CreateVolume(pool, cfg.Name+"-2.qcow2", cfg.ExtraDiskSizeGB)
		if err != nil {
			emit(in, "extra_disk", "error", err.Error(), "")
			return nil, wrap("create", err)
		}
		cfg.ExtraDisks = []vmtool.DiskDevice{{Path: path, Target: "vdb"}}
		emit(in, "extra_disk", "done", fmt.Sprintf("extra disk %s → vdb", path), "")
	}

	// What the VM was made from, recorded in the store (not in the domain)
	// once the host has given the domain its UUID.
	notes := createNotes{in: in, ref: ref, pools: pools, cloned: cfg.DiskPath, sha: imageSHA, created: s.now()}
	emit(in, "start", "start", "defining and starting", "")
	if err := m.Define(cfg); err != nil {
		emit(in, "start", "error", err.Error(), "")
		return nil, wrap("create", err)
	}
	// The extra cards go in while the VM is shut off, each with its reservation
	// made first, so names and addresses exist from the first boot. A failure
	// takes back the cards already made (and their reservations) and leaves the
	// VM defined, not started.
	var cards []vmtool.NetAttachment
	// The reservations carry the VM's UUID (store mode), which the host gave the
	// domain at Define.
	cardUUID := ""
	if len(in.Networks) > 0 && s.storeMode(ctx) {
		if info, ierr := m.Info(cfg.Name); ierr == nil && info != nil {
			cardUUID = info.UUID
		}
	}
	for _, r := range in.Networks {
		emit(in, "network", "start", "attaching a card on network "+r.Network, "")
		att, err := s.attachCard(ctx, sites[r.Network], cfg.Name, cardUUID, r.Network, r.IP)
		if err != nil {
			for i := len(cards) - 1; i >= 0; i-- {
				if uerr := s.detachCard(ctx, sites[cards[i].Network], cfg.Name, cards[i].Network, cards[i].MAC); uerr != nil {
					err = fmt.Errorf("%w; and the card %s on network %q could not be taken back: %v", err, cards[i].MAC, cards[i].Network, uerr)
				}
			}
			err = fmt.Errorf("%s is defined but not started: %w", ref, err)
			emit(in, "network", "error", err.Error(), "")
			return nil, netErr("create", err)
		}
		cards = append(cards, *att)
		emit(in, "network", "done", fmt.Sprintf("%s %s %s", att.Network, att.MAC, att.IP), "")
	}
	if err := m.Start(cfg.Name); err != nil {
		emit(in, "start", "error", err.Error(), "")
		return nil, wrap("create", err)
	}
	emit(in, "start", "done", "started", "")

	// The inventory needs no address (ansible_host is host/vm through the
	// tunnel), so it exists in every outcome where the VM is running.
	if s.InventoryPath != "" {
		emit(in, "write_inventory", "start", "writing inventory", "")
		if err := s.WriteInventory(ref, s.InventoryPath, vmtool.Auth{User: cfg.SSHUser, Password: cfg.SSHPass}); err != nil {
			emit(in, "write_inventory", "error", err.Error(), "")
			info, _ := m.Info(cfg.Name) // best effort: the domain exists
			if info != nil {
				s.recordNotes(ctx, m, notes, info.UUID)
			}
			return info, wrap("create", err)
		}
		emit(in, "write_inventory", "done", "inventory written", "")
	}

	if in.NoWait {
		// No address, no SSH: nothing here can grow the filesystem.
		if in.DiskSizeGB > 0 {
			emit(in, "grow_disk", "done", "not growing the filesystem (--no-wait); cloud-init grows it at first boot on images built since C3", "")
		}
		info, err := m.Info(cfg.Name)
		if err != nil {
			return nil, wrap("create", err)
		}
		s.recordNotes(ctx, m, notes, info.UUID)
		return info, nil
	}

	emit(in, "wait_ip", "start", "waiting for IP", "")
	ipTimeout := s.ipTimeout()
	ip, ipErr := m.WaitForIP(cfg.Name, ipTimeout)
	info, infoErr := m.Info(cfg.Name)
	if infoErr != nil {
		return nil, wrap("create", infoErr)
	}
	s.recordNotes(ctx, m, notes, info.UUID)
	if ipErr != nil {
		// The VM stays defined and running; the caller decides.
		err := fmt.Errorf("%s got no address within %s (%w); the VM exists and is running: delete it, or create with --no-wait for a guest whose address vmtool cannot see", ref, ipTimeout, ipErr)
		if cfg.Firmware == vmtool.FirmwareEFI {
			err = fmt.Errorf("%w; %s", err, vmtool.UEFIHint)
		}
		emit(in, "wait_ip", "error", err.Error(), "")
		return info, &Error{Kind: KindInternal, Op: "wait_ip", Err: err}
	}
	emit(in, "wait_ip", "done", "IP "+ip, "")

	note := s.noter(in.OnProgress)
	if in.Noclone {
		// The disk was kept, so a folder from before may be this VM's.
		s.prepareCreds(ctx, ref, info.UUID, note)
	}
	auth := vmtool.ResolveVMAuth(ref.Host, ref.Name, vmtool.Auth{User: cfg.SSHUser, Password: cfg.SSHPass})
	credsDir, err := vmtool.AbsCredsDir(ref.Host, ref.Name)
	if err != nil {
		return info, wrap("create", err)
	}

	// An address is not sshd: wait for the guest to accept SSH, on every
	// create. The grow and the playbook then run without a second wait.
	emit(in, "wait_ssh", "start", "waiting for SSH", "")
	invPath, cleanup, err := vmtool.TempVMInventory(s.Tunnel, ref.Host, ref.Name, auth, credsDir)
	if err != nil {
		emit(in, "wait_ssh", "error", err.Error(), "")
		return info, wrap("create", err)
	}
	sshTimeout := s.sshTimeout()
	out, err := vmtool.WaitSSH(s.Tunnel, invPath, sshTimeout)
	cleanup()
	if err != nil {
		err = fmt.Errorf("%s did not accept SSH within %s (%w); the VM exists and is running: delete it, or create with --no-wait for a guest vmtool cannot reach by SSH", ref, sshTimeout, err)
		emit(in, "wait_ssh", "error", err.Error(), out)
		return info, &Error{Kind: KindInternal, Op: "wait_ssh", Err: err, Output: out}
	}
	emit(in, "wait_ssh", "done", "SSH is up", "")

	if in.DiskSizeGB > 0 {
		emit(in, "grow_disk", "start", "growing filesystem", "")
		invPath, cleanup, err := vmtool.TempVMInventory(s.Tunnel, ref.Host, ref.Name, auth, credsDir)
		if err != nil {
			emit(in, "grow_disk", "error", err.Error(), "")
			return info, wrap("create", err)
		}
		out, err := vmtool.GrowDisk(s.Tunnel, invPath) // once: no blind retry
		cleanup()
		if err != nil {
			emit(in, "grow_disk", "error", err.Error(), out)
			return info, &Error{Kind: KindInternal, Op: "grow_disk", Err: err, Output: out}
		}
		emit(in, "grow_disk", "done", "filesystem grown", "")
	}

	if in.Playbook != "" {
		pb, err := s.resolvePlaybook(in.Playbook)
		if err != nil {
			return info, err
		}
		emit(in, "playbook", "start", "running "+in.Playbook, "")
		invPath, cleanup, err := vmtool.TempVMInventory(s.Tunnel, ref.Host, ref.Name, auth, credsDir)
		if err != nil {
			emit(in, "playbook", "error", err.Error(), "")
			return info, wrap("create", err)
		}
		before := credSnapshot(ref)
		out, err := vmtool.RunPlaybook(s.Tunnel, invPath, pb, withDeviceID(cfg.Name, in.ExtraVars))
		cleanup()
		// setup_bot.yml writes the folder: hand it to the store after a failed
		// run as well as a successful one.
		s.handOffCreds(ctx, ref, info.UUID, before, note)
		if err != nil {
			emit(in, "playbook", "error", err.Error(), out)
			return info, &Error{Kind: KindInternal, Op: "playbook", Err: err, Output: out}
		}
		emit(in, "playbook", "done", in.Playbook+" done", out)

		emit(in, "reboot", "start", "rebooting", "")
		if err := m.Reboot(cfg.Name); err != nil {
			emit(in, "reboot", "error", err.Error(), "")
			return info, wrap("create", err)
		}
		emit(in, "reboot", "done", "rebooted", "")
	}

	info, err = m.Info(cfg.Name)
	if err != nil {
		return nil, wrap("create", err)
	}
	return info, nil
}

// poolIsThin reports whether the pool named name in pools is a thin pool.
func poolIsThin(pools []vmtool.PoolInfo, name string) bool {
	for _, p := range pools {
		if p.Name == name {
			return p.Type == vmtool.PoolTypeThin
		}
	}
	return false
}

// cloneCacheDefault is the cache mode for a new clone when none was asked
// for: none for an LV in a logical or thin pool (the host's O_DIRECT path, no
// double caching), unsafe (C10) for anything else. The clone's pool is the one
// whose path is the directory of clonedPath (a thin pool's path /dev/<vg> may
// equal a logical pool's; both give none).
func cloneCacheDefault(pools []vmtool.PoolInfo, clonedPath string) string {
	dir := filepath.Dir(filepath.Clean(clonedPath))
	for _, p := range pools {
		if (p.Type == "logical" || p.Type == vmtool.PoolTypeThin) && p.Path != "" && filepath.Clean(p.Path) == dir {
			return vmtool.DiskCacheNone
		}
	}
	return vmtool.DiskCacheUnsafe
}

// hostImageSHA is the SHA-256 the host's manifest lists for image after the
// sync: the digest of the bytes the clone is made from. Best effort: "" when
// the host cannot say (no entry, a template-only host, an error).
func hostImageSHA(st ImageStore, image string) string {
	rem, err := st.ImageManifest(image)
	if err != nil || rem.Foreign {
		return ""
	}
	return rem.SHA256
}

// originOf is what a create records about the VM in the store: the image,
// its digest when known, how the disk was made and where, and when. kind and
// pool come from the clone's path against the host's pool list: a directory
// that is a thin pool's path is a thin snapshot, one that is a logical pool's
// (or any /dev path) a thick LV, anything else a file. Without a clone
// (noclone) the image is booted in place: kind none, no pool. The template of
// a thin snapshot is filled in by recordNotes, which can ask the host.
func originOf(in CreateVMInput, pools []vmtool.PoolInfo, cloned, sha string, now time.Time) *vmnotes.Origin {
	o := &vmnotes.Origin{Image: in.Image, ImageSHA256: sha, Created: now.UTC()}
	if in.Noclone {
		o.CloneKind = vmnotes.CloneKindNone
		return o
	}
	o.Pool = in.Pool
	o.CloneKind = vmnotes.CloneKindFile
	if strings.HasPrefix(cloned, "/dev/") {
		o.CloneKind = vmnotes.CloneKindBlock
	}
	dir := filepath.Dir(filepath.Clean(cloned))
	for _, p := range pools {
		if p.Path == "" || filepath.Clean(p.Path) != dir {
			continue
		}
		if p.Type == vmtool.PoolTypeThin {
			o.CloneKind, o.Pool = vmnotes.CloneKindThin, p.Name
			break
		}
		if o.Pool == "" {
			o.Pool = p.Name
		}
	}
	return o
}
