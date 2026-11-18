# Work log

Append-only history of what shipped from `ROADMAP.md`. FOREMAN appends
an entry per landed step or tranche. Newest at the bottom. Corrections
are appended, never edited in; a hook refuses rewrites.

```
## YYYY-MM-DD — <step id and title>

- What shipped
- Commits: <shas>
```

---

## 2026-11-14 — T0 orch set up

- `/orch init`: `.orch.json`, `docs/LOG.md`, `AGENTS.md`, the
  `CLAUDE.md` lines, `.grok/agents/`; `ROADMAP.md` and `tmp/` ignored.

## 2026-11-14 — T0a Remove the WebSocket console

- Dropped `openVMConsole` / `openMachineConsole` from the spec, regenerated
  `internal/api`, deleted `internal/server/console.go` + test, wired the
  strict handler directly; `gorilla/websocket` and `creack/pty` gone.
- Commits: 0ee7231

## 2026-11-14 — T2 `just token` and `just token-fetch`

- `scripts/token.sh` (generate | fetch), `.env` written atomically 0600, token
  to OpenBao on stdin only; `scripts/scripts_test.go` with stub `bao`.
- Commits: 4f07993

## 2026-11-14 — T1 config.yml and token loading

- `internal/config` (Load/defaults/validation, Token from env or .env,
  constant-time Check, Verify, atomic 0600 Write); `vmtool config write` and
  `vmtool config host`; `config.example.yml`; `.gitignore` entries.
- Commits: 68d05a6

## 2026-11-14 — T3 HTTPS, bearer auth and the bind rule for `vmtool server`

- Spec: `bearerAuth` global, `security: []` on the three docs routes, 401
  everywhere else. Server: spec-derived public set, constant-time token
  check before body/handlers, TLS from config.yml, no off-loopback bind
  without TLS + hash. AGENTS.md rule 5 rewritten.
- Commits: f935909

## 2026-11-14 — T4 `just cert` and `just install`

- `scripts/cert.sh` (CA key only via process substitution; remote copy over
  scp), `scripts/install.sh`, `deploy/vmtool.service.in`; tests with a real
  throwaway CA and stub bao/scp/ssh/sudo/systemctl.
- Commits: 9f7b5ab

## 2026-11-14 — T5 Enforce the token on every command

- Root `PersistentPreRunE` gate (config.yml + VMTOOL_TOKEN vs token_sha256)
  on every command except help, completion, `config write`; tree-walking
  test. `vmtool server` now HTTPS + hash only. From here the binary needs
  `just token` + `just install` to run.
- Commits: 0a3aaf3

## 2026-11-14 — T6 Versioned, host-scoped API paths and `/version`

- `/v1/hosts/{host}/...` for VMs and inventories, `/v1/{playbooks,metal}`,
  `GET /v1/hosts`, unversioned `GET /version` (API 1.0.0); `internal/hosts`
  resolver (local only), `internal/buildinfo`; unknown host 404.
- Commits: 971152c

## 2026-11-14 — T7 `host/vm` names, `.machines/<host>/<vm>/`, migration

- `app.VMRef` + `ParseVMRef`; credentials keyed by (host, vm) only (IP
  lookup removed); `vmtool_machines_dir` passed to setup_bot.yml;
  `vmtool machines migrate [--apply]`; host/machine name collision rules;
  TUI rows host/vm, dead stageClone chain removed; README examples.
- Commits: f0e1bb5

## 2026-11-14 — T5 follow-up: server gate

- `vmtool server` needs config.yml with `token_sha256` but no local token
  (hypervisors hold only the hash); found while building T14.
- Commits: 48b7cfa

## 2026-11-14 — T8 Tunnel endpoint and `vmtool tunnel`

- `GET /v1/hosts/{host}/vms/{name}/tunnel` (HTTP/1.1 Upgrade to the guest's
  port 22, libvirt IP only, dial before hijack); `internal/remote` Dial/Pipe;
  `vmtool tunnel host/vm`; server ALPN pinned to http/1.1.
- Commits: a41ac84

## 2026-11-14 — T9 Guest SSH through the tunnel; `vmtool ssh-config`

- `vmtool.Tunnel`; all guest ssh/cmd/grow/playbook/inventory use
  `ProxyCommand=<vmtool> tunnel host/vm`, token in child env only (CLI token
  or request bearer); `vmtool ssh-config`; TUI ssh via internal/app.
- Commits: 78fb694

## 2026-11-14 — T10 Daemon primitives and the hypervisor role

- Eight `/v1/hosts/{host}` primitives (image path, volumes create/clone/
  resize, define, raw undefine, wait-ip, raw resize-volume) with the
  in-pool path rule; `x-vmtool-workstation-only` ops are 403 on a
  hypervisor; Manager method map in spec/README.md.
- Commits: ac9b2fb

## 2026-11-14 — T14 `just deploy <host>` and `just prereqs <host>`

- `scripts/deploy.sh` (Go >= go_min check first, tar copy, remote build,
  cert, config write with token on stdin, unit, GET /version);
  `prereqs <host>`; setup_host.yml opens the daemon port under ufw.
- Commits: 0980f66

## 2026-11-14 — T11 Remote `app.Manager` and the host resolver

- `internal/remote.Manager` over the generated client (bearer, system
  trust, exact API version check, 30 s / 30 min timeouts); resolver reaches
  configured hosts with the CLI token or the request bearer; CLI/TUI/REST
  act on remote VMs; `--host` on networks/bridges/images/pool.
- Commits: 34c17bc

## 2026-11-14 — T12 Image manifests and upload

- `uploadImage` / `getImageManifest`; daemon owns only manifest-listed
  volumes (foreign → 409 before reading the body); `.part` + hash + link
  commit; workstation hash cache; CreateVM uploads only on a miss, with
  CLI/TUI progress.
- Commits: c9fb43e

## 2026-11-14 — T13 Combined listing and `vmtool hosts`

- `app.ListAll` fan-out (5 s per host, dead host = error row); `vmtool list
  [host]`; `vmtool hosts` (reachable, API version, cert days, <30-day
  warning); `GET /v1/hosts` status via the caller's bearer; async TUI
  refresh across hosts.
- Commits: 611e908

## 2026-11-14 — F1 `/version` public; no API version comparison

- `getVersion` is `security: []`, a reachability probe; `remote.Manager`
  no longer checks versions; `VersionError` gone; Probe sends no token and
  never flags a differing `api_version`.
- Commits: 0d6375e

## 2026-11-14 — F2 CLI cleanups

- `vmtool machines migrate` removed (with `internal/app/migrate.go`);
  `ssh-config` shell-quotes the ProxyCommand dir, ssh_config-quotes the
  IdentityFile, writes `%` as `%%`, refuses control characters; config.yml
  errors name the yaml line, never file text.
- Commits: 53966de

## 2026-11-14 — F3 Hypervisor pool `vmtool`

- Empty pool = the host's default on every surface; libvirt Manager maps it
  to `default`, a hypervisor's resolver to `vmtool`; image cache in the
  `vmtool` pool; TUI create form offers `(host default)` first.
- Commits: f8b3724

## 2026-11-14 — F4 Delete stale `.part` files

- In-process upload registry (second PUT of a running name 409); a stale
  `.part` is unlinked (file or symlink only) before the `O_EXCL` create; a
  hypervisor removes matching `.<name>.part` files in the `vmtool` pool at
  start, after binding.
- Commits: c942e69

## 2026-11-14 — F5 `just prereqs <host>` prepares a hypervisor

- `vmtool` system user, `/opt/vmtool`, dir pool `vmtool` at
  `/var/lib/vmtool/images` (no `default` pool), Go tarball only when needed;
  fixed the login-user libvirt task replacing the user's groups (`append`
  was under `vars`).
- Commits: 3ae2450

## 2026-11-14 — F6 `just deploy` / `just cert` into `/opt/vmtool`

- Prereq check before copying; build as the login user in
  `~/.cache/vmtool-deploy`; token only on stdin of a no-tty, no-sudo ssh
  call; one `ssh -t` / one sudo installs everything as `vmtool`; unit runs
  as `vmtool`; `/version` probed without a token.
- Commits: 75cec3c

## 2026-11-16 — C1 Resolve a VM's address by its own MAC

- `getIP` reads the domain's MACs and its networks' full lease tables;
  pure `pickIP` (`pkg/vmtool/ipselect.go`): lease, then agent, then ARP,
  own MACs only, newest expiry wins, an address whose newest lease is a
  foreign MAC's is never returned. `Info`, `List`, `WaitForIP` use it.
- Commits: the tranche-1 commit on `clone-identity` (one commit, all six steps).

## 2026-11-16 — C11 UUID and SMBIOS serial for each new VM

- `Define` fills a random v4 UUID (`prepareDefine`), writes `<uuid>`, the
  serial `ds=nocloud;h=<name, _→->;i=<uuid>` and `<smbios mode='sysinfo'/>`;
  `osXML` is the only writer of `<os>`; name XML-escaped. Help/README cover
  `--noclone` and the `cloud-init clean --machine-id` remedy.
- Commits: the tranche-1 commit on `clone-identity`.

## 2026-11-16 — U1 UEFI guests; `firmware` on every surface

- Spec `firmware` (efi|bios, default efi) on CreateVMRequest,
  DefineVMRequest, VMInfo; `--firmware`, TUI field; efi `<os>` with
  secure boot off; domcaps check before an efi define; NVRAM removed on
  undefine/delete; `GET` VM returns firmware; create's IP timeout on efi
  hints `--firmware bios`.
- Commits: the tranche-1 commit on `clone-identity`.

## 2026-11-16 — C10 Disk cache `unsafe` by default

- Spec `disk_cache` (none|writeback|writethrough|unsafe); app resolves
  `unsafe` for clones, nothing for `--noclone`; extra and added disks take
  vda's mode; MigrateDisk keeps it; `GET` VM returns it; `--disk-cache`,
  TUI field. `xmlAttr` now reads libvirt's double-quoted attributes.
- Commits: the tranche-1 commit on `clone-identity`.

## 2026-11-16 — C5 Wait for SSH before grow and playbook

- `wait_ssh` stage (`ansible -m wait_for_connection`, 120 s) after
  `wait_ip` when create grows or runs a playbook; wait and grow errors
  carry ansible's output (password redacted); no grow retry; resize's
  `grow_error` includes the output.
- Commits: the tranche-1 commit on `clone-identity`.

## 2026-11-16 — C12 Grow finds the root partition; NOCHANGE is success

- `growDiskCommand` resolves root via findmnt / sysfs / `lsblk PKNAME`
  (fails clearly on LVM), growpart exit 1 + NOCHANGE is success, fs step
  always runs (resize2fs or btrfs resize).
- Commits: the tranche-1 commit on `clone-identity`.

## 2026-11-16 — U3 Hosts get OVMF

- `setup_host.yml` installs `ovmf` (Debian/Ubuntu) and `edk2-ovmf` (Arch)
  in the base package lists; `TestSetupHostInstallsOVMF` pins them;
  README prerequisites say why (UEFI default, packer UEFI builds) and
  that no daemon restart is needed.
- Commits: the tranche-2 commit on `clone-identity`.

## 2026-11-16 — C3 Images end clean for cloud-init (README part)

- `packer_images` images now end with `cloud-init clean --machine-id`,
  so each clone takes its own identity at first boot (details in
  `packer_images/docs/LOG.md`). README Troubleshooting gains "Two VMs
  show the same IP": cause, `vmtool playbook host/vm commission.yml` for
  VMs cloned from older images, rebuild the image.
- Commits: the tranche-2 commit on `clone-identity`.

## 2026-11-16 — Ansible output is passed through unscrubbed

- Removed `redact` (added with C5): the wait_ssh and grow output in
  create, and the grow output in resize, are no longer rewritten.
  Decided by the user: playbook output was never scrubbed, and the
  replacement also hid ordinary words equal to the password.
- Commits: follow-up commit on `clone-identity`.

## 2026-11-16 — C14 Images ship without the installer's `127.0.1.1` line

- `packer_images`: every template deletes `/etc/hosts`' `127.0.1.1`
  lines just before `cloud-init clean`, so a clone's `127.0.1.1` line
  names only the VM, not the image too (B0 finding). New shape check
  `check_no_installer_hosts_line`. Details in `packer_images/docs/LOG.md`.
- Commits: the C14 commit on `clone-identity`.

## 2026-11-16 — C15 `ubuntu2404_aws` takes its checksum from Canonical's SHA256SUMS

- `packer_images`: the aws template's `iso_checksum` reads
  `noble/current/SHA256SUMS` (`file:` form) instead of a pinned sum that
  went stale on the rolling URL. New shape check
  `check_rolling_url_checksum`. Details in `packer_images/docs/LOG.md`.
- Commits: the C15 commit on `clone-identity`.

## 2026-11-17 — C8 Image lookup refreshes before "not found"; foreign refusal says how to get past it

- On the workstation (CLI, TUI, workstation `vmtool server`), a create
  that finds its image in no pool refreshes the active pools once
  (`Manager.RefreshPools`) and looks again (`vmtool.FindImage`,
  `app.FindLocalImage`). A hypervisor daemon never refreshes pools it
  does not own. The foreign-image refusal (app and daemon) now ends
  with `app.ForeignImageHint`: copy the image under another name
  (e.g. `<name>-vmtool.qcow2`) and create from that.
- Commits: the C8 commit on `clone-identity`.

## 2026-11-17 — C9 `getImageManifest` stays fast when the hypervisor is busy

- The daemon resolves its `vmtool` pool directory once (warmed at start,
  kept for its life) and answers a listed image from the manifest plus
  one `Lstat`, with no libvirt connection. Unlisted names and a cold
  cache still resolve the Manager.
- The daemon logs `slow request` (Warn) for any request over 5 s:
  operation id, host, name, status, duration; never a body, header or
  query. Tunnel sessions (101) are exempt.
- The remote client retries `getImageManifest` once on a timeout only
  (typed `timeoutError`); no other call retries.
- Commits: the C9 commit on `clone-identity`.

## 2026-11-17 — C19 Tests cannot start real tools; the gate runs capped

- After the 2026-11-17 fork bomb (a test's real `ansible` -> `ssh` used
  the test binary as its ProxyCommand, which re-ran the suite).
- `pkg/vmtool/exec.go`: every external tool starts through `command()`.
  Under `go test` (`testing.Testing()`) it refuses, without starting a
  process, any name that does not resolve inside `VMTOOL_TEST_STUBS`:
  `test tried to run real "<name>"; install stubs with testexec.Stub(t, ...)
  (internal/testexec)`. All six exec sites (ansible x2, ansible-playbook,
  ssh, sshpass, sudo) use it.
- `vmtool.CurrentExe()` errors under `go test` or for a `*.test`
  executable, and `Tunnel.check()` refuses a test binary given as `Exe`.
  The server's `guestSvc` then has no tunnel; the CLI's `env` gains a
  `tunnelExe` seam for tests, like the server's.
- New `internal/testexec`: `Stub(t, scripts)` and `Ansible(t)` (the old
  `sshStub`). The app and pkg/vmtool stubs use it.
- `just test` (so `just check` and the orch commit hook) runs
  `scripts/test-capped.sh`: `go test` in a `systemd-run --user --scope`
  with TasksMax 256 (measured peak 114, doubled, rounded up) and
  CPUQuota of half the online CPUs; plain `go test` with a warning when
  `systemd-run --user` is unavailable. `just test` takes package args.
- AGENTS.md hard rule 6 (Tests) and a "Test stubs" row.
- Commits: the C19 commit on `clone-identity`.

## 2026-11-17 — C13 `create` fails without an address or SSH; `--no-wait`; both waits in config.yml

- Every create now waits for the address and then for SSH (one
  `wait_ssh`, before the grow and the playbook). Either failure returns
  the VM info plus an `*app.Error` (Op `wait_ip` / `wait_ssh`) saying the
  VM exists and is running and naming `--no-wait`; the VM is left
  defined and running. `wait_ip` keeps the underlying text and, for efi,
  `UEFIHint`.
- `--no-wait` (spec `no_wait`, TUI "Wait for address" yes/no) returns
  right after `start`; refused with `--playbook` before anything is
  cloned; with `--disk-size` the volume is resized and the filesystem
  grow is skipped with a `grow_disk` event.
- The inventory is written right after `start`.
- config.yml `ip_wait_seconds` / `ssh_wait_seconds` (0 or absent = 120,
  else 1..3600) feed the CLI, TUI and workstation server Service.
- CLI: after any failure once the domain exists it prints
  `VM "<ref>" exists and is running, but create did not finish:` and
  the error, non-zero; the success and `IP:` lines only on success.
  REST: 500 with `vm`; `no_wait`+`playbook` 400.
- Every create test in app, server, CLI and TUI installs
  `testexec.Ansible` and a stub tunnel `Exe`.
- Commits: the C13 commit on `clone-identity`.

## 2026-11-17 — C16 `setup_bot.yml` keeps the tunnel when it reconnects as bot

- The "Switch remaining tasks to bot key auth" `set_fact` appends
  `-o IdentitiesOnly=yes` to the existing `ansible_ssh_common_args`
  (default: today's fixed args for a hand-run inventory) instead of
  replacing it, so a VM keeps its `vmtool tunnel` ProxyCommand and the
  first run no longer ends "Could not resolve hostname host/vm".
- The playbook header says a half-done VM is finished by a re-run.
- New `scripts/setup_bot_test.go` (yaml.v3); its
  `ansible-playbook --syntax-check` subtest runs only with
  `VMTOOL_TEST_ANSIBLE_SYNTAX=1`, never in the gate.
- Commits: the C16 commit on `clone-identity`.

## 2026-11-17 — C17 `setup_bot.yml` really disables SSH password login on VMs

- The VM drop-in is now `/etc/ssh/sshd_config.d/01-vmtool-no-password.conf`,
  which sorts before cloud-init's `50-cloud-init.conf` (sshd keeps the
  first value). The old `99-disable-password-auth.conf` is removed, so a
  re-run fixes older VMs. cloud-init's file is left alone.
- On VMs the playbook now runs `sshd -t` and `sshd -T` for bot and root
  and asserts password and keyboard-interactive login are off, naming
  any file that still says yes; this runs before packer is removed. The
  metal/alien path is unchanged.
- `scripts/setup_bot_test.go`: sort order, the old file's removal, and
  that the VM check runs for VMs before "Remove packer user". README
  says what setup_bot does on a VM.
- Commits: the C17 commit on `clone-identity`.

## 2026-11-17 — C18 One SSH name for every tool: `<vm>.<host>.vmtool`; credentials under that name

- `pkg/vmtool`: `SSHName`, `ParseSSHName` (split at the last dot before
  the lowercase `.vmtool`), `ValidHostName` (no dots). `CredsDir` is
  `.machines/<vm>.<host>.vmtool`, the one place the path is built; so
  `AbsCredsDir`, `ResolveVMAuth`, `ForgetVM`, the inventory key path and
  `vmtool_machines_dir` follow. The old `.machines/<host>/<vm>/` is never
  read, written or removed; no migration code.
- config load refuses a dotted `name:` or `hosts:` key; AddMachine refuses
  names ending in `.vmtool` (any case).
- `vmtool tunnel` also takes `<vm>.<host>.vmtool`; `ssh-config` prints
  one `Host *.vmtool` block using `%n` in ProxyCommand and IdentityFile.
  Help, README and the setup_bot header say so.
- Cut-over (copy script re-run, `~/.ssh/config`, checks) is user-run.
- Commits: the C18 commit on `clone-identity`.

## 2026-11-17 — C7 `cmd` becomes `exec`: exact argv, everywhere (API 2.0.0)

- `vmtool exec host/vm [--] <arg>...` and `vmtool metal exec <name>
  [--] <arg>...`; `cmd` is gone, no alias. REST:
  `POST /v1/hosts/{host}/vms/{name}/exec` (`execVM`) and
  `POST /v1/metal/{name}/exec` (`execMachine`), `ExecRequest.command`
  an argv with `minItems: 1`; the old `/cmd` paths are 404.
  Workstation-only (403 on a hypervisor) as before.
- `RemoteCmd` / `MachineRemoteCmd` single-quote every argument
  (`'` as `'\''`) so the guest gets exactly the argv; shell features
  only via `-- sh -c '...'`. `vmtool ssh` (`buildSSH`) is unchanged.
  `metal exec` now uses the non-interactive, quoted command.
- The machine probe runs `sh -c <probeScript>`.
- app: `ExecVM`, `ExecMachine`, `ExecResult` (were `RunCommand`,
  `RunMachineCommand`, `CmdResult`).
- `info.version` 2.0.0, so `/version` reports API 2.0.0; C13's
  createVM change ships under it.
- Commits: the C7 commit on `clone-identity`.

## 2026-11-17 — C20 REST `exec` takes an optional `stdin` (API 2.1.0)

- `ExecRequest.stdin` (optional string): written to the command's
  standard input, then closed; absent or empty is an empty stdin as
  before. Never logged, never in an error or argv; bounded by the 1 MiB
  body limit. Both exec operations carry a `with_stdin` example.
- app: `ExecVM(ctx, m, ref, argv, stdin)` and `ExecMachine(ctx, name,
  argv, stdin)`; `runRemote` no longer forces `Stdin = nil` (probe and
  reboot still get /dev/null). Password machines carry it through
  `sshpass -e ssh`.
- `info.version` 2.1.0; tranches 4 and 5 ship under it.
- Commits: the C20 commit on `clone-identity`.

## 2026-11-17 — C21 Playbook variables travel in the environment, not argv

- `vmtool.RunPlaybook` (the only builder of the ansible-playbook command)
  emits, per variable in sorted order, `--extra-vars
  name={{ lookup("ansible.builtin.env", "VMTOOL_VAR_name") }}`; values go
  in the child environment as `VMTOOL_VAR_<name>`, inherited
  `VMTOOL_VAR_*` entries stripped. Covers create `--playbook`,
  `playbook`, `metal playbook` and `metal add`, built-ins (`deviceid`,
  `vmtool_kind`) included.
- New `vmtool.CheckExtraVars`: names `^[A-Za-z_][A-Za-z0-9_]*$` (empty
  refused), NUL values refused; messages name the variable, never the
  value. Run by app (KindInvalid, REST 400; CreateVM before any libvirt
  call), by `RunPlaybook`, and by the CLI's `parseExtraVars`.
- Spec `ExtraVars` description states the rule; no version change
  (ships under 2.1.0).
- Commits: the C21 commit on `clone-identity`.
