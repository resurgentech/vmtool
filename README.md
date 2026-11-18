# vmtool

Library and CLI/TUI for creating VMs on a Linux machine using KVM/QEMU and libvirt.

The HTTP API is **spec-first**: edit `spec/openapi/vmtool.yaml`, then
`go generate ./internal/api/...`. See [`AGENTS.md`](AGENTS.md) and
[`spec/README.md`](spec/README.md).

## Prerequisites

### Ansible (Ubuntu/Debian and Omarchy/Arch)

One playbook does everything below — packages, libvirt daemon, `libvirt` group
membership, and the `default` network and storage pool vmtool expects. Install
ansible yourself (it is the one prerequisite that cannot bootstrap itself), then:

```bash
sudo pacman -S --needed ansible     # or: sudo apt install ansible

just prereqs                        # or: scripts/prereqs.sh
```

`scripts/prereqs.sh` fails with install instructions if `ansible-playbook` is
missing, adds `--ask-become-pass` unless sudo is already passwordless, and runs
[`ansible/playbooks/setup_host.yml`](ansible/playbooks/setup_host.yml). It is
safe to re-run. Any extra arguments go through to `ansible-playbook`:

```bash
just prereqs --check --diff                       # dry run
just prereqs -e vmtool_install_go=true            # Debian/Ubuntu: Go tarball if none >= 1.21; Arch: pacman go
just prereqs -e vmtool_install_packer_deps=true   # extras for packer_images/run
just prereqs -e vmtool_pool_path=/srv/vms         # back the default pool elsewhere

# just re-splits recipe arguments, so pass quoted JSON to the script directly:
scripts/prereqs.sh -e '{"vmtool_extra_pools":[{"name":"fast","path":"/srv/vms"}]}'
```

Log out and back in afterwards (or `newgrp libvirt`) to pick up the group.

The playbook also installs OVMF (UEFI firmware: `ovmf` on Debian/Ubuntu,
`edk2-ovmf` on Arch). New VMs default to UEFI, and packer's UEFI image builds
need it on the workstation. A running daemon needs no restart; libvirt reads
the firmware descriptors each time a domain is defined.

### Hypervisors

`just prereqs <host>` (a host from `config.yml`) runs the same playbook over
ssh with `-e vmtool_role=hypervisor` and asks for the sudo password. On top of
the packages and libvirt it creates:

- the system user and group `vmtool` (no login shell, no sudo, in `libvirt`
  and, where it exists, `kvm`);
- `/opt/vmtool` (`vmtool:vmtool 0750`, with `tls/` `0700`), where `just deploy`
  puts the daemon;
- `/var/lib/vmtool/images` and a libvirt dir pool `vmtool` on it, owned by
  `vmtool` (mode `0711`). No `default` pool is created, and no other pool is
  touched. If a pool called `vmtool` already points somewhere else, the play
  fails and says so.
- Go, only when `/usr/local/go/bin/go` (else `go` on the login user's PATH)
  is missing or older than `go_min` (1.21): the upstream tarball for the
  `go` line of `go.mod`, checked against its published sha256, replaces
  `/usr/local/go`. A Go at or above the minimum is left alone.

The paths and versions are variables at the top of the [`justfile`](justfile)
(`hv_dir`, `hv_pool_dir`, `go_min`, `go_install_version`). Re-running is a
no-op.

Then put the daemon on it, from the workstation:

```bash
just prereqs veritech04     # once per host: vmtool user, /opt/vmtool, pool
just deploy veritech04      # build, certify, configure, start
```

`just deploy <host>` stops before copying anything if the host's Go is too
old or if `just prereqs <host>` has not run (no `vmtool` user or no
`/opt/vmtool`). It copies the tracked files to
`~/.cache/vmtool-deploy/src` on the host and builds there as your login user
(nothing is built with sudo), issues the host's certificate, writes
`config.yml` (role `hypervisor`, the token's hash only; the token goes over
ssh stdin, never in an argument and never through a sudo prompt) and then
installs everything with a single `sudo`, which may ask for your password
once. The service runs as `vmtool`, never as your user:

```
/opt/vmtool/                 vmtool:vmtool 0750  working directory and HOME
  vmtool                     0755  the daemon
  config.yml                 0600  role: hypervisor, listen 0.0.0.0:9473, token hash
  tls/<host>.labtwo.crt      0644
  tls/<host>.labtwo.key      0600
  image-manifest.yml         written by the daemon
/etc/systemd/system/vmtool.service   User=vmtool, ExecStart=/opt/vmtool/vmtool server
```

The staged `config.yml` and key are removed after the install. `just cert
<host>` on its own renews the certificate the same way (into
`/opt/vmtool/tls`, owner `vmtool`) and restarts the service if it is running.

### Ubuntu / Debian (manual)

```bash
sudo apt install libvirt-dev pkg-config qemu-kvm libvirt-daemon-system ansible
```

### Omarchy / Arch (manual)

```bash
sudo pacman -S --needed libvirt pkgconf qemu-desktop dnsmasq iptables-nft ansible

sudo systemctl enable --now libvirtd.socket
sudo usermod -aG libvirt "$USER"        # log out and back in
# Until then, virsh talks to qemu:///session; vmtool always uses qemu:///system:
export LIBVIRT_DEFAULT_URI=qemu:///system
sudo virsh net-autostart default && sudo virsh net-start default
```

Ubuntu's `libvirt-daemon-system` enables the daemon, creates the `libvirt` group,
and starts the default network for you; on Arch those steps are manual.
`virsh` without a URI uses `qemu:///session` until you are in the `libvirt` group.

- **libvirt-dev** / **libvirt** — C headers for the Go libvirt bindings (Arch ships them in the main `libvirt` package)
- **pkg-config** / **pkgconf** — required by cgo to find libvirt
- **qemu-kvm** / **qemu-desktop** — KVM/QEMU hypervisor (`qemu-full` only adds non-x86 targets)
- **libvirt-daemon-system** / **libvirt** — libvirt daemon and default network
- **dnsmasq**, **iptables-nft** — Arch only; optional deps of `libvirt` that the default NAT/DHCP network requires
- **ansible** — used for VM provisioning and playbook execution

## Build

Go 1.24+.

```bash
go build -o vmtool ./cmd/vmtool
go generate ./internal/api/...          # after editing spec/openapi/vmtool.yaml
scripts/verify-generate.sh             # fail if generated code is stale
```

## First run

Every `vmtool` command (except `help`, `completion` and `config write`) needs
`config.yml` in the working directory and a `VMTOOL_TOKEN` whose SHA-256
matches its `token_sha256`. The token comes from the environment, else from
`.env` in the working directory; a real `VMTOOL_TOKEN` in the environment
wins over `.env`. Without them every command stops with one line naming the
fix.

```bash
just token          # new token into .env (and OpenBao if VAULT_ADDR/VAULT_TOKEN are set)
# or: just token-fetch   # the existing token from OpenBao into .env
just install        # build, cert, config.yml, systemd unit (vmtool server over HTTPS)
```

`vmtool server` serves HTTPS only and needs `tls_cert`, `tls_key` and
`token_sha256` in `config.yml`; `just install` sets all three.

## Usage

```bash
# Interactive TUI (run from project root)
./vmtool i

# CLI — VMs are named <host>/<vm>; the local host is its hostname (jetfire here)
./vmtool create <host>/<name> <image>
./vmtool create jetfire/web ubuntu.qcow2 --net-type direct --net-source eth0 --macvtap-mode bridge
./vmtool create jetfire/scratch ubuntu.qcow2 --noclone  # boot the image in place
./vmtool create jetfire/web ubuntu.qcow2 --extra-disk-size 50   # second empty disk as vdb
./vmtool create jetfire/web ubuntu.qcow2 --no-wait      # return right after start; no address or SSH wait
./vmtool add-disk jetfire/web 50                        # attach a new empty disk (vdb, vdc, …)
./vmtool reboot jetfire/web                             # ACPI reboot, stays defined
./vmtool exec jetfire/web uname -a                      # exact argv, no shell (was `cmd`; API 2.0.0)
./vmtool exec jetfire/web -- sh -c 'echo hi | wc -c'    # shell features: run a shell; prints 3
./vmtool exec jetfire/web -- sh -c 'cat > /p/.env' < .env   # the CLI passes your stdin through
# REST: POST /v1/hosts/{h}/vms/{n}/exec and /v1/metal/{n}/exec take an optional
# "stdin" string (API 2.1.0): text written to the command's stdin, then closed.
# Never logged, never in an error or on a command line; bounded by the 1 MiB body limit.
./vmtool list                                           # every host (5 s each; a dead host is an ERROR row)
./vmtool list jetfire                                   # one host
./vmtool hosts                                          # reachability, API version, cert days left
./vmtool delete <host>/<name>
./vmtool delete jetfire/scratch --noclone               # undefine only, keep the disk
./vmtool server                                         # HTTPS REST on 127.0.0.1:9473; GET / is Swagger UI
```

### Waits at create (`--no-wait`)

`create` returns when the VM is usable: it waits for the guest's address, then
for SSH (sshd often answers some seconds after the address appears), and only
then runs the grow and the playbook, if asked. Each wait is 120 s unless
`config.yml` says otherwise:

```yaml
ip_wait_seconds: 300     # address wait; 0 or absent = 120, otherwise 1 to 3600
ssh_wait_seconds: 180    # SSH wait; same bounds
```

`create` runs on the workstation (the CLI, the TUI, or its own `vmtool
server`; a hypervisor answers createVM 403), so the workstation's
`config.yml` governs every create, including one on a remote host: it sends
its `ip_wait_seconds` to that host's daemon. The keys in a hypervisor's
`config.yml` are accepted and unused. A value below 0 or above 3600 is a
config load error naming the key.

If no address arrives, or SSH does not answer, in time, `create` fails (exit
non-zero, REST 500 with the VM in the body) but leaves the VM defined and
running: delete it, or fix the guest and use it. The CLI says `VM "<ref>"
exists and is running, but create did not finish:` and then the error. The same
holds for any failure after the domain exists (grow, playbook, reboot).

`--no-wait` (REST `no_wait`; the TUI form's "Wait for address" set to `no`)
returns right after the VM starts, with no address and no SSH wait; the result
is success and the address is usually empty. Use it for a guest whose address
vmtool cannot see (a static address, a DHCP server libvirt does not run and no
guest agent), a VM with no network, or a very slow boot. `--inventory` is still
written, right after the start. `--no-wait` cannot be combined with
`--playbook`: a playbook needs a guest vmtool can reach (refused before
anything is cloned). With `--disk-size` the volume is still resized before boot,
but the filesystem grow, which needs SSH, is skipped; cloud-init grows it at
first boot on images built since C3.

### VM identity at create

Every new VM gets a random UUID and an SMBIOS serial
`ds=nocloud;h=<hostname>;i=<uuid>`, made on the host that defines the domain.
`<hostname>` is the VM name with `_` turned into `-`, cut to 63 characters.
A cloud-init guest reads the serial as its NoCloud datasource and instance id,
so each clone of a base image takes its own name, keys and machine-id.

Images without cloud-init, or with it still disabled (older `packer_images`
builds whose last step was not `cloud-init clean --machine-id`), ignore the
serial: nothing changes for them.

`--noclone` gets the same serial. Booting a cleaned base image in place uses up
its first boot: it takes this VM's name, keys and machine-id, and so would
every later clone of it. Before shutting the guest down, run
`sudo cloud-init clean --machine-id` in it to make the image fresh again.
`--noclone` stays allowed.

### Firmware (UEFI by default)

New VMs boot with UEFI (OVMF, secure boot off): libvirt picks the firmware and
makes a per-VM NVRAM file, which `delete` removes with the VM. An image that
was installed for BIOS does not boot under UEFI: create it with
`vmtool create --firmware bios host/name image` (the TUI create form has a
Firmware field). There is no detection; you name `bios`. If `create` fails
waiting for an address on a UEFI VM (the VM is left running), the error says so.

A host without OVMF refuses a UEFI create with `UEFI firmware (OVMF) is not
installed on <host>`: install it (`just prereqs <host>`) or create with
`--firmware bios`. `GET` VM (`vmtool` API) reports `firmware` (`efi` or
`bios`). Existing VMs are not changed.

### Disk cache (`unsafe` by default)

A clone that vmtool creates gets `cache='unsafe'` on its disk driver, so guest
writes are fast. `unsafe` keeps guest writes that the guest believes are
flushed in host memory; a host crash or power loss can corrupt the VM's disk.
Pick `none` for a VM whose data matters:
`vmtool create --disk-cache none host/name image` (modes: `none`, `writeback`,
`writethrough`, `unsafe`; the TUI create form has a Disk cache field).

`--noclone` gets no cache attribute (the hypervisor's default) unless you name
a mode: `unsafe` on a base image would risk the image itself. Extra disks, and
disks added later by `add-disk`, take the mode of the VM's boot disk
(`vda`); `migrate-disk` keeps it. Existing VMs are not changed. `GET` VM
reports `disk_cache`; `vmtool list` is unchanged.

## Guest SSH and `ssh-config`

vmtool's own commands name a VM `host/vm`. Other tools (ssh, scp, rsync, sftp)
use one SSH name, `<vm>.<host>.vmtool`, split at the last dot before
`.vmtool`: VM names may contain dots (`a.b.jetfire.vmtool` is VM `a.b` on
`jetfire`), host names may not (a dotted `name:` or `hosts:` key fails config
load). A VM's credentials (bot key, password) live in
`.machines/<vm>.<host>.vmtool/`; the old `.machines/<host>/<vm>/` folders are
never read, moved or deleted by vmtool. Names ending in `.vmtool` cannot be
metal/alien machine names.

```bash
./vmtool ssh-config >> ~/.ssh/config     # one fixed block, `Host *.vmtool`
ssh web.jetfire.vmtool
scp f vt-r7.veritech03.vmtool:/tmp/x
rsync -a dir/ web.jetfire.vmtool:/srv/
```

Remove an old `Host */*` block first: it has no credentials folder behind it
any more. The block runs `vmtool tunnel %n` in vmtool's working directory
(`vmtool tunnel` takes `host/vm` too) and needs `VMTOOL_TOKEN` in the
environment or in `.env` there. It uses `%n` because ssh lowercases `%h`.

## Metal and alien machines

vmtool can also drive machines its libvirt does not manage: **metal** (physical
boxes) and **alien** (VMs on another hypervisor or a cloud). They are
SSH/Ansible targets only — exec, ssh, playbook, reboot. There is no create,
start, power-off, or disk management. Each one is registered in
`.machines/<name>/machine.yml` (`kind`, `host`) next to its credentials (VM credentials are in `.machines/<vm>.<host>.vmtool/`), and
shows up in the TUI list with its kind.

Requirements: an Ubuntu-compatible OS, and SSH as `packer`/`packer` with
passwordless sudo for onboarding. `prep_machine.yml` sets that up from any
existing admin account; aliens built from a vmtool image already have it.

```bash
ansible-playbook -i '10.0.0.42,' -u <admin> -K ansible/playbooks/prep_machine.yml
./vmtool metal add nuc1 10.0.0.42                         # probe, register, run setup_bot.yml
./vmtool metal add cloud1 203.0.113.7 --kind alien --commission  # cloned alien: new identity first
./vmtool metal ls                                         # probes each machine over SSH
./vmtool metal exec nuc1 uname -a
./vmtool metal playbook nuc1 vm_upgrade.yml
./vmtool metal reboot nuc1
./vmtool metal rm nuc1                                    # forget locally; the machine is untouched
```

Onboarding runs `setup_bot.yml` with `vmtool_kind=<kind>`: it replaces packer
with `bot` and key auth, but on machines it disables password login for `bot`
only and leaves display-manager autologin alone, since the machine has an
owner. On a VM it disables password login for every user with
`/etc/ssh/sshd_config.d/01-vmtool-no-password.conf`, which sorts before
cloud-init's `50-cloud-init.conf` (sshd keeps the first value it reads),
and checks the effective policy with `sshd -T` before it removes packer.
Re-running it fixes a VM set up by an older version. `commission.yml` is refused for machines except through
`add --kind alien --commission`. A VM delete or create never touches a
registered machine's `.machines` entry, and VM and machine names cannot collide.

## Troubleshooting

### Two VMs show the same IP

Clones of one image share its DHCP client id when they share its machine-id,
so the DHCP server hands them the same lease. VMs cloned from an image built
before `packer_images` ended clean for cloud-init keep the image's
identity: the same machine-id, SSH host keys and hostname. Fix one with

```bash
vmtool playbook host/vm commission.yml
```

and rebuild the image so new clones are right from the start: `just generate
<template>` in `packer_images`. The next `create` re-uploads it, since the hash
differs.

### A new VM never gets an IP

`create` fails with `timed out waiting for IP` (the whole message says the VM
exists and is running and names `--no-wait`; the VM is not removed). Two independent causes, both of
which a fresh Omarchy host hits:

**The host firewall drops the guest's DHCP request.** ufw defaults to deny
incoming and deny routed, and it does not log broadcasts at its default log
level, so the DISCOVER vanishes without a trace — `virsh net-dhcp-leases
default` stays empty and dnsmasq logs nothing. libvirt's own nftables table
cannot save you here: at the same hook, a drop in ufw's table wins.
`just prereqs` adds the rules; by hand:

```bash
sudo ufw allow in on virbr0 to any port 67 proto udp
sudo ufw allow in on virbr0 to any port 53
sudo ufw route allow in on virbr0
sudo ufw route allow out on virbr0
```

**The guest's netplan names an interface that does not exist.** Packer attaches
the NIC directly to `pcie.0`, so the installer sees `ens3`; libvirt puts it
behind a pcie-root-port, so the same image boots with `enp1s0`. Netplan then
configures nothing and the guest never sends DHCP at all. Images built from
`packer_images/qemu_images/ubuntu2404` ship `/etc/netplan/99-wildcard.yaml`
(`match: name: "en*"`) with cloud-init's network layer disabled so it cannot
rewrite it — check both inside a stuck image:

```bash
sudo qemu-nbd -c /dev/nbd0 /var/lib/libvirt/images/<image>.qcow2   # modprobe nbd first
sudo mount /dev/nbd0p2 /mnt && ls /mnt/etc/netplan/
sudo journalctl -D /mnt/var/log/journal | grep -i networkd         # "Configuring with ..." or nothing
```

A guest that logs `enp1s0: Configuring with /run/systemd/network/10-netplan-*`
but still gets no lease is the firewall case, not this one.
