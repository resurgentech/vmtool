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
./vmtool add-disk jetfire/web 50                        # attach a new empty disk (vdb, vdc, …)
./vmtool reboot jetfire/web                             # ACPI reboot, stays defined
./vmtool list                                           # every host (5 s each; a dead host is an ERROR row)
./vmtool list jetfire                                   # one host
./vmtool hosts                                          # reachability, API version, cert days left
./vmtool delete <host>/<name>
./vmtool delete jetfire/scratch --noclone               # undefine only, keep the disk
./vmtool server                                         # HTTPS REST on 127.0.0.1:9473; GET / is Swagger UI
```

## Metal and alien machines

vmtool can also drive machines its libvirt does not manage: **metal** (physical
boxes) and **alien** (VMs on another hypervisor or a cloud). They are
SSH/Ansible targets only — cmd, ssh, playbook, reboot. There is no create,
start, power-off, or disk management. Each one is registered in
`.machines/<name>/machine.yml` (`kind`, `host`) next to its credentials, and
shows up in the TUI list with its kind.

Requirements: an Ubuntu-compatible OS, and SSH as `packer`/`packer` with
passwordless sudo for onboarding. `prep_machine.yml` sets that up from any
existing admin account; aliens built from a vmtool image already have it.

```bash
ansible-playbook -i '10.0.0.42,' -u <admin> -K ansible/playbooks/prep_machine.yml
./vmtool metal add nuc1 10.0.0.42                         # probe, register, run setup_bot.yml
./vmtool metal add cloud1 203.0.113.7 --kind alien --commission  # cloned alien: new identity first
./vmtool metal ls                                         # probes each machine over SSH
./vmtool metal cmd nuc1 uname -a
./vmtool metal playbook nuc1 vm_upgrade.yml
./vmtool metal reboot nuc1
./vmtool metal rm nuc1                                    # forget locally; the machine is untouched
```

Onboarding runs `setup_bot.yml` with `vmtool_kind=<kind>`: it replaces packer
with `bot` and key auth, but on machines it disables password login for `bot`
only and leaves display-manager autologin alone, since the machine has an
owner. `commission.yml` is refused for machines except through
`add --kind alien --commission`. A VM delete or create never touches a
registered machine's `.machines` entry, and VM and machine names cannot collide.

## Troubleshooting

### A new VM never gets an IP

`create` prints `timed out waiting for IP`. Two independent causes, both of
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
