# Work log (packer_images)

Append-only history of what shipped from `packer_images/ROADMAP.md`.
FOREMAN appends an entry per landed step or tranche. Newest at the
bottom. Corrections are appended, never edited in; a hook refuses
rewrites.

```
## YYYY-MM-DD — <step id and title>

- What shipped
- Commits: <shas>
```

---

## 2026-11-14 — T1 Local orch scaffolding + `validate` / `check` recipes

- `.gitignore`: `/ROADMAP.md`, `/docs/reports/`, `/tmp/` ignored.
- `docs/LOG.md` (this file) created.
- `justfile`: `validate <template>` (render user-data, `packer init`,
  `packer validate` with a non-existent `output_directory`; no build,
  no sudo) and `check` (`bash -n run` + `validate` on every dir in
  `qemu_images/`, fails if any fails).
- Commits: the commit that adds this entry ("packer_images T1: ...").

## 2026-11-14 — T2 `ubuntu2604` server template

- `qemu_images/ubuntu2604/`: copy of `ubuntu2404/` with the 26.04.1
  live-server ISO (sha256 `cc8a95cd…f1d927`, re-checked against
  `releases.ubuntu.com/26.04.1/SHA256SUMS` on 2026-11-14) and the
  variable description example. `http/user-data` ignored, not
  committed. `just validate ubuntu2604` and `just check` green. Real
  build (T4) is user-run and still open.
- Commits: the commit that adds this entry ("packer_images T2: ...").

## 2026-11-14 — T3 `ubuntu2604_gui` template

- `qemu_images/ubuntu2604_gui/`: copy of `ubuntu2404_gui/` with the
  same 26.04.1 ISO URL/checksum change as T2; 20G disk; ansible
  provisioner still runs the shared `ansible/playbooks/install_gui.yml`
  (not forked, not edited). `http/user-data` ignored. `just validate
  ubuntu2604_gui` and `just check` green. Real build (T5) is user-run
  and still open.
- Commits: the commit that adds this entry ("packer_images T3: ...").

## 2026-11-14 — T6 Docs: list the 26.04 templates

- `CLAUDE.md`: table of all five template dirs, `just
  generate|validate|check <template>` and `./run generate
  --template=<dir>`, layout and "adding a distro" made per-template.
  `README.md` (was empty): short summary and pointer to `CLAUDE.md`.
  No behaviour change.
- Commits: the commit that adds this entry ("packer_images T6: ...").

## 2026-11-14 — T4 `ubuntu2604` real build

- User ran the build and reported it passed. Image inspected offline:
  GPT, 1M BIOS boot partition, ext4 root on partition 2.

## 2026-11-14 — `ubuntu2604_btrfs` and `ubuntu2604_btrfs_gui` templates

- Built outside the roadmap at the user's direction. Both are the
  ext4 templates with an explicit autoinstall `storage: config:`: same
  BIOS layout, flat btrfs root on partition 2, no swap file.
- Real packer builds (no libvirt install) succeeded: server in 9 min,
  GUI in 30 min with `install_gui.yml` green (ok=8, failed=0). Images
  inspected offline: btrfs on partition 2. Not yet booted under vmtool.
- vmtool's grow step now handles btrfs (`pkg/vmtool/ansible.go`).
- `UEFI.md` added: notes on converting to UEFI.

## 2026-11-14 — T5 `ubuntu2604_gui` real build (build half)

- Packer build (no libvirt install) succeeded in 16 min;
  `install_gui.yml` green on 26.04 unchanged (ok=8, failed=0). Image
  inspected offline: 20G, ext4 root on partition 2.
- Still open: install into libvirt and the booted smoke checks
  (logged-in desktop session, chromium, session type).

## 2026-11-16 — U4 Templates build UEFI images, in place

- All seven templates: `efi_boot = true`, OVMF code/vars from new
  variables (Arch `edk2-ovmf` non-secure-boot defaults), `boot_wait`
  10s; a new last shell provisioner installs the fallback loader
  `EFI/BOOT/BOOTX64.EFI` (`grub-install --removable --no-nvram`) if
  missing. btrfs templates: 512M ESP replaces the bios_grub partition
  as p1, root stays p2. `check` gains shape assertions (efi_boot,
  fallback loader in the last provisioner). `UEFI.md` rewritten as the
  record. Not built; B0/B prove it.
- Commits: the tranche-2 commit on `clone-identity` (vmtool repo).

## 2026-11-16 — C3 Images end clean for cloud-init: identity at first boot

- New shared `files/99-vmtool.cfg` (NoCloud/None datasource, no default
  user, password SSH, hostname not preserved, `manage_etc_hosts:
  localhost`, growpart + resize_rootfs on `/`), installed by all seven
  templates to `/etc/cloud/cloud.cfg.d/`. Named `99-` (not `90-` as
  planned) so it sorts after the package's `90_dpkg.cfg`, which also
  sets `datasource_list`.
- The last provisioner now runs `cloud-init status --wait || true`,
  strips `autoinstall` and packer's `ds=` token from
  `GRUB_CMDLINE_LINUX_DEFAULT` + `update-grub`, the BOOTX64.EFI guard,
  `apt-get clean`, removes root's history, and ends on
  `cloud-init clean --machine-id --logs`.
- `ubuntu2404_aws` no longer disables cloud-init (user-data and
  template); installs the cfg and `99-disable-network-config.cfg`.
- `check` fails on a missing clean / wait, wrong order, clean not last,
  cfg not installed, or any mention of `cloud-init.disabled`.
  `CLAUDE.md` gains "Clean first boot (cloud-init)". Not built; B0/B
  prove it.
- Commits: the tranche-2 commit on `clone-identity` (vmtool repo).

## 2026-11-16 — C4 `dhcp-identifier: mac`, one shared netplan file

- `files/99-wildcard.yaml` is the one netplan file (`all-en`, `dhcp4`,
  now `dhcp-identifier: mac`); the six per-template copies are gone. All
  seven templates copy it in and `install -m 0600` it to
  `/etc/netplan/99-wildcard.yaml`. `ubuntu2404_aws` dropped its inline
  `write_files` netplan and the runcmd `rm 50-cloud-init.yaml` +
  `netplan apply`: the cloud image's own DHCP config carries the build,
  and the shared file takes over at a clone's first boot.
- `check` fails on a missing shared file or one without
  `dhcp-identifier: mac`, any yaml under a template dir, an inline
  netplan in a `user-data.tpl`, or a template not installing the shared
  file with mode 0600. Not built; B0/B prove it.
- Commits: the tranche-2 commit on `clone-identity` (vmtool repo).

## 2026-11-16 — B0 pilot: `ubuntu2604` built UEFI and checked on jetfire

- Built with `just generate ubuntu2604` in 7 min 33 s; `boot_wait = "10s"`
  reached GRUB under OVMF at the first attempt. The old BIOS image is
  kept as `ubuntu2604-bios.qcow2` in the default pool.
- Image, offline: GPT with ESP p1 (538M) and ext4 root p2; `BOOTX64.EFI`
  on the ESP; `/etc/machine-id` is `uninitialized`; no
  `/etc/cloud/cloud-init.disabled` and no `99-installer.cfg`;
  `99-vmtool.cfg` present; netplan `99-wildcard.yaml` mode 0600 with
  `dhcp-identifier: mac`; `GRUB_CMDLINE_LINUX_DEFAULT=""`.
- Two clones made by vmtool (branch `clone-identity`, defaults; one with
  `--disk-size 20`): both boot under UEFI with cache `unsafe`; address in
  18 s and 26 s; hostname is the VM name; machine-id and instance-id are
  the domain UUID; different host keys and addresses; DHCP client id is
  `01:<mac>`; no `ubuntu` user; packer password login works; the 20G
  clone's root is 19.5G and vmtool's grow step reports success. After a
  reboot the identity is unchanged, a line added to `/etc/hosts`
  survives and the lease stays one row per MAC.
- Fault: `/etc/hosts` keeps the installer's `127.0.1.1 ubuntu2604` line;
  cloud-init adds `127.0.1.1 <vm> <vm>` below it instead of replacing
  it. The VM name resolves; the image name also resolves on every clone.
  To fix in the templates before the other six are built.
- A BIOS image created with the default firmware does not boot (OVMF:
  "No bootable option or device was found"), as expected.
- Both clones deleted.
