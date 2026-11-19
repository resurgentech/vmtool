# packer_images

Packer templates that build qcow2 VM images for vmtool and install them
into a folder. `just generate` copies the image to `$VMTOOL_IMAGE_DIR`
(default `<repo>/images/`); vmtool finds it through `image_dir` in
`config.yml`, so set `image_dir` to the same folder. One template per directory in
`qemu_images/`: `ubuntu2404`, `ubuntu2404_gui`, `ubuntu2404_aws`,
`ubuntu2604`, `ubuntu2604_btrfs`, `ubuntu2604_btrfs_gui`, `ubuntu2604_gui` (`just templates` lists them).

```bash
just generate ubuntu2604        # build + copy to image_dir (needs KVM, ~3 GB ISO download; no sudo)
./run generate --template=ubuntu2604   # older script; still installs to the libvirt pool
just validate ubuntu2604        # offline packer validate, no build
just check                      # bash -n run + validate every template
```

See [CLAUDE.md](CLAUDE.md) for the pipeline, layout and variables.
