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
