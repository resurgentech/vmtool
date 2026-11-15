# packer_images

Packer templates that build qcow2 VM images for vmtool and install them
into the libvirt `default` pool. One template per directory in
`qemu_images/`: `ubuntu2404`, `ubuntu2404_gui`, `ubuntu2404_aws`,
`ubuntu2604`, `ubuntu2604_btrfs`, `ubuntu2604_gui` (`just templates` lists them).

```bash
just generate ubuntu2604        # build + install (needs KVM, sudo, ~3 GB ISO download)
./run generate --template=ubuntu2604   # same, older script
just validate ubuntu2604        # offline packer validate, no build
just check                      # bash -n run + validate every template
```

See [CLAUDE.md](CLAUDE.md) for the pipeline, layout and variables.
