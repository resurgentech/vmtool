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
just prereqs veritech02 -e '{"vmtool_nvme_drives":["/dev/disk/by-id/nvme-A","/dev/disk/by-id/nvme-B"]}'
```

The other variables are `vmtool_nvme_vg` (`vmtool-nvme`), `vmtool_nvme_md`
(`vmtool-nvme`, the array `/dev/md/<name>`) and `vmtool_nvme_pool` (`nvme`).
Logical volumes that vmtool did not create are yours; the play never removes
anything.

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
once. The service runs as `vmtool`, never as your user. Its unit is
`vmtool-daemon.service` and it listens on the host's `port` (default 9473;
see [One daemon per host](#one-daemon-per-host)):

```
/opt/vmtool/                 vmtool:vmtool 0750  working directory and HOME
  vmtool                     0755  the daemon
  config.yml                 0600  role: hypervisor, listen 0.0.0.0:<port>, token hash
  tls/<host>.labtwo.crt      0644
  tls/<host>.labtwo.key      0600
  image-manifest.yml         written by the daemon
/etc/systemd/system/vmtool-daemon.service   User=vmtool, ExecStart=/opt/vmtool/vmtool server
```

The staged `config.yml` and key are removed after the install. `just cert
<host>` on its own renews the certificate the same way (into
`/opt/vmtool/tls`, owner `vmtool`) and restarts the daemon if it is running.

Before it installs, deploy checks that nothing but its own daemon holds the
port, and refuses if something else does. It also stops, disables and removes
an older `vmtool.service` that ran from `/opt/vmtool` (the daemon's name
before `vmtool-daemon.service`); a `vmtool.service` that runs from anywhere
else is left alone.

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
it, the address is `VMTOOL_SELF_ADDRESS` (default `<host>.<domain>`) and the
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

Name the store in `config.yml` (https only, no credentials in the URL):

```yaml
server:
  url: https://vmtool-store.labtwo
```

The client's token is `VMTOOL_SERVER_TOKEN`, read like `VMTOOL_TOKEN`: from
the environment, else from `.env` in the working directory (a real
environment variable wins). It is never read from `config.yml`. The
tokens are made by `make_secrets.sh tokens` in `ansible_labtwo` (one
OpenBao field per caller name, under the path `vault_vmtool_store_tokens`,
`infra/labtwo/vmtool_store/tokens` on mount `kv`). Fetch your own with
`bao kv get -mount=kv -field=<your name> infra/labtwo/vmtool_store/tokens`
and put it in `.env` as `VMTOOL_SERVER_TOKEN=<token>`.

Trust: the store is served over HTTPS by Caddy with a certificate issued
under the `hulbert.local` CA, the same CA as the daemons. **That CA must be
in the system trust store of every client and of every hypervisor that
fetches images** (ansible_labtwo's `selfsigned_cert` role installs it).
vmtool does no CA pinning and has no way to skip verification.

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
  says so: `vmtool-store.labtwo unreachable: using this machine's .machines/
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
   (the SHA-256 and size must match). Progress shows as for an upload. The
   image bytes never pass through the client. A daemon that already holds
   the image at the store's digest does nothing.
2. If the daemon cannot fetch (no `store_url`: 409; a daemon older than 2.3.0:
   404; the store not reachable from the host), the client **relays**: it
   streams the image from the store into the daemon's `uploadImage` with no
   local file, after one line saying so. A digest or size mismatch is not
   relayed; `create` fails.
3. If the store lacks the image but `image_dir` or a local pool has it, it is
   uploaded to the daemon as in local mode, with a line suggesting
   `vmtool images push`.

**A ticket opens one image only.** It is for one image at one digest,
read-only, good for one download and 15 minutes, held by the store as a hash in
memory only. Every other store route refuses it with 401. Daemons hold no
store token and no S3 key.

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

#### Building and deploying the store

The store is a static Go binary (`CGO_ENABLED=0`, distroless, runs as uid
65532). From this repository:

```bash
just store-image <tag>    # builds registry.labtwo/vmtool-store:<tag> and :latest, pushes both
```

`<tag>` is a version; empty and `latest` are refused. No secret goes into the
image. It is deployed to sdf1 by the `vmtool_store` role in `ansible_labtwo`,
behind Caddy as `https://vmtool-store.labtwo` (port 8095, not behind
Authelia). The role's own `README.md` has the detail. In outline, with `bao`
logged in:

```bash
# once, in ansible_labtwo
./make_secrets.sh tokens <caller> <caller> ...   # REPLACES the list: name every caller
./make_secrets.sh enckey                         # refuses to overwrite an existing key
# each deploy, with VAULT_ADDR and VAULT_TOKEN set
ANSIBLE_ROLES_PATH=$PWD/public/roles:$PWD/roles:$HOME/.ansible/roles \
  ansible-playbook -i inventories/labtwo.yml playbooks/sdf1_vmtool_store.yml
```

The role renders `tokens.yaml` with the SHA-256 of each token, so the store
never holds a token, and writes `enc.key` (0600) from OpenBao
(`vault_vmtool_store_enc_key`, field `key`). MinIO is reached by container
name with the shared S3 key; the store creates its bucket `vmtool-store`
itself at start if it is missing. Image blobs live in the bucket, and
metadata and encrypted credentials in `data/store.db` on sdf1. The store
refuses a token or key file readable by group or others, and a data
directory looser than 0700. It serves plain HTTP only on loopback or with
`behind_proxy: true` behind a TLS-terminating proxy (rule 5 in `AGENTS.md`);
it needs a token on every route except `GET /version`.

**Key loss: `enc.key` encrypts every stored credential.** If it is lost from
both OpenBao and the file on sdf1, every stored credential is lost, and
nothing can recover them. A new key (`make_secrets.sh enckey --force`)
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
  `token_sha256` (`just token-fetch` gets it from OpenBao);
- the `hulbert.local` CA in the system trust store, so TLS to the daemons
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
./vmtool exec -t jetfire/web -- top                     # -t: a live terminal (ssh -tt), like docker exec -it
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
  veritech02:
    address: veritech02.example
    ssh_user: ubuntu
    default_pool: nvme          # new VM disks created on veritech02
```

Scope: where new VM disks go; images stay in the cache pool. It applies to an
empty pool only, and only to the clone target, the extra disk and `add-disk`
(`CreateVolume`, `AddDisk`) and the domain's `Pool`; a named pool always wins.
Image upload, listing and delete, `ListPools`, `migrate-disk` and resize are
unchanged. On a hypervisor `default_pool` wins over `vmtool` for disks while
`vmtool` stays the image-cache pool. The value is a plain pool name
(`^[A-Za-z0-9][A-Za-z0-9._-]*$`); anything else is a config load error naming
the key.

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

A clone that vmtool creates in a dir pool gets `cache='unsafe'` on its disk
driver, so guest writes are fast (a clone in a `logical` pool gets `none`; see
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

Measured on veritech02 (4 vCPUs, fio on the bare virtual disk, 4k I/O):

| backing / host cache | write+fsync IOPS | randwrite IOPS | randread IOPS |
|---|---|---|---|
| qcow2 on the boot drive, `unsafe` (the dir-pool default) | 9.5–9.8k | 74–81k | 129–132k |
| LV on the stripe, `none`, `io='native'`, 4 queues, an IO thread (what vmtool writes) | 15.5k | 136k | 132k |

```bash
vmtool pool list --host veritech02                       # TYPE logical for nvme
vmtool create veritech02/build1 ubuntu2604 --pool nvme --disk-size 40
vmtool add-disk veritech02/build1 100 --pool nvme         # LV build1-2, attached as a block disk
vmtool delete veritech02/build1                           # removes both LVs
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

Not supported: growing an LV-backed disk, `migrate-disk` to or from a logical
pool or of a block disk (refused before the VM is stopped), thin snapshots
or instant clones, moving or converting existing VMs, creating a logical pool
from the CLI or REST (`pool create` makes dir pools only), and RAID levels
other than 0.

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
