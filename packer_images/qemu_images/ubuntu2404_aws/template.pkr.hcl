packer {
  required_plugins {
    qemu = {
      version = ">= 1.0.0"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

source "qemu" "ubuntu" {
  iso_url          = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
  iso_checksum     = "53fdde898feed8b027d94baa9cfe8229867f330a1d9c49dc7d84465ee7f229f7"
  disk_image       = true
  cd_files         = ["./http/user-data", "./http/meta-data"]
  cd_label         = "cidata"
  output_directory = var.output_directory
  vm_name          = var.distro
  format           = "qcow2"
  disk_size        = "10G"
  memory           = 4096
  cpus             = 4
  accelerator      = "kvm"
  machine_type     = "q35"
  net_device       = "virtio-net-pci"
  disk_interface   = "virtio"
  headless         = true
  boot_wait        = "10s"

  # UEFI: OVMF without secure boot (paths are Arch edk2-ovmf; see variables).
  efi_boot          = true
  efi_firmware_code = var.efi_firmware_code
  efi_firmware_vars = var.efi_firmware_vars

  http_directory   = "http"
  shutdown_command = "echo '${var.password}' | sudo -S shutdown -P now"
  ssh_username     = var.username
  ssh_password     = var.password
  ssh_timeout      = "60m"
}

build {
  sources = ["source.qemu.ubuntu"]

  # cloud-init stays enabled: the image is cleaned at the end (last provisioner)
  # and each clone configures itself at first boot. See files/99-vmtool.cfg.
  provisioner "file" {
    source      = "${path.root}/../../files/99-vmtool.cfg"
    destination = "/tmp/99-vmtool.cfg"
  }

  # The wildcard netplan is shared by every template (files/99-wildcard.yaml).
  # It is not applied during the build: cloud-init's own DHCP config keeps the
  # build VM reachable, and the file takes over at the clone's first boot.
  provisioner "file" {
    source      = "${path.root}/../../files/99-wildcard.yaml"
    destination = "/tmp/99-wildcard.yaml"
  }

  provisioner "shell" {
    inline = [
      "sudo rm -f /etc/netplan/50-cloud-init.yaml",
      "sudo install -m 0600 /tmp/99-wildcard.yaml /etc/netplan/99-wildcard.yaml",
      "sudo rm -f /etc/ssh/sshd_config.d/60-cloudimg-settings.conf",
      "sudo ssh-keygen -A",
      "sudo install -m 0644 /tmp/99-vmtool.cfg /etc/cloud/cloud.cfg.d/99-vmtool.cfg",
      "printf 'network: {config: disabled}\\n' | sudo tee /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg",
    ]
  }

  # Fallback UEFI loader. The boot entry the installer wrote lives in packer's
  # NVRAM, not in the qcow2; a clone starts with fresh NVRAM and can only boot
  # via the removable-media path EFI/BOOT/BOOTX64.EFI. `sudo test`: /boot/efi
  # is mounted root-only, so an unprivileged test would always say "missing".
  # This is the LAST provisioner; later steps append their lines here.
  provisioner "shell" {
    inline = [
      "sudo cloud-init status --wait || true",
      "sudo sed -i '/^GRUB_CMDLINE_LINUX_DEFAULT=/{s/\\bautoinstall\\b//g;s/\\bds=\\\\\\?\"[^\"]*\"//g;s/\\bds=nocloud[^ \"]*//g;s/  */ /g;s/=\" */=\"/;s/ *\"$/\"/}' /etc/default/grub",
      "sudo update-grub",
      "if ! sudo test -e /boot/efi/EFI/BOOT/BOOTX64.EFI; then sudo grub-install --target=x86_64-efi --efi-directory=/boot/efi --removable --no-nvram; fi",
      "sudo apt-get clean",
      "sudo rm -f /root/.bash_history",
      "sudo sed -i '/^127[.]0[.]1[.]1[[:space:]]/d' /etc/hosts",
      "sudo cloud-init clean --machine-id --logs",
    ]
  }
}

variable "efi_firmware_code" {
  description = "OVMF firmware code file (non-secure-boot). Default is Arch's edk2-ovmf; other distros use different paths."
  type        = string
  default     = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
}

variable "efi_firmware_vars" {
  description = "OVMF NVRAM template (non-secure-boot). Default is Arch's edk2-ovmf; other distros use different paths."
  type        = string
  default     = "/usr/share/edk2/x64/OVMF_VARS.4m.fd"
}

variable "output_directory" {
  description = "The directory where the output files will be stored."
  type        = string
}

variable "distro" {
  description = "Name of the VM distro, usually short name like ubuntu2404."
  type        = string
}

variable "username" {
  description = "Initial username for the VM."
  type        = string
}

variable "password" {
  description = "Password for the initial user."
  type        = string
}
