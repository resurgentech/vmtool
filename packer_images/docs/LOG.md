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

## 2026-11-16 — C14 Images ship without the installer's `127.0.1.1` line

- All seven templates' last provisioner now runs
  `sudo sed -i '/^127[.]0[.]1[.]1[[:space:]]/d' /etc/hosts` as the line
  just before `cloud-init clean --machine-id --logs` (still last). It
  drops the installer's `127.0.1.1 <image>` line found in the B0 pilot,
  so a clone's first boot writes the only `127.0.1.1` line, naming the
  VM; the localhost and IPv6 lines stay. `ubuntu2404_aws` gets it too
  (a no-op if its image has no such line).
- `just check` gains `check_no_installer_hosts_line`: fails a template
  whose last provisioner lacks that exact line or has it after
  `cloud-init clean`. Red for all seven before the edit, green after.
- `CLAUDE.md` "Clean first boot" lists the delete in the last
  provisioner's order, says why, and names the new check failure.
- Not built: B (all seven builds) proves it, including the aws image.
- Commits: the C14 commit on `clone-identity` (vmtool repo).

## 2026-11-16 — B, jetfire part: six of seven images rebuilt and checked

- Built with `just generate`: `ubuntu2404` 8 min, `ubuntu2404_gui` 22 min,
  `ubuntu2604` 7.5 min, `ubuntu2604_btrfs` 9.5 min,
  `ubuntu2604_btrfs_gui` 24.5 min, `ubuntu2604_gui` 15 min. All include
  the `/etc/hosts` delete. `boot_wait = "10s"` worked for every one.
- `ubuntu2404_aws` did not build: the pinned `iso_checksum` no longer
  matches the rolling `noble/current` cloud image (failed in 17 s,
  before boot). Its build and checks are still open.
- Per image, two clones made by vmtool (branch `clone-identity`,
  defaults; the second with `--disk-size` 20, or 30 for the 20G GUI
  images): all twelve boot under UEFI with cache `unsafe`; hostname is
  the VM name; machine-id and instance-id are the domain UUID; host keys
  and addresses differ; DHCP client id is `01:<mac>`; no `ubuntu` user;
  packer password login works; the larger disk is filled (ext4 and
  btrfs) and vmtool's grow step reports success; `/etc/hosts` has
  exactly one `127.0.1.1` line and it names the VM. After a reboot of
  one clone per image: identity unchanged, an added `/etc/hosts` line
  survives, still one `127.0.1.1` line, one lease row per MAC.
- cloud-init is 25.2 on the 24.04 images and 26.1 on the 26.04 images;
  both behave the same here.
- The three GUI images reach the GNOME desktop with the `packer` user
  logged in (gdm active, screenshot checked).
- Offline inspection was done for `ubuntu2404` and `ubuntu2604` only
  (ESP p1, root p2, `BOOTX64.EFI`, machine-id `uninitialized`, no
  disable marker, no `127.0.1.1` line, empty grub command line); for
  the other four the clone results stand in for it.
- create took 23 to 66 s; the btrfs clones are the slow ones (create 46
  to 66 s, reboot about 41 s against 11 to 24 s for ext4). Not looked
  into.
- BIOS image: `ubuntu2604-bios.qcow2` with the default firmware does not
  boot (B0); with `--firmware bios` it boots under BIOS and keeps the
  old behaviour (image hostname, shared machine-id).
- Every VM made here is deleted. Still open in B: `ubuntu2404_aws`,
  veritech04, the cache measurement.

## 2026-11-16 — C15 `ubuntu2404_aws` takes its checksum from Canonical's SHA256SUMS

- `qemu_images/ubuntu2404_aws/template.pkr.hcl:12`: `iso_checksum` is now
  `file:https://cloud-images.ubuntu.com/noble/current/SHA256SUMS` instead
  of a pinned bare hex sum, which went stale when Canonical republished
  `noble/current/` (B's aws build failed in 17 s). `iso_url` stays the
  rolling URL; each build gets the newest image, verified against the
  sums file fetched over HTTPS (no gpg check).
- New shape check `check_rolling_url_checksum`, run for every template:
  an `iso_url` containing `/current/` needs an `iso_checksum` starting
  with `file:`. Before the template change `check` failed for
  `ubuntu2404_aws` only. On a scratch copy, bare hex, `sha256:` and
  `md5:` sums fail on the rolling URL; a dated URL passes with any sum.
- `CLAUDE.md`: the aws table row and a "Checksums" note under the table;
  "Adding a new distro" says which checksum form a URL takes.
- Not built: `packer validate` downloads nothing, so only B's aws build
  proves packer 1.16.1 reads Ubuntu's `<hash> *<file>` sums lines.
- Commits: the C15 commit on `clone-identity` (vmtool repo).

## 2026-11-16 — B, jetfire part: `ubuntu2404_aws` built and checked

- Built in 55 s. packer 1.16.1 accepts
  `iso_checksum = "file:…/noble/current/SHA256SUMS"` and verified the
  rolling cloud image against it. `cloud-init status --wait` returned
  `done` in the build VM (the ISO templates report `disabled` there).
- Two clones by vmtool (defaults; the second `--disk-size 20`): UEFI
  boot, cache `unsafe`, hostname = VM name, machine-id = domain UUID,
  different host keys and addresses, DHCP client id `01:<mac>` (the
  shared netplan takes over from the cloud image's own config), no
  `ubuntu` user, root (vda1) fills the 20G disk and vmtool's grow
  reports success, exactly one `127.0.1.1` line naming the VM; all
  unchanged after a reboot, with an added `/etc/hosts` line kept.
  create took 13 s and 19 s. cloud-init here is 26.1 (the ISO-built
  24.04 images have 25.2).
- One clone with `--firmware bios`: boots under BIOS with the same
  identity behaviour, so the image boots both ways.
- All seven images are now rebuilt and checked on jetfire; every VM made
  here is deleted. Still open in B: veritech04 and the cache
  measurement.

## 2026-11-17 — B closed: hypervisor checks on veritech03

- Run on veritech03 instead of veritech04 (user's decision): same
  libvirt 10.0.0 / QEMU 8.2.2, daemon built from `clone-identity`.
- One clone of the cleaned `ubuntu2404` with defaults: boots under UEFI
  (OVMF 4M from the `ovmf` package), hostname = VM name, machine-id =
  domain UUID, one `127.0.1.1` line, cloud-init done, cache `unsafe`.
  `vmtool delete` removed `/var/lib/libvirt/qemu/nvram/<name>_VARS.fd`
  and the disk.
- Write speed, `dd bs=4k count=1000 oflag=dsync`, three runs each, host
  under light load (about 3): default `unsafe` 0.48 to 0.62 s (6.6 to
  8.6 MB/s); `--disk-cache none` 1.62 to 1.98 s (2.1 to 2.5 MB/s).
  About 3 to 4 times faster.
- Seen on the way: `create` returned with an IP and a `cmd` run at once
  got "connection refused" on port 22; sshd was up about 15 s later.
  Nothing waits for SSH unless a grow or playbook follows.
- Both test VMs deleted; the seven VMs of other sessions there were not
  touched. veritech04 still runs the old daemon; upgrading it is now a
  rollout item, not a check.

## 2026-11-18 — I1 Images keep /tmp on disk, emptied at boot, as on 24.04

- Why: 26.04's systemd mounts /tmp as a tmpfs (`tmp.mount`, `size=50%`,
  `nr_inodes=1m`); runners on 26.04 VMs used up its inodes with the disk
  half empty. User's rule: "2604 should match the 2404 rules".
- New shared `files/00-vmtool-tmp.conf`: `D /tmp 1777 root root 30d`
  (emptied at every boot, aged at 30 days). `00-` sorts it before the
  release's `tmp.conf`, so its line wins. /var/tmp is untouched.
- All seven templates: a `provisioner "file"` for it beside the
  `99-vmtool.cfg` one; `sudo install -m 0644 ... /etc/tmpfiles.d/` right
  after the `99-vmtool.cfg` install in the network provisioner; and
  `"sudo systemctl mask tmp.mount",` in the last provisioner after the
  BOOTX64.EFI guard, before `apt-get clean` (no `--now`: the build VM's
  provisioner scripts live in /tmp). A no-op symlink on 24.04.
- `just check`: a top-level check of the shared file and
  `check_tmp_on_disk` per template (takes and installs the file; the
  exact mask line in the last provisioner before `cloud-init clean`; no
  `mask --now tmp.mount`). Red with one failure for the missing file and
  three per template; green (`check: ok`) after. The ordering and
  `--now` failures were also shown on a scratch copy.
- `CLAUDE.md`: new section "/tmp on disk" (why, the rule, mask, the
  `00-` file, the expected journal line, the one-line remedy for
  existing VMs), the provisioner order, the tree and the check list.
- Not done (user-run): rebuilding the four 26.04 images and the clone
  checks; the 24.04 images pick it up at their next rebuild.
- Commits: the I1 commit on `clone-identity` (vmtool repo).
