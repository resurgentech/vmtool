# UEFI images

Record of the conversion from BIOS to UEFI templates (roadmap step U4).
Nothing here has been built yet: the templates validate, but only a real
build proves they install and boot. The open items are under "Checks at
B0/B".

## What was done

- **vmtool firmware option** shipped earlier (tranche 1, step U1):
  `--firmware efi|bios`, default `efi`. The VM definition uses libvirt's
  `firmware='efi'` with secure boot off.
- **Templates converted in place.** No `_uefi` templates. Old BIOS images keep
  their names; a rebuild replaces them (new hash, re-uploaded on the next
  create). Clones are full copies and keep their BIOS definitions.
- **Every `source "qemu"`** has `efi_boot = true`, with `efi_firmware_code`
  and `efi_firmware_vars` taken from two variables defaulting to Arch's
  non-secure-boot `edk2-ovmf` files (`/usr/share/edk2/x64/OVMF_CODE.4m.fd`,
  `OVMF_VARS.4m.fd`). Other distros use other paths: set
  `PKR_VAR_efi_firmware_code` / `PKR_VAR_efi_firmware_vars`. `edk2-ovmf` must
  be installed on the build host.
- **`boot_wait`** raised from `3s` to `10s`: OVMF reaches GRUB later than
  SeaBIOS. The ISO's GRUB menu is the same in both modes, so `boot_command` is
  unchanged.
- **ext4 ISO templates** keep `layout: direct`. Under UEFI the installer puts
  an EFI system partition (ESP) where the 1 MiB BIOS boot partition was (p1);
  root is believed to stay p2.
- **btrfs templates** (`ubuntu2604_btrfs`, `ubuntu2604_btrfs_gui`): the
  explicit `storage: config:` in `http/user-data.tpl` replaces the 1M
  `bios_grub` partition with a 512M ESP as partition 1 (`flag: boot`,
  `grub_device: true`, fat32, mounted at `/boot/efi`); `grub_device` moved off
  the disk entry. Root stays partition 2, btrfs. The `/` mount is listed before
  `/boot/efi` (curtin mounts in list order).
- **`ubuntu2404_aws`**: the Ubuntu cloud image already carries both a BIOS
  boot partition and an ESP (root is p1). Building it under UEFI is just
  `efi_boot`; it stays bootable both ways.
- **Fallback loader provisioner.** The last provisioner of every template
  installs `EFI/BOOT/BOOTX64.EFI` with `grub-install --target=x86_64-efi
  --efi-directory=/boot/efi --removable --no-nvram` if the file is missing
  (tested with `sudo test -e`, because `/boot/efi` is root-only). The boot
  entry the installer writes lives in packer's NVRAM, not in the qcow2; a clone
  gets fresh NVRAM and boots only from this fallback path.
- **GRUB config edits** work the same under UEFI: GRUB reads
  `/boot/grub/grub.cfg` in both modes.
- **`just check`** fails any template without `efi_boot = true`, or whose last
  provisioner is not a shell provisioner holding the fallback-loader line.

Firmware is a property of the image: a BIOS image does not boot under UEFI and
the reverse, so the VM definition and the image must agree. vmtool's default
is now `efi`; pass `--firmware bios` for images built before this change.

## Checks at B0/B

Not proven without a real build:

- **`boot_command` timing at 10s.** If the keystrokes miss, the autoinstall
  never starts and Packer waits out the 60-minute `ssh_timeout`. Watch the VNC
  console on the first build.
- **Root is still partition 2 on ext4 `layout: direct`.** vmtool's disk grow
  assumes `/dev/vda2` (`growDiskCommand` in `../pkg/vmtool/ansible.go`).
- **The fallback loader is present** in the built image
  (`EFI/BOOT/BOOTX64.EFI` on the ESP), and a clone with fresh NVRAM boots.
- **The netplan fix still holds.** It does not depend on firmware; the smoke
  test (the VM gets a DHCP lease) is the proof.

Inspect an image without booting it (no sudo needed):

```bash
qemu-nbd --read-only --format=qcow2 --socket=/tmp/n.sock --persistent --fork output/<template>/<template>
mkdir -p /tmp/nbdmnt && nbdfuse --readonly /tmp/nbdmnt 'nbd+unix:///?socket=/tmp/n.sock' &
sfdisk -l /tmp/nbdmnt/nbd
fusermount3 -u /tmp/nbdmnt    # then kill qemu-nbd
```

Add `blkid -p --offset <start*512> /tmp/nbdmnt/nbd` to read a partition's
filesystem type.

## Related

- **Omarchy guest:** its ISO boots with UEFI in the project's own QEMU and
  Proxmox examples, and supports an unattended install from a second drive
  labelled `cidata` (the mechanism `ubuntu2404_aws` already uses via
  `cd_files`). It would probably want a `virtio` video device in place of
  `qxl`.
- **btrfs root:** separate from firmware. The grow command handles both ext4
  and btrfs.
