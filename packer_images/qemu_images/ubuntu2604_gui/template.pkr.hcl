packer {
  required_plugins {
    qemu = {
      version = ">= 1.0.0"
      source  = "github.com/hashicorp/qemu"
    }
    ansible = {
      version = ">= 1.1.0"
      source  = "github.com/hashicorp/ansible"
    }
  }
}

source "qemu" "ubuntu" {
  iso_url          = "https://releases.ubuntu.com/26.04.1/ubuntu-26.04.1-live-server-amd64.iso"
  iso_checksum     = "sha256:cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927"
  output_directory = var.output_directory
  vm_name          = var.distro
  format           = "qcow2"
  disk_size        = "20G"
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

  boot_command = [
    "e<wait>",
    "<down><down><down><end>",
    " autoinstall ds=\"nocloud-net;s=http://{{.HTTPIP}}:{{.HTTPPort}}/\" ",
    "<f10>"
  ]
  http_directory   = "http"
  shutdown_command = "echo '${var.password}' | sudo -S shutdown -P now"
  ssh_username     = var.username
  ssh_password     = var.password
  ssh_timeout      = "60m"
}

build {
  sources = ["source.qemu.ubuntu"]

  provisioner "file" {
    source      = "${path.root}/../../files/99-wildcard.yaml"
    destination = "/tmp/99-wildcard.yaml"
  }

  # cloud-init config shared by every template; installed by the network
  # provisioner below. See files/99-vmtool.cfg and CLAUDE.md.
  provisioner "file" {
    source      = "${path.root}/../../files/99-vmtool.cfg"
    destination = "/tmp/99-vmtool.cfg"
  }

  # The wildcard netplan is what makes the image portable: packer attaches the
  # NIC straight to pcie.0 (ens3), libvirt puts it behind a pcie-root-port
  # (enp1s0), so a config naming one interface is dead on the other host.
  #
  # Deleting 50-cloud-init.yaml is not enough — cloud-init rewrites it on every
  # boot from subiquity's 90-installer-network.cfg, which hardcodes ens3. Drop
  # that file and turn cloud-init's network layer off so 99-wildcard.yaml is the
  # only thing that ever configures the guest NIC.
  provisioner "shell" {
    inline = [
      "sudo rm -f /etc/netplan/50-cloud-init.yaml",
      "sudo rm -f /etc/cloud/cloud.cfg.d/90-installer-network.cfg",
      "printf 'network: {config: disabled}\\n' | sudo tee /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg",
      "sudo install -m 0644 /tmp/99-vmtool.cfg /etc/cloud/cloud.cfg.d/99-vmtool.cfg",
      "sudo install -m 0600 /tmp/99-wildcard.yaml /etc/netplan/99-wildcard.yaml",
    ]
  }

  # use_proxy = false: talk to the qemu port-forward directly; packer's SSH
  # proxy breaks scp/sftp with current OpenSSH. Password auth needs sshpass.
  provisioner "ansible" {
    playbook_file = "${path.root}/../../../ansible/playbooks/install_gui.yml"
    user          = var.username
    use_proxy     = false
    extra_arguments = [
      "--extra-vars", "ansible_password=${var.password}",
      "--scp-extra-args", "'-O'",
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
  description = "Name of the VM distro, usually short name like ubuntu2604."
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
