# packer_images

Packer templates that build qcow2 VM images for vmtool and install them
into a folder or the store. When `config.yml` has `server:`, `just generate`
pushes the image to the store (`vmtool images push`, as `<template>.qcow2`);
if the store does not answer it says so and falls back to the folder.
Otherwise it copies the image to `$VMTOOL_IMAGE_DIR`
(default `<repo>/images/`); vmtool finds it through `image_dir` in
`config.yml`, so set `image_dir` to the same folder. One template per directory in
`qemu_images/`: `ubuntu2404`, `ubuntu2404_gui`, `ubuntu2404_aws`,
`ubuntu2604`, `ubuntu2604_btrfs`, `ubuntu2604_btrfs_gui`, `ubuntu2604_gui` (`just templates` lists them).

```bash
just generate ubuntu2604        # build + copy to image_dir (needs KVM, ~3 GB ISO download; no sudo)
just update ubuntu2604 [host]   # smoke-boot a throwaway clone <host>/ubuntu2604-update (host defaults to this machine)
just validate ubuntu2604        # offline packer validate, no build
just check                      # shape checks + validate every template
```

See [CLAUDE.md](CLAUDE.md) for the pipeline, layout and variables.
