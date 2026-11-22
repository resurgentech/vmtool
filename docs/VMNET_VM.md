# The `vmnet` VM

How the lab's `vmnet` machine was built, so it can be rebuilt. It was made
by hand on 2026-11-21 and 2026-11-22; nothing here is automated. Where a
command below differs from what was actually typed, the text says so.

## What it is

A vmtool network can span several hosts, so VMs on different hosts share
one private network. One machine serves each such network: it hands out
the addresses and names (DHCP, DNS) and is the network's way out to the
internet (NAT). vmtool calls that machine the network's *home*. See
"A network home (a host that runs no VMs)" in the [README](../README.md).

`vmnet` is the lab's home: a small VM on sdf1 that runs libvirt and the
vmtool daemon and no VMs of its own.

| | |
|---|---|
| Runs on | sdf1, as libvirt domain `vmnet` (autostart on) |
| Address | `vmnet.labtwo`, 192.168.42.3/24, static |
| Gateway, DNS | 192.168.42.1, 192.168.42.2 (MAAS), search `labtwo` |
| Card | macvtap on sdf1's `eno4`, bridge mode, MAC `52:54:00:2a:00:03` |
| Size | 1 vCPU, 1 GiB memory, 10 GiB disk |
| System | Ubuntu 24.04 minimal cloud image, UEFI, q35 |
| Disk | `/mnt/secondarypool/vm/vmnet.qcow2` on sdf1 |
| Seed | `/mnt/secondarypool/vm/vmnet-seed.iso`, attached as virtio disk `vdb` |
| Seed sources | `~/vmnet-seed/` on sdf1 (`user-data`, `meta-data`, `network-config`) |
| Login | `ssh ubuntu@vmnet.labtwo`, key only, passwordless sudo |
| vmtool | daemon on port 9473; `hosts.vmnet.vms: false` in the client's `config.yml` |

It is modelled on the `maas` VM on the same host, which has its own
address on the lab LAN the same way.

## 1. sdf1 must be able to talk to its own guests

A macvtap guest can reach every machine on the LAN except the host it
runs on. The store runs on sdf1, so without a fix `vmnet` cannot reach it.
The fix is a macvlan interface on sdf1 that carries sdf1's LAN address;
interfaces of that kind on one port can talk to each other. It was made on
2026-11-22 and is described in [section 7](#7-the-macvlan-shim-on-sdf1).
Do that first on a fresh sdf1.

## 2. Build the VM on sdf1

Needs `virt-install`, `genisoimage` and `qemu-img` on sdf1 (all were
already there).

**Seed files.** In `~/vmnet-seed/` on sdf1:

`meta-data`

```yaml
instance-id: vmnet-trial-1
local-hostname: vmnet
```

`user-data` (put your own public key in; no password is set)

```yaml
#cloud-config
hostname: vmnet
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - <the workstation's public key>
ssh_pwauth: false
```

`network-config`

```yaml
version: 2
ethernets:
  lan:
    match:
      macaddress: '52:54:00:2a:00:03'
    addresses: [192.168.42.3/24]
    routes:
      - to: default
        via: 192.168.42.1
    nameservers:
      addresses: [192.168.42.2]
      search: [labtwo]
```

**Seed image.** cloud-init reads any disk whose volume label is `cidata`:

```bash
cd ~/vmnet-seed
genisoimage -quiet -output seed.iso -volid cidata -joliet -rock \
  user-data meta-data network-config
```

**Disk.** The minimal cloud image, checked against Ubuntu's checksum file,
then grown to 10 GiB:

```bash
U=https://cloud-images.ubuntu.com/minimal/releases/noble/release
curl -fsSL -o img.part  $U/ubuntu-24.04-minimal-cloudimg-amd64.img
curl -fsSL -o SHA256SUMS $U/SHA256SUMS
sha256sum img.part        # compare with the image's line in SHA256SUMS

D=/mnt/secondarypool/vm
sudo install -m 0664 -o libvirt-qemu -g kvm img.part $D/vmnet.qcow2
sudo install -m 0664 -o libvirt-qemu -g kvm seed.iso $D/vmnet-seed.iso
sudo qemu-img resize -q $D/vmnet.qcow2 10G
```

**Define and start it.**

```bash
virt-install --connect qemu:///system --name vmnet \
  --memory 1024 --vcpus 1 --cpu host-passthrough --machine q35 --boot uefi \
  --osinfo ubuntu24.04 --import \
  --disk path=$D/vmnet.qcow2,format=qcow2,bus=virtio \
  --disk path=$D/vmnet-seed.iso,format=raw,bus=virtio,readonly=on \
  --network type=direct,source=eno4,source.mode=bridge,model=virtio,mac=52:54:00:2a:00:03 \
  --graphics none --console pty,target.type=serial --rng /dev/urandom \
  --autostart --noautoconsole
```

This exact command has not been run. The VM was first made with the seed
as a SATA CD (`device=cdrom,bus=sata`), and the minimal image's kernel has
no driver for that, so the VM came up with no settings. The seed was then
moved to a virtio disk, which is what the command above does in one go:

```bash
virsh -c qemu:///system attach-disk vmnet /mnt/secondarypool/vm/vmnet-seed.iso vdb \
  --config --targetbus virtio --driver qemu --subdriver raw --mode readonly
```

**Check.** From the workstation, about a minute after start:

```bash
ssh ubuntu@192.168.42.3 hostname     # vmnet
```

To start over, change `instance-id` in `meta-data` (cloud-init applies a
seed once per instance id), rebuild the seed image and recreate the disk.

## 3. Its DNS name

Lab names are declared in the `labtwo_core` repo, `infra/tf/dns.tf`, and
applied from there; never by hand in MAAS. `vmnet` is an A record to
192.168.42.3. vmtool's client must list the host by name, because the
daemon's certificate carries the name and not the address.

After the record exists, the workstation needs the VM's SSH host key under
the name as well as the address (`ssh ubuntu@vmnet.labtwo` once and accept
it, after comparing with the key already trusted for 192.168.42.3).

## 4. The lab's CA on `vmnet`

The store is served with a certificate from the lab's own CA,
`hulbert.local`. A machine that does not trust that CA cannot talk to the
store ("unable to get local issuer certificate"). The other hosts got the
CA from `ansible_labtwo`'s `selfsigned_cert` role; `vmnet` is not in that
inventory, so on 2026-11-22 the CA's public certificate was copied by hand
from veritech03:

```bash
scp veritech03.labtwo:/usr/local/share/ca-certificates/hulbertlocal.ca.crt .
scp hulbertlocal.ca.crt ubuntu@vmnet.labtwo:/tmp/
ssh ubuntu@vmnet.labtwo 'sudo install -m 0644 -o root -g root \
  /tmp/hulbertlocal.ca.crt /usr/local/share/ca-certificates/hulbertlocal.ca.crt \
  && sudo update-ca-certificates'
```

The file holds only the public certificate. A running vmtool daemon reads
the trust store once at start, so restart it afterwards
(`sudo systemctl restart vmtool-daemon` on `vmnet`).

The cleaner way, not done: add `vmnet` to `ansible_labtwo`'s inventory and
run `selfsigned_cert` on it.

## 5. Make it a vmtool host

All from the vmtool checkout on the workstation. `just deploy` needs an
OpenBao session (`VAULT_ADDR`, `VAULT_TOKEN`) for the daemon's certificate
and the host token.

```bash
# the client's entry; vms: false marks it as a home that runs no VMs
./vmtool config set-host vmnet --address vmnet.labtwo --ssh-user ubuntu --vms=false

# packages, libvirt, the vmtool user, the firewall port; no QEMU, no pools
just prereqs vmnet -e '{"vmtool_vm_host":false}'

# a store token for the host (in ansible_labtwo), then render it on the store
roles/vmtool_store/make_secrets.sh hosttokens vmnet
ansible-playbook -i inventories/labtwo.yml playbooks/sdf1_vmtool_store.yml

# the daemon, its certificate, the helpers and the token
just deploy vmnet
```

## 6. Checks

```bash
./vmtool hosts                 # vmnet (network home), reachable, same API as the others
./vmtool inventory             # vmnet in HOSTS, with a recent LAST REPORT
ssh ubuntu@vmnet.labtwo 'curl -s https://vmtool-store.labtwo/version'   # {"version":"…"}
```

A full check of a spanning network, as run on 2026-11-22:

```bash
./vmtool net create vmnet x1
./vmtool net join veritech03 x1
./vmtool net join veritech04 x1
./vmtool create veritech03/xa ubuntu2604.qcow2 --pool nvme-thin --network x1
./vmtool create veritech04/xb ubuntu2604.qcow2 --pool nvme-thin --network x1
./vmtool exec veritech03/xa -- ping -c2 xb.x1.test
# then: delete both VMs, net leave both hosts, net delete vmnet x1
```

Not yet checked: that `vmnet` restores its links to the other hosts by
itself after a reboot.

## 7. The macvlan shim on sdf1

Made by hand on sdf1 on 2026-11-22. sdf1's LAN address 192.168.42.50 moved
from `eno4` onto `lab0`, a macvlan (bridge mode) on top of `eno4`. Since
then sdf1 and its macvtap guests (`maas`, `vmnet`) reach each other, and
sdf1 resolves lab names through MAAS DNS. Do it from sdf1's console or
through its other address, 192.168.1.50 on `br1`, not through
192.168.42.50.

Netplan cannot describe a macvlan, so the interface is two systemd-networkd
files and a drop-in that tells `eno4` to carry it.

`/etc/systemd/network/05-lab0.netdev`

```ini
[NetDev]
Name=lab0
Kind=macvlan
MACAddress=0e:c4:7a:93:c7:a9

[MACVLAN]
Mode=bridge
```

The MAC is `eno4`'s own with the "locally assigned" bit set, fixed so it
does not change at every boot.

`/etc/systemd/network/05-lab0.network`

```ini
[Match]
Name=lab0

[Network]
Address=192.168.42.50/24
DNS=192.168.42.2
Domains=maas
```

`/etc/systemd/network/10-netplan-eno4.network.d/lab0.conf`

```ini
[Network]
MACVLAN=lab0
```

In `/etc/netplan/99-vmtool-br1.yaml` the `eno4` block lost its address and
DNS lines and became:

```yaml
    eno4:
      dhcp4: false
      link-local: []
      optional: true
```

`optional: true` keeps boot from waiting two minutes for an address `eno4`
will never have. Then:

```bash
sudo netplan generate && sudo netplan apply
ip -br addr show lab0          # 192.168.42.50/24
ping -c2 192.168.42.3          # vmnet answers
resolvectl query vmnet.labtwo  # 192.168.42.3, via link lab0
```

To undo: restore the netplan file (a copy from before the change is at
`/root/99-vmtool-br1.yaml.before-lab0` on sdf1), remove the three files,
`sudo netplan apply`, `sudo ip link del lab0`.

Not yet checked: that `lab0` comes back after an sdf1 reboot.

## What is by hand, and where it would belong

| Step | Today | Would belong in |
|---|---|---|
| The VM (section 2) | typed on sdf1 | a role in `ansible_labtwo`, with the seed files |
| DNS name | `labtwo_core`, `infra/tf/dns.tf` | already there |
| Lab CA on `vmnet` | copied by hand | `ansible_labtwo`'s `selfsigned_cert` role |
| vmtool host setup | `just prereqs`, `just deploy` | already in this repo |
| sdf1's macvlan shim | typed on sdf1 | a role or a note in `ansible_labtwo` |
