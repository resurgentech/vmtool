# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Packer-based QEMU/KVM VM image builder. Builds qcow2 images and installs them into libvirt. One template per directory under `qemu_images/`:

| Template | Source | Notes |
|----------|--------|-------|
| `ubuntu2404` | 24.04 live-server ISO, subiquity autoinstall | 10G disk; default template |
| `ubuntu2404_gui` | 24.04 live-server ISO | 20G disk; ansible runs `../ansible/playbooks/install_gui.yml` (GNOME, gdm3 autologin, chromium) |
| `ubuntu2404_aws` | 24.04 (noble) cloud image, cloud-init via `cd_files` | 10G disk |
| `ubuntu2604` | 26.04.1 live-server ISO, subiquity autoinstall | 10G disk |
| `ubuntu2604_btrfs` | 26.04.1 live-server ISO, explicit `storage: config:` | 10G disk; ESP on partition 1, flat btrfs root on partition 2, no swap file |
| `ubuntu2604_btrfs_gui` | 26.04.1 live-server ISO, explicit `storage: config:` | 20G disk; `ubuntu2604_btrfs` storage (ESP p1, btrfs root p2) plus the `install_gui.yml` playbook |
| `ubuntu2604_gui` | 26.04.1 live-server ISO | 20G disk; same shared `install_gui.yml` playbook |

`just templates` lists them.

## UEFI

All templates build **UEFI** images (see `UEFI.md` for the record and open checks):

- Every `source "qemu"` sets `efi_boot = true`, `efi_firmware_code`, `efi_firmware_vars` (variables defaulting to Arch's non-secure-boot `edk2-ovmf` files `/usr/share/edk2/x64/OVMF_CODE.4m.fd` / `OVMF_VARS.4m.fd`; override with `PKR_VAR_efi_firmware_code` / `PKR_VAR_efi_firmware_vars` on other distros). `edk2-ovmf` must be installed on the build host. Secure boot stays off.
- `boot_wait = "10s"`: OVMF reaches GRUB later than SeaBIOS. `boot_command` is unchanged.
- ISO templates with `layout: direct` get an EFI system partition as p1 and root on p2. The btrfs templates declare the ESP explicitly (512M, fat32, `/boot/efi`, `grub_device`) as p1, btrfs root as p2.
- The **last** provisioner of every template is a shell provisioner that installs the fallback loader `EFI/BOOT/BOOTX64.EFI` (`grub-install --removable --no-nvram`) if missing. Clones start with fresh NVRAM, so only that path boots them. Later steps add their lines to this same provisioner.
- `just check` fails a template lacking `efi_boot = true`, or whose last provisioner is not a shell provisioner containing the BOOTX64.EFI guard and the `grub-install` fallback line. New shape checks are small `check_*` functions in the `check` recipe.
- Old BIOS images keep their names; a rebuild replaces them. There are no `_uefi` templates.

## Clean first boot (cloud-init)

Every image ends clean for cloud-init, so each clone takes its identity at first boot.

- **Why.** Clones of one image used to share the image's machine-id, hence the same DHCP client id (so the same lease and IP), the same SSH host keys and the same hostname. Ubuntu's installer also leaves cloud-init disabled (`/etc/cloud/cloud-init.disabled`, `99-installer.cfg`), so nothing re-ran at first boot.
- **`files/99-vmtool.cfg`** is installed by every template to `/etc/cloud/cloud.cfg.d/` (a `provisioner "file"` to `/tmp`, then `sudo install` in the network shell provisioner, next to `99-disable-network-config.cfg`). It names the datasource (`[NoCloud, None]`), sets `users: []` (no default `ubuntu` user), `ssh_pwauth: true` (vmtool's first login is the packer password), `preserve_hostname: false`, `manage_etc_hosts: localhost` and grows `/` (`growpart` `mode: auto`, `resize_rootfs: true`). `99-disable-network-config.cfg` and the wildcard netplan (see Network below) stay: cloud-init's network layer is off. The name sorts last on purpose: `cloud.cfg.d` merges in lexical order and the last file wins, and the cloud-init package's `90_dpkg.cfg` (which survives the clean) sets its own `datasource_list`; `90-vmtool.cfg` would sort before it and lose.
- **The last provisioner** runs, with sudo and in this order: `cloud-init status --wait || true`; strip `autoinstall` and the `ds=nocloud-net;s=...` token from `GRUB_CMDLINE_LINUX_DEFAULT` and `update-grub`; the BOOTX64.EFI guard; `apt-get clean`; empty root's shell history; delete every `127.0.1.1` line from `/etc/hosts` (`sed -i '/^127[.]0[.]1[.]1[[:space:]]/d'`); `cloud-init clean --machine-id --logs`. Nothing runs after it but packer's shutdown.
- **Why the `/etc/hosts` delete.** The installer writes `127.0.1.1<TAB><image name>`, and cloud-init's `manage_etc_hosts: localhost` appends its own `127.0.1.1 <vm> <vm>` line when that one is missing; it never replaces or removes another `127.0.1.1` line. Left in, the image name resolves to 127.0.1.1 on every clone (B0 pilot, `docs/LOG.md`). With the line gone, a clone's first boot writes the only one, naming the VM. The localhost and IPv6 lines stay. It runs just before the clean so the fewest sudo calls run after the build VM's own hostname stops resolving from `/etc/hosts`; sudo may then warn `unable to resolve host` on stderr, which does not fail the build. `ubuntu2404_aws` gets the line too; `sed` exits 0 when there is nothing to delete.
- **`ubuntu2404_aws`** is a cloud image: cloud-init runs during the build from the `cidata` CD and is no longer disabled (`touch /etc/cloud/cloud-init.disabled` is gone from the template and `user-data.tpl`). Its grub line has no packer token, so the strip does nothing.
- **At first boot of a clone**, cloud-init reads the SMBIOS serial vmtool sets (`ds=nocloud;h=<name>;i=<uuid>`) and: sets the hostname (and the `127.0.1.1` line of `/etc/hosts`), generates new SSH host keys, takes a machine-id from the domain UUID, and grows the root partition and filesystem.
- **`--noclone`** on a cleaned image boots the image itself: it uses up the image's first boot, so the image takes that VM's name, keys and machine-id. Run `sudo cloud-init clean --machine-id` in the guest before shutting it down to make it fresh again.
- `just check` fails a template whose last provisioner lacks `cloud-init clean --machine-id`, does not end on it, lacks `cloud-init status --wait` before it, lacks the exact `/etc/hosts` `127.0.1.1` delete line or has it after `cloud-init clean`, or any template that does not install `files/99-vmtool.cfg` to `/etc/cloud/cloud.cfg.d/99-vmtool.cfg`, or whose `template.pkr.hcl` or `user-data.tpl` mentions `cloud-init.disabled`. It also fails if `files/99-vmtool.cfg` is missing.

## Network (netplan)

- **`files/99-wildcard.yaml`** is the one netplan file for all seven templates: `all-en` matches `en*`, `dhcp4: true`, `dhcp-identifier: mac`. Each template takes it with a `provisioner "file"` (source `${path.root}/../../files/99-wildcard.yaml`) and `sudo install -m 0600` to `/etc/netplan/99-wildcard.yaml` in its network shell provisioner. No template carries its own copy, and no `user-data.tpl` writes a netplan inline.
- **Why the wildcard.** packer attaches the NIC to `pcie.0` (`ens3`), libvirt puts it behind a pcie-root-port (`enp1s0`), so a config naming one interface is dead on the other host.
- **Why `dhcp-identifier: mac`.** networkd's default DHCP client id derives from the machine-id; clones of one image would otherwise ask for the same lease. The MAC differs per clone.
- **`ubuntu2404_aws`** is not given the netplan during the build: cloud-init's own DHCP config keeps the build VM reachable, and the template's first provisioner removes `50-cloud-init.yaml` and installs the wildcard for the clone's first boot.
- `just check` fails if `files/99-wildcard.yaml` is missing or lacks `dhcp-identifier: mac`, if a template dir holds any `*.yaml`/`*.yml`, if a `user-data.tpl` writes a netplan (an uncommented `path: /etc/netplan...` or `ethernets:`), or if a template does not take the shared file or install it with `install -m 0600` to `/etc/netplan/99-wildcard.yaml`.

## Commands

`justfile` is the main entry point; `./run` is the older equivalent. Both take a template dir name (default `ubuntu2404`).

```bash
just generate <template>       # render user-data, packer build, install to libvirt (sudo, KVM, ISO download)
just validate <template>       # render user-data, packer init + validate; offline, no build, no sudo
just check                     # bash -n run + shape checks (UEFI) + validate every template (the gate for this directory)
just clean <template>          # delete output for one template
just distclean                 # delete all output

./run generate --template=<dir>  # --template is accepted only by generate and update
./run clean                      # default template only
./run distclean
```

`generate` does the full pipeline:
1. `envsubst` renders `http/user-data.tpl` → `http/user-data` (cloud-init autoinstall config)
2. `packer init .` + `packer build .` in `qemu_images/<template>/`
3. Copies built qcow2 to `/var/lib/libvirt/images/<template>.qcow2` (requires sudo)
4. `virsh -c qemu:///system pool-refresh default`

## Architecture

```
justfile                         # recipes: generate, validate, check, clean, ...
run                              # older entry script, sets PKR_VAR_* env vars
qemu_images/<template>/          # ubuntu2404*, ubuntu2604* (see table above)
  template.pkr.hcl               # Packer QEMU builder config
  .gitignore                     # ignores http/user-data
  http/
    user-data.tpl                # cloud-init autoinstall template (envsubst vars)
    user-data                    # generated at build/validate time (gitignored; holds a password hash)
    meta-data                    # empty, required by cloud-init
files/                           # shared by every template
  99-vmtool.cfg                  # cloud-init config installed to /etc/cloud/cloud.cfg.d/
  99-wildcard.yaml               # netplan config installed to /etc/netplan/ (mode 0600)
keys/                            # SSH keys added to authorized_keys (gitignored)
output/                          # packer build output, qcow2 files (gitignored)
```

## Key variables

Set in `justfile` / `run`, exported as `PKR_VAR_*` for Packer:

| Var | Default |
|-----|---------|
| `PKR_VAR_distro` | template dir name (`ubuntu2404`) |
| `PKR_VAR_username` | `packer` |
| `PKR_VAR_password` | `packer` |
| `PKR_VAR_hashedpassword` | sha512crypt of password |
| `PKR_VAR_output_directory` | `output/<template>` |

## Adding a new distro

1. Create `qemu_images/<distro>/` mirroring an existing template (e.g. `ubuntu2604`), including its `.gitignore`
2. Update ISO url/checksum in `template.pkr.hcl`
3. Adjust boot commands if distro installer differs
4. `just validate <distro>` and `just check`; no script change is needed, templates are picked up by directory name
