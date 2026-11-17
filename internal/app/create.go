package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

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
	local := ""
	if s.LocalImages != nil {
		p, err := s.LocalImages(in.Image)
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
		return &Error{Kind: KindNotFound, Op: "create", Err: fmt.Errorf("image %q is neither in this machine's pools nor cached on %s: %w", in.Image, in.Host, vmtool.ErrNotFound)}
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
	if in.Name == "" || in.Image == "" {
		return nil, invalid("create", fmt.Errorf("name and image are required"))
	}
	ref := VMRef{Host: in.Host, Name: in.Name}
	if err := validRef("create", ref); err != nil {
		return nil, err
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
	if in.NoWait && in.Playbook != "" {
		return nil, invalid("create", fmt.Errorf("--no-wait cannot be combined with --playbook: a playbook needs a guest vmtool can reach"))
	}
	if vmtool.IsMachine(in.Name) {
		return nil, conflict("create", fmt.Errorf("%q is registered as a metal/alien machine", in.Name))
	}

	// A host that caches images (a remote daemon) gets this machine's copy
	// first; a local libvirt host already has its own pools.
	if st, ok := m.(ImageStore); ok {
		if err := s.syncImage(ctx, st, in); err != nil {
			return nil, err
		}
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
	cfg.DiskCache = in.DiskCache
	if cfg.DiskCache == "" && !in.Noclone {
		cfg.DiskCache = vmtool.DiskCacheUnsafe
	}
	cfg.ExtraDiskSizeGB = in.ExtraDiskSizeGB
	cfg.ExtraDiskPool = in.ExtraDiskPool

	if !in.Noclone {
		// A fresh clone only has image-default users; drop creds left by a
		// previous VM of the same name on this host.
		if err := vmtool.ForgetVM(ref.Host, ref.Name); err != nil {
			return nil, wrap("create", err)
		}
		emit(in, "clone", "start", "cloning image", "")
		cloned, err := m.CloneImage(cfg.DiskPath, cfg.Name, cfg.Pool)
		if err != nil {
			emit(in, "clone", "error", err.Error(), "")
			return nil, wrap("create", err)
		}
		cfg.DiskPath = cloned
		emit(in, "clone", "done", fmt.Sprintf("cloned → %s.qcow2", cfg.Name), "")
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

	emit(in, "start", "start", "defining and starting", "")
	if err := m.Define(cfg); err != nil {
		emit(in, "start", "error", err.Error(), "")
		return nil, wrap("create", err)
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
		return info, nil
	}

	emit(in, "wait_ip", "start", "waiting for IP", "")
	ipTimeout := s.ipTimeout()
	ip, ipErr := m.WaitForIP(cfg.Name, ipTimeout)
	info, infoErr := m.Info(cfg.Name)
	if infoErr != nil {
		return nil, wrap("create", infoErr)
	}
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
		out, err := vmtool.RunPlaybook(s.Tunnel, invPath, pb, withDeviceID(cfg.Name, in.ExtraVars))
		cleanup()
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
