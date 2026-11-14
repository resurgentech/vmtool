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
| `ubuntu2604_gui` | 26.04.1 live-server ISO | 20G disk; same shared `install_gui.yml` playbook |

`just templates` lists them.

## Commands

`justfile` is the main entry point; `./run` is the older equivalent. Both take a template dir name (default `ubuntu2404`).

```bash
just generate <template>       # render user-data, packer build, install to libvirt (sudo, KVM, ISO download)
just validate <template>       # render user-data, packer init + validate; offline, no build, no sudo
just check                     # bash -n run + validate every template (the gate for this directory)
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
qemu_images/<template>/          # ubuntu2404, ubuntu2404_gui, ubuntu2404_aws, ubuntu2604, ubuntu2604_gui
  template.pkr.hcl               # Packer QEMU builder config
  .gitignore                     # ignores http/user-data
  http/
    user-data.tpl                # cloud-init autoinstall template (envsubst vars)
    user-data                    # generated at build/validate time (gitignored; holds a password hash)
    meta-data                    # empty, required by cloud-init
  files/                         # (ISO-based templates only)
    99-wildcard.yaml             # netplan config pushed into image
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
