
#cloud-config
hostname: ${PKR_VAR_distro}

# Set up the user
users:
  - name: ${PKR_VAR_username}
    passwd: "${PKR_VAR_hashedpassword}"
    lock_passwd: false
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    groups: [sudo, adm]

# Enable password authentication for the user
ssh_pwauth: true

# Install necessary packages
packages:
  - qemu-guest-agent

# Network: no netplan here. The cloud image's own cloud-init DHCP config keeps
# the build VM reachable; the shared files/99-wildcard.yaml is installed by a
# packer file provisioner (see template.pkr.hcl).

# Finalize configuration
runcmd:
  - rm -f /etc/ssh/sshd_config.d/60-cloudimg-settings.conf
  - ssh-keygen -A
