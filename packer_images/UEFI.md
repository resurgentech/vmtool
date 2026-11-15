# Converting to UEFI

Notes on what it would take to build and boot UEFI images. Written
2026-11-14. Nothing here has been built or tested yet; the items under
"Check after the first build" are the open risks.

## Where we are today: BIOS everywhere

- **vmtool defines BIOS guests.** The `<os>` block in
  `../pkg/vmtool/libvirt.go` (`domainXML`) has a machine type and a boot
  device, with no `firmware='efi'` attribute and no `<loader>` element.
  libvirt then starts the guest with SeaBIOS.
- **Packer installs under BIOS.** No template sets an EFI option.
- **The images are BIOS-only.** `ubuntu2604` is GPT with a 1 MiB "BIOS boot"
  partition and an ext4 root on partition 2. It has no EFI system partition.

Firmware is a property of the image: a BIOS-installed image does not boot
under UEFI, and the reverse. So the image and the VM definition must agree.

## Order of work

1. vmtool gains a firmware option (Go and spec change, below).
2. One UEFI template, built and then booted through vmtool.
3. The remaining templates.

A UEFI image is not usable from vmtool until step 1 is done.

## Step 1: vmtool

This is outside `packer_images/` and follows the repo's spec-first rule.

1. **Spec:** add a `firmware` field (`bios` | `efi`, default `bios`) to the
   create request in `../spec/openapi/vmtool.yaml`, then
   `go generate ./internal/api/...`.
2. **`VMConfig`** (`../pkg/vmtool/config.go`): add the field. The default
   stays `bios` so every existing image keeps booting.
3. **Domain XML** (`domainXML` in `../pkg/vmtool/libvirt.go`): for `efi`,
   emit

   ```xml
   <os firmware='efi'>
     <type arch='x86_64' machine='q35'>hvm</type>
     <firmware><feature enabled='no' name='secure-boot'/></firmware>
     <boot dev='hd'/>
   </os>
   ```

   libvirt picks the OVMF files from the descriptors in
   `/usr/share/qemu/firmware/` and creates a per-VM NVRAM file (the UEFI
   settings store). Secure boot is off because Omarchy needs it off and
   Ubuntu does not need it on.
4. **Undefine:** libvirt refuses to undefine a domain that has NVRAM unless
   told what to do with the file. Both `dom.Undefine()` calls in
   `libvirt.go` (`Undefine` and `Delete`) need `UndefineFlags` with the
   NVRAM flag, or `vmtool delete` fails on UEFI guests.
5. **Plumbing:** pass the option through `internal/app` and the CLI, TUI and
   REST adapters.

The caller has to pass the mode that matches the image. Detecting it from
the image (is there an EFI system partition?) could come later.

## Step 2: a Packer template

Add UEFI images as new templates, for example `ubuntu2604_uefi`, and leave
the BIOS ones alone: existing VMs were cloned from BIOS images.

In the `source "qemu"` block of `template.pkr.hcl`:

```hcl
efi_boot          = true
efi_firmware_code = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
efi_firmware_vars = "/usr/share/edk2/x64/OVMF_VARS.4m.fd"
```

- These are the non-secure-boot OVMF files from Arch's `edk2-ovmf` package.
  The paths differ on other distros, so make them variables if the build
  ever has to run elsewhere.
- **`boot_wait`:** UEFI firmware reaches the boot menu later than BIOS does.
  The current `3s` may send the keystrokes too early; expect to raise it.
- **`boot_command`:** should carry over, because Ubuntu's server ISO uses
  GRUB in both modes. Watch the VNC console on the first build. If the
  keystrokes miss, the autoinstall never starts and Packer waits out the
  60-minute `ssh_timeout`.
- **Autoinstall (`http/user-data.tpl`):** no change. `layout: direct` adapts:
  under UEFI the installer creates an EFI system partition where the BIOS
  boot partition was.

`just validate <template>` and `just check` cover the template syntax. Only a
real build proves it installs.

## Check after the first build

- **Root is still partition 2.** vmtool's disk grow runs
  `growpart /dev/vda 2 && resize2fs /dev/vda2` (`../pkg/vmtool/ansible.go`).
  Inspect the image without booting it:

  ```bash
  qemu-nbd --read-only --format=qcow2 --socket=/tmp/n.sock --persistent --fork output/<template>/<template>
  mkdir -p /tmp/nbdmnt && nbdfuse --readonly /tmp/nbdmnt 'nbd+unix:///?socket=/tmp/n.sock' &
  sfdisk -l /tmp/nbdmnt/nbd
  fusermount3 -u /tmp/nbdmnt    # then kill qemu-nbd
  ```

  No sudo is needed. Add `blkid -p --offset <start*512> /tmp/nbdmnt/nbd` to
  read a partition's filesystem type.
- **The fallback bootloader exists.** The boot entry the installer writes
  lives in the Packer build's NVRAM, which is not part of the qcow2. A VM
  cloned from the image gets fresh NVRAM and can boot only from the standard
  fallback path, `EFI/BOOT/BOOTX64.EFI` on the EFI system partition. Ubuntu
  is believed to install one there by default. If it is missing, add a
  provisioner line that runs `sudo grub-install --removable`.
- **The netplan fix still holds.** It does not depend on firmware; the smoke
  test (the VM gets a DHCP lease) is the proof.

## Alternative: one image that boots both ways

Lay the disk out with both a BIOS boot partition and an EFI system
partition, and install GRUB for both targets, as Ubuntu's own cloud images
do. Firmware then stops being something the caller must get right.

- It needs an explicit `storage: config:` list in the autoinstall file
  instead of `layout: direct`, and a second `grub-install` run.
- Root must stay on partition 2, or the grow command has to change.
- A btrfs template needs an explicit storage config anyway, so that is the
  natural place to try this.

## Related

- **Omarchy guest:** its ISO boots with UEFI in the project's own QEMU and
  Proxmox examples, and supports an unattended install from a second drive
  labelled `cidata` (the mechanism `ubuntu2404_aws` already uses via
  `cd_files`). It would need step 1 first, and probably a `virtio` video
  device in place of `qxl`.
- **btrfs root:** separate from firmware, but the grow command above is
  ext4-only; btrfs needs `btrfs filesystem resize max /`.
