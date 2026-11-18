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

## 2026-11-17 — C25 `vmtool exec -t` / `vmtool metal exec -t`: a live terminal

- `-t`/`--tty` on `exec` and `metal exec` runs ssh with `-tt` right
  before the destination, the caller's terminal attached live (raw
  mode, size and resizes forwarded by ssh). Without `-t` the argv is
  unchanged; quoting is unchanged either way. A `-t` after the target
  belongs to the remote command (`SetInterspersed(false)` kept).
- pkg/vmtool: `SSHOption` / `WithTTY()`, a trailing variadic on
  `RemoteCmd`, `MachineRemoteCmd`, `vmSSH`, `buildSSH`; `vmtool ssh`
  unchanged. app's `Service.RemoteCmd` / `MachineRemoteCmd` pass it
  through; REST exec untouched.
- Commits: the C25 commit on `clone-identity`.

## 2026-11-17 — C23 WebSocket console for VMs and machines

- `GET /v1/hosts/{host}/vms/{name}/console` (`openVMConsole`) and
  `GET /v1/metal/{name}/console` (`openMachineConsole`), WebSocket
  upgrades, workstation-only, behind the bearer middleware. Optional
  `cols`/`rows` (1-65535, default 80x24). Exec paths get no GET.
- Handshake refusals are JSON before the upgrade, in order: 401, 403
  workstation, 403 any `Origin`, 426 not HTTP/1.1, 400 not an upgrade,
  400 name/size, 404, 409 no address, 429 (16 consoles), 502 start.
- ssh `-tt` (login shell, `vmtool.ShellCmd` / `MachineShellCmd` with
  `WithTTY`) on a local pty (`creack/pty`, new session) with
  `TERM=xterm-256color`, started before the upgrade through the guarded
  constructor. Binary both ways; text = JSON `resize` control, bad text
  ignored; >64 KiB in closes 1009; output chunks <=32 KiB. Close 4000
  `exit N` on shell exit (output drained first), 4503 on shutdown, 1011
  `console failed` / `ping timeout` (30 s ping and pong). Every ending
  SIGKILLs the process group. Nothing from the stream is logged.
- New deps: `github.com/coder/websocket` v1.8.14, `github.com/creack/pty`
  v1.1.24. API stays 2.1.0.
- Commits: the C23 commit on `clone-identity`.

## 2026-11-17 — C24 Console contract and client example in the docs

- spec/README.md "Console (WebSocket)": handshake and refusal order,
  message table, resize, binary-only input, ignored bad text, close codes
  (4000 `exit N` with its regex, 4503, 1009, 1011), limits, no logging,
  a Python `websockets` example (bearer header, CA from env, bytes input,
  exit code from the close), the comparison with interloc's contract,
  and a hermes_setup porting note (`send_secret` becomes exec with
  `stdin`).
- README: a console paragraph linking there. The two console operations'
  descriptions cross-link; the machine one no longer lists a 409.
- Commits: the C24 commit on `clone-identity`.

## 2026-11-18 — C26 `exec host/vm -- cmd`: the `--` after the target is the separator

- `vmtool exec` and `vmtool metal exec` drop one `--` that sits directly
  after the target; everything after it is the command, exactly. Any
  other `--` is the command's. `exec -- T cmd` still works (pflag).
- From C7 until C26 the documented form `exec … -- …` sent a literal `--`
  to the guest as the first word of the command.
- `exec T` and `exec T --` are the same usage error, "exec: no command
  given after the target", raised before any lookup or ssh.
- `internal/cli/exec_args.go` (`execCommand`, `execArgs`), used by both
  commands; `Long` of both says it. README examples already matched.
- Tests: `TestExecDash`, `TestExecNoCommand`, tightened `TestExecTTYFlag`.
- Commits: the C26 commit on `clone-identity`.

## 2026-11-18 — L1 Pool type and disk backing are reported (API 2.2.0)

- Spec 2.2.0: optional `PoolInfo.type` (libvirt's pool type: `dir`,
  `logical`, …) and `VMInfo.disk_backing` (`file|block`, of `vda`); both
  omitted when unknown or from an older daemon.
- `pkg/vmtool`: `parsePoolType`; `parseDisks` replaces `parseFileDisks`
  and returns file and block disks with their kind. `ListPools` and
  `Info` fill `PoolInfo.Type` / `VMInfo.DiskBacking`.
- No behaviour change for storage: `Delete` still removes file disks
  only; resize-disk and migrate-disk refuse a block `vda` with a clear
  error (L2 adds block handling). `AddDisk` now counts block targets as
  used.
- Server, remote client and `pool list` (new TYPE column, `-` when
  unknown) carry the fields; a 2.1.0 answer decodes to "".
- Commits: the L1 commit on `clone-identity`.

## 2026-11-18 — L2 Block disks in the libvirt layer

- `pkg/vmtool`: a block disk (`<source dev=…>`, raw, `discard='unmap'`,
  `queues` = min(vCPUs, 4), `io='native'` only with cache `none` /
  `directsync`, `iothread='1'` when the domain has iothreads). The domain
  gets `<iothreads>1</iothreads>` iff a disk is block. File disks keep
  today's XML byte for byte (golden tests).
- Define resolves each disk's kind from the volume libvirt finds at its
  path (unknown path: file). `DiskDevice.Kind`, `VMConfig.DiskKind`.
- CreateVolume / AddDisk in a `logical` pool make an LV named without
  `.qcow2`, no `<format>`, allocation = capacity, attached as block.
- Delete removes a block disk only when its volume is in a `logical`
  pool; block paths elsewhere are never touched.
- libvirt cannot resize a logical volume (verified on veritech02), so
  resize-disk on a block `vda` fails before touching the VM: "growing a
  block-device disk is not supported …; create the VM with --disk-size".
  ResizeVolume on a block volume already large enough is a no-op.
- migrate-disk refuses a block `vda` or a logical target before stopping
  the VM.
- Commits: the L2 commit on `clone-identity`.

## 2026-11-18 — L3 Create clones into a logical pool by conversion

- `CloneImage(base, name, pool, sizeBytes)`: into a `logical` pool it
  makes an LV named `<vm>` (no suffix, no format, capacity = allocation
  = max(image, size), in bytes) through `StorageVolCreateXMLFrom`, so
  libvirtd converts qcow2 to raw as root. Dir pools: today's XML; size
  ignored.
- Spec: optional `CloneVolumeRequest.size_bytes` (min 1); `.qcow2` names
  apply to dir pools only. Still API 2.2.0. The remote client sends it
  only when > 0; the server passes it on (400 below 1).
- create passes `--disk-size` to the clone. A clone with no
  `--disk-cache` lists pools first (a failure stops create before the
  clone) and gets cache `none` in a logical pool, `unsafe` elsewhere
  (C10). The clone event names the real volume.
- The daemon's path check accepts an LV path unresolved when it is
  exactly `<logical pool path>/<name>` and exists; dir pools keep the
  resolved comparison.
- Commits: the L3 commit on `clone-identity`.

## 2026-11-18 — L4 A host's default pool for new VM disks

- Workstation `config.yml`: optional `default_pool` (the local host) and
  `hosts.<name>.default_pool` (a remote host), a plain pool name; a bad
  value is a load error naming the key.
- `internal/hosts`: a `defaultPools` wrapper sends an empty pool to that
  pool for the clone target, `CreateVolume`, `AddDisk` and Define's
  `Pool` only; a named pool wins; images, `ListPools`, migrate and resize
  are unchanged. On a hypervisor it sits outside `hypervisorPools`, so
  `vmtool` stays the image-cache pool. For a remote host the workstation
  fills the pool before the request. The wrapper keeps the workstation's
  pool refresh (C8) and the image cache's `RefreshPool` visible.
- README: the key, its scope and where it lives.
- Commits: the L4 commit on `clone-identity`.

## 2026-11-18 — L5 Host setup builds the NVMe stripe and the logical pool, opt-in

- New `ansible/playbooks/tasks/nvme_stripe.yml`, included by
  `setup_host.yml` only when `vmtool_nvme_drives` (a list of
  `/dev/disk/by-id/` paths) is non-empty; every task carries that guard.
  Destructive by nature, runs only with the drives named, refuses
  non-blank drives.
- Preflight before any write: Debian family, at least 2 whole by-id
  drives, no duplicates after `realpath`, mdadm and lvm2 installed.
- Adopt first: an existing VG `vmtool_nvme_vg` is adopted without
  examining the drives; an md array of that name without the VG fails.
- Blank checks (read-only): partitions, signatures (`--no-act` dry run
  and `blkid -p`), holders, mounts, swap. Only then `mdadm --create`
  (RAID 0), `pvcreate`, `vgcreate`: the only writing storage commands.
- Persistence: the ARRAY line in `/etc/mdadm/mdadm.conf` (by UUID),
  `update-initramfs -u` only when it changed.
- libvirt `logical` pool `vmtool_nvme_pool` defined without
  `pool-build`; an existing pool of the same type and VG is adopted
  (autostart and start as needed), a different one fails.
- Structural tests in `scripts/nvme_stripe_test.go`; the playbook was not
  executed. README "Hypervisors" and `scripts/prereqs.sh` give the command.
- Commits: the L5 commit on `clone-identity`.

## 2026-11-18 — L6 Docs for fast block disks

- README "Fast block disks (LVM on NVMe)": when to use it (disposable
  workers, no redundancy), the two key measured rows, clone by
  conversion, size at create (libvirt cannot resize an LV; resize-disk
  refuses), cache `none` with `io='native'` by default and why, IO thread
  and queues, `default_pool`, delete removes only LVs of logical pools,
  API 2.2.0 fields, non-goals. The disk-cache section notes the logical
  exception.
- spec/README: `PoolInfo.type`, `disk_backing`, `size_bytes`, the LV path
  rule for logical pools, and `default_pool` in the empty-pool paragraph.
- The user-run checks on veritech02 (M1–M7) are not done; they are listed
  in the step's report.
- Commits: the L6 commit on `clone-identity`.

## 2026-11-18 — I1 Images keep /tmp on disk, emptied at boot, as on 24.04

- All seven packer templates mask `tmp.mount` (no `--now`) in the last
  provisioner and install the new shared `packer_images/files/00-vmtool-tmp.conf`
  (`D /tmp 1777 root root 30d`) to `/etc/tmpfiles.d/`, so 26.04 images
  keep /tmp on disk, emptied at every boot, as 24.04 does.
- `just -f packer_images/justfile check` gains the shared-file check and
  `check_tmp_on_disk`; red before, green after. `packer_images/CLAUDE.md`
  has a "/tmp on disk" section; details in `packer_images/docs/LOG.md`.
- Image rebuilds and clone checks are user-run and not done.
- Commits: the I1 commit on `clone-identity`.

## 2026-11-18 — L7 CLI help states both pool kinds and default_pool

- Help only, no behaviour change. create `--pool` and add-disk `--pool`
  name the host's `default_pool` (workstation `config.yml`) before the
  built-in `default` / `vmtool`, and the volume names per pool kind
  (`<name>.qcow2` in a dir pool, an LV `<name>` in a logical pool).
- create `Long`, `--disk-cache`, `--extra-disk-size` and add-disk `Long`
  state the logical-pool names, the cache default `none` for a clone in a
  logical pool, and that such a disk is sized at create and cannot grow.
  resize-disk and migrate-disk `Long` state their refusals of LVs.
- TUI comments for the host-default pool option and the cache default
  updated; the form itself is unchanged. `images delete --pool` keeps
  its text (default_pool does not apply to images).
- Test `TestDiskHelpNamesPoolKinds` (`internal/cli/pool_test.go`): red
  on today's text (11 failures), green after; `just check` green.
- Commits: the L7 commit on `clone-identity`.

## 2026-11-18 — L8 NVMe stripe: no regex back-references

- The first real run on ansible-core 2.21.5 failed in `_uuid`, `_type`
  and `_src` (`'NoneType' object has no attribute 'group'`): the decoded
  `'\\1'` and `\\s` were wrong there (L5's escaping decision). The three
  vars in `nvme_stripe.yml` now match without a group and cut the match
  down with `regex_replace`; no task, `when` or module argument changed.
- `TestNvmeStripeRegexShape` (in the gate, no Ansible) replaces
  `TestNvmeStripeBackrefsSurviveYAML`: no decoded `regex_` string under
  `ansible/` holds `\\`, and `nvme_stripe.yml` holds no back-reference.
- `TestNvmeStripeExpressionsEvaluate` (opt-in,
  `VMTOOL_TEST_ANSIBLE_SYNTAX=1`) evaluates the very strings with
  `ansible localhost -c local -m debug` on sample mdadm and pool XML.
- Both red on the old file, green after; `just check` green.
- Commits: the L8 commit on `clone-identity`.

## 2026-11-18 — L9 `just prereqs` passes each argument through unchanged

- The `prereqs` recipe has just's per-recipe `[positional-arguments]`
  and ends `"{{ root }}/scripts/prereqs.sh" "$@"`, so the documented
  `just prereqs veritech02 -e '{"vmtool_nvme_drives":[…]}'` reaches
  prereqs.sh as three arguments instead of a brace-expanded, unquoted
  JSON. No other recipe changed.
- The header of `scripts/prereqs.sh` shows the same `just prereqs` line
  as `README.md` and `nvme_stripe.yml`; `TestNvmeStripeWarnsDestructive`
  lost its prereqs.sh exemption.
- `TestJustPrereqsPassesArgsVerbatim` (the real justfile over a
  `testexec.Stub` prereqs.sh) and `TestPrereqsDocsAgree`: red before,
  green after; `just check` green.
- Commits: the L9 commit on `clone-identity`.
