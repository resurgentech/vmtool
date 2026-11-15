#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: ${PKR_VAR_distro}
    username: ${PKR_VAR_username}
    password: "${PKR_VAR_hashedpassword}"
  ssh:
    install-server: true
    allow-pw: true
  # network:
  #   network:
  #     version: 2
  #     ethernets:
  #       all-en:
  #         match:
  #           name: en*
  #         dhcp4: true
  packages:
    - qemu-guest-agent
  # Same shape as `layout: direct` under BIOS (1M bios_grub + root on
  # partition 2, which vmtool's grow step expects), but root is a flat btrfs
  # filesystem. No subvolumes here: create them in the guest afterwards.
  # swap size 0: the default /swap.img would block snapshots of the volume
  # that holds it.
  storage:
    swap:
      size: 0
    config:
      - type: disk
        id: disk0
        match:
          size: largest
        ptable: gpt
        wipe: superblock-recursive
        preserve: false
        grub_device: true
      - type: partition
        id: part-bios
        device: disk0
        number: 1
        size: 1M
        flag: bios_grub
        preserve: false
      - type: partition
        id: part-root
        device: disk0
        number: 2
        size: -1
        preserve: false
      - type: format
        id: fmt-root
        volume: part-root
        fstype: btrfs
        preserve: false
      - type: mount
        id: mnt-root
        device: fmt-root
        path: /
  late-commands:
    - echo '${PKR_VAR_username} ALL=(ALL) NOPASSWD:ALL' > /target/etc/sudoers.d/${PKR_VAR_username}
