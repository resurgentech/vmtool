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

On a hypervisor never set up, `--check` cannot show the `vmtool` directories'
ownership or the `vmtool` pool before the `vmtool` user exists; those tasks are
skipped, with one line saying so.

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

#### Fast block disks (opt-in NVMe stripe)

On a Debian/Ubuntu hypervisor with blank NVMe drives, host setup can also build
an md RAID 0 stripe, an LVM volume group on it and a libvirt `logical` pool
`nvme` (use it with `--pool nvme`). This is **destructive by nature, runs only
with the drives named, and refuses non-blank drives**: nothing happens unless
you pass `vmtool_nvme_drives`, a list of at least two `/dev/disk/by-id/` paths.
Each drive must be whole, with no partition, no signature, no holder, and not
mounted or swap; otherwise the play fails and changes nothing. If the volume
group already exists it is adopted and the drives are not touched; an existing
pool of the same type and volume group is adopted too, and one that differs
makes the play fail.

```bash
just prereqs hv2 -e '{"vmtool_nvme_drives":["/dev/disk/by-id/nvme-A","/dev/disk/by-id/nvme-B"]}'
```

The other variables are `vmtool_nvme_vg` (`vmtool-nvme`), `vmtool_nvme_md`
(`vmtool-nvme`, the array `/dev/md/<name>`) and `vmtool_nvme_pool` (`nvme`).
Logical volumes that vmtool did not create are yours; the play never removes
any of them. The only volume it removes is its own tagged scratch volume (see
[Old data and discard](#old-data-and-discard)).

**Thin pool (opt-in, `vmtool_nvme_thin`).** The same play can also add an LVM
thin pool to that volume group, for [thin clones](#thin-pools-clones-from-templates).
It is off unless `vmtool_nvme_thin` is true, and it needs `vmtool_nvme_drives`
too: name the array's drives again, and the existing volume group is adopted
and the drives are not written. The thick pool `nvme` and every volume on it
are not touched.

```bash
just prereqs <host> -e '{"vmtool_nvme_drives":["/dev/disk/by-id/nvme-PLACEHOLDER_1","/dev/disk/by-id/nvme-PLACEHOLDER_2"],"vmtool_nvme_thin":true}'
```

| Variable | Default | Meaning |
|---|---|---|
| `vmtool_nvme_thin` | `false` | make the thin pool |
| `vmtool_nvme_thin_pool` | `nvme-thin` | the thin-pool LV's name, and the name vmtool lists it by (`--pool nvme-thin`) |
| `vmtool_nvme_thin_size` | `70%FREE` | any `lvcreate -l` or `-L` value: 70 % of the volume group's free space when the pool is made, or a size such as `2T` |
| `vmtool_nvme_thin_chunk` | `512k` | chunk size |
| `vmtool_nvme_thin_metadata` | `8G` | metadata size |

Check `vgs vmtool-nvme` for free space first: the pool takes its share of what
is free at that moment. The play installs `thin-provisioning-tools` and an LVM
profile (`/etc/lvm/profile/vmtool-thin.profile`), enables `lvm2-monitor`, and
makes the pool tagged `vmtool_thinpool`, with zeroing off (zeroing halves
sequential writes), monitoring on, and autoextend at 80 % by 10 % of the pool,
which can take only what the volume group still has free. The pool queues
writes when full (LVM's default; vmtool sets it with `lvchange
--errorwhenfull n` only if the host's LVM made it `error`).
A pool of that name that already exists with the tag is adopted (zeroing and
profile corrected if they differ). **One that exists without the tag, or that
is not a thin pool, makes the play fail, naming it, and changes nothing**: a
trial pool made by hand has to be removed by hand, or the pool named
differently with `vmtool_nvme_thin_pool`. The play never removes or reduces a
pool, a volume group or a volume of yours, and wipes nothing. It does discard
once, when it makes a pool, and it sets LVM's `issue_discards`; see
[Old data and discard](#old-data-and-discard).

##### Old data and discard

The pool is made with zeroing off, so a block it hands out keeps what its last
owner left on it. vmtool keeps one VM from reading another's deleted data by
**discarding** (telling the drives a block is free) instead of wiping:

- **Thin and export volumes:** the root helper's `remove` runs `blkdiscard`
  on the volume's whole device, and only then `lvremove`. A volume LVM
  reports as open is refused; an inactive one is activated with `lvchange -ay
  -K` first. dm-thin frees, and passes down to the drives, only blocks no other
  volume shares, so removing a clone, or a template that has clones, never
  touches the others' data.
- **Thick volumes** (pool `nvme`, and export volumes): host setup sets
  `issue_discards = 1` in the `devices` section of `/etc/lvm/lvm.conf` and
  checks it with `lvmconfig devices/issue_discards`. It is LVM-wide, so it
  covers libvirt's `vol-delete` too. It does not apply to thin volumes (the
  helper does those). Thick disks removed before `issue_discards` was set are
  not covered.
- **Once, when the pool is made:** the drives hold old data, so before `lvcreate
  --type thin-pool` the play makes a scratch volume `vmtool-discard-scratch`
  (tagged `vmtool_scratch`) over all of the volume group's free space,
  discards it and removes it. This runs only when the pool does not exist yet,
  never against an existing pool. An untagged volume of that name fails the
  play; a tagged leftover is reused. If its discard fails, the play stops and
  leaves it: remove it with `lvremove` once nothing has it open. Pools are made
  with `--discards passdown`, and an adopted pool is set to it (a pool at
  `ignore` must be inactive to change).

**A failed discard keeps the volume.** Nothing un-discarded goes back to the
pool. The helper says:

```
vmtool-thin: discard of VG/NAME failed (<reason>); volume kept so its data does not return to the pool; retry: sudo -n /usr/local/libexec/vmtool-thin remove VG NAME
```

Run that retry line on the host once the cause is gone (an open volume,
usually). A half-made export volume whose discard fails stays, tagged, and the
24-hour expiry retries it; leftover building volumes are retried when the
daemon starts.

**Two assumptions.** Each drive is assumed to be one that returns zeros after
a discard; this is not tested at setup, and there is no automatic fallback to
wiping (the manual check below tests one drive set). And only removals that go through vmtool and libvirt are covered:
a volume removed by hand (outside vmtool and libvirt) is not.

**Check a drive by hand**, on a scratch volume only, never on a VM disk or a
pool. With a small free volume (placeholders `vg-test`, `scratch1`):

```bash
sudo lvcreate -L 1G -n scratch1 --zero n --wipesignatures n vg-test
sudo dd if=/dev/urandom of=/dev/vg-test/scratch1 bs=1M count=64 oflag=direct
sudo blkdiscard /dev/vg-test/scratch1
sudo cmp -n 67108864 /dev/vg-test/scratch1 /dev/zero && echo "reads zeros"
sudo lvremove vg-test/scratch1
```

`cmp` printing a difference means the drive returned old data after a discard:
the assumption does not hold for it, so tell the people who run the host.

**A pool made before vmtool discarded at pool creation** (before 2026-11-20)
was not discarded when it was made,
so old data can sit in the volume group's free space (which autoextend takes).
Discard that free space once, through a scratch volume only, never the pool or
a VM disk. Run it while the pool has headroom, since autoextend may need that
space meanwhile, or use a smaller `-L` size than `100%FREE` and repeat:

```bash
sudo lvcreate -l 100%FREE -n vmtool-discard-scratch --zero n --wipesignatures n --addtag vmtool_scratch vg-test
sudo blkdiscard /dev/vg-test/vmtool-discard-scratch
sudo lvremove vg-test/vmtool-discard-scratch
```

These commands destroy the data on the volume they name, which here is the
scratch volume and nothing else.

Then put the daemon on it, from the workstation:

```bash
just prereqs hv1     # once per host: vmtool user, /opt/vmtool, pool
just deploy hv1      # build, certify, configure, start
```

`just deploy <host>` needs two settings, `VMTOOL_DOMAIN` (the DNS domain of
the host certificates) and `BAO_CA_PATH` (the OpenBao kv path of the CA), with
no default: set them in the environment or in `deploy.env` (see [Deploy
settings](#deploy-settings-and-just-settings)). A missing one stops it by name
before anything is copied, built or installed, as does a Go that is too
old on the host or a `just prereqs <host>` that has not run (no `vmtool` user
or no `/opt/vmtool`). It copies the tracked files to
`~/.cache/vmtool-deploy/src` on the host and builds there as your login user
(nothing is built with sudo; the build is stamped with the checkout's commit,
plus `+dirty` for a tracked change, so `vmtool inventory` shows it as the
daemon's version), issues the host's certificate, writes
`config.yml` (role `hypervisor`, the token's hash only; the token goes over
ssh stdin, never in an argument and never through a sudo prompt) and then
installs everything with a single `sudo`, which may ask for your password
once. The service runs as `vmtool`, never as your user. Its unit is
`vmtool-daemon.service` and it listens on the host's `port` (default 9473;
see [One daemon per host](#one-daemon-per-host)):

```
/opt/vmtool/                 vmtool:vmtool 0750  working directory and HOME
  vmtool                     0755  the daemon
  config.yml                 0600  role: hypervisor, listen 0.0.0.0:<port>, token hash
  tls/<host>.<domain>.crt    0644
  tls/<host>.<domain>.key    0600
  image-manifest.yml         written by the daemon
/etc/systemd/system/vmtool-daemon.service   User=vmtool, ExecStart=/opt/vmtool/vmtool server
/usr/local/libexec/vmtool-thin              root:root 0755  the one root helper, for thin pools
/etc/sudoers.d/vmtool-thin                  root:root 0440  lets user vmtool run that one path as root
```

The helper and its sudoers rule are new with API 2.4.0 and install on every
deployed host, whether or not it has a thin pool (without one the helper
refuses everything). The rule is checked with `visudo -cf` under a temporary
name before it is moved into place; if the check fails nothing of the daemon is
updated. See [Thin pools](#thin-pools-clones-from-templates).

The staged `config.yml` and key are removed after the install. `just cert
<host>` on its own renews the certificate the same way (into
`/opt/vmtool/tls`, owner `vmtool`) and restarts the daemon if it is running; it
needs `VMTOOL_DOMAIN` and `BAO_CA_PATH` too (`just cert` and `just install`
stop by name without them, before any certificate is issued).

Before it installs, deploy checks that nothing but its own daemon holds the
port, and refuses if something else does. It also stops, disables and removes
an older `vmtool.service` that ran from `/opt/vmtool` (the daemon's name
before `vmtool-daemon.service`); a `vmtool.service` that runs from anywhere
else is left alone.

#### A network home (a host that runs no VMs)

A network that spans hosts has a *home*: the host whose libvirt does the
routing, addresses and names. The home can be a small host or VM that runs no
VMs of its own (here `home1`). Set it up with the VM switch off, then deploy as
usual:

```bash
just prereqs home1 -e '{"vmtool_vm_host":false}'
just deploy home1
```

With `vmtool_vm_host` false the playbook still installs libvirt, dnsmasq, the
`vmtool` user and directories, and what `just deploy` builds with (Go, the
libvirt development files, a C compiler). It leaves out QEMU, the UEFI
firmware, the storage pools, the NVMe stripe and the `/dev/kvm` check. Naming
`vmtool_nvme_drives` or `vmtool_extra_pools` with it is an error. The default is
true, so an existing host is prepared exactly as before. A home needs no QEMU:
libvirt manages its networks without it.

A home can be a small VM on one of the hypervisors (1 vCPU, 1 GiB and a 10 GiB
disk is enough). Give it a static address on the LAN rather than a vmtool
network, since it serves that network's DHCP and DNS. Put its name in your DNS
and list the host by name in `config.yml`, because the daemon's certificate
carries the name and not the address. A macvtap guest cannot reach its own
hypervisor, so if the VM must reach services on the hypervisor that runs it,
add a macvlan interface on that hypervisor that carries its LAN address.

Then mark the host in the workstation's `config.yml`:

```yaml
hosts:
  home1: { address: 192.0.2.13, ssh_user: ubuntu, vms: false }
```

A `vms: false` host is a full daemon host: its inventory report is an empty
list, and networks and links work on it. `vmtool create` on it is refused
("host home1 is a network home (vms: false in config.yml) and runs no VMs"),
`vmtool hosts` writes `home1 (network home)`, `vmtool list` shows one
`home1  network home: runs no VMs` row, and the TUI and `GET /v1/hosts`
(`network_home`) carry the same mark.

`vmtool config set-host home1 --vms=false` edits only that key of the existing
entry (`vms: false`; `--vms=true` removes it) and keeps the rest of the entry
and of `config.yml`; for a host with no entry it is refused. With `--address`
and `--ssh-user` the whole entry is written as before, and `--vms` left out
keeps whatever the entry has.

The home's own daemon refuses too, so a direct REST call cannot make a VM
there. `just deploy` reads the client's `hosts.<h>.vms` (`vmtool config
host-vms <h>` prints it) and, for `vms: false`, writes `network_home: true`
into the daemon's `config.yml` (`vmtool config write --network-home`). Such
a daemon answers 409 "home1 is a network home and runs no VMs" on
`defineVM`, `cloneVolume`, `createVolume` and `createTemplate`, before any
libvirt call; networks, reservations, links and the inventory are served as
before. The key is configuration, never a note in libvirt, and `config.yml`
is rewritten on every deploy: after you change the client's `vms:`, deploy
that host again.

### One daemon per host

Every host in `hosts:` is reached through its daemon. Per host:

- **The unit** is `vmtool-daemon.service`, on every host, this machine
  included.
- **The port** is the host's `port` in `config.yml`, default 9473. `just
  prereqs` (the firewall rule) and `just deploy` (the listen address and
  the `/version` check) read it from there: `vmtool config host <host>`
  prints `address ssh_user port`.
- **The loopback server** is another thing: `just install` runs
  `vmtool.service` on `127.0.0.1:9473` for this machine's own REST and TUI,
  and it stays beside a daemon's unit. `just install` refuses on a host
  whose `vmtool.service` is a daemon's (an older unit run from
  `/opt/vmtool`): run `just deploy <host>` first, which replaces it. Any
  other `vmtool.service` is replaced as before. Install keeps every `hosts:`
  entry and `image_dir` in `config.yml`.

### This machine, with or without a daemon

A machine's own name is `name:` in `config.yml`, else `hostname -s`. A
workstation may list its own name under `hosts:`, and so give itself a daemon,
reached like any other host. The three cases:

| `hosts:` lists the own name? | Local libvirt? | What happens |
|---|---|---|
| yes | not used | Every operation on the own name goes to that daemon. Local libvirt is never opened. |
| no | yes | Operations on the own name use local libvirt, opened only when an operation names the own name (never at start-up, never for a command on another host). |
| no | no (a laptop) | Listed hosts work. `vmtool hosts` shows the own name as unreachable. `vmtool list` leaves it out with one `note:` line on stderr. A command that names it fails with `no daemon listed for <name> and no local libvirt`. |

A hypervisor's own `config.yml` may not list itself: it fails to load. The
binary still links libvirt's client library, even where it never opens it.

To give this machine a daemon:

```bash
just prereqs ws1     # ws1 is this machine's name: runs locally, no ssh
just deploy ws1      # builds here, one sudo, installs vmtool-daemon.service
```

For the own name both run locally: no ssh, and deploy does every step in a
local shell with a single `sudo`. If `config.yml` has no `hosts:` entry for
it, the address is `VMTOOL_SELF_ADDRESS` (default `<host>.<domain>`, or
`127.0.0.1` when `VMTOOL_SELF_ADDRESS` is unset and that name has no DNS
record, with one line on stderr saying so) and the
port is `VMTOOL_SELF_PORT` (default **9474**, clear of `just install`'s 9473).
After the daemon answers `GET /version`, deploy runs `vmtool config
set-host`, which writes the entry. An entry you listed yourself is used as it
stands and is never rewritten. `just prereqs` for the own name takes its port
the same way.

```bash
VMTOOL_SELF_PORT=9480 just deploy ws1    # not yet listed: daemon on 9480
```

### Base images (`image_dir`)

`image_dir` in `config.yml` is a folder of base images on the machine that
runs the command. A relative path is taken from the directory holding
`config.yml`; an absolute one is used as given.

```yaml
image_dir: images       # <config dir>/images
```

`create <host>/<vm> <image>` looks the image up in this order:

1. the local libvirt pools, only when local libvirt is in use (the own name
   has no daemon);
2. `image_dir`: a regular file named `<image>`, then `<image>.qcow2`. A
   symlink to a regular file counts; a directory does not.

The file is then sent to the host's daemon and cached there (see
[`spec/README.md`](spec/README.md#image-cache)). `just generate <template>`
in `packer_images` copies its image to `$VMTOOL_IMAGE_DIR` (default
`<repo>/images/`, gitignored; no sudo), so set `image_dir` to that folder.

### The image and credential store (optional)

`vmtool-store` is a separate service (`cmd/vmtool-store`) that keeps two
things in one place: base images, and each guest's credentials (the VM's
user, password and SSH key, and the same for metal machines). Without it,
images live in `image_dir` and credentials in `.machines/` on the machine
that created the VM, as before. The store is optional; nothing below is
needed to use vmtool.

**The store token opens every guest's credentials.** A token that the store
accepts can read the password and private key of every VM and metal machine
in it. Treat it like the keys it guards: one named token per caller, kept in
OpenBao, never in `config.yml`, never in git.

#### Using it from a client

Name the store in `config.yml` (https only, no credentials in the URL). The
URL may carry a path, when the store serves under a `path_prefix` (see
[Building and deploying the store](#building-and-deploying-the-store)):

```yaml
server:
  url: https://proxy.example/vmstore
```

The client's token is `VMTOOL_SERVER_TOKEN`, read like `VMTOOL_TOKEN`: from
the environment, else from `.env` in the working directory (a real
environment variable wins), else the key in the token file (see
[The client's key](#the-clients-key)). It is never read from `config.yml`. The
tokens are one per caller; if you keep them in OpenBao, one field per caller
name (for example under `kv/example/vmtool_store/tokens`). Fetch your own with
`bao kv get -mount=kv -field=<your name> example/vmtool_store/tokens`
and put it in `.env` as `VMTOOL_SERVER_TOKEN=<token>`.

Trust: the store is served over HTTPS by a reverse proxy with a certificate
issued under your CA, the same CA as the daemons. **That CA must be
in the system trust store of every client and of every hypervisor that
fetches images.** vmtool does no CA pinning and has no way to skip
verification.

Two modes, decided once at the start of each command that uses
credentials or base images, by one authenticated `GET /v1/whoami` with a
3 second limit:

- **Store reachable: store mode.** Credentials, metal records and base
  images come from the store. `.machines/<vm>.<host>.vmtool/` is a cache of
  the store's record (still 0700/0600, and plain `ssh`, `scp` and Ansible
  still read the key from it).
- **Not configured, no token, or not reachable: local mode.** Credentials
  stay in `.machines/` and images come from `image_dir` and the local pools,
  uploaded to the daemon as before. When `server:` is set, one stderr line
  says so: `proxy.example unreachable: using this machine's .machines/
  and image_dir` (with the host of your `server.url`; a missing or unusable
  token gives the same ending with the reason in place of `unreachable`).

If the store fails during a store-mode command, the command still
succeeds: credential writes land in `.machines/` marked pending, reads fall
back to the cache, and one line says the store could not be reached and the
credentials are only on this machine. There is no queue. Each cache folder
has a `.vmtool-source` marker, `store <uuid>` (a copy of the store's record)
or `pending <uuid>` (written here, not in the store; a folder with no marker
counts as pending). In store mode the first command that uses a VM compares
the folder with the VM's current UUID (reported by its daemon, API 2.3.0 or
newer):

- pending, and the UUID matches (or is unknown and the store has no record):
  the folder is pushed to the store and the marker becomes `store`, with one
  notice line;
- pending, but the VM was recreated (another UUID): the folder is stale. It
  is moved to `.machines/.stale/<folder>-<date>/`, never pushed and never
  deleted, with one notice line;
- `store`, but the store has lost the record: it is pushed back, with a
  notice.

The store answers 404 for a record whose UUID is not the VM's current one,
so a stale record is never used. After every playbook run on a VM, success or
failure, the client pushes the folder if it changed (so rerunning
`setup_bot.yml` completes a hand-off that failed). It fetches the record when
the folder is missing, and once more when `ssh` exits 255. `vmtool delete`
and the replace in `create` remove the record and the cache.

Base images, in store mode, `create` asks the store first:

1. If the store has the image, the client asks it for a download ticket and
   tells the hypervisor's daemon to fetch the image itself
   (`fetchImage`, `POST /v1/hosts/{host}/images/{name}/fetch`). The daemon
   downloads from the `store_url` in its own `config.yml` (`just deploy`
   writes it from the client's `server.url`), https only, system trust
   store, no redirects, through the same checked receive path as an upload
   (the SHA-256 and size must match). Progress shows as `fetching: N%` and the
   timings line names the stage `fetch` (a `create` by this machine's
   client; API 2.8.0). The image bytes never pass through the client. A daemon that already holds
   the image at the store's digest does nothing.
2. If the daemon cannot fetch (no `store_url`: 409; a daemon older than 2.3.0:
   404; the store not reachable from the host), the client **relays**: it
   streams the image from the store into the daemon's `uploadImage` with no
   local file, after one line saying so (progress `relaying: N%`, stage
   `relay`). A digest or size mismatch is not
   relayed; `create` fails.
3. If the store lacks the image but `image_dir` or a local pool has it, it is
   uploaded to the daemon as in local mode, with a line suggesting
   `vmtool images push`.

**A ticket opens one image only.** It is for one image at one digest,
read-only, good for one download and 15 minutes, held by the store as a hash in
memory only. Every other store route refuses it with 401. Daemons hold no
store token that opens credentials and no S3 key; each holds at most a host
token, which only reports its own VMs (below).

```bash
vmtool images push ./img1.qcow2 --name img1   # file into the store
vmtool images list --store                    # what the store holds
vmtool images pull img1                       # store to a file
```

`vmtool hosts` shows a row for the store: whether it answers, its version
and the name its token is known by. `just -f packer_images/justfile generate
<template>` pushes the built image to the store as `<template>.qcow2` when
`config.yml` has `server:` and `./vmtool` is built; when the store does not
answer, or there is no `server:`, it copies to `image_dir` with a line saying
so.

#### The inventory

The store keeps an inventory: one row per VM (host, libvirt UUID) with its
state, vCPUs, memory, address, disks, origin, comment and when it was last
seen, plus the hosts and the metal and alien machines. It is a **record of what each
daemon last said, not a live view**; `vmtool list` stays the live view. Its
use is to find which host holds a VM so that a person or a tool can wake that
host by other means.

```bash
vmtool inventory                 # hosts, VMs, templates, machines
vmtool inventory --host hv1      # one host's row, VMs and templates (machines are always listed)
vmtool inventory --gone          # also VMs their host no longer has, with removal dates
vmtool inventory --json
```

It needs the store (a `server:` block and `VMTOOL_SERVER_TOKEN`); without one,
or with one that does not answer, it says so and exits 1 (REST: 503). The TUI
has the same view under `I` (`g` toggles gone VMs, `r` refreshes). A VM marked
`*` had its disks unreadable on the host; its last known disks are shown. If
a VM's disks cannot be read, it is still listed.

**Who reports.** Each hypervisor's daemon sends its full VM list to the store
when it starts and after each change made through vmtool (create, delete,
start, stop, disk changes). A new VM's address arrives with one
follow-up report once it appears, for up to `ip_wait_seconds` after the VM
starts. The store orders a host's reports by its own clock, so a host with a
wrong clock is not silenced.

**Templates.** The same report carries the host's templates (see
[Templates](#templates-save-export-pull)), so a template can be found while its
host sleeps: `vmtool inventory` lists them as `host/name` with the kind (`image`
or `saved`), pool, the image an image template was built from, its size and
when it was made, and `--json` and `GET /v1/inventory` carry them as
`templates`. Only what the daemon knows is sent: a saved template has no source
image, and the creation time is left out when the host does not give one. The
store keeps each host's last list whole (a new list replaces it, an empty one
clears it) and, as for VMs, keeps it when the host stops reporting. A host row
shows `templates_reported`: false (`not reported` in the table) for a host whose
daemon is too old to send templates or has never been able to list them, so no
templates there is not read as none; a report without a template list never
changes what the store has. If a daemon cannot read its thin pools at a report,
it sends the VMs without templates, and the store keeps its last list.

**What the store does not do.** It never decides whether a host is up: no
heartbeat, no "stale" mark, and a sleeping or switched-off host keeps its VMs
listed as its last report left them. A report that is empty does not delete
anything early: a VM a host stops listing is marked gone, hidden from the
default listing, and its login is kept for **7 days** after that (the grace
period), then the store's daily sweep deletes the row and the login for good.
`vmtool delete` is explicit: it removes the login and the row at once.

**What is and is not tracked.** Changes made outside vmtool (a `virsh`
define or undefine, a VM added by hand) are not seen when they happen. They
are fixed at the next report (a daemon start, or a change made through
vmtool) or on touch: when a command names `host/vm`, the host says it has no
such VM and the store had it, the store delists it and the error says so, with
the date its login is kept until (a store that is down adds nothing). A host
that is off is not touched, so its VMs stay listed.

**A VM's notes in the store: origin and comment.** vmtool writes nothing of
its own into a VM's libvirt definition. What it records about a VM besides
what the host reports lives in the store only, in plain text, keyed by host
and libvirt UUID, and no report touches it:

- **Origin** (the image and its SHA-256 when known, how the disk was cloned:
  `thin`, `block`, `file` or `none`, the thin template, the pool, the
  creation time) is recorded once, by `vmtool create`, right after the VM is
  defined, and only with a store in use. It is never edited. VMs created with
  no store in use, before this version, or outside vmtool have none. If the
  store fails at that moment, the create still succeeds and one line says the
  notes were not saved.
- **Comment**: `vmtool create --comment TEXT` or `vmtool comment hv1/web TEXT`
  (`--clear` removes it) sets a note of at most 512 bytes on one line. It
  needs a store: with none in use, `--comment` is refused before anything is
  created and `vmtool comment` fails (REST: 503). The host is not asked, so
  the comment can be set or cleared while the host sleeps; the store must list
  the VM (its host has reported it, and it is not gone). Never put a secret in
  it.

`vmtool info` shows origin and comment from the store (nothing with no store
in use, or when the store does not answer); `vmtool inventory`, the TUI and
REST `GET /v1/inventory` show them too. Notes go with their inventory row
(`vmtool delete`, the sweep).

**Host tokens.** A daemon reports with a *host token*, one per hypervisor,
accepted by the store on its own report route and `whoami` and nowhere else
(not on credentials, images or other hosts). Make one token per host, put
each field in OpenBao, and give the store the SHA-256 of each token so it
learns them; a host's token is revoked by removing its field and its hash:

```bash
just deploy hv1                               # installs the token on the host
```

The tokens live in OpenBao under the kv path in `BAO_STORE_HOST_TOKENS_PATH`
(no default; one field per host, e.g. `kv/example/vmtool_store/host_tokens`).
With a `server:` block and that setting, `just deploy` reads the host's field
and installs it on the
hypervisor as `store-host-token` in the daemon's directory (vmtool:vmtool,
0600), and `config write --store-token-file store-host-token` names it as
`store_token_file` in the host's `config.yml`; the token is never in
`config.yml`, argv or a log. A host with no token, or a deploy with
`BAO_STORE_HOST_TOKENS_PATH` unset (then OpenBao is not asked), gets one
warning and the deploy goes on; its daemon does not report. To revoke: remove
the host's field and its hash from the store's token list, then `just deploy`
that host again (a deploy with no token removes the old file).

#### Building and deploying the store

The store is a static Go binary (`CGO_ENABLED=0`, distroless, runs as uid
65532). From this repository:

```bash
just store-image <tag>    # builds <registry>/vmtool-store:<tag> and :latest, pushes both
```

`<registry>` is the setting `VMTOOL_IMAGE_REGISTRY` (`host[:port][/path]`, e.g.
`registry.example.lab`; no default; environment or `deploy.env`). Without it the
recipe stops by name before `docker build`. Log in to the registry first.
`<tag>` is a version; empty and `latest` are refused. No secret goes into the
image. To deploy it, run the image behind a reverse proxy; the store checks
its own bearer tokens.

The store reads a `config.yaml` (listen address, `tokens.yaml` with the
SHA-256 of each caller's token, so the store never holds a token, an
`enc.key` file of mode 0600 for the credential encryption, the S3 endpoint
and bucket, with the S3 credentials in the environment, and the data
directory). The store creates its bucket itself at start if it is missing. Image blobs live in the bucket, and metadata and
encrypted credentials in `data/store.db` under the data directory. The store
refuses a token or key file readable by group or others, and a data
directory looser than 0700. It serves plain HTTP only on loopback or with
`behind_proxy: true` behind a TLS-terminating proxy (rule 5 in `AGENTS.md`);
it needs a token on every route except `GET /version` and `GET /v1/key-request`.

**Serving under a path.** A proxy that serves several services on one host
and passes the full path on unchanged (not one that strips the prefix)
needs the store to know its prefix. Set `path_prefix` in the
store's `config.yaml`, beside `behind_proxy`:

```yaml
behind_proxy: true
path_prefix: /vmstore
```

The store then answers only under the prefix:
`https://proxy.example/vmstore/version` (public) and
`https://proxy.example/vmstore/v1/...`; any other path, and `/vmstore` alone,
is `404` before auth, and the store sends no redirect. Clients name the same
base, `server.url: https://proxy.example/vmstore` (and a hypervisor's
`store_url` is the same address). The value is empty (the default: serve at
the root) or one or more `/segment` of letters, digits and `. _ ~ -`, with no
empty or `.` segment, no `..` and no trailing `/`; anything else stops the
store at start with an error naming `path_prefix`. Tokens, host tokens,
tickets and everything else are unchanged.

**Personal keys (store API 1.5.0).** Besides the callers in `tokens.yaml`, the
store can accept the personal keys a key provider lists, and tell a tool where
to ask for one. Both are off unless set, and a `config.yaml` without a
`personal_keys:` block loads and behaves as before:

```yaml
personal_keys:
  url: http://keys.internal:8080/keys/v1/lists/vmtool
  allow_insecure_http: true     # plain http to a host that is not loopback
  token_file: personal-keys.pull-token   # optional pull token; one line
  cache_file: personal-keys.cache        # required with url
  refresh: 1m                            # time between pulls, greater than 0
  refresh_on_miss: 5s                    # 0s turns it off
  request_url: https://keys.example.lab/v1/services/example/requests
```

- `url` is the pulled list; `https`, or plain `http` only to a loopback host
  unless `allow_insecure_http: true`, which is for a network path you trust
  (the example is a provider on the container network the store shares); no
  credentials in it. `token_file`, `cache_file` and `allow_insecure_http` need
  `url`, and `cache_file` is required with it. Relative paths are used as
  written, like the store's other paths (relative to the process's working
  directory); in the container, name an absolute path under the data
  directory. `request_url` is the address where a tool files a key request,
  only reported, never dialled: absolute `https`, or `http` to a loopback host.
  A bad value stops the store at start with an error that names the setting and
  never prints the value.
- The list holds SHA-256 hashes of keys; the store accepts an entry whose role
  is `full`. **tokens.yaml callers and host tokens stay the operator's
  fallback and always win:** an entry whose name or hash is a `tokens.yaml`
  caller's or a host's is skipped (logged by name). `tokens.yaml` is still
  required, with at least one entry.
- Bearer order: a `tokens.yaml` caller; then a host token (a match is final:
  on a host route the host, on any other route `401`, and the list is never
  consulted, so a known host token starts no pull); only a token that is
  neither goes to the list, where a miss triggers one conditional pull (at most
  one per `refresh_on_miss`) before it is refused. Download tickets are
  unchanged. A pulled key is a user caller named by its key name:
  `GET /v1/whoami` answers that name and kind `user`, and the request log names
  it as it names the other callers. A host token is never a pulled key.
- The store pulls at start and every `refresh`, keeps the last good list in
  `cache_file` and starts with the provider down; while it is down the last
  good list is served however old it is. A removed key stops at the next pull.
  At start the store logs `personal_keys: list origin=... entries=...
  age=...`. No log line holds a key, a hash or the pull token. The database
  and its schema are untouched, so the previous store image still starts on the
  same data.

`GET /v1/key-request` is public and answers `200 {"request_url": "<url>"}`, or
`404` when none is set. `GET /v1/personal-keys` (authenticated, for user
callers; a host token gets `401` as on every route outside its allow-list)
answers `200` also when the list is off:

```json
{"enabled": true, "origin": "provider", "entries": 7, "age_seconds": 12,
 "list_time": "...", "last_success": "...", "cache_failing": false,
 "local_tokens": [{"name": "alice", "count": 3, "last_used": "..."},
                  {"name": "bob", "count": 0}]}
```

`local_tokens` has one row per `tokens.yaml` caller, sorted by name: the
requests that caller's token authenticated since the store started and the time
of the last one (absent when none). It is the evidence that nothing still uses
an old token before anyone retires it. It names callers, never a hash. With the
list off the object is `{"enabled": false, "local_tokens": [...]}`.

**Key loss: `enc.key` encrypts every stored credential.** If it is lost from
both OpenBao and the file on the store host, every stored credential is lost, and
nothing can recover them. A new key (a fresh `enc.key`)
makes every stored credential unreadable. A copy of `store.db` is useless
without the key it was written with. Keep the key with the backup policy for
OpenBao.

**Backups.** The store writes a consistent copy of its database (SQLite
`VACUUM INTO`) to the bucket as `backups/store-<UTC yyyymmddThhmmssZ>.db`
every 24 hours, and once 5 minutes after it starts when the newest backup is
older than 24 hours or there is none. After each one it keeps the 14 newest
`backups/store-*.db` and deletes the rest. To take one now, in the app
directory:

    docker compose exec vmtool-store vmtool-store backup

It prints one line, the object name, and exits 0; any failure exits non-zero.
A failure is logged with the operation, object name, error class and
duration, and never a credential, token, key or S3 key.

A backup is a copy of the database, so the credential columns stay
AES-256-GCM ciphertext. **The encryption key is not in the backup.** It is
kept separately (OpenBao and `etc/enc.key`), and a backup restored without
the key it was written under is unreadable. Image blobs are not in the backup
either; they stay in the bucket and can be pushed again from `image_dir`.

To restore: stop the container; fetch the chosen object (`mc cp` or the
console) to `data/store.db`, owned by 65532 with mode 0600 in the 0700
directory; make sure `etc/enc.key` is the key that backup was written under;
start the container; check `GET /version` and read one credential back.
vmtool adds no restore command.

### A new machine

To run `vmtool` from another machine (a laptop, a second workstation) it
needs:

- a checkout of this repository, built (`go build -o vmtool ./cmd/vmtool`);
- a `config.yml` with a `hosts:` section naming the daemons it will talk to
  (`just install` writes one and keeps `hosts:` on re-runs);
- a `.env` with `VMTOOL_TOKEN`, the token whose hash is each daemon's
  `token_sha256` (`just token-fetch` gets it from OpenBao), or a key of your own in
  the token file (`vmtool key request`; see [The client's key](#the-clients-key));
- your CA in the system trust store, so TLS to the daemons
  (and to the store) verifies;
- libvirt's client library (`libvirt-dev` on Debian/Ubuntu, `libvirt` on
  Arch), which the binary links even if no libvirt daemon runs here;
- `ssh` and `ansible`, for `exec`, `ssh`, `playbook` and the guest setup
  that `create` runs (both run on the machine that runs the command);
- the `.machines/` folders of the VMs it will log in to. They hold each VM's
  key and password and are never in git: copy them from the machine that
  created the VMs, keeping their permissions. With
  a store (above), put `server:` in `config.yml` and `VMTOOL_SERVER_TOKEN`
  in `.env` instead and the folders are fetched on first use.

With these, the listed hosts work and the own name is one of the three cases
above.

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

Go 1.25+.

```bash
go build -o vmtool ./cmd/vmtool
go generate ./internal/api/...          # after editing spec/openapi/vmtool.yaml
scripts/verify-generate.sh             # fail if generated code is stale
```

## First run

Every `vmtool` command (except `help`, `completion`, `config write` and the
`key` commands) needs `config.yml` in the working directory and a key. A
`VMTOOL_TOKEN` is checked against `token_sha256`: it comes from the
environment, else from `.env` in the working directory; a real `VMTOOL_TOKEN`
in the environment wins over `.env`. With none, the key in the token file is
used instead (next section). With no key anywhere every command stops with
one line naming the fix.

```bash
just token          # new token into .env (and OpenBao if BAO_TOKEN_PATH and VAULT_ADDR/VAULT_TOKEN are set)
# or: just token-fetch   # the existing token from OpenBao into .env (needs BAO_TOKEN_PATH)
just install        # build, cert, config.yml, systemd unit (vmtool server over HTTPS)
```

`BAO_TOKEN_PATH` is the OpenBao kv path of the operator token (no default; the
environment or `deploy.env`). `just token-fetch` stops by name without it;
`just token` without it writes `.env` only, says the token was not stored in
OpenBao and calls no `bao`. `just install` needs `VMTOOL_DOMAIN` and
`BAO_CA_PATH` (see [Deploy settings](#deploy-settings-and-just-settings)).

`vmtool server` serves HTTPS only and needs `tls_cert`, `tls_key` and
`token_sha256` in `config.yml`; `just install` sets all three.


## The client's key

A client has one key for the daemons and the local service (`VMTOOL_TOKEN`) and one for the store
(`VMTOOL_SERVER_TOKEN`). Each is found in this order, and the first source that has one wins:

1. the environment variable;
2. `.env` in the working directory;
3. the token file.

So while `.env` holds both tokens nothing changes: the token file is not looked at.

The token file is `VMTOOL_TOKEN_FILE`, else `token_file:` at the top of `config.yml` (a leading `~/` is your home
directory, any other relative path is relative to `config.yml`'s directory), else
`$XDG_CONFIG_HOME/vmtool/token`, else `~/.config/vmtool/token`. It holds one key. It is read trimmed, and refused,
with an error that names the file and never shows it, unless it is a regular file (a symlink is refused) with no
group or other permission bits (`0600` or tighter) and is not empty. A missing file is no key.

A `VMTOOL_TOKEN` that hashes to `token_sha256` passes, offline, as always. Any other key (one from the token file, or
a `VMTOOL_TOKEN` that is not the operator's, as the child of a daemon carries) is not in `config.yml`, so the command
asks the local service (`vmtool server`, the address in `listen`, trusting `tls_cert`) one authenticated question,
`GET /v1/personal-keys`, with a 2 s limit. 200 passes; 401 stops the command with an error (for a token-file key it
names the file; for a `VMTOOL_TOKEN` it says the token does not match `token_sha256` and the local service did not
accept it either); a service that does not answer stops it with "vmtool.service is not answering at <address>; start
it: sudo systemctl start vmtool". That question goes only to this machine: the address in `listen` must be loopback
or assigned to one of this machine's interfaces (an unspecified host such as `0.0.0.0` means loopback), or the key
is not sent and the error says that `listen` is not an address of this machine. A `config.yml` that names someone
else's address therefore cannot collect a key. `vmtool config write` never takes the token file's key: a personal
key must not become a daemon's `token_sha256`, so it needs the operator token in `VMTOOL_TOKEN` or `--token-stdin`,
and it refuses a token that equals the token file's key (a child of vmtool carries the caller's key as
`VMTOOL_TOKEN`). `vmtool config write --no-personal-keys` removes the whole `personal_keys:` block first (the key
list off), then applies any `--personal-keys-*` or `--key-request-url` flag given.
`vmtool server` reads its store key in the same order, at each store request, and never asks for a key.

### First use with no key

With no key from any source, a command asks the key provider for one, unless you pass `--no-key-request`. The
request URL is found at `server.url` (the store tells a client where to ask), or is `--key-request-url` or
`VMTOOL_KEY_REQUEST_URL`. With no URL the error is the usual "no VMTOOL_TOKEN" line.

- In a terminal (stderr is one) the command prints where a person approves the request and the code to check,
  waits for the approval, stores the key in the token file (mode `0600`) and carries on. After a fresh key, one 401
  from the local service is retried once after 5 s, while it learns the new key.
- Anywhere else (an agent, a script) the command files one request, or resumes the one already pending (a pending
  file beside the token file; never a second request), prints the notice and "key request pending; run this again
  after approving" on stderr, and exits 75. With `--json` the error code is `key_pending` and the notice is in
  `message`. Run the command again after the approval and it collects the key.
- A denied, expired or consumed request, a refused poll and a provider failure exit 1 (`key_denied`, `key_expired`,
  `key_consumed`, `key_unauthorized`, `key_provider`); `key_busy` (75) means another process is handling the request.
- A `VMTOOL_TOKEN` with no store key never files a request just for the store: the store commands run in local mode
  as before.

### `vmtool key`

None of these prompts; all take `--json`, and none needs the token gate.

- `vmtool key request [--role R]` files or resumes a request and stores the key. With a key already in the token
  file it says "a key is already at <path>" (`{"token_file", "result": "present"}`). It needs no `config.yml` when
  `--key-request-url` (or the variable) gives the URL; else `server.url`.
- `vmtool key status` is read-only and offline: the token file's path and whether it is present, absent or
  refused; which source a daemon call and a store call would use now (`env`, `.env`, `file` or `none`; never a value);
  and the pending request (where to approve it, the code, when it expires).
- `vmtool key check` presents the token file's key, whatever the order would pick, to the local service, to each host's
  daemon directly, through the local service to each host, and to the store (`GET /v1/whoami`, which names the key).
  One row per target: `accepted`, `refused` or `unreachable`. It exits 0 only when every reachable target accepted
  the key. Use it to prove a new key before removing the old tokens from `.env`.

## Usage

```bash
# Interactive TUI (run from project root)
./vmtool i

# CLI — VMs are named <host>/<vm>; the local host is its hostname (hv1 here)
./vmtool create <host>/<name> <image>
./vmtool create hv1/web ubuntu.qcow2 --net-type direct --net-source eth0 --macvtap-mode bridge
./vmtool create hv1/scratch ubuntu.qcow2 --noclone  # boot the image in place
./vmtool create hv1/web ubuntu.qcow2 --extra-disk-size 50   # second empty disk as vdb
./vmtool create hv1/web ubuntu.qcow2 --no-wait      # return right after start; no address or SSH wait
./vmtool add-disk hv1/web 50                        # attach a new empty disk (vdb, vdc, …)
./vmtool create hv1/web img1 --pool nvme-thin           # thin clone of a template (see Thin pools)
./vmtool templates save hv1/build1 t1-tpl               # keep a shut-off thin VM's disk as a template
./vmtool templates list                                 # templates on every host
./vmtool pool list --host hv1                           # pools, with thin usage
./vmtool reboot hv1/web                             # ACPI reboot, stays defined
./vmtool exec hv1/web uname -a                      # exact argv, no shell (was `cmd`; API 2.0.0)
./vmtool exec hv1/web -- sh -c 'echo hi | wc -c'    # shell features: run a shell; prints 3
./vmtool exec hv1/web -- sh -c 'cat > /p/.env' < .env   # the CLI passes your stdin through
./vmtool exec -t hv1/web -- top                     # -t: a live terminal (ssh -tt), like docker exec -it
# REST: POST /v1/hosts/{h}/vms/{n}/exec and /v1/metal/{n}/exec take an optional
# "stdin" string (API 2.1.0): text written to the command's stdin, then closed.
# Never logged, never in an error or on a command line; bounded by the 1 MiB body limit.
./vmtool list                                           # every host (5 s each; a dead host is an ERROR row)
./vmtool list hv1                                   # one host
./vmtool hosts                                          # reachability, API version, cert days left
./vmtool delete <host>/<name>
./vmtool delete hv1/scratch --noclone               # undefine only, keep the disk
./vmtool server                                         # HTTPS REST on 127.0.0.1:9473; GET / is Swagger UI
```

The command line's conventions (`--json`, the error object, exit codes, the agent tips in `vmtool --help`) are
in [docs/CLI.md](docs/CLI.md).

Every command except the interactive ones (`i`, `ssh`, `tunnel`, `server` and `metal ssh`) takes `--json` and
prints exactly one JSON object on stdout (lists under a named key); progress and human text go to stderr. An
error is `{"error":{"code","message"}}` on stderr. Exit codes: 0 success, 1 error, 2 usage; `exec` and
`metal exec` exit with the remote command's code. An interactive command given `--json` exits 2. Details and the
shape of each result are in [docs/CLI.md](docs/CLI.md).

### A host's default pool for new VM disks (`default_pool`)

When a request names no pool (the CLI and the TUI's "(host default)" send
none), a new VM disk goes to the host's default pool: `default` on a
workstation, `vmtool` on a hypervisor. To send them elsewhere, for example a
logical pool over an NVMe stripe, set `default_pool` in the **workstation's**
`config.yml` (`just deploy` rewrites a hypervisor's `config.yml`, so a key
kept there would be lost):

```yaml
default_pool: nvme              # this host's own new VM disks
hosts:
  hv2:
    address: hv2.example.lab
    ssh_user: ubuntu
    default_pool: nvme          # new VM disks created on hv2
```

Scope: where new VM disks go; images stay in the cache pool. It applies to an
empty pool only, and only to the clone target, the extra disk and `add-disk`
(`CreateVolume`, `AddDisk`) and the domain's `Pool`; a named pool always wins.
Image upload, listing and delete, `ListPools`, `migrate-disk` and resize are
unchanged. On a hypervisor `default_pool` wins over `vmtool` for disks while
`vmtool` stays the image-cache pool. The value is a plain pool name
(`^[A-Za-z0-9][A-Za-z0-9._-]*$`); anything else is a config load error naming
the key.

### Where a tool asks for an API key (`personal_keys.request_url`, API 2.11.0)

A daemon can tell a tool where to ask for a key of its own. Set the address of
the request endpoint of your key provider in the daemon's `config.yml`:

```yaml
personal_keys:
  request_url: https://keys.example.lab/v1/services/example/requests
```

or `vmtool config write --key-request-url <url>`, which keeps every other key
of the file. Off unless set: there is no default. The value is an absolute
`https` URL; plain `http` only to a loopback host (`localhost`, 127.0.0.0/8,
`::1`); no user info, no fragment. Anything else is a config load error that
names `personal_keys.request_url` and never prints the value.

The daemon only reports the address and never dials it. `GET /v1/key-request`
is public (no token needed) and answers `200 {"request_url": "<url>"}`, or `404`
with the usual error body when none is set. It is the discovery endpoint of the
key-request contract a tool such as `keyclient`'s `request.Ensure` follows. It
is additive: nothing else in the API changes, and a client of an older 2.x API
works against it unchanged. A daemon older than 2.11.0 answers `401` there.
### Keys from a pulled list (`personal_keys.url`, API 2.11.0)

Besides the one shared token (`token_sha256`), a daemon can accept the personal
keys that a key provider lists. Both roles do it, workstation and hypervisor.
It is off unless `url` is set; there is no default address.

```yaml
personal_keys:
  url: https://keys.example.lab/keys/v1/lists/vmtool
  token_file: personal-keys.pull-token   # optional pull token; one line
  cache_file: personal-keys.cache        # required with url
  refresh: 1m                            # time between pulls, greater than 0
  refresh_on_miss: 5s                    # 0s turns it off
  # allow_insecure_http: true            # plain http to a host that is not loopback
```

- `url`: absolute `https` (plain `http` only to a loopback host, unless
  `allow_insecure_http: true`); no credentials in it. `token_file` and
  `cache_file` are relative to the directory of `config.yml`, like `tls_cert`.
  `token_file`, `cache_file` and `allow_insecure_http` without `url` are a
  config error. A bad value is a config load error that names the setting and
  never prints the value.
- The list holds SHA-256 hashes of keys; the daemon accepts an entry whose role
  is `full`. An entry that carries the hash of `token_sha256` is skipped: the
  local token always wins.
- The daemon pulls at start and every `refresh`, and keeps the last good list
  in `cache_file`, so it starts with the provider down. While the provider is
  down it serves the last good list however old it is. A key that is not in the
  list triggers one conditional pull (at most one per `refresh_on_miss`) before
  it is refused, so a key issued a moment ago works on first use, and a key
  removed from the list stops at the next pull. With no cache and no provider
  only `token_sha256` works.
- A request is authenticated by `token_sha256` first, then by the list. The
  token a workstation forwards to a hypervisor, and gives to ssh children, is
  always the caller's own, whichever way it was accepted.
- At start the daemon logs `personal_keys: list origin=... entries=...
  age=...`. The list's own lines (a skipped entry by name, a failed pull) never hold a
  key, a hash or the pull token.

`GET /v1/personal-keys` (authenticated) reports it. It answers `200` to any
caller the daemon accepts, also when no list is configured, so a client can use
it as the one authenticated "does this daemon accept my key" check:

```json
{"enabled": true, "origin": "provider", "entries": 7, "age_seconds": 12,
 "list_time": "...", "last_success": "...", "cache_failing": false,
 "local_token": {"count": 3, "last_used": "..."}}
```

`origin` is `none`, `cache` or `provider`; `last_error` appears only after a
failed pull (cut to 200 characters); `enabled: false` leaves out everything but
`local_token`. There is no hash, name or URL in it.

The local token stays the operator's fallback: it is required to start, it is
never replaced by a listed key, and it works with the provider down.
`local_token` counts the requests it authenticated since the daemon started and
gives the time of the last one; the daemon also logs one line "local token used"
(the route, never the token) at most once an hour. That is the evidence that
nothing still uses the shared token, before anyone retires it.

#### Deploy settings and `just settings`

The recipes that deploy hold no value of your site. Eight settings, **none
with a default**, come from the environment or from `deploy.env` (below):

| Setting | Needed by | Meaning |
|---|---|---|
| `VMTOOL_DOMAIN` | `cert`, `install`, `deploy` (required) | the DNS domain of the host certificates, e.g. `example.lab` |
| `BAO_CA_PATH` | `cert`, `install`, `deploy` (required) | the OpenBao kv path of the CA (`<path>/crt` and `<path>/key`) |
| `BAO_STORE_HOST_TOKENS_PATH` | `deploy` (optional) | the kv path whose fields are the hosts' store tokens; unset: no host token, one warning |
| `BAO_TOKEN_PATH` | `token-fetch` (required), `token` (optional) | the kv path of the operator token; `token` without it writes `.env` only |
| `VMTOOL_IMAGE_REGISTRY` | `store-image` (required) | `host[:port][/path]` the store image is pushed to, e.g. `registry.example.lab` |
| the three key-list settings | `install`, `deploy` (optional) | below |

A required setting that is missing or empty stops the recipe with a line like
`set VMTOOL_DOMAIN in the settings file (deploy.env) or the environment`,
before any build, copy, `docker build`, push, `sudo` or restart. Values are
checked as plain strings (a DNS name, a kv path, a registry) before they reach
a command. `just settings` is read-only and lists each setting as `set` or
`unset`, where a set one comes from, and the recipes an unset one blocks; it
never prints a value and always exits 0.

#### Deploying the key-list settings (`just deploy`, `just install`)

`just deploy <host>` and `just install` can write the `personal_keys` block
and install the pull token. Three more settings, **none with a default** (all
off unless set):

| Setting | Meaning |
|---|---|
| `VMTOOL_PERSONAL_KEYS_URL` | the key list's URL, e.g. `https://keys.example.lab/keys/v1/lists/vmtool` |
| `VMTOOL_KEY_REQUEST_URL` | the request URL the daemon tells tools about, e.g. `https://keys.example.lab/v1/services/vmtool/requests`; independent of the list |
| `BAO_KEYS_PULL_TOKEN_PATH` | the OpenBao kv path of the pull token, e.g. `kv/example/keys/pull/vmtool`; the field is always `token` |

Set them (and the settings above) in the environment, or put them in
`deploy.env` in the checkout root (gitignored), one `KEY=value` per line:

```bash
# deploy.env
VMTOOL_DOMAIN=example.lab
BAO_CA_PATH=kv/example/ca
VMTOOL_PERSONAL_KEYS_URL=https://keys.example.lab/keys/v1/lists/vmtool
VMTOOL_KEY_REQUEST_URL=https://keys.example.lab/v1/services/vmtool/requests
BAO_KEYS_PULL_TOKEN_PATH=kv/example/keys/pull/vmtool
```

The scripts read the file themselves with a strict parser: blank lines and `#`
comments are skipped, a value may be wrapped in one pair of quotes, nothing is
executed or expanded, and any other line is not allowed (the error names its
line number and never its content; only the eight setting names are accepted).
A variable already in the environment, even an empty one, wins over the file.
(`.env` is not read for these, and the justfile passes none of them on
explicitly, so the shell's own environment reaches the scripts as it is.)

- **URL unset:** no OpenBao read and no pull token. It also cleans up after a
  list that was on: `just deploy` removes the host's `keys-pull-token` and
  `personal-keys.json` (as it removes a stale host token), and `just install`
  removes both from the checkout root and passes `config write
  --no-personal-keys`, which drops the `personal_keys` block from `config.yml`
  (its other settings stay) before `--key-request-url`, if set, is applied
  again. `just deploy` writes `config.yml` from scratch, so the block is gone
  there already.
- **URL set:** the pull token is read from OpenBao into a variable (never in a
  command line, an echo or a log), and a missing or empty one stops the run
  before anything is installed or restarted, so the old daemon keeps running.
  On a hypervisor (`just deploy`, ssh or this machine) it is staged on the
  stdin of one ssh call and installed `vmtool:vmtool` 0600 as
  `keys-pull-token` in the daemon's directory, beside `store-host-token`, and
  the staged copy is removed; the cache is `personal-keys.json` there.
  `just install` writes `keys-pull-token` (0600, you) and the cache
  `personal-keys.json` in the checkout root; both are gitignored. `config
  write` gets `--personal-keys-url`, `--personal-keys-token-file
  keys-pull-token` and `--personal-keys-cache-file personal-keys.json`
  (relative names, resolved against `config.yml`'s directory).
- **Request URL set:** `--key-request-url` is passed, with or without a list.
- **The operator token.** Both scripts need `VMTOOL_TOKEN` (environment, else
  `.env`) for `config write`, which hashes it into `token_sha256`, and stop
  without it before any build or restart; they never write `.env`. Once your
  clients use personal keys and `.env` no longer holds it, export the operator
  token in the shell that runs `just deploy` / `just install`:
  `export VMTOOL_TOKEN=...`.

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

### Streamed create over REST (API 2.6.0)

`POST /v1/hosts/{host}/vms` returns one JSON answer when the create is over.
A client that wants to see the stages as they happen (clone, boot, address,
SSH, playbook) asks for a stream with `Accept: application/x-ndjson`; the
body of the request is the same. Only that value asks: a wildcard such as
`*/*`, or `application/x-ndjson;q=0`, does not. Without that header the call
is unchanged: 201 with the VM, or 400, 404, 409 or 500 as before. A request
refused before any work starts (a bad body, a name that is taken, an unknown
host, an image that is not found) also gets its plain JSON status, with or
without the header. The stream is workstation only, like any create: a
hypervisor answers 403.

```bash
curl -N -H 'Accept: application/x-ndjson' \
     -H "Authorization: Bearer $VMTOOL_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"name":"vm1","image":"img1"}' \
     https://127.0.0.1:9473/v1/hosts/hv1/vms
```

The answer is 200 `application/x-ndjson`: one JSON object per line, each sent
as it is written, each with an `event`:

```
{"event":"progress","progress":{"stage":"clone","status":"start","detail":"cloning image","elapsed_ms":4}}
{"event":"done","vm":{"name":"vm1","state":"running",...},"timings":{"stages":[{"stage":"clone","ms":1626},...],"total_ms":41200}}
```

- `progress` holds a progress record: `stage`, `status`, `detail`, `sent` and
  `total` for an upload, `output` on a step that failed or finished, and
  `elapsed_ms`.
- A run ends with exactly one `done` (the VM and the `timings`) or one
  `error` (`error`, a `kind` of `invalid`, `not_found`, `conflict` or
  `internal`, `output` when a tool's output was captured, the `vm` when the
  domain exists, and the `timings` so far).
- The status is 200 either way, so read the last line to learn the outcome.
  A stream with no `done` and no `error` was cut off.
- `timings` are milliseconds from the start of the workflow, on the
  workstation's clock. `elapsed_ms` stamps each event; `stages` lists the
  stages that ran, in the order they began, with the time from the first to
  the last event of each; `total_ms` is the stamp of the last event. These are
  the numbers the CLI prints on its closing `timings:` line.
- `output` can hold a playbook's output, so treat it as possibly sensitive. It
  is never logged, and neither is any event line.
- A dropped connection ends the create as it does for the plain JSON call. A
  VM that was already defined stays, as described under Waits at create.

### REST errors (API 2.9.0)

An error response is `{"error": "<text>", "code": "<code>"}`. Branch on `code`,
never on the text: `bad_request`, `unauthorized`, `workstation_only`,
`not_found`, `conflict`, `store_unavailable`, `host_unreachable`,
`host_timeout`, `bad_gateway` or `internal`. When a workstation forwards an
operation to a host's daemon and the host cannot be reached, the answer is
`502` with `host_unreachable`; when it does not answer in time, `504` with
`host_timeout` (the operation may or may not have run). `execVM` keeps `502`
with `bad_gateway` for a guest that ssh cannot reach. The CLI is unchanged.

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

A clone that vmtool creates in a dir pool gets `cache='unsafe'` on its disk
driver, so guest writes are fast (a clone in a `logical` or thin pool gets `none`; see
[Fast block disks](#fast-block-disks-lvm-on-nvme)). `unsafe` keeps guest writes that the guest believes are
flushed in host memory; a host crash or power loss can corrupt the VM's disk.
Pick `none` for a VM whose data matters:
`vmtool create --disk-cache none host/name image` (modes: `none`, `writeback`,
`writethrough`, `unsafe`; the TUI create form has a Disk cache field).

`--noclone` gets no cache attribute (the hypervisor's default) unless you name
a mode: `unsafe` on a base image would risk the image itself. Extra disks, and
disks added later by `add-disk`, take the mode of the VM's boot disk
(`vda`); `migrate-disk` keeps it. Existing VMs are not changed. `GET` VM
reports `disk_cache`; `vmtool list` is unchanged.

### Fast block disks (LVM on NVMe)

A host with two spare NVMe drives can give VMs raw block devices: LVM logical
volumes on an md RAID 0 stripe of the two drives, exposed as a libvirt
`logical` pool (`nvme` by default). Host setup builds it, opt-in and only
for drives you name ([Hypervisors](#fast-block-disks-opt-in-nvme-stripe)).
Use it for disposable workers that need disk speed: the stripe has no
redundancy, so losing one drive loses every VM disk on it.

Measured on hv2 (4 vCPUs, fio on the bare virtual disk, 4k I/O):

| backing / host cache | write+fsync IOPS | randwrite IOPS | randread IOPS |
|---|---|---|---|
| qcow2 on the boot drive, `unsafe` (the dir-pool default) | 9.5–9.8k | 74–81k | 129–132k |
| LV on the stripe, `none`, `io='native'`, 4 queues, an IO thread (what vmtool writes) | 15.5k | 136k | 132k |

```bash
vmtool pool list --host hv2                       # TYPE logical for nvme
vmtool create hv2/build1 ubuntu2604 --pool nvme --disk-size 40
vmtool add-disk hv2/build1 100 --pool nvme         # LV build1-2, attached as a block disk
vmtool delete hv2/build1                           # removes both LVs
```

- **Clone by conversion.** `create` asks libvirtd to copy the cached qcow2
  image (still kept in the `vmtool` dir pool) into a new LV named after the
  VM, converted to raw. The image cache does not move; LVs never show as
  images.
- **Size it at create.** libvirt cannot resize a logical volume, so
  `resize-disk` on such a disk fails with a message that says so, before it
  touches the VM. Give `--disk-size` at create: the LV is made at the larger
  of the image's size and `--disk-size`.
- **Cache `none` by default.** A clone in a `logical` pool gets
  `cache='none'` with `io='native'`: the host page cache is bypassed, which is
  both the fast path for an LV and safe across a host crash. `--disk-cache`
  still overrides; with any mode other than `none` or `directsync` the
  `io='native'` attribute is left out (QEMU needs O_DIRECT for it). A block
  disk added to a file-backed VM takes that VM's boot-disk mode, as every
  added disk does.
- **IO thread and queues.** A VM with a block disk gets one IO thread;
  each block disk gets `queues` = min(vCPUs, 4) and `discard='unmap'`. A
  block disk added later to a VM without an IO thread gets none. File-backed
  disks are unchanged.
- **No `--pool` every time.** Set `hosts.<host>.default_pool: nvme` in the
  workstation's `config.yml` ([`default_pool`](#a-hosts-default-pool-for-new-vm-disks-default_pool)).
- **Delete removes the LV**, but only a volume of a `logical` pool; a block
  device in no pool or in any other kind of pool is never deleted. Volumes
  vmtool did not create are left alone.
- `vmtool info` / `GET` VM report `disk_backing` (`file` or `block`);
  `pool list` / `GET` pools report each pool's `type` (API 2.2.0). A daemon
  must run 2.2.0 (`just deploy <host>`) before a logical pool works through
  it.

Not supported: growing a disk on a thick logical pool (a disk on a
[thin pool](#thin-pools-clones-from-templates) grows), `migrate-disk` to or
from a logical or thin pool or of a block disk (refused before the VM is
stopped), moving or converting existing VMs (thick or qcow2 to thin), creating
a logical or thin pool from the CLI or REST (host setup makes them; `pool
create` makes dir pools only; vmtool never removes a thin pool), and RAID
levels other than 0.

### Thin pools: clones from templates

The same volume group can also hold an LVM **thin pool** (host setup:
[Thin pool](#fast-block-disks-opt-in-nvme-stripe)). A VM created into it is
not a copy of its image: its boot disk is a thin snapshot of a **template**,
which costs a fraction of a second whatever the image's size, and the VM then
boots as usual. In a measurement on a test host before vmtool used it,
`lvcreate -s` plus activation took 0.24 s, and a fresh clone took about 10 s
from start until SSH answered (the cause of those 10 s is not yet found; the
`timings:` line that `create` prints shows where its own time goes). A thin
LV wrote 4k random at about 84k IOPS against about 135k for a thick LV, with
the same sequential speed, when pool zeroing is off, which host setup makes
sure of.

**The thick pool and its VMs are untouched.** libvirt's `logical` pool does not
see thin pools or thin volumes, so the thin pool sits beside the thick volumes
in the volume group and nothing about pool `nvme` or any VM on it changes. The
daemon touches only volumes it tagged itself (below). Existing VMs are not
converted: a VM gets a thin disk only when it is created into a thin pool.

```bash
vmtool pool list --host hv1                                # nvme-thin, TYPE thin, with usage
vmtool create hv1/vm1 img1 --pool nvme-thin --disk-size 40 # a snapshot of img1's template
vmtool add-disk hv1/vm1 100 --pool nvme-thin               # thin LV vm1-2, a block disk
vmtool resize-disk hv1/vm1 60                              # grows at once, guest included
vmtool delete hv1/vm1                                      # removes vm1 and vm1-2; templates stay
```

- **Removing a volume discards it first.** The helper discards a thin or
  export volume's blocks before removing it, so a later VM does not read its
  data; if the discard fails the volume is kept and the error gives the retry
  command. What this covers and does not (removals outside vmtool and libvirt,
  drives assumed to return zeros), and the manual checks:
  [Old data and discard](#old-data-and-discard).
- **A thin pool is a vmtool pool, not a libvirt one.** The daemon finds the
  pools by the tag `vmtool_thinpool` and lists each as a pool named after its
  thin-pool LV (`nvme-thin` by default), `type: thin`, path `/dev/<vg>`. A name
  that clashes with a libvirt pool is left out, with one log line.
  `--pool nvme-thin`, `default_pool` and `hosts.<h>.default_pool` work as for
  any pool. Thin pools exist only behind a hypervisor daemon at API 2.4.0 or
  newer (`just deploy <host>`): a host served by local libvirt lists none, and
  an older daemon lists none and does not know the pool name.
- **Volumes.** A VM's boot disk is the thin LV `<vm>`, extra disks are `<vm>-N`
  (as on the thick pool), tagged `vmtool_vol`. Every thin disk is a block disk
  with cache `none`, `io='native'`, one IO thread and `queues` = min(vCPUs, 4),
  as in the thick case. Snapshots are made with the activation-skip flag off,
  so the clones activate at host boot like any LV. `--disk-size` grows the
  clone (a smaller size leaves the template's size); resize grows it at once
  (`lvextend`, then the running VM is told, then the guest grows) and never
  shrinks. `create --noclone` is refused on a thin pool, and so is
  `--extra-disk-size` (attach a thin disk with `add-disk`). A VM name may not
  start with `vmtpl-`. `migrate-disk` does not work with thin pools.
  `vmtool delete` removes only `<vm>` and `<vm>-N` when they are tagged
  `vmtool_vol`, never a template or a volume it did not make.
- **Image templates.** The first `create` of an image into a thin pool builds
  the image's template there once, from the host's cached qcow2 (converted to
  raw, under a `vmtool_building` tag and renamed into place, so a half-made one
  is never used; a leftover one is removed when the daemon starts). Every later
  create of that image is a snapshot. A template is keyed by (image name,
  SHA-256), so a rebuilt image gets a new template and the old one becomes
  *stale* ([Templates](#templates-save-export-pull)). `vmtool templates
  prepare hv1 img1` builds it ahead of the first create.
- **`pool list` columns.** `NAME TYPE SIZE USED VIRTUAL DATA% META% ACTIVE
  PATH`. `VIRTUAL` is the sum of the sizes of every thin volume in the pool:
  what it has promised. `DATA%` and `META%` are the pool's data and metadata
  use. SIZE and USED are filled for an active libvirt pool too; the others are
  thin only. A blank cell is unknown (an older daemon). A thin pool at or above
  80 % data or metadata has a `!` after its name.
- **Overcommit and what a full pool does.** A thin pool lets the volumes
  promise more than it holds, and guests that fill their disks would then run
  it out. So every operation that adds or grows a thin volume (a clone, a
  template build, `templates save`, `add-disk`, `resize-disk`) is refused by
  the daemon, before any LVM call, when the pool's data or metadata use is at
  or above 80 %, or when the sum of all thin volumes' sizes plus the new one
  would exceed 4 times the pool's size. The answer is 409 with the numbers
  (the CLI prints them, exit 1); nothing is written. Running VMs are not
  touched. Autoextend at 80 % by 10 % grows the pool while the volume group has
  free space. If the pool does fill (the volume group is out of space and
  guests keep writing), LVM holds guest writes for a short time, then fails
  them (`--errorwhenfull n`), and a guest sees I/O errors. Guests give space
  back through `discard='unmap'` (Ubuntu's weekly `fstrim` timer); free the
  rest with `vmtool delete` and `templates delete`.
- **Room for exports.** A template export takes a thick volume from the same
  volume group's free space, and leaves at least 10 % of the thin pool's size
  free for the pool to grow (see [Templates](#templates-save-export-pull)); an
  export that would not leave it is refused. Exports are removed after 24 hours,
  so that space comes back.
- **The root helper.** LVM needs root and the daemon (user `vmtool`) has none,
  so it runs one script as root, `/usr/local/libexec/vmtool-thin`
  (`deploy/vmtool-thin`), installed root:root 0755 by `just deploy`, with this
  line in `/etc/sudoers.d/vmtool-thin`, the only sudo `vmtool` has:

  ```
  vmtool ALL=(root) NOPASSWD: /usr/local/libexec/vmtool-thin
  ```

  The daemon runs `sudo -n /usr/local/libexec/vmtool-thin <verb> <args>`. The
  verbs are fixed (`report`, `create`, `snapshot`, `extend`, `remove`,
  `rename`, `tag`, `import`, `export`), every name and number is validated,
  and the helper never runs LVM with options from its caller. It acts only in
  a volume group that holds a `vmtool_thinpool` pool and, except for `report`,
  only on thin volumes tagged `vmtool_vol`, `vmtool_template` or
  `vmtool_building`: it never removes a pool, never touches an untagged or a
  thick volume, and never anything of another tenant. `report` lists the
  names, sizes and tags of every LV in such a volume group, read-only; on a
  host with no such volume group, or with no LVM installed at all, it lists
  none, so the host reports an empty template list. An LVM that is there and
  fails is an error, and the daemon then sends no template list. Its file
  paths are limited to the daemon's image cache directory, regular files only,
  no symlinks, and the image converter (`qemu-img`) runs as the `vmtool` user
  on that one volume's device node, never as root. `sudo -l -U vmtool` on a
  host shows exactly the helper line.
- **A template is per host.** It lives in one host's thin pool; vmtool never
  copies one to another host by itself. [Templates](#templates-save-export-pull)
  says how to take one to another host.
- **Timings.** `create` ends with one stdout line such as `timings: clone
  0.3s, start 1.1s, wait_ip 2.0s, wait_ssh 7.0s, grow_disk 0.6s, total 11.0s`
  (the stages that ran; also after a failure). `wait_ssh` is a direct
  `ssh ... true` through the tunnel (a login, not an open port; retried every
  second up to `ssh_wait_seconds`), so it adds no Ansible start. REST callers of `createVM` get
  none: that route is a plain JSON call with no progress stream.

### Templates: save, export, pull

Two kinds of template live in a thin pool, and `vmtool templates list` shows
both (`NAME ORIGIN IMAGE SHA256 POOL SIZE USED CLONES STALE CREATED`, names as
`host/name`; one `ERROR` row for a host that does not answer):

- **image** templates, one per (image name, SHA-256) and pool, built by the
  first create or by `templates prepare`. An image template is **stale** when
  that image is no longer the host's current copy under that name (the image
  was rebuilt or deleted). `templates prune <host>` removes every stale one.
- **saved** templates, made by you from a VM (below). They are never stale and
  are removed only by name.

```bash
vmtool templates delete hv1/t1-tpl      # one template; its clones keep working
vmtool templates prune hv1              # every stale image template
```

Each host also reports its templates to the store with its VMs, so they stay
listed in `vmtool inventory` while the host sleeps (see
[The inventory](#the-inventory)); that list is a record, and `templates list`
stays the live view.

Removing a template never affects its clones: a thin snapshot does not depend
on its origin. A name that matches more than one template (a current and a
stale image template, or the same name in two pools) is refused with 409 and
the candidates listed; repeat with the LV name (`vmtpl-<12 hex>`, the SHA256
column) and `--pool`. `templates prune` is client-side: it lists, then deletes
each stale template by its LV name. A VM's volume is never removed by these
commands. In the TUI, `T` lists templates (`D` deletes after a confirmation) and
`W` on a VM row saves it.

**Save.** `vmtool templates save hv1/build1 t1-tpl` keeps the boot disk of a
shut-off VM on a thin pool as it is: the daemon takes a thin snapshot of it in
one step, and the VM keeps working and can be started again. vmtool does not
boot, mount or read the disk. It checks only what it can see without that: the
VM is shut off (not running, paused or crashed: stop it first and wait),
its boot disk is a thin volume vmtool made, the pool has room, and the name is
not used by a template or a cached image on the host. `--replace` replaces a
saved template of that name (the new one is made first), never an image
template or a cached image. A VM with extra disks (`vdb`...) is saved, with a
warning: they are not part of the template. A template is created in the VM's
own pool and belongs to that host.

Clone it with `vmtool create hv1/job1 t1-tpl --pool nvme-thin`: the template's
name is given as the image, and the host needs no copy of an image of that
name. Whether the clone is fit to use is what
[What a template must be](#what-a-template-must-be) is about.

**To another host.** A template is per host, so it moves as an image:

```bash
vmtool templates export hv1/t1-tpl       # the daemon writes it as a qcow2 onto its NVMe array, kept 24 hours
vmtool images pull hv1/t1-tpl            # that qcow2 into this machine's image_dir (or --to <dir>)
vmtool create hv2/job1 t1-tpl --pool nvme-thin   # hv2 builds its own template from it, then clones
```

`templates export` makes the daemon convert the template to qcow2 (through the
root helper) onto a temporary **thick** logical volume in the array's volume
group, in the space the thin pool leaves free: not into the host's image cache,
not onto its boot drive, and not into the thin pool. The volume is named
`vmexp-<12 hex>` and tagged `vmtool_export`; `virsh vol-list nvme` shows it,
and it is vmtool's, so leave it alone. There is one per template (exporting
again reuses it; a template replaced with `templates save --replace` gets a new
one). It is removed 24 hours after it was made (the daemon checks at start and
every hour), and when its template is deleted (by `templates delete`, or by
`templates prune <host>` when the template is stale).
`templates list` shows each export in its `EXPORT` column: its size, whether
its SHA-256 is known yet, and when it expires.

What changed from earlier versions (API 2.5.0), in plain words:

- `templates export` no longer puts the image in the host's image cache.
  `images list <host>` does not show it, and `images delete` does not apply to
  it. It waits on the array for 24 hours for an `images pull` or a push.
- A VM name may not start with `vmexp-` on any pool.
- With no room on the array the export is **refused** instead of filling the
  boot drive; nothing is written. The room rule: the volume's size is what a
  fully allocated qcow2 of the template needs (`qemu-img measure`, rounded up to
  the extent); the export is refused when the volume group's free space outside
  the thin pool, less that size, would fall below 10 % of the thin pool's size,
  the room the pool keeps to grow. The message gives the numbers, for example
  `hv1: export of t1-tpl needs 21.5 GB in volume group vg-test; 9.8 GB is free
  outside the thin pool after keeping 30 GB for it to grow; nothing was
  written`. There is no fallback to the boot drive.
- The SHA-256 is no longer computed by the export. The daemon computes it while
  it sends the bytes to `images pull`; when it does not know it yet, it sends it
  as a trailer after the last byte, and the pull compares it with what it
  wrote and keeps nothing on a mismatch or when none arrives. After a full send
  the daemon records the digest on the export and the template, so a later pull
  gets it up front, and a later create of `t1-tpl` on that same host clones the
  template with no upload and no rebuild. `templates export --push` reads the
  export once for its digest first (the store wants it before the bytes).
- An old client pulling an export whose digest is not yet known is refused with
  "did not send a SHA-256 digest"; update the client.

A name already in the host's image cache is refused unless it is this same
template's own cached image; an image template exports only as the cached image
it came from, so only a saved template makes a new export. `images pull
host/<name>` (an argument with a `/`; a bare `images pull <name>` pulls from the
store) downloads an image that daemon created, or a template's export by the
template's name, never a file it did not create, to a `.part` file, checks its
size and SHA-256 and links it into place; an identical file already there is
left as it is, a different one is refused, and there is no `--force`. After 24
hours the export is gone and the pull answers 404 (make a new export). The first
create on the other host builds that host's own template in a few seconds.
With the store, `images push` sends the pulled file there, and `vmtool
templates export hv1/t1-tpl --push` streams from the daemon into the store with
no file on this machine (the store must be configured and answering; without
one the command refuses before it asks the daemon). A daemon at API 2.4.0 or
newer serves both routes.

### What a template must be

A saved template is the disk exactly as it was, and **vmtool does not check
items 1 to 9 below**: the author makes the template fit, and vmtool says here
what fit means. Item 10 is the one thing `templates save` checks. Each item
says why vmtool needs it and what a clone shows when it is not met. Most of it
comes for free from a VM made from one of the `packer_images` images and left
alone; it breaks when the VM is customised.

1. **Shut down cleanly, and not booted again before `templates save`.** The
   template is the disk as it is. `vmtool stop` asks the guest to shut down
   (`Manager.Stop` calls libvirt's `Shutdown`, in `pkg/vmtool/libvirt.go`); a
   guest that ignores it stays running and `save` refuses until it is off.
   `vmtool poweroff` is a forced stop and leaves the filesystem as a crash
   would. A boot after the reset gives the disk an identity again (items 3 and
   4). *If not met:* clones replay a dirty filesystem journal, or all share the
   identity of that last boot. Booting the build VM *after* `save` is fine: the
   template is a snapshot and does not change.
2. **A login create can use: the user and password create is told, with
   password SSH allowed, and passwordless sudo.** A new VM has no credentials
   folder (create forgets any left by an earlier VM of the name, `forgetCreds`
   in `internal/app/create.go`), so create logs in with `--ssh-user` and
   `--ssh-pass`, which default to `packer` and `packer` (`defaultedAuth` in
   `pkg/vmtool/ssh.go`, reached through `ResolveVMAuth`), by password
   (ansible's `ansible_ssh_pass`, so `sshpass` on the machine running create),
   and runs the grow step and any playbook with become
   (`ansible_become: true`, `pkg/vmtool/inventory.go`). So: **do not run
   `setup_bot.yml` on a template's VM** (it deletes `packer` and installs
   `/etc/ssh/sshd_config.d/01-vmtool-no-password.conf`, which turns password
   login off), and keep `ssh_pwauth: true` from
   `/etc/cloud/cloud.cfg.d/99-vmtool.cfg`. A template with another login needs
   `--ssh-user` and `--ssh-pass` on every create. *If not met:* create fails at
   the SSH wait with `<host>/<vm> did not accept SSH within ...` (`wait_ssh`);
   the VM is left defined and running.
3. **No machine identity: an empty `/etc/machine-id`**, and
   `/var/lib/dbus/machine-id` a link to it or absent. vmtool does not
   regenerate it: systemd makes a new one at first boot only when the file is
   empty, which `cloud-init clean --machine-id` does (the last step of the
   `packer_images` builds, `provisioner "shell"` at the end of
   `packer_images/qemu_images/ubuntu2404/template.pkr.hcl`). *If not met:*
   every clone has the same machine-id, so the same systemd and journal
   identity and, on a network config without `dhcp-identifier: mac`, the same
   DHCP lease: two VMs at one address (see [Two VMs show the same
   IP](#two-vms-show-the-same-ip)).
4. **cloud-init enabled, cleaned, and still reading vmtool's datasource.**
   vmtool gives each VM its identity only through the SMBIOS serial
   `ds=nocloud;h=<name>;i=<uuid>` (`identityXML`, `pkg/vmtool/libvirt.go`); a
   cloud-init that is cleaned sees a new instance and sets the hostname,
   regenerates the SSH host keys (cloud-init's default; this repository does
   not set it), grows the root partition and applies the network config. Keep
   `99-vmtool.cfg` (`datasource_list: [NoCloud, None]`, `preserve_hostname:
   false`), do not disable cloud-init, do not set `ssh_deletekeys: false`, and
   run `cloud-init clean --logs` last (with `--machine-id`, as item 3 needs).
   *If not met:* clones keep the template's hostname and share its SSH host
   keys, and the root filesystem is not grown at boot.
5. **No leftover hostname line:** no `127.0.1.1 <old name>` line in
   `/etc/hosts` (the images delete it as the step before the clean; cloud-init's
   `manage_etc_hosts: localhost` does not remove one). *If not met:* each clone
   resolves its own name to the template VM's old name.
6. **Network by DHCP on any `en*` interface, not tied to a MAC or a fixed
   address.** Each clone gets a new MAC and vmtool finds its address from
   libvirt by that MAC (`WaitForIP`, `pkg/vmtool/libvirt.go`). Keep the images'
   `/etc/netplan/99-wildcard.yaml` (`match: name: "en*"`, `dhcp4: true`,
   `dhcp-identifier: mac`) and the `network: {config: disabled}` that keeps
   cloud-init from rewriting it. *If not met:* the clone gets no address and
   create fails at the address wait (`got no address within ...`; the VM is
   left running).
7. **A root filesystem create can grow, and Python.** Root is a partition (not
   an LVM volume inside the guest, not a whole-disk filesystem) that can
   extend to the end of the disk, ext4 or btrfs (xfs is not handled), with
   `growpart`, `findmnt`, `lsblk` and `resize2fs` (or `btrfs`) present
   (`growDiskCommand`, `pkg/vmtool/ansible.go`), and `python3`, which Ansible
   needs for every Ansible step create runs after the SSH wait (the wait is a
   direct `ssh ... true`, not Ansible). `--disk-size` grows the volume and then the guest; create's own
   grow step runs only when `--disk-size` is given and not with `--no-wait`,
   and cloud-init's `growpart` does the same at first boot. *If not met:*
   create fails at `grow_disk` (`vmtool grow: cannot find root partition ...`
   for LVM or a whole-disk filesystem), or the filesystem stays at the
   template's size. A smaller `--disk-size` leaves the template's size: **the
   template's virtual size is every clone's minimum.**
8. **Boots with a fresh NVRAM.** Every clone gets new, empty UEFI variables, so
   only the removable loader boots: `/boot/efi/EFI/BOOT/BOOTX64.EFI` (the
   images install it with `grub-install --removable` when it is missing), and
   no `ds=` or `autoinstall` on the kernel command line (the images strip
   both). Firmware is not detected: a BIOS template needs `create --firmware
   bios` every time. *If not met:* the clone stops in firmware and create fails
   at the address wait with the UEFI hint (`the VM uses UEFI firmware; an image
   installed for BIOS does not boot under UEFI ...`); the VM is left running.
9. **Nothing every clone must not share.** No private keys, tokens, `.env`
   files, cloud or registry credentials, `authorized_keys` entries for one
   machine's key (such as the `bot` key from `setup_bot.yml`), shell histories
   or agent state. vmtool cannot see these, and **every clone, and everyone who
   can reach any clone, gets them.** (hermes_setup's image build checks its
   template home for `.ssh` and for `KEY=` lines before its reset: `docs/IMAGE.md`,
   step 5.)
10. **On a thin pool, shut off.** The one item `templates save` checks, with
    the VM's boot disk being a thin volume vmtool made and the name being free.

**The author's flow.**

1. Create a build VM from a base image on the thin pool, **without**
   `--playbook setup_bot.yml`, so it keeps the `packer` login:
   `vmtool create hv1/build1 img1 --pool nvme-thin --disk-size 40`.
2. Install what the project needs: `vmtool playbook hv1/build1 ...` or
   `vmtool exec hv1/build1 -- ...`.
3. Reset the guest as the last thing: the optional reset playbook
   (below) runs the reset commands and powers the VM off, so wait for it to be
   shut off; or run the same commands by hand (what the images' last
   provisioner does: the GRUB and EFI loader steps, `apt-get clean`, delete
   root's history and the `127.0.1.1` line, `cloud-init clean --machine-id
   --logs`), then `vmtool stop hv1/build1` and wait for it to be shut off. Do
   not boot it again.
4. Check items 2 and 9 yourself; no playbook can.
5. `vmtool templates save hv1/build1 t1-tpl`.
6. Test a clone the way it will be used: `vmtool create hv1/test1 t1-tpl --pool
   nvme-thin --playbook setup_bot.yml`. It should come up with its own hostname
   and machine-id, and `ssh test1.hv1.vmtool true` should work. Delete the test
   VM afterwards.

**The optional reset playbook.** `ansible/playbooks/image_reset.yml` is the
author's shortcut for step 3, and vmtool never runs it itself. It runs the
same commands as the last provisioner of the `packer_images` templates
(`cloud-init status --wait`, the GRUB command-line cleanup and `update-grub`,
`grub-install --removable` when the loader is missing, `systemctl mask
tmp.mount` to keep `/tmp` on disk (26.04 mounts it as a tmpfs otherwise;
harmless on 24.04), `apt-get clean`, root's history, the `127.0.1.1` line, `cloud-init clean --machine-id --logs`) and
then powers the VM off a moment after it returns. Run it by hand as the last
thing on the build VM: `vmtool playbook hv1/build1
ansible/playbooks/image_reset.yml`; wait until `vmtool info hv1/build1` shows
it shut off, then `vmtool templates save`. Never give it to `create
--playbook`: create reboots the guest after a create-time playbook, and the
reboot gives the disk an identity back. It does not fix items 2 or 9.

### Rolling out thin pools

In this order, one host at a time. Each step is safe to stop after.

1. **Deploy** the helper, its sudoers line and API 2.4.0: `just deploy <host>`
   (and rebuild the workstation's `vmtool` for the new `pool list` columns and
   commands). Nothing visible changes: `vmtool pool list --host <host>` still
   shows no thin pool. `sudo -l -U vmtool` on the host shows exactly the
   helper line.
2. **Make the pool** with `vmtool_nvme_thin`, after checking `vgs vmtool-nvme`
   for free space, then run the `just prereqs` line of
   [Thin pool](#fast-block-disks-opt-in-nvme-stripe) with the array's own drive
   ids and `"vmtool_nvme_thin":true`. The pool then shows in `pool list`.
3. **Create thin** with `--pool nvme-thin`: the first create of an image builds
   its template (cold), the next ones are snapshots (warm). `templates list`
   shows the template. After a host reboot the clones start again.
4. **Make it the default** where wanted: `default_pool: nvme-thin` under
   `hosts.<host>` in the workstation's `config.yml`
   ([`default_pool`](#a-hosts-default-pool-for-new-vm-disks-default_pool)).
   Existing VMs stay where they are; move one by `vmtool delete` and `create
   --pool nvme-thin`. With a thin default pool, `create --noclone` and
   `--extra-disk-size` are not for those creates and may be refused by the
   daemon: name another pool for them.

### VM networks

A *vmtool network* is a named libvirt network that vmtool made: NAT to the
outside, DHCP and DNS, on a /24 of 10.77.0.0/16. VMs on it reach each other by
address and by name. Every network has a *home*: the host whose libvirt does
the NAT, the DHCP and the DNS. vmtool writes nothing of its own into the
network's definition; its bridge name, `vmt-` plus six hex digits, is what
marks it as vmtool's. Every other libvirt network (`default` included) is left
alone. Examples use hosts `hv1`, `hv2` and `home1`, and networks `net1` and
`net2`.

#### A network on one host

```bash
vmtool net create hv1 net1                  # /24 and bridge picked for you
vmtool net create hv1 net2 --subnet 10.77.1.0/24 --domain lab.test
vmtool create hv1/web img1 --network net1   # a card on net1 at create (repeatable; NAME=IP fixes the address)
vmtool net attach hv1/web net1               # an extra card on a VM that exists, live if it runs
vmtool net detach hv1/web net1               # the card and its reservation
vmtool net show hv1 net1
vmtool networks --host hv1                  # net1 (vmtool), default, ...
vmtool net delete hv1 net1                  # refused while a VM has a card on it; on a registered network's home it removes every member's and the registry entry
```

`vmtool networks` writes `net1 (vmtool)` for a network vmtool made, `net1
(vmtool, member)` for the member form on a host that is not its home (below),
and the bare name for any other libvirt network. A VM keeps its own NAT card
first; a network's card is an extra one, and `vmtool info` still shows the
first card's address as the VM's IP.

- **Bridge numbers.** The bridge is `vmt-` plus a number as six lowercase hex
  digits (`vmt-000001`, `vmt-002711`). The number is the network's VNI.
  Without a store the host numbers its networks from 1 to 9999. With a store
  the registry gives them out, from 10000 and up (10001 is `vmt-002711`), so
  the two ranges never meet.
- **Subnets.** Without `--subnet` the first /24 of 10.77.0.0/16 that no libvirt
  network and no route on the host uses (with a store, the first that no
  registered network uses). One that overlaps a libvirt network or a route is
  refused and the message names it.
- **Reuse.** When a network is deleted, its number and its subnet are handed
  out again to the next one made.
- **MTU.** The network's MTU is 1450, so a frame still fits the LAN once the
  overlay's header is added.

#### Names and fixed addresses

Attaching a VM reserves an address for the card's MAC in the home's network
(`--ip` picks it, else the lowest free address of .100 to .200) and gives it the
VM's name. A VM called `web` resolves as `web` and as `web.net1.test` (the
domain is `--domain`, else `<name>.test`; so `vm.net1.test` for a VM called
`vm`) from the first boot. The one-word name (`web`) needs the guest to take
the network's search domain from DHCP; images built since 2026-11-21 do
(`dhcp4-overrides: {use-domains: true}` in their netplan). Images built
before, and VMs cloned from them, keep the old behaviour until the images are
rebuilt or the guest's netplan is changed by hand; the full name
`web.net1.test` always works. By hand, in the guest:
`sudo sed -i 's/^\( *\)dhcp-identifier: mac$/&\n\1dhcp4-overrides: {use-domains: true}/' /etc/netplan/99-wildcard.yaml && sudo netplan apply`.
Reservations never expire:
a VM's fixed address survives a reboot, a shutdown and a month off, and is
freed only by `vmtool net detach` or `vmtool delete`. A name that is already on
the network, or an address already held, is refused.

#### A network that spans hosts

A host that joins a network becomes a *member*: it defines the same network as
only a bridge (no address, no NAT, no DHCP) and links to the home and to every
other member with a VXLAN device carried in UDP port 4789 over the LAN. The
VMs attach on the member as on the home; their address and name come from the
home's DHCP over the link. This needs a store: the registry keeps each
network's members and their LAN addresses.

```bash
vmtool net create home1 net1               # on the home; the store gives VNI 10001 and 10.77.1.0/24
vmtool net join hv1 net1                   # the first join also records the home as a member
vmtool net join hv2 net1
vmtool net attach hv2/web net1             # reserved on home1, attached on hv2
vmtool networks --host hv2                 # net1 (vmtool, member)
vmtool net leave hv2 net1                  # refused while a VM of hv2 has a card on it, and for the home
```

- **Store mode.** `net create` asks the store for the VNI and the /24 first,
  then has the host make the network with that bridge. If the host refuses,
  the registry entry is removed again. A host that runs a vmtool too old to take
  a network number is detected by its bridge; the network is undone and the
  message says to redeploy it (`just deploy <host>`).
- **A network made without a store** is registered by its first `net join`,
  under its bridge number as its VNI, if the registry does not have that number
  yet. Name the home with `--home` (`vmtool net join hv2 lab --home hv1`). If
  the number is taken the join is refused: recreate the network with a store.
- **The home** is often a small host that runs no VMs; see
  [A network home](#a-network-home-a-host-that-runs-no-vms). That setup uses
  `hosts.<h>.vms: false` and opens UDP 4789 from the LAN when ufw is active.
  Other firewalls must pass UDP 4789 between the hosts yourself.
- **Security.** VXLAN on the LAN is neither authenticated nor encrypted:
  anyone on the LAN can read the overlay's traffic and inject frames into a
  network whose VNI they guess. Use it on a LAN you trust.
- **At daemon start.** Each daemon reads its networks from the registry and
  links them, but only a network of that VNI that has the same name and lists
  this host (numbers of deleted networks are reused). With the store down it
  links nothing, logs once and retries, backing off from 30 seconds to 5
  minutes; serving is never delayed.

#### When the home or a member is down

Hosts are often off. Nothing waits for a member to wake, and the store never
records whether a host is up.

- **The home is down.** VM-to-VM traffic by address keeps working, host to
  host over the links. Names, new addresses (DHCP) and NAT through the overlay
  fail until it returns. A reservation never expires, so every VM keeps its
  address. `net attach`, `net detach`, `create --network` and `delete` for a VM
  on a member need the home (and the store, to find it); they stop with a
  message rather than guess. `vmtool delete` still deletes the VM, and the
  error names the reservation left on the home, and the registry marks it
  orphaned. The next `net attach`, `net detach`, `create --network`, `delete`
  or `net show` on that network, with the home answering, removes the orphaned
  reservations on the home and then their records, one line each; with the
  home silent it changes nothing (`net show` lists them). The registry also
  marks the reservations of a VM whose gone inventory row the 7-day sweep
  removes (a reservation records its VM's UUID); a host that is only silent
  marks nothing, and a VM with no row yet is never orphaned.
- **A member is asleep.** `net join` and `net leave` only need the host being
  changed (and, on the first join, the home). Every other member that answers
  gets the new peer list; one that does not is skipped with a line and
  takes the peer list from the registry when it wakes (its daemon reads it at
  start).
- **Deleting a registered network.** `vmtool net delete HOME NAME` removes the
  network everywhere and its registry entry. It must run on the home (on a
  member it says to run `net leave`, or to use the home). It is refused while
  any VM has a card on the network, on the home or in a live inventory row of
  the store, and lists the VMs and the `net detach` commands. Each member host
  that answers unlinks and removes its member network, then its row is
  removed; a member that does not answer only has its row removed and the
  command names it: its daemon removes the network when it starts and finds the
  registry does not list the host for it (a member-form network on a `vmt-`
  bridge with no card on it; a home-form network is never removed that way,
  and with the store unreachable nothing is). Then the home's network, its
  reservation records, its row and the entry go. A member that refuses (a card
  on it) stops the delete; the message says what was done, and the command can
  be run again, as it can after any failure. With a store configured that does
  not answer, `net delete` is refused (try again); with no store it removes
  only the host's network.

#### After a network restart

The daemon links a network again when libvirt starts it (its "network started"
event) and says so in its log. But the VMs' cards stay detached from the
bridge after a `virsh net-destroy` and `net-start` of a vmtool network, and
vmtool never restarts libvirt for you. Repair by hand on that host: restart
libvirtd (`sudo systemctl restart libvirtd`; the VMs keep running), and the
cards come back on the bridge. The event watch is one libvirt connection: after
a libvirtd restart, events are not seen until the vmtool daemon restarts
(`sudo systemctl restart vmtool-daemon`). The daemon's start-time pass and
`net join` still link, so restart the daemon after a libvirtd restart.

## Guest SSH and `ssh-config`

vmtool's own commands name a VM `host/vm`. Other tools (ssh, scp, rsync, sftp)
use one SSH name, `<vm>.<host>.vmtool`, split at the last dot before
`.vmtool`: VM names may contain dots (`a.b.hv1.vmtool` is VM `a.b` on
`hv1`), host names may not (a dotted `name:` or `hosts:` key fails config
load). A VM's credentials (bot key, password) live in
`.machines/<vm>.<host>.vmtool/`; the old `.machines/<host>/<vm>/` folders are
never read, moved or deleted by vmtool. Names ending in `.vmtool` cannot be
metal/alien machine names.

```bash
./vmtool ssh-config >> ~/.ssh/config     # one fixed block, `Host *.vmtool`
ssh web.hv1.vmtool
scp f vt-r7.hv2.vmtool:/tmp/x
rsync -a dir/ web.hv1.vmtool:/srv/
```

Remove an old `Host */*` block first: it has no credentials folder behind it
any more. The block runs `vmtool tunnel %n` in vmtool's working directory
(`vmtool tunnel` takes `host/vm` too) and needs `VMTOOL_TOKEN` in the
environment or in `.env` there. It uses `%n` because ssh lowercases `%h`.

### Console (WebSocket)

For a program rather than a person at a terminal, the REST server also
offers a console: `GET /v1/hosts/{host}/vms/{name}/console` and
`GET /v1/metal/{name}/console` upgrade to a WebSocket that is a login shell on
the target (`ssh -tt` on a pty on the server). Terminal bytes travel as binary
messages, a resize is a JSON text message, and the close code 4000 with the
reason `exit N` reports the exit status. It needs the bearer token and refuses
any `Origin`, so it is not for browsers. The handshake, the close codes, the
limits and a Python example are in
[`spec/README.md`](spec/README.md#console-websocket).

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
./vmtool metal exec -t nuc1 -- top                        # -t: a live terminal (ssh -tt)
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
