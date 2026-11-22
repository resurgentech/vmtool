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

## 2026-11-18 — NVMe pools live on veritech03 and veritech04; runners rebuilt on them (XO)

- On the user's word ("Veritech three and four are yours ... Delete any
  existing VMs on those two machines and replace them with the new
  system"): both daemons deployed at API 2.2.0; vt-r7..vt-r12 and
  int_bot1 (veritech03) and vt-r1..vt-r6 (veritech04) deleted.
- Host setup with each host's two Samsung drives, first real run of L5.
  It stopped on veritech03 after creating the array and the volume group
  (the regex defect fixed in L8); the documented `just prereqs` form
  could not pass the drive list (fixed in L9). After L8/L9: veritech03
  re-run took the adopt path (ARRAY line written, initramfs rebuilt, pool
  `nvme` defined, autostarted, started); veritech04 ran in full; a second
  pass on each changed nothing. `VMTOOL_TEST_ANSIBLE_SYNTAX=1 just test
  ./scripts/` green on ansible-core 2.21.5.
- Tranche 7 checks on veritech03 with a trial VM: M1 pool `nvme` is
  `logical`, active, autostart; M2 block disk `raw`, `cache='none'`,
  `io='native'`, `discard='unmap'`, `iothread='1'`, `queues='4'`,
  `<iothreads>1</iothreads>`, root 20 GB; M3 fio in a file on the guest's
  ext4: 4k fsync 5.2k IOPS, 4k random write 134k IOPS; M4 add-disk made LV
  `<name>-2`; M5 resize-disk refused up front, VM kept running; M6 delete
  removed both LVs. M7 happened unplanned: veritech03 was rebooted by
  another tool at 14:15 PST; array, volume group, pool and the three VMs
  defined on it came back by themselves (VMs shut off: autostart is off).
- Twelve VMs recreated on the `nvme` pools from the rebuilt
  `ubuntu2604.qcow2` (UEFI, 40 GB, `setup_bot.yml`): vt-r1, vt-r2 16 vCPU /
  12 GiB, vt-r3..vt-r12 8 / 8. Each checked: user `bot`, `packer` gone,
  password login off, `/tmp` on the root disk. Create took 30-35 s per VM.
- Created with a side build of HEAD; the workstation install is still the
  Nov 16 build (cutover held). Credentials were copied into the old folder
  layout so the installed `vmtool cmd` reaches the new VMs.
- Found, not fixed: the README example `vmtool create veritech02/build1
  ubuntu2604 --pool nvme` needs `ubuntu2604.qcow2`; `vmtool info` does not
  print `disk_backing` / `disk_cache`, which the API returns.

## 2026-11-18 — W1 Config: the own name may be listed; `image_dir`; `config set-host`

- A workstation's `config.yml` may list its own name under `hosts:`
  (a hypervisor still may not); the entry is validated like any other,
  and every host's `port` must now be 0..65535. A top-level
  `default_pool` beside a listed own name is a load error pointing to
  `hosts.<name>.default_pool`.
- New optional key `image_dir`, resolved by `Config.Path`
  (`Config.ResolvedImageDir`).
- `vmtool config set-host <name> --address A --ssh-user U [--port P]
  [--default-pool X]` (`config.SetHost`) adds or replaces one `hosts:`
  entry whole, editing the YAML node tree so every other key, entry and
  comment is kept; validated before an atomic 0600 write. `Write` shares
  the same edit path. No token needed by `SetHost`; the CLI gate still
  applies to the command.
- Tests `TestOwnNameListedAsWorkstation`, `TestOwnNameListedValidatedLikeAnyEntry`,
  `TestImageDir`, `TestSetHostRoundTrip`, `TestSetHostOnMissingFileAndInvalid`,
  `TestConfigSetHost`: red before, green after; `just check` green.
- Commits: the W1 commit on `clone-identity`.

## 2026-11-18 — W2 Resolver: own name through its daemon when listed; local libvirt lazy

- `hosts.NewWith` drops a listed own name from the remote list and
  records it; `Resolver.OwnListed()` says whether the own name has a
  daemon. Listed: `Resolve(<own>)` is the remote connector with that
  entry's `default_pool`; `List`/`Status` show it once, first, as a
  probed hypervisor row; local libvirt is never opened.
- Not listed: libvirt still opens only inside `Resolve(<own>)`; a failed
  open (not on a hypervisor) reads "no daemon listed for <name> and no
  local libvirt: <cause>". `Status`'s own row opens and releases the
  local connector once and is unreachable with that sentence on failure.
- `defaultPools` forwards `Probe`: a host with `default_pool` used to show
  "cannot be probed" in `vmtool hosts`.
- Tests in `internal/hosts/own_test.go` for the three machines (daemon
  listed; local libvirt; neither, with two listed remotes): red before,
  green after; `just check` green. Two status fixtures and the server's
  `TestListHosts` connector now allow the one local open.
- Commits: the W2 commit on `clone-identity`.

## 2026-11-18 — W3 Surfaces never open libvirt eagerly

- No surface calls `vmtool.NewManager()` any more; every local open goes
  through the resolver and only when the own name is used and not
  listed. `metal add` (CLI and REST) skips the libvirt name check
  silently when local libvirt is not used or cannot be opened.
- Base-image lookup in local pools (CLI, server, TUI) answers "not found"
  without opening anything when the own name is listed, and also when
  libvirt cannot be opened, so create uses the host's cached copy.
- `vmtool tunnel <own>/x` and `x.<own>.vmtool` with the own name listed
  go through the daemon's tunnel route.
- The TUI starts without opening libvirt (`newModel`); the local host is
  resolved per action; with neither, its row is an ERROR row and the
  create form opens with empty pickers.
- `vmtool list` with neither leaves the own name out with one stderr
  `note:` line; exit status unchanged.
- A workstation server with its own name listed forwards
  getImageManifest for that name to the daemon (403 only when not listed).
- Tests in `internal/{cli,server,tui}/ownlisted_test.go`: red before,
  green after; `just check` green.
- Commits: the W3 commit on `clone-identity`.

## 2026-11-18 — W4 Base images from `image_dir`

- `app.ImageSource` (`internal/app/imagesource.go`) replaces
  `Service.LocalImages`: `Service.Images`, with one implementation here,
  `app.LocalImages{Dir, Pools}`: local libvirt pools first (only when
  local libvirt is in use), then `vmtool.FindImageInDir(image_dir, name)`
  (`pkg/vmtool/imagedir.go`: `name`, then `name.qcow2`; regular files,
  symlinks to them included; invalid names are not-found).
- Create hashes and uploads as before; with no local copy it uses the
  host's cached one or fails with `image "X" is neither in this
  machine's pools or image_dir (<dir> | not set) nor cached on <host>`.
- CLI, server and TUI wire `image_dir` from `config.yml`
  (`tui.Run` takes it as a trailing argument).
- Tests `TestLocalImagesOrder`, `TestFindImageInDirRules`,
  `TestCreateVMImageFromImageDir` (fake `ImageStore`, pool lookup not
  called when the own name is listed), `TestSvcImagesFromImageDir`,
  `TestServiceImagesFromImageDir`, `TestServerImageDirHelper`: red against
  a lookup with no folder search, green after; `just check` green.
- Commits: the W4 commit on `clone-identity`.

## 2026-11-18 — W5 prereqs and deploy: this machine, per-host port, vmtool-daemon.service, the hosts entry

- `vmtool config host <h>` prints `address ssh_user port` (port default
  9473); deploy.sh, prereqs.sh and cert.sh read three fields.
- prereqs passes `vmtool_daemon_port=<port>`; deploy writes
  `--listen 0.0.0.0:<port>` and checks `https://<host>.<domain>:<port>/version`.
- This machine (`cert.sh --local-name`: `name:` else `hostname -s`):
  prereqs uses `ansible_connection=local`; deploy runs every step in
  local bash, no ssh, one sudo. Unlisted: VMTOOL_SELF_ADDRESS (default
  `<host>.<domain>`) / VMTOOL_SELF_PORT (default 9474), and after a good
  `/version` deploy runs `vmtool config set-host`; a listed entry is never
  rewritten.
- The daemon unit is `vmtool-daemon.service`. The one sudo script first
  refuses a port held by any pid but our daemon (`ss -Htlnp`, MainPIDs),
  then disables, stops and removes a `vmtool.service` whose
  WorkingDirectory is HV_DIR (others left alone), then installs and
  restarts. cert.sh's remote restart covers both unit names.
- Test seams UNIT_DIR and GO_BIN_DIR in deploy.sh.
- Tests: `TestConfigHostPort`, `TestDeployLocal*`, `TestDeployUnit*`,
  `TestDeployRefusesForeignListener`, `TestDeployAccepts*Listener`,
  `TestDeployListedPort`, `TestPrereqsHostPort`, `TestPrereqsThisMachine`,
  `TestCertRemoteOldUnitOnly`: red against the old scripts, green after;
  `just check` green.
- Commits: the W5 commit on `clone-identity`.

## 2026-11-18 — W6 Packer images go to `image_dir`

- `packer_images/justfile` `generate` copies the built image to
  `${VMTOOL_IMAGE_DIR:-<repo>/images}/<template>.qcow2` (folder created if
  missing; temp name then `mv`); no `sudo`, no `pool-refresh`.
- `.gitignore` lists `images/`.
- Shape check `check_generate_image_dir` in the image gate: red on the old
  recipe (copy target, sudo, pool-refresh, .gitignore), green after;
  `just -f packer_images/justfile check` and `just check` green.
- packer_images CLAUDE.md and README say where the image goes and that
  `image_dir` must name the same folder. `./run generate` and `update`
  are unchanged (outside the step).
- Commits: the W6 commit on `clone-identity`.

## 2026-11-18 — W7 `just install` keeps the hosts entries and refuses a daemon's unit

- `scripts/install.sh` dies before building or writing anything when
  UNIT_PATH (`vmtool.service`) has `WorkingDirectory=$HV_DIR` (an older
  hypervisor daemon), telling the user to run `just deploy <host>` first;
  any other `vmtool.service` is replaced as before. The justfile's
  `install` passes `HV_DIR`.
- install's `config write` keeps every `hosts:` entry (own name included)
  and `image_dir`: already true of `config.Write`; pinned by
  `TestConfigWriteInstallKeepsHosts` (real command, install's flags) and
  `TestInstallConfigWriteFlags` (install's exact argv).
- `TestInstallRefusesDaemonUnit`: red before, green after;
  `TestInstallReplacesLoopbackUnit` green; `just check` green.
- Commits: the W7 commit on `clone-identity`.

## 2026-11-18 — W8 Docs for group 1 (a daemon on every VM host)

- README.md: the hypervisor layout now names `vmtool-daemon.service` and
  `0.0.0.0:<port>`; the port guard and the old-unit removal; new sections
  "One daemon per host" (unit, per-host port, the loopback
  `vmtool.service` beside it, install's refusal), "This machine, with or
  without a daemon" (the three cases; `just prereqs`/`just deploy` for
  the own name, VMTOOL_SELF_ADDRESS/VMTOOL_SELF_PORT 9474, set-host after
  `/version`), "Base images (`image_dir`)" (lookup order, packer's
  `VMTOOL_IMAGE_DIR`), "A new machine" (checkout, `config.yml` with
  `hosts:`, `.env`, the CA, libvirt's client library, ssh, ansible,
  `.machines/`).
- spec/README.md: "Daemon address, unit and the own name" (per-host port,
  unit, own-name routing as an API client sees it, where createVM's
  images come from).
- Placeholder host names only in the new text. Docs only; `just check`
  green.
- Group 1 of tranche 10 (W1–W8) is built. Phase A and B of the rollout
  are the user's and XO's.
- Commits: the W8 commit on `clone-identity`.

## 2026-11-18 — S1 `VMInfo.uuid` on the daemon API (2.3.0)

- `spec/openapi/vmtool.yaml`: `info.version` 2.3.0; optional `uuid` on
  `VMInfo` (plain string); `internal/api` regenerated.
- `pkg/vmtool` `Info` fills `VMInfo.UUID` from the domain
  (`GetUUIDString`); `List` leaves it empty. The server sends `uuid` only
  when known; `internal/remote` maps it and treats a missing field (an
  older daemon) as "" with no error; `vmtool info` prints a last line
  `UUID:` with the value or `unknown` (`writeVMInfo`).
- Tests: `TestToAPIUUID`, `TestRemoteMapsUUID`,
  `TestRemoteOldDaemonLacksUUID`, `TestWriteVMInfoUUID` (red: build
  failures on the missing fields; green after), `TestVersion` pins 2.3.0.
  `just check` green.
- Commits: the S1 commit on `clone-identity`.

## 2026-11-18 — S2 Store spec, libvirt-free command, config and auth

- `spec/openapi/store.yaml` 1.0.0 (new): `GET /version` public, `GET
  /v1/whoami`, and the image, blob, ticket, VM-credential and
  machine-credential operations as stubs answering 501; generated into
  `internal/storeapi`. `scripts/verify-generate.sh` and `just generate`
  cover both specs.
- `internal/tokenhash` (libvirt-free `Hash`, `Check`, `Valid`);
  `internal/config` re-exports `Hash` and `Check`.
- `internal/store`: config (`/etc/vmtool-store/config.yaml`; tokens
  `{name: sha256-hex}`; S3 keys from `VMTOOL_STORE_S3_ACCESS_KEY` /
  `_SECRET_KEY`; refuses no token, a bad hash or name, a token or key file
  with group/other bits, unknown keys), the listening rule (plain HTTP off
  loopback only with `behind_proxy: true`, else TLS; every bind needs a
  token hash), auth (one `Authorization` header, all hashes checked,
  caller name in the context; public routes from the embedded spec), one
  log line per request (method, route pattern, status, caller, duration),
  fixed error bodies that never echo an error's text.
- `cmd/vmtool-store` (`-config`); tests prove `go list -deps` has no
  `libvirt.org` and it builds with `CGO_ENABLED=0`.
- AGENTS.md hard rule 5 gains the store exception sentence.
- Red: build failures (undefined `Config`, `Load`, `NewServer`; no Go
  files in `cmd/vmtool-store`); green after. `just check` green.
- Commits: the S2 commit on `clone-identity`.

## 2026-11-18 — S3 `BlobStore`: S3 implementation and test double

- `internal/store/blob`: the `BlobStore` interface (`Put(ctx, key, r,
  size)`, `Get`, `Stat`, `Delete`, `EnsureBucket`), sentinels
  `ErrNotFound` and `ErrShortBody`, and one size-bounded reader that checks
  the context before each read and turns an early EOF into `ErrShortBody`.
- `NewMemory()`: the concurrency-safe double; an object is stored only
  when its whole body arrived (a short or cancelled put keeps the old one).
- `NewS3`: thin glue on `github.com/minio/minio-go/v7` v7.0.95 (the newest
  cached version that keeps go.mod at `go 1.24.0`); a bare `host:port`
  endpoint is plain HTTP, `https://` turns TLS on; minio's not-found
  answers map to `ErrNotFound`; Delete of a missing key is nil in both.
- Tests: a contract suite over the double (5 MiB round trip, not found,
  short body, cancel mid-stream, concurrency) and offline tests of the S3
  glue (validation, endpoint parsing, error mapping); no S3 server is ever
  contacted. Red: build failure (undefined `BlobStore`, `NewMemory`, ...);
  green after. `go list -deps ./cmd/vmtool` has no minio. `just check`
  green.
- Commits: the S3 commit on `clone-identity`.

## 2026-11-18 — S4 SQLite store with encrypted credential columns

- `internal/store/db` on `modernc.org/sqlite` v1.46.1 (pure Go; go.mod
  stays `go 1.24.0`): tables `schema_version`, `meta`, `images`,
  `vm_credentials`, `machines`; forward migrations, one transaction each;
  a database newer than the code is refused.
- username, password, private_key, public_key and the machines'
  credentials blob are sealed with AES-256-GCM, a fresh 12-byte nonce per
  write, bound to table, column and row by the additional data. A sealed
  key check in `meta` makes a wrong key the start error "encryption key
  does not match this database", checked before any migration runs.
- `LoadKey`: 32 raw bytes or 64 hex characters, file 0600; errors give the
  length only. `Open` makes the directory 0700 and the file 0600 and
  refuses looser ones (journal siblings too). `Snapshot` runs `VACUUM INTO`
  a new 0600 file.
- 13 tests, among them two keys (0x01, 0x02), a fresh nonce on every write,
  ciphertext bound to its row, and the raw db file and a snapshot holding
  neither placeholder secret. Red: build failure (undefined `DB`, `Open`,
  `ErrWrongKey`); green after. `just check` green.
- Commits: the S4 commit on `clone-identity`.

## 2026-11-18 — S5 Image endpoints and download tickets

- `spec/openapi/store.yaml`: image list, metadata, push (streamed
  `application/octet-stream`, `X-Image-SHA256`, `Content-Length`), blob
  (streamed, `Content-Length`, `X-Image-SHA256`), delete, tickets;
  `Image`, `ImageList`, `Ticket`; a `ticketAuth` scheme on the blob route
  only. Still 1.0.0; `internal/storeapi` regenerated.
- `internal/store/images.go`: a push streams through a SHA-256 hasher into
  `BlobStore.Put` under `images/<name>/<sha256>` (never over the live
  object), with no body cap; the index row is written only when size and
  hash match, otherwise the new object is deleted and the answer is 400;
  a replace then deletes the old object; a second push or a delete of a
  name in flight → 409; no Content-Length → 411.
- Tickets: 32 random bytes, 15 minutes, one use, kept only as SHA-256 in
  memory, bound to the image and its digest; redeemed only on a clean,
  unencoded `GET /v1/images/{name}/blob` for that image; every other
  route answers 401 and leaves the ticket unspent; every failure is the
  same 401 as a bad token. Logged caller `ticket:<issuer>`.
- `cmd/vmtool-store` opens the key, the database and S3 and makes the
  bucket at start; the key file is now required.
- 13 new tests over `blob.NewMemory()` and a temp database, including a
  5 MiB push, replace and failed replace, 409, one-use, other image,
  expired, replaced, unclean path, a ticket on every other route, and a
  captured debug log and every response free of ticket, token, their
  hashes and the body. Red: the 500-stub handlers failed the tests; green
  after; two mutations (no spend, no RawPath check) made tests fail.
  `just check` green.
- Commits: the S5 commit on `clone-identity`.

## 2026-11-18 — S6 Credential and metal endpoints; nothing secret in logs

- `spec/openapi/store.yaml`: `GET/PUT/DELETE
  /v1/credentials/vms/{host}/{name}` (`uuid` required in the PUT body and
  the GET query), `GET /v1/credentials/machines` (`{name, kind}` only),
  `GET/PUT/DELETE /v1/credentials/machines/{name}`; a 404 body with
  `stale`; 413; the 501 response is gone. `internal/storeapi` regenerated.
- `internal/store/credentials.go`: a GET with another UUID answers 404
  `"stale": true` and sends no credential field; PUT replaces the record
  and answers 200 with no secret field; DELETE 204/404. Validation answers
  400 `invalid field: <name>` and never the value (host without a dot,
  names, UUID, kind `metal|alien`, address, secret lengths). Credential
  PUTs need `application/json` and are capped at 1 MiB (413); decoder and
  parameter errors answer fixed text.
- Tests: round trip, stale, replace, delete, 13 validation cases, oversize
  body, machines, bearer-only on all seven routes (a ticket answers 401),
  `TestCredentialSecretsNeverLeak` (debug log and every response free of
  the placeholders outside the 200 GET bodies) and
  `TestNoRequestBodyLogging` (a source scan of the store). Red: the 501
  stubs; green after. FOREMAN checked the secrecy test with a debug line
  logging the password (it failed) and reverted. `just check` green.
- Commits: the S6 commit on `clone-identity`.

## 2026-11-18 — S7 Client store config, per-command mode, the two sources

- `config.yml` `server: {url}` (https, no credentials, query or fragment;
  the error never echoes it); `VMTOOL_SERVER_TOKEN` from the environment,
  else `.env`, never from config.yml (`config.ServerToken`).
- `internal/storeclient` (new): HTTP client over `internal/storeapi`;
  bearer only in `Authorization` (not on `GET /version`), no redirects, no
  proxy, system trust store; errors name the operation and status only.
- `internal/app`: `StoreSession` decides the mode once per command (one
  `GET /v1/whoami`, 3 s); reachable → store mode for the whole command,
  otherwise local mode and one line "<host> unreachable: using this
  machine's .machines/ and image_dir" (a missing token gets its own
  variant). `CredentialSource` (`LocalCredentials`, `StoreCredentials`)
  and `ImageCatalog` (`StoreImages`, `LocalCatalog`) beside W4's
  `ImageSource`; `Service.Store`. No workflow uses them yet (S8, S10).
- CLI: create, delete, ssh, exec, playbook, resize-disk, i and every
  `metal` subcommand decide at start; `vmtool hosts` prints a store table
  (URL, reachable, caller, version). The loopback service decides per
  request for the store operations and puts the line in the
  `X-Vmtool-Warnings` response header (documented in the spec's
  description; API stays 2.3.0); a hypervisor never uses the store.
- `cmd/vmtool` `TestNoStoreServerDeps`: no minio-go, sqlite or
  `internal/store/...` in the vmtool binary.
- Tests in config, storeclient, app, cli, server, tui, cmd/vmtool; red
  was build failure, plus a mutation (`usesStore` false) failing
  `TestStoreModeDecidedAtCommandStart`. FOREMAN found the bearer skipped
  for any path ending in `/version` (an image named `version`); fixed,
  `TestBearerOnEveryRouteButVersion` red then green. `just check` green.
- Commits: the S7 commit on `clone-identity`.

## 2026-11-18 — S8 Client credentials in store mode; fallback and reconciliation

- `pkg/vmtool/credcache.go`: the `.vmtool-source` marker (`store <uuid>`,
  `pending <uuid>`; unreadable = none), atomic 0600 writes, 0700 folders,
  `LookupVMAuth` (missing vs present), `MoveStale` to
  `.machines/.stale/<folder>-<date>[-n]/` (never deletes).
- `internal/app/credsync.go`: reconciliation before ssh, exec, playbook,
  console, resize-disk and a `--noclone` create (the six cases of the
  design, plus `store <other uuid>` and an other-UUID record treated as
  stale); hand-off after every playbook run when the folder changed,
  success or failure; a failed PUT marks `pending <uuid>` and prints one
  line naming the store ("... are only on this machine"); exec refetches
  once on exit 255 and reruns only if the cache changed; `vmtool delete`
  and create's replace remove the record and the cache. Pre-2.3.0 daemon
  (no uuid): no push, notice "redeploy <host>".
- `internal/app/machinecache.go`: metal/alien records through the store in
  store mode (`.machines/<name>/` as a marked cache); local mode unchanged.
- CLI `vmtool creds fetch|forget host/vm` (forget removes only the cache
  and refuses a pending or unmarked folder); `vmtool tunnel` fetches a
  missing cache folder before piping (stdout stays the byte stream).
- The loopback service carries mid-request notices in `X-Vmtool-Warnings`;
  create also emits them as progress events `credentials/notice`.
- ssh, sshpass and ansible children no longer inherit
  `VMTOOL_SERVER_TOKEN` (S7 A7).
- Tests: ~45 new in pkg/vmtool, app and cli with a fake store over
  `httptest` and `testexec` stubs, stub argv, progress events, errors and
  notices searched for the placeholders. Red: build failure, then a
  mutation (`pushFolder` returning before the PUT; the env strip removed)
  failing 9 tests. `just check` green.
- Commits: the S8 commit on `clone-identity`.

## 2026-11-18 — S9 The daemon fetches an image from the store (API 2.3.0)

- `spec/openapi/vmtool.yaml`: `POST /v1/hosts/{host}/images/{name}/fetch`
  (`fetchImage`) `{sha256, size, ticket}`, NDJSON `FetchImageEvent`
  (`progress`, `done`, `error` with `reason` `store_unreachable`,
  `store_refused`, `mismatch`, `other`); still 2.3.0.
- `internal/server/images.go`: upload's receive path factored into
  `beginReceive` + `receipt.receive(reader, size, sha)`; upload behaviour
  and messages unchanged. `internal/server/fetch.go`: a hypervisor dials
  only its `store_url` plus `/v1/images/<name>/blob` (https, no proxy, no
  redirects, system trust store), `Authorization: Ticket`, checks the
  store's Content-Length and X-Image-SHA256 before reading, writes through
  the same receive path; cached with that digest → `done` at once; no
  `store_url` → 409 "no store configured on <host>; redeploy with a
  store". A workstation forwards for a listed host and answers 403 for its
  own unlisted name. The ticket is scrubbed from every message.
- `config.yml` `store_url` (validated, never echoed); `vmtool config write
  --store-url`; `vmtool config server-url` prints the client's
  `server.url`; `scripts/deploy.sh` adds `--store-url` only when that is
  set (and refuses anything but a plain https URL).
- `internal/remote/fetch.go`: `Manager.FetchImage` reads NDJSON;
  `FetchError` kinds tell the three "cannot fetch" cases (409 no store,
  404 old daemon, store unreachable) from mismatch and refusal.
- Tests over in-process TLS stores: route, forward, 403, dial-only-store_url
  with decoys, redirect not followed, untrusted store, foreign 409 with no
  download, one-at-a-time, digest and size mismatch leave nothing,
  cached, header mismatch, a debug-log capture across eight scenarios
  free of the ticket; deploy stub tests both ways. Red: build failure,
  then four mutations (ticket unscrubbed, redirects followed, header
  checks off, cached shortcut off) and the deploy arg removed, each
  failing its test. `just check` green.
- Commits: the S9 commit on `clone-identity`.

## 2026-11-18 — S10 Client images in store mode

- `internal/app/fetch.go`: `ImageFetcher` (implemented by
  `remote.Manager.FetchFromStore`; `app` does not import `remote`) and
  `RelayableFetch`. `syncImage` in store mode: the daemon's manifest
  already has the store's digest → nothing; otherwise a ticket and
  `fetchImage`, daemon progress mapped to the `upload` events; the client
  reads no image bytes. Store lacks the image → this machine's copy is
  uploaded as in local mode, with one line suggesting `vmtool images
  push`; a store failing for metadata or a ticket → the same, with a line.
  A relayable fetch failure fails create until S11. Local mode unchanged.
- CLI `vmtool images push <file> [--name]` (hashed through the image
  manifest cache, streamed once; the store verifies), `images pull <name>
  [--to] [--force]` (`.part`, size and SHA-256 checked, rename; default
  `image_dir/<name>`), `images list --store`; push and pull decide the
  mode at start, list only with `--store`.
- `internal/hosts`: a host wrapped for `default_pool` keeps `ImageStore`
  and `ImageFetcher` visible (gap since L4, found in S9).
- Tests over `httptest` stores and daemons: current host, fetch with no
  bytes read, store lacks image with hint, store failing, foreign first,
  local mode, ticket absent from errors, events, notices and the daemon
  request's URL and headers, push, pull (corrupt download leaves nothing),
  list, refusals in local mode, the hosts wrapper. Tests were written
  after the code; red by mutation (store branch off: 5 app tests fail;
  pull's SHA check and the mode hook off: 2 cli tests fail; FOREMAN:
  remote progress mapping off: `TestCreateFetchTicketOnlyInRequestBody`
  fails). `just check` green.
- Commits: the S10 commit on `clone-identity`.

## 2026-11-18 — S11 Relay when a host cannot fetch

- `internal/app/fetch.go`: on a relayable fetch failure (409 no
  `store_url`, 404 pre-2.3.0 daemon, `store_unreachable`) the client
  prints one line "relaying <image> to <host> through this machine:
  <reason>" and streams the store's blob (its own token) into the daemon's
  `UploadImage` with the store's digest and size; no local file, no local
  lookup. A blob whose headers disagree with the store's metadata is not
  uploaded. Mismatch and refusals do not relay: create fails. A relay that
  fails falls back to this machine's copy with a line, or fails create
  when there is none. `remote.FetchError.RelayReason()` gives the reason.
- Tests, one per branch, over doubles: the three relay triggers and a
  default reason, no relay for mismatch or refusal, relay failure with and
  without a local copy, blob disagreeing (digest, size). Red before the
  code: S10's "cannot relay yet" failure on every relay test; green after.
  `just check` green.
- Commits: the S11 commit on `clone-identity`.

## 2026-11-18 — T1 Host setup: a thin pool in the array's VG

- `ansible/playbooks/tasks/nvme_stripe.yml`: opt-in thin-pool section
  (`vmtool_nvme_thin`, default false; defaults in `setup_host.yml`: pool
  `nvme-thin`, size `70%FREE` (`-l` for a `%` value, else `-L`), chunk
  `512k`, metadata `8G`). Installs `thin-provisioning-tools`, the LVM
  profile `ansible/files/vmtool-thin.profile` (autoextend at 80 % by 10 %)
  to `/etc/lvm/profile/`, enables `lvm2-monitor`. A JSON `lvs` probe
  (parsed with `from_json`, no regex): absent → `lvcreate --type
  thin-pool … --zero n --errorwhenfull n --monitor y --metadataprofile
  vmtool-thin --addtag vmtool_thinpool`; a tagged thin pool is adopted
  (`lvchange --zero n` / `--metadataprofile` only when they differ);
  anything else of that name fails the play, nothing changed. The summary
  shows the pool's size, data % and meta %. Header comments in the task
  file and `scripts/prereqs.sh` give the `just prereqs <host> -e …
  "vmtool_nvme_thin":true` line. Never executed on a host.
- Tests (`scripts/nvme_stripe_test.go`, `scripts/prereqs_test.go`):
  defaults, every thin task guarded (also by content, not only by name),
  create flags, adopt/refuse branches, package, profile, monitor,
  summary; `OnlyCreatesNeverDestroys` allows only `lvcreate --type
  thin-pool` and the two `lvchange` forms; the opt-in expression test
  evaluates the probe parsing on seven sample reports and the `-l`/`-L`
  choice. Red before the YAML (missing defaults, no thin tasks, no doc
  line), green after; `just check` and `VMTOOL_TEST_ANSIBLE_SYNTAX=1 just
  test ./scripts/` green.
- Commits: the T1 commit on `clone-identity`.

## 2026-11-18 — T2 The root helper `vmtool-thin`, its sudoers rule, installed by deploy

- `deploy/vmtool-thin`: the only thing the daemon runs as root (`sudo -n
  /usr/local/libexec/vmtool-thin <verb> …`). Verbs report, create,
  snapshot (`-kn`, then activated), extend (grow only), remove (`lvremove
  -y`, never forced), rename, tag, import, export. It acts only in a VG
  holding a thin pool tagged `vmtool_thinpool`, only on thin volumes
  tagged `vmtool_vol`/`vmtool_template`/`vmtool_building`, never on a pool;
  names, tags, BYTES and paths validated (flat files in the image
  directory, rendered in by deploy from `HV_POOL_DIR`). qemu-img runs as
  user `vmtool` (`setpriv`) against the LV's device node, handed to vmtool
  for the call and given back to `root:disk` by a trap, so a qcow2's
  backing or data file cannot make root read anything; no root `chown` of
  any image-directory path. PATH fixed, `set -euo pipefail`, no `eval`.
- `deploy/vmtool-thin.sudoers`: `vmtool ALL=(root) NOPASSWD:
  /usr/local/libexec/vmtool-thin`. `scripts/deploy.sh` stages both and,
  in its one sudo script, installs the helper root:root 0755 and the rule
  0440 under a temporary dotted name, `visudo -cf`, then moves it to
  `vmtool-thin` (a failed check removes it and stops before the daemon's
  files). Test seams `LIBEXEC_DIR`, `SUDOERS_DIR`; the justfile's
  `deploy` passes `HV_POOL_DIR`. `setup_host.yml`'s "no sudo" comment
  corrected.
- Tests: `scripts/vmtool_thin_test.go` runs a copy of the script with the
  PATH line pointed at stub tools and the image dir rendered (the installed
  copy reads no environment): every verb's argv, one refusal per
  validation (no writing stub runs), script-text checks;
  `scripts/deploy_test.go`: install modes, check-before-move, failed
  visudo, unsafe dirs, the rendered pool dir. Red: script absent, deploy
  expectations unmet; green after. 21 mutations of the helper (FOREMAN
  20, one at a time) and one of deploy's visudo gate each fail a test.
  `just check` green. Never run on a host.
- Commits: the T2 commit on `clone-identity`.

## 2026-11-18 — T3 `pkg/vmtool` thin primitives: helper client and pure decisions

- `pkg/vmtool/thin.go`: `Thin{Helper}` runs `sudo -n <helper> <verb> …`
  through `command()` (typed methods per verb, arguments checked with the
  helper's own rules before sudo; the helper's stderr is the error text).
  `ParseThinReport` (pools: `t…` attr and the exact `vmtool_thinpool` tag;
  volumes: `V…` in such a pool with a kind tag, building > template > vol;
  `VirtualBytes` counts every thin LV in the pool); `TemplateTags` /
  `TemplateFromVolume` / `ThinTemplates`; `TemplateLVName`;
  `CheckThinRoom` (80 % data or metadata, 4x promised; `*ThinRoomError`
  matching `ErrThinNoRoom`, text with the numbers); `StaleTemplates`
  (with `ImageSHAs` from the manifest); `FindTemplate`. Nothing calls it
  yet (T4).
- Tests (`pkg/vmtool/thin_test.go`, `testexec.Stub` for `sudo`): argv of
  every verb, helper refusal and malformed report errors, report fixtures
  (two VGs, foreign, thick and hidden LVs, empty), the limits at and
  around 80 % and 4x, stale and find tables, tag round trip. Red: build
  failure before `thin.go`, then mutations (limits, clone count; FOREMAN:
  `sudo` without `-n`) each failing. `just check` green.
- Commits: the T3 commit on `clone-identity`.

## 2026-11-18 — S12 Packer pushes to the store when configured

- `packer_images/justfile` `generate` (now a bash-shebang recipe): with a
  `^server:` line in the repo's `config.yml` and a built `<repo>/vmtool`,
  runs `vmtool images push --name <template>.qcow2` from the repo root
  (vmtool reads the store token itself). Push says "no store in use"
  (store unreachable or no token) → `store did not answer; falling back to
  image_dir` and W6's copy; any other push failure fails the recipe, the
  image stays in `output/`. No `server:` or no binary → a line, then W6's
  copy.
- Shape check `check_generate_store_push` (red before the recipe change,
  green after); `bash -n` of the recipe body. Docs: packer_images
  `CLAUDE.md`, `README.md`, `docs/LOG.md`. `./run generate` and `update`
  unchanged (libvirt-pool path). `generate` never run.
- Commits: the S12 commit on `clone-identity`.

## 2026-11-18 — S13 The store's container image

- `deploy/vmtool-store.Dockerfile`: builder `golang:1.24.0-bookworm`,
  `CGO_ENABLED=0 go build ./cmd/vmtool-store`; runtime
  `gcr.io/distroless/static-debian12:nonroot` (CA bundle included),
  `USER 65532:65532`, data and config dirs owned by it (0700), `EXPOSE
  8095`, `VOLUME` `/var/lib/vmtool-store` and `/etc/vmtool-store`,
  entrypoint `-config /etc/vmtool-store/config.yaml`; no ARG or ENV.
  Its config must set `listen: 0.0.0.0:8095` and `behind_proxy: true`.
- `.dockerignore` keeps `.git`, `.env`, `config.yml`, `.machines`, `tls`,
  `images`, `tmp`, the built binary and other local files out of the
  context. `just store-image <tag>` builds once with both tags and pushes
  `registry.labtwo/vmtool-store:<tag>` and `:latest`; refuses an empty tag
  or `latest`; no build args. Never run by the builder.
- Test `scripts/store_image_test.go` (text only, no docker): red with the
  files missing, green after. `just check` green.
- Commits: the S13 commit on `clone-identity`.

## 2026-11-18 — T4 Thin pools in pool listings; pool usage (API 2.4.0)

- Spec 2.4.0: `PoolInfo` gains optional `size_bytes`, `used_bytes`,
  `virtual_bytes`, `data_percent`, `metadata_percent`; `type` may be
  `thin`. Regenerated.
- `vmtool.PoolInfo` carries them as pointers (nil = unknown); libvirt
  pools get size/used from `GetInfo` (active pools only);
  `ThinPool.PoolInfo()` maps a thin pool (path `/dev/<vg>`, used =
  size x data%).
- `internal/hosts/thinpools.go`: `ThinPools{Manager, Thin, Log}` wraps a
  hypervisor-role local Manager (inside `hypervisorPools`; `New` gives a
  hypervisor `&vmtool.Thin{}`): `ListPools` appends the helper's thin
  pools; a name clash is left out with a log line; a failing helper or
  `sudo -n` is one log line and no thin pools. T5 extends this type.
- Server and remote map the fields (absent = unknown). `pool list`:
  NAME TYPE SIZE USED VIRTUAL DATA% META% ACTIVE PATH, blanks when
  unknown, `!` after a thin pool's name at >= 80 % data or metadata. TUI
  pool picker shows `name (type)`.
- Tests: wrapper with a fake inner Manager and a stub `sudo` (one pool,
  clash, helper failure, no pools, pass-through, role wiring), remote
  decoding of an old daemon's response, server mapping, CLI columns,
  blanks and mark, TUI label, `TestVersion` 2.4.0. Red: build failures
  (tests before code); green; `just check` green.
- Commits: the T4 commit on `clone-identity`.

## 2026-11-18 — T5 Create and delete on a thin pool; image templates

- Spec (2.4.0): `CloneVolumeRequest.template` (the image name as asked;
  informational), `cloneVolume` 409 when a thin pool has no room;
  `getImagePath` falls back to a saved template's `/dev/<vg>/<lv>`.
- `internal/hosts/thinclone.go` (on `ThinPools`): clone into a thin pool =
  template of the cached image's (file name, manifest SHA-256), else
  built under one per-(VG, template) lock shared by the resolver (create
  `vmtbld-<12hex>` tagged `vmtool_building`, import, one tag change,
  rename to `vmtpl-<12hex>`), then snapshot (`vmtool_vol`) and extend to
  max(template, size). Room check before any helper write (409). A
  failure after create removes only the LV that call made. Define marks
  thin `vmtool_vol` disks block (tranche 7's `blockDiskXML`, cache none)
  and refuses template/building LVs. Delete removes, after the inner
  delete, only `vmtool_vol` LVs named `<vm>` or `<vm>-N`. Resize: no-op
  when already big enough (growing is T6); extra disks on a thin pool
  refused (T6). A hypervisor removes `vmtool_building` LVs at start.
- `internal/server/primitives.go`: the path gate accepts `/dev/<vg>/<lv>`
  of a thin `vmtool_vol` LV, and a template only as a clone's base.
- `internal/app`: optional `TemplateCloner`, `ThinHost`, `TemplateFinder`
  (forwarded by every hosts wrapper); create sends the image name;
  `cloneCacheDefault` none for thin; `--noclone` or a `vmtpl-` name on a
  named thin pool refused; syncImage accepts a host's saved template.
- Tests: stub `sudo` + fake inner Manager: exact argv of warm and cold
  creates, no-room (no write, 409 end to end), concurrent build once,
  build/extend/snapshot failure removals, delete kinds and names, define
  XML, path gate, wrappers, start-up cleanup, remote `template`. No
  existing test changed. Red: build failures and app behaviour, plus
  mutations; green; `just check` green.
- Commits: the T5 commit on `clone-identity`.

## 2026-11-18 — S14 ansible_labtwo role `vmtool_store` and playbook

- In `/home/jared/repos/resurgentech/ansible_labtwo`, branch `simplify`
  (clean at start and before staging), commit `3d0f09b`, not pushed:
  `roles/vmtool_store/` (tasks, vars, compose/config/tokens templates,
  `make_secrets.sh tokens|enckey [--force]`, README),
  `playbooks/sdf1_vmtool_store.yml`, and two OpenBao path names in
  `inventories/group_vars/all`.
- Secrets fetched on the control machine with `no_log`; tokens rendered as
  SHA-256 only; compose root 0600 (S3 keys, no published port);
  `etc/{config.yaml,tokens.yaml,enc.key}` and `data/` owned 65532:65532
  (files 0600, dirs 0700), `etc/` mounted read-only; recreate only when a
  file changed. Caddy route `vmtool-store.labtwo` →
  `vmtool-store-vmtool-store-1:8095`, `flush_interval: -1`, no body limit,
  no Authelia; sdf1 `/etc/hosts` line.
- Checks: `bash -n` rc 0; `ansible-playbook --syntax-check` rc 0; the
  tokens template renders only `hash('sha256')`; every secret task has
  `no_log: true`. Nothing run against sdf1, OpenBao or docker.
- Found: the store's backups (daily `VACUUM INTO`, `vmtool-store backup`)
  were never built; the README documents a manual backup and restore.
- vmtool: no code change; `just check` green. Commits: `3d0f09b`
  (ansible_labtwo), the S14 commit on `clone-identity`.

## 2026-11-18 — S15 Docs for group 2 (the store)

- `README.md`: "The image and credential store (optional)": `server:`,
  `VMTOOL_SERVER_TOKEN` (env, else `.env`; from `make_secrets.sh tokens`,
  fetched with `bao kv get … -field=<name>`, path names only); the store
  token opens every guest's credentials; the `hulbert.local` CA on clients
  and hypervisors; the two modes, fallback, markers and reconciliation;
  fetch, relay, tickets (one image only); build (`just store-image`) and
  deploy (the ansible_labtwo role); the key-loss warning; backups (manual
  only: automatic backups were never built).
- `spec/README.md`: `store.yaml` in the layout and codegen; `## The
  store` (auth, operations, tickets, credentials, modes, markers,
  `fetchImage` and relay, backups and the key).
- `AGENTS.md`: rule 1 names `spec/openapi/store.yaml` / `internal/storeapi`;
  three "Where to look" rows.
- Docs only, no test; `just check` green. Commits: the S15 commit on
  `clone-identity`.

## 2026-11-18 — T6 add-disk and resize-disk on thin pools

- `internal/hosts/thindisk.go` (on `ThinPools`): add-disk into a thin
  pool = room check, helper `create <vg> <pool> <vm>-N <bytes>
  vmtool_vol`, attach as block; a failed attach removes only that LV.
  resize-disk of a thin vda = refuse not-larger, room check on the
  growth, `extend`, then `BlockResize` when running (not shrunk if that
  fails). `ResizeVolume` grows thin too. Everything else goes to the
  inner Manager (thick block keeps `errBlockGrow`; qcow2 unchanged).
  create's `--extra-disk-size` on a thin pool stays refused.
- `pkg/vmtool/domaindisks.go`: `DomainDisks`, `AttachBlockDisk`,
  `BlockResizeBoot`, `ExtraLVName` (pure parsers tested).
- CLI help of `resize-disk` / `add-disk`: thin grows, thick does not.
- Known gap: no-room on these two routes is 500 over REST (spec lists no
  409 there); needs a follow-up step.
- Tests: 14 in `internal/hosts/thindisk_test.go`, parsers, help. Red:
  build failures and help assertions; green; `just check` green.
- Commits: the T6 commit on `clone-identity`.

## 2026-11-18 — T7 Template management: list, build ahead, delete, prune

- Spec (2.4.0, no bump): `TemplateInfo`, `CreateTemplateRequest`;
  `GET/POST /v1/hosts/{host}/templates`, `DELETE
  /v1/hosts/{host}/templates/{name}` (`?pool=`). Regenerated;
  `spec/README.md` route rows.
- `internal/hosts/thintemplates.go` (on `ThinPools`): list (clones, stale
  against the daemon's manifest); prepare through T5's `ensureTemplate`
  (room check for the template only); delete resolves `{name}` (tag name
  or `vmtpl-` LV name) among `vmtool_template` volumes only, 409 when
  ambiguous, takes the build lock, re-reads, removes that one LV. Wrappers
  forward; no thin pools -> `app.ErrNoThinPools`.
- `internal/app/templates.go`: `ListTemplates`, `ListAllTemplates`,
  `PrepareTemplate` (create's `syncImage` first), `DeleteTemplate`,
  `PruneTemplates` (stale image templates only). Server handlers forward
  for a listed host; remote client; CLI `templates list|prepare|delete|
  prune`; TUI templates view (`T`, stale mark, confirm delete).
- Tests: delete with clones removes only the template; clone, building,
  untagged and path names refused with no write; prune only stale image
  templates; statuses (404/409/400); workstation forwarding; CLI, TUI,
  remote, app. Red: build failures; green; `just check` green.
- Commits: the T7 commit on `clone-identity`.

## 2026-11-18 — T8 Save a VM as a project template

- Spec (2.4.0, no bump): `CreateTemplateRequest` gains `from_vm`, `name`,
  `replace`; `TemplateInfo` gains optional `extra_disks`. Exactly one of
  `image` / `from_vm`; regenerated.
- `internal/hosts/savetemplate.go` (on `ThinPools`): shut off only (409
  "shut it down first"), boot disk must be a `vmtool_vol` thin LV (400
  "templates are saved from VMs on a thin pool"), name free of templates
  and cached images (409; `--replace` replaces only one saved template,
  made new first, old removed under lock after a re-read), room check
  (409), then one helper write: `snapshot <vg> <vm> vmtpl-<12 random
  hex>` with the saved tags. Extra disks returned as a warning.
- `internal/app` `Service.SaveTemplate` (optional `TemplateSaver`; syncs
  no image); server dispatch and no store probe for a save; remote client;
  CLI `templates save host/vm <name> [--replace]` ending with a pointer to
  README "What a template must be"; TUI `W` with a name prompt.
- Tests: fake helper asserts the only write naming the source LV is the
  snapshot origin; refusals; replace order and re-read; statuses;
  workstation forwarding. Red: build failures; green; `just check` green.
- Commits: the T8 commit on `clone-identity`.

## 2026-11-18 — T9 Export a template as an image; pull an image from a daemon

- Spec (2.4.0): `exportTemplate` (`POST /v1/hosts/{host}/templates/{name}/export`,
  NDJSON `ExportTemplateEvent`), `getImageBlob` (`GET
  /v1/hosts/{host}/images/{name}/blob`, `Content-Length`,
  `X-Image-SHA256`); regenerated; `spec/README.md` rows.
- Daemon (`internal/server/export.go`; `images.go` gains `beginReceiveAs`
  and a shared `commit`): export resolves one template, helper `export`
  to `.<name>.part`, hash, tag the template `vmtool_sha256`, link and
  manifest entry under the S9 lock and foreign rules; an existing manifest
  name is 409 unless the template's digest already matches (one `done`,
  no rewrite). Blob serves only manifest-listed, regular, right-size files
  opened `O_NOFOLLOW`. Workstations forward both.
- `internal/hosts`: lookup/export/tag on `ThinPools`; `imageTemplate`
  also matches a saved template carrying the cached file's digest, so the
  next create clones it with no upload or rebuild.
- App `ExportTemplate`, `PullHostImage` (temp `.part` `O_EXCL`, size and
  digest checked, `os.Link` never replaces; same file is a no-op);
  remote client; CLI `templates export host/<name>`, `images pull
  host/<name> [--to dir]` (a `/` means a host; bare name is the store).
- Tests: blob refusals (not listed, foreign, symlink, dir, size, `..`
  names), pull never overwrites (incl. race), mismatch leaves nothing,
  export failure removes only its part, no body logged, forwarding. Red:
  build failures; mutations (no `O_NOFOLLOW`, `Rename` for `Link`) fail
  the safety tests; green; `just check` green.
- Commits: the T9 commit on `clone-identity`.

## 2026-11-18 — T10 `templates export --push`: straight to the store

- `internal/app/exportpull.go` `Service.ExportTemplateToStore`: store mode
  only (else, and when the store is down, refused before the daemon is
  asked, naming `images pull host/<name>` and `images push`); export, then
  the daemon's blob streamed into the S10 store push with no local file;
  the blob's size and digest must match the export's, and the store
  checks the digest.
- CLI `templates export host/<name> --push` (push progress, final store
  line); `usesStore` probes the store only with `--push`.
- Tests (httptest daemon and store doubles): bytes equal, digest mismatch
  rejected, local mode and store down refused, daemon failures push
  nothing, no store token in output, progress or errors (hostile store).
  Red: build failure / unknown flag; green; `just check` green.
- Known gap: `internal/remote` quotes a daemon's error body, so a daemon
  that echoes headers would show its own token (all remote calls).
- Commits: the T10 commit on `clone-identity`.

## 2026-11-18 — T11 Create stage timings

- Spec (2.4.0): a documented `ProgressEvent` schema with `elapsed_ms`.
  No route references it (createVM is a synchronous JSON call and streams
  no progress), so it generates no Go; `api.gen.go` unchanged.
- `internal/app`: `ProgressEvent.ElapsedMS`, `Service.Now` (injectable
  clock); `CreateVM` stamps every event with ms since create started.
- CLI create prints one closing line on success and on failure, e.g.
  `timings: clone 0.3s, start 1.1s, wait_ip 2.0s, wait_ssh 7.0s, grow_disk
  0.6s, total 11.0s` (each stage first to last event; only stages that
  ran).
- Not built: the step's "REST NDJSON" part (no create stream exists).
- Tests with a fake clock (app stamps, failure stamps, fields unchanged,
  CLI line on success and failure). Red: runtime assertion failures;
  green; `just check` green.
- Commits: the T11 commit on `clone-identity`.

## 2026-11-18 — T6a A full thin pool is 409 on add-disk and resize-disk

- Spec (2.4.0, additive): `'409'` on `addDisk`, `resizeDisk` and
  `resizeVMVolume` (the daemon-side resize a client reaches; it answered
  500 too). `api.gen.go` regenerated.
- Server: the three handlers map a conflict (`primKind`, which also catches
  a bare `ErrThinNoRoom`) to 409 with the pool's numbers.
- Remote: a 409 from `addDisk` or `resize-volume` becomes a conflict
  matching `vmtool.ErrThinNoRoom`, the daemon's text kept. CLI unchanged
  (prints the message, exit 1); proven by test.
- Other room-checked routes checked: createTemplate build-ahead and save,
  cloneVolume already 409 with tests; createVM's clone path gains a test.
- Red: 500 where 409 was wanted; remote error not matching. Green;
  `just check` green.
- Commits: the T6a commit on `clone-identity`.

## 2026-11-18 — S16 The store's backups: daily, on command, 14 kept

- `BlobStore.List(ctx, prefix)` (S3 `ListObjects`, the in-memory double).
- `internal/store/backup.go`: one backup = `VACUUM INTO` a 0600 temp file in
  the data dir (removed in every outcome) streamed to
  `backups/store-<UTC yyyymmddThhmmssZ>.db`; retention keeps the 14 newest
  and deletes only keys of exactly that shape; one at a time (TryLock);
  failures logged as op, object, class, duration, never error text.
- Schedule from `cmd/vmtool-store` serve (signal context): 5 min after start
  when the newest backup is stale or absent, then every 24 h.
- `vmtool-store backup`: its own SQLite connection, one line with the object
  name, exit 0, else non-zero. No route, no spec change.
- README.md and spec/README.md: backups, the key not in the backup, restore.
- ansible_labtwo `roles/vmtool_store/README.md` corrected, commit `ab0db41`
  on `simplify` (not pushed).
- Red: build failures (no List, run, BackupDeps); green; `just check` green.
- Commits: the S16 commit on `clone-identity`; ansible_labtwo `ab0db41`.

## 2026-11-18 — T13 Docs for tranche 11

- README.md: thin pool host setup (`vmtool_nvme_thin`), "Thin pools: clones
  from templates" (thick pool untouched, `pool list` columns, overcommit 80 %
  / 4x as 409, full pool, the root helper and its sudoers line, per host,
  timings as the CLI line only), "Templates: save, export, pull", "What a
  template must be" (items 1-10, vmtool checks none of 1-9; author flow),
  "Rolling out thin pools"; "Not supported" loses thin snapshots and thin
  growth.
- spec/README.md: "Thin pools and templates (API 2.4.0)"; route table fixed
  (SaveTemplate, PrepareTemplate, add/resize-disk 409).
- CLI help: create (--pool, thin), pool list, migrate-disk, templates,
  templates save/export, images pull; pinned by `TestThinHelpNamesTheThinPath`.
- `TestPrereqsDocsAgree` now covers README's thin prereqs line.
- Roadmap citations corrected from the code (packer defaults in
  `defaultedAuth`, `forgetCreds`, `identityXML`, grow only with
  `--disk-size`).
- Red: help keywords missing, README thin line absent; green; `just check` green.
- Commits: the T13 commit on `clone-identity`.

## 2026-11-18 — T14 Optional reset playbook for template authors

- `ansible/playbooks/image_reset.yml`: the images' last-provisioner commands
  in order (cloud-init wait, GRUB cmdline sed, update-grub, removable loader
  if missing, apt-get clean, root history, 127.0.1.1 line, cloud-init clean
  --machine-id --logs), then an async (poll 0) poweroff. Run by hand only;
  never by vmtool, never as create --playbook.
- `scripts/image_reset_test.go`: order, sed equal to the packer template's,
  shutdown last and async, no setup_bot, header; opt-in --syntax-check.
- README: the playbook paragraph in "What a template must be".
- Red: playbook missing; green; opt-in syntax run ok; `just check` green.
- Commits: the T14 commit on `clone-identity`.

## 2026-11-20 — E1 Helper: thick export volumes and `send`

- `deploy/vmtool-thin`: `export VG TEMPLATE NAME TAG...` replaces the file
  export: a vmtool_template thin source, a new `vmexp-<12 hex>` name, tags
  with `vmtool_export`; `qemu-img measure` (as vmtool) rounded up to the
  extent size; refuses `no room: need= free= reserve=` (reserve a tenth of
  the thin pool) before `lvcreate -L <bytes>b` (linear); `convert -p -t none
  -W -m 16` with progress on stdout; `check --output=json` gives
  `vmtool_bytes`. A failure after `lvcreate` removes NAME only; both device
  nodes given back by trap on every exit.
- `send VG NAME`: exactly `vmtool_bytes` bytes of a thick vmtool_export LV
  through `dd iflag=count_bytes` as vmtool; length only from its own tag.
- `remove`/`tag` accept vmtool_export thick LVs, still refuse other thick
  LVs; `vmtool_export`/`vmtool_bytes` cannot be set by create, snapshot or
  tag. The `.part` file form is gone; `import` unchanged.
- Tests: `scripts/vmtool_thin_export_test.go` (argv, no room, room at the
  reserve, convert failure removes NAME only, send, refusals).
- Red: export/send tests failing on the old verb; green; `just check` green.
- Commits: `f4af028`.

## 2026-11-20 — E2 Export volumes on the daemon side

- `pkg/vmtool/thin_export.go` (new): `ThinKindExport`, `vmexp-` names
  (random 12 hex), `ThinExport` (bytes, sha, created, expires),
  `ParseExportProgress` (qemu-img `-p`), `ExportRoomError` (matches
  `ErrThinNoRoom`), helper client `Thin.ExportThick` (streams progress) and
  `Thin.Send` (reader of exactly N bytes; short stream is an error).
  `ParseThinReport` lists thick vmtool_export LVs; `Thin.Export` (file form)
  removed. Image listings in `libvirt.go` leave out `vmexp-`.
- `internal/hosts`: `ExportTemplateVolume` reuses a finished export of the
  template LV, removes an unfinished one and those of a replaced template of
  the same name; no room is a 409 with the numbers. `OpenExport`,
  `RecordTemplateSHA` tags the export too, `DeleteTemplate` (and so prune)
  removes the template's exports, `ExpireExports(now)` (24 h from lv_time,
  under the template lock, failures logged and retried). `templates list`
  carries the finished export.
- `internal/server/server.go`: expiry at start and hourly, hypervisor only.
  `export.go` changed only to the new interface (still caches a file; E3).
- `internal/app/create.go`, `internal/hosts/thinclone.go`: `vmexp-` VM names
  refused on every pool.
- Red: build failures on the missing symbols, and
  `TestCreateVMRefusesTheExportPrefix` failing; green; `just check` green.
- Commits: the E2 commit on `clone-identity` (it also carries E1's entry
  above).

## 2026-11-20 — E3 Export, pull and push over the API (API 2.5.0)

- `spec/openapi/vmtool.yaml` 2.5.0: exportTemplate `hash`; getImageBlob
  `X-Image-Size` and the `X-Image-SHA256` trailer; `TemplateInfo.export`
  {bytes, sha256, expires}. `internal/api` regenerated.
- `internal/server/export.go`: exportTemplate makes no `.part`, cache file
  or manifest entry; progress from the helper; a no-room refusal is a plain
  409; `done` carries name and bytes, the digest only with `hash=true` (one
  read when not yet tagged). getImageBlob serves a manifest entry as before,
  else the template's export through `OpenExport`, hashed while sent: header
  digest when tagged, else `X-Image-Size` and a trailer, tag recorded only
  after a full send; 404 names `templates export`. forwardBlob relays
  header or trailer.
- `internal/app/exportpull.go`: PullHostImage takes a trailer digest,
  compares after the last byte, keeps nothing on mismatch or none;
  ExportTemplateToStore asks `hash=true`; `ExportedLine` says the export is
  on the NVMe array and kept 24 hours. `internal/remote` reads the trailer.
- CLI `templates list` and TUI templates view: EXPORT column.
- `spec/README.md` route notes.
- Red: tests written after the code (implementer); mutation reds: server
  trailer line disabled fails the trailer and relay tests; the PullHostImage
  trailer path disabled fails 2 tests (6 cases); green; `just check` green.
- Commits: the E3 commit on `clone-identity`.

## 2026-11-20 — E4 Docs for tranche 13

- README "Templates: save, export, pull": the export is a thick `vmexp-`
  volume on the NVMe array, not in the image cache (`images list`/`images
  delete` do not apply), kept 24 hours, one per template, removed with its
  template; `virsh vol-list nvme` shows it and it is vmtool's; the room rule
  and its message; no fallback to the boot drive; the digest computed while
  sent (trailer), `--push` reads once first; old clients refused. "Thin
  pools": a "Room for exports" bullet.
- spec/README.md: "Export volumes (2.5.0)".
- CLI help of `templates export`, `images pull`, `templates list`,
  `templates prune`; `TestExportHelpNamesTheArray` pins the keywords.
- Red: 17 keywords missing; green; `just check` green.
- Commits: the E4 commit on `clone-identity`.

## 2026-11-20 — D1 The helper discards a volume before removing it

- `deploy/vmtool-thin`: new `discard_lv` (the script's only `blkdiscard`,
  no option, on `/dev/VG/NAME`). `remove` refuses a volume whose `lv_attr`
  shows it open, activates an inactive one with `lvchange -ay -K`,
  discards, and removes only after a successful discard; a failed discard
  is one stderr line naming the volume, saying it was kept so its data does
  not return to the pool, and the retry command. The export trap discards
  before removing and keeps the volume when the discard fails.
- Tests: `scripts/vmtool_thin_discard_test.go` (call order for a thin
  volume, a building volume, a template and an export; activation first;
  open refused; discard failure keeps; export trap both ways; no other verb
  discards); `TestThinScriptText` pins one `blkdiscard` line and one `-K`
  line. `internal/hosts/thindiscard_test.go`: the helper's line reaches the
  delete error and the building-cleanup log (no daemon change needed;
  these were green at first run, regression pins).
- Red: 8 helper tests failing; green; `just check` green.
- Commits: the D1 commit on `clone-identity`.

## 2026-11-20 — D2 Host setup: discards for thick volumes and a clean start for the pool

- `ansible/playbooks/tasks/nvme_stripe.yml`: `issue_discards = 1` in
  lvm.conf (lineinfile, a commented/0/1 line replaced or the line added
  after `devices {`), checked with `lvmconfig devices/issue_discards`
  (skipped under `--check`). Before a pool that does not exist: a leftover
  `vmtool-discard-scratch` is reused only if tagged `vmtool_scratch`
  (untagged fails the play), else one is made over 100%FREE (`--zero n
  --wipesignatures n`); `blkdiscard` it; a failed discard stops the play
  with the volume left and the manual removal; else `lvremove -y` it. Pool
  created with `--discards passdown`; the probe reads `discards`; an
  adopted pool not at `passdown` gets `lvchange --discards passdown`, with
  a plain failure if LVM refuses.
- `scripts/nvme_stripe_test.go`: the never-destroys test narrowed
  (`lvremove` once, scratch only; lvcreate thin-pool or scratch; lvchange
  three forms); six new tests (lvm.conf edit and its regexp, scratch
  guards, scratch-only discard, sequence and failure, passdown).
- Red: 8 tests failing; green; `just check` green;
  `VMTOOL_TEST_ANSIBLE_SYNTAX=1 just test ./scripts/` green.
- Commits: the D2 commit on `clone-identity`.

## 2026-11-20 — D3 Docs for tranche 14

- README: new "Old data and discard" under the NVMe thin pool text (the
  helper's discard on removal, `issue_discards` for thick volumes, the
  one-time scratch discard at pool creation and `--discards passdown`; a
  failed discard keeps the volume, with the helper's retry line; the two
  assumptions; the manual drive check on a scratch volume only; the manual
  one-time discard for a pool made before this); "never removes anything"
  sentences corrected; a Thin pools bullet linking to it.
- `nvme_stripe.yml` header comment: the scratch volume is the only volume
  the file removes; `issue_discards`.
- `scripts/discard_docs_test.go` `TestDiscardDocsKeywords`.
- Red: 15 README keywords and 3 header keywords missing; green; `just
  check` green.
- Commits: the D3 commit on `clone-identity`. Tranche 14 is built.

## 2026-11-21 — N1 Inventory tables and their rules in the store's database

- `internal/store/db/db.go`: migration 2 (`hosts`, `vms` keyed (host,
  uuid), index on (host, name); no secret column; migration 1 untouched);
  `DB.SetClock` so the store can pass its `Deps.Now`.
- `internal/store/db/inventory.go` (new): `ApplyReport` (one transaction;
  stale `generated_at` ignored, relist, omitted live rows gone, orphan
  credential records get a gone row, comment nil keeps / "" clears),
  `Delist`, `DeleteVMRow`, `ListInventory` (with `has_credentials`),
  `Sweep` (gone 7 days or more; credential removed only on matching UUID;
  never reads `last_seen` or `last_report`); `GracePeriod` = 7 days.
- `internal/store/db/inventory_test.go`: 16 tests, including the version-1
  upgrade, the empty report, 6 d 23 h kept / 7 d removed, the newer
  same-name credential, and a year-old host keeping its rows.
- Red: build failed on the missing methods and types; green; `just check`
  green.
- Commits: the N1 commit on `clone-identity`.

## 2026-11-21 — N2 Host tokens: a limited store credential

- `spec/openapi/store.yaml` 1.1.0: `Whoami.kind` (user|host), host-token
  rule in the descriptions; `internal/storeapi` regenerated.
- `internal/store/config.go`: `host_tokens_file` (default
  `/etc/vmtool-store/host_tokens.yaml`); missing file = no host tokens; a
  present one must be 0600, `{host: sha256-hex}`, valid host names; a hash
  also in `tokens.yaml`, or shared by two hosts, refuses to start; errors
  name a file or host, never a value.
- `internal/store/server.go`: a host token passes only `GET /v1/whoami` and
  `PUT /v1/hosts/{host}/inventory` (allow-list), else 401; whoami answers
  `host/<h>`, kind `host` (user tokens: kind `user`); log caller `host/<h>`.
- `internal/store/host_tokens_test.go`: config tests and a 401 table over
  every operation in the embedded spec; token, hash and header absent from
  bodies and logs. `spec/README.md` store auth paragraph.
- Red: 5 tests failing (load, refusals, whoami kind, allow-list); green;
  `just check` green.
- Commits: the N2 commit on `clone-identity`.

## 2026-11-21 — N3 Store inventory routes, the sweep, and the store client

- `spec/openapi/store.yaml` (1.1.0): `putHostInventory` (PUT
  `/v1/hosts/{host}/inventory`), `getInventory` (GET `/v1/inventory`,
  `?host=`, `?gone=true`), `delistVM` (POST
  `/v1/inventory/{host}/{name}/delist`), `deleteInventoryRow` (DELETE
  `/v1/inventory/{host}/{name}?uuid=`); schemas for the report, the
  inventory and the delist answer; `internal/storeapi` regenerated.
- `internal/store/inventory.go` (new): handlers; the report route takes only
  the host's own token (another host or a user token 403), JSON only, 1 MiB,
  4096 VMs, comment 512 bytes one line; VM names any libvirt name (no `/`,
  no control character); the sweep at start and every 24 h on the store's
  clock, a failure logged as a class only. `server.go` passes `Deps.Now` to
  the database; `cmd/vmtool-store` runs the sweeper beside backups.
- `internal/storeclient`: `PutHostInventory`, `Inventory`, `Delist`,
  `DeleteInventoryRow`, `ErrForbidden` on 403.
- Tests: `internal/store/inventory_test.go` (10, end to end included: two
  VMs, one, gone with kept-until +7 days, swept with its credential, a
  year with no report keeps the live row), `TestInventoryClient`.
- Red: build failure on the missing handlers, sweep and client methods;
  then (foreman) an outside-vmtool VM name refused the whole report; green;
  `just check` green.
- Commits: the N3 commit on `clone-identity`.

## 2026-11-21 — N4 Origin and comment kept with the VM

- `spec/openapi/vmtool.yaml` 2.6.0: `VMOrigin`; `VMInfo.origin`/`comment`;
  `CreateVMRequest.comment`; `DefineVMRequest.origin`/`comment`; new `PUT
  /v1/hosts/{host}/vms/{name}/comment` (`setVMComment`); `internal/api`
  regenerated; `spec/README.md` route rows and "Origin and comment".
- `pkg/vmtool/metadata.go` (new): one `<metadata>` element in namespace
  `urn:vmtool:vm:1` with `<origin/>` and `<comment>`; pure builder, parser,
  `ReplaceComment`, `ValidateComment` (512 bytes, one line, no control
  characters), `ValidateOrigin`; `Manager.SetComment` through libvirt's
  metadata call (config, plus live when running), no redefine. `domainXML`
  writes it; `Info` and `List` read it.
- `internal/app`: create validates the comment first, records origin
  (thin/block/file/none, image SHA-256 from the host manifest, pool, time
  from `Service.Now`); `Service.SetComment`; `Manager.SetComment`.
  `internal/hosts` refines a thin origin (template, LV). `internal/remote`,
  `internal/server` (400 naming `comment` or `origin.<field>`),
  `internal/cli` (`create --comment`, `comment host/vm TEXT|--clear`,
  `info` shows origin and comment), `internal/tui` (detail line).
- Tests in seven new files; fakes, version pins and the VM-command list
  follow.
- Red: build failures on the missing types and methods; green; `just check`
  green.
- Commits: the N4 commit on `clone-identity`.

## 2026-11-21 — N5 The daemon reports its VMs

- `internal/config`: `store_token_file` (a path, resolved like `tls_key`);
  `StoreHostToken()` refuses a missing, non-regular, non-0600 or empty file,
  or no `store_url`, with a reason that never quotes the content.
  `internal/cli`: `config write --store-token-file`.
- `internal/app/inventory_reporter.go` (new; `inventory.go` already held
  other code): one goroutine; sends the full list at start and after kicks
  (coalesced; `Kick` never blocks); no timer-driven send; backoff 30 s
  doubling to 5 min with the list rebuilt each try; a listing or disk error
  sends nothing; 401/403 retried at 5 min and logged once per change of
  state; every VM carries origin and its comment ("" when none); 10 s send
  limit.
- `pkg/vmtool`: `List` fills UUID and firmware; new `Manager.VMDisks`
  (target, kind, pool, capacity). `internal/hosts/inventory.go`: the
  reporter's source, thin volumes reported as `thin`.
- `internal/server`: `vmChangeOps` (the 14 operations); the request log
  kicks the reporter after a 2xx of one; the reporter starts after bind,
  hypervisor role only, or logs one line why not.
- Tests: reporter (start, comment, no timer send over 30 days, coalescing,
  backoff, denied, listing and disk errors), every `vmtool.yaml` operation
  classified, kick on success only, an operation returns at once with a
  hung store, start-time refusals, a report reaching an in-process store.
- Red: build failures on the missing symbols; green; `just check` green.
- Commits: the N5 commit on `clone-identity`.

## 2026-11-21 — N6 `just deploy` installs the host token

- `scripts/deploy.sh`: with a `store_url`, the host's field of
  `BAO_STORE_HOST_TOKENS_PATH` (default
  `kv/infra/labtwo/vmtool_store/host_tokens`) is read with `bao kv get
  -field=<host>` into a variable, sent over the stdin of one ssh call into
  `stage/store-host-token` (umask 077), installed `vmtool:vmtool 0600` as
  `HV_DIR/store-host-token`, and `--store-token-file store-host-token` goes
  to `config write`. No field or a bao failure: one warning, deploy
  continues, an old token file is removed. The staged copy is removed
  either way. `justfile`: `deploy` passes `BAO_STORE_HOST_TOKENS_PATH`.
- Tests in `scripts/deploy_test.go` with the stub `bao`: install, path
  override, missing and empty field, no `store_url`, a failed install
  removes the staged copy, the recipe passes the path; the placeholder token
  is absent from every argv and from stdout/stderr.
- Red: six assertion failures; green; `just check` green.
- Commits: the N6 commit on `clone-identity`.

## 2026-11-21 — N8 Delist on "the host does not have it"; delete removes the row

- `internal/app/inventorysource.go` (new): `InventoryWriter` (Delist,
  DeleteRow) over the store client; `Service.missingVM`, the helper every
  VM-ref workflow (lifecycle, comment, autostart, disk, exec, playbook,
  console, creds prepare and fetch, delete) runs its error through: on a
  not-found it asks the host's `Info`, and only a not-found there delists;
  with a live row the error (still not-found, REST 404) says the host has no
  such VM, that it was removed from the inventory, and the date its login is
  kept until. The delist does not decide the store mode, so a down store
  prints nothing; no store configured, or a mode decided local, makes no
  call.
- `DeleteVM` reads the UUID first (store mode) and removes the inventory row
  after a successful delete (also with `--noclone`); a 404 is ignored; an
  unreachable store adds one note.
- Lifecycle, comment, autostart, migrate-disk and add-disk take a `VMRef`;
  CLI, REST and TUI pass the ref. REST: those nine operations get a store
  session with no probe (`lazyStoreOperations`), workstation only.
- Tests: `internal/app/delist_test.go`, `internal/server/delist_test.go`.
- Red: assertion failures (no delist text, no delist call; the fallback line
  printed for a down store); green; `just check` green.
- Commits: the N8 commit on `clone-identity`.

## 2026-11-21 — N9 Read the inventory: `vmtool inventory`, REST, TUI

- `spec/openapi/vmtool.yaml` (still 2.6.0): `GET /v1/inventory`
  (`getInventory`, workstation-only, `?host=`, `?gone=`; 503 without a
  usable store) and its schemas, mirroring the store's; `internal/api`
  regenerated; `spec/README.md` route list and a section on reading the
  inventory.
- `internal/app`: `Service.Inventory`, the one call of all three surfaces;
  it decides the store mode without the local-mode fallback line and answers
  a new `KindUnavailable` (REST 503) with one message when no store is
  configured or usable; `WriteInventory`, the text tables (hosts, VMs,
  machines) shared by CLI and TUI.
- `internal/server`: the handler (403 on a hypervisor, like createVM).
  `internal/cli`: `vmtool inventory` (`--host`, `--gone`, `--json`; exit 1
  without a store). `internal/tui`: a read-only view (`I`; `g` gone, `r`
  refresh).
- Also: the N5 reporter tests seeded the store's errors after the reporter
  had started, so the start-time send could run ahead of them
  (`TestReporterBackoff` failed once under load); now seeded before start.
- Red: assertion failures in app, server and CLI tests (a compile failure
  for the TUI); green; `just check` green.
- Commits: the N9 commit on `clone-identity`.

## 2026-11-21 — N11 Keep the inventory current: the new VM's address, a host's wrong clock, unreadable disks

- XO's call, fixing assumptions N3 A5 and N5 A3, A7.
- Address follow-up (`internal/app/inventory_reporter.go`): a VM running
  with no address at a build (daemon start, kick or retry) is watched by
  UUID for the daemon's `IPWait()` (`internal/server/inventory.go` passes
  it); while any is watched, the reporter re-reads the list every 5 s and
  sends one full report when a watched VM has an address; once per run (a
  run ends when the VM is seen not running); no timer when nothing is
  watched; the watch's own reads add no VM; no polling during a backoff.
- Store ordering by receive time (`internal/store/db`): `ApplyReport` reads
  the store's clock on entry; a report received before the last applied one
  is stale; `generated_at` is stored only. A daemon's clock a year ahead or
  behind no longer silences it.
- Unreadable disks: such a VM is reported with `disks_unreadable` and no
  disks; the store (migration 3) keeps its last known disks and the flag,
  cleared by the next readable report; both specs carry the optional flag
  (store.yaml 1.1.0, vmtool.yaml 2.6.0); `vmtool inventory` and the TUI mark
  the disks cell with `*` and a footnote "disks unreadable on the host; last
  known shown".
- Red: assertion failures in db, store, reporter, CLI, TUI and server tests;
  green; reporter tests 30 runs with no failure; `just check` green.
- Commits: the N11 commit on `clone-identity`.

## 2026-11-21 — N7 ansible_labtwo: mint and render host tokens

- Built after N11: ansible_labtwo was not clean at the first two checks (a
  `.pyc` not ours), clean after its `e2cff11`.
- `roles/vmtool_store/make_secrets.sh hosttokens NAME...` adds or replaces
  only the named hosts' fields (patch, or put when the secret is absent;
  values through a private file, never argv or output; any read error other
  than "absent" stops it); `--revoke` removes fields.
- The role reads `vault_vmtool_store_host_tokens` (new in
  `inventories/group_vars/all`; missing: empty map and a warning), renders
  `host_tokens.yaml` (SHA-256, 0600, uid 65532), sets `host_tokens_file`; a
  change recreates the store as `tokens.yaml` does. README: mint, revoke,
  `just deploy` fetches it.
- Gate: `bash -n` and `ansible-playbook --syntax-check` green; `just check`
  in vmtool unchanged. No red/green (no harness in the role).
- Commits: `9d8d9ca` in ansible_labtwo on `simplify`, not pushed.

## 2026-11-21 — N10 Docs for tranche 12

- `README.md`: a new "The inventory" section in the store part (the command
  and TUI view, who reports and when, the address follow-up, ordering by the
  store's clock, that the store never judges a host up and keeps a sleeping
  host's VMs, the 7-day grace and that an empty report never deletes early,
  explicit delete, outside changes fixed at the next report or on touch,
  origin only from this version on, plain-text comments, the unreadable-disks
  mark, minting, deploying and revoking host tokens); the "daemons hold no
  store token" line corrected.
- `spec/README.md`: the stale-report rule replaced by receive-time ordering;
  `disks_unreadable`, delist-on-touch and delete in the store and inventory
  sections.
- CLI help: `vmtool inventory` long help carries the rules; `create` says
  origin is recorded from this version on; `TestInventoryHelpPinsKeyWords`
  pins the words. `AGENTS.md`: one "Where to look" row for the inventory.
- Red: the help test failing on 15 missing phrases; green; `just check`
  green.
- Commits: the N10 commit on `clone-identity` (it also carries the N7 entry
  above).

## 2026-11-21 — P1 Create timings computed in `internal/app`

- `internal/app/timings.go`: `CreateTimings` (stages in order with ms,
  `total_ms`) and the incremental `CreateTimer`; rules unchanged from T11
  (first to last event per stage, only stages that ran, total is the last
  stamp, stages so far on failure). The CLI's `timings:` line formats it,
  byte for byte as before. No spec change.
- Red: the app tests against a zero-value stub; green; the CLI golden test
  pins one success and one failure line (a refactor: green before and
  after). `just check` green.
- Commits: `7fe8afb` on `clone-identity` (this entry rides with P2's commit).

## 2026-11-21 — P2 createVM streams NDJSON when asked

- `POST /v1/hosts/{host}/vms` with `Accept: application/x-ndjson` (q above
  0; wildcards do not count) answers 200 `CreateVMEvent` lines, flushed per
  line: `progress` (a `ProgressEvent` with `elapsed_ms`), then one `done`
  (VMInfo, timings) or one `error` (message, kind, output, VMInfo when the
  domain exists, timings so far). Refusals before the first event stay the
  JSON statuses; without the header nothing changes. New schemas
  `CreateTimings`, `CreateVMEvent`; API stays 2.6.0 (shared with tranche 12,
  nothing deployed between).
- `tunnelMux.CreateVM` only marks the request; the strict handler writes the
  stream through the existing `ndjsonStream` and app.CreateTimer, so the
  hypervisor 403, store session and body decoding are unchanged. No line is
  logged. spec/README: route notes and a Python example.
- Red: the stream tests got one JSON 201 body (deadline, wrong shape); green;
  `just check` green.
- Commits: the P2 commit on `clone-identity`.

## 2026-11-21 — P3 Docs for tranche 15

- `README.md`: a "Streamed create over REST (API 2.6.0)" section: how to ask
  (`Accept: application/x-ndjson`; wildcards and q=0 do not), the plain JSON
  call unchanged, refusals before work stay JSON, a `curl -N` example with
  placeholders, what each line holds, read the last line, the timings'
  meaning (milliseconds from the start of the workflow, workstation's clock),
  `output` can hold a playbook's output and is never logged.
- `spec/README.md`: the `output` bullet P2 left out.
- `scripts/stream_docs_test.go`: `TestStreamedCreateDocsKeywords`. Red on 20
  missing phrases; green; `just check` green.
- Commits: the P3 commit on `clone-identity`.

## 2026-11-21 — N12 vmtool's own notes about a VM (origin and comment) live in the store only

- vmtool writes nothing of its own into a libvirt definition: `pkg/vmtool`
  loses `metadata.go` (the `urn:vmtool:vm:1` element, `SetComment`) and the
  origin/comment fields of `VMConfig` and `VMInfo`; `domainXML` writes no
  `<metadata>`; a domain still carrying the old element defines, lists and
  reports without error. Removed from the API (no version bump, nothing
  deployed): `DefineVMRequest.origin`/`.comment`, `VMInfo.origin`/`.comment`,
  `VMOrigin`, the daemon's `setVMComment`, the report's origin/comment; from
  `internal/remote`, `internal/server` (`origin.go`), `internal/hosts`
  (`thinOrigin`) and the reporter.
- Store: migration 4 adds `vm_notes` keyed (host, uuid), copies the `vms`
  origin and comment columns over and drops them; the inventory listing joins
  it; one new user-token route `PUT /v1/inventory/{host}/{name}/notes`
  (origin once, same again 200, different 409; comment 512 bytes one line,
  empty clears, 409 on a gone row; with `uuid` before the first report); no
  report touches notes; explicit delete and the sweep remove them; orphan
  notes swept 7 days after their last write; host tokens 401.
- Workstation: `setVMComment` is workstation-only and writes the store (also
  while the host sleeps; 503 with no store). Create writes origin (template
  found through the host's templates for thin clones) and the comment after
  define; local mode records nothing and refuses `--comment` before any
  Manager call; a store failure after define prints one line and create
  succeeds. `vmtool info` and the TUI show notes from the store. New package
  `internal/vmnotes` (comment rule, clone kinds, `Origin`).
- Docs: README's and spec/README's "a VM's notes in the store";
  `scripts/notes_docs_test.go` pins it.
- Red: compile failures on the missing symbols (store, db, vmnotes, app,
  cli, tui); green on the new store, db, app, cli, server, tui, storeclient
  and docs tests; `just check` green.
- Commits: the N12 commit on `clone-identity`.

## 2026-11-21 — F1 The client removes its own token from a server's error text

- `internal/remote`: one cleaner `cleanText(s, token)` (`manager.go`)
  replaces `sanitize`: every `Bearer <token>` (any case of `Bearer`, any
  whitespace) and every bare occurrence of the client's token become
  `[redacted]` before control characters are cleaned and the text truncated;
  an empty token redacts nothing. Used by `statusErr`, the image manifest's
  path, fetch's status error and NDJSON error (also the event the callback
  sees), export's NDJSON error, the probe's version and the tunnel's
  `statusError` (its own copy of the cleaning removed).
- Tests: `internal/remote/redact_test.go` (one per path, fake daemons
  echoing the placeholder token in four shapes; errors, events and captured
  logs checked); `TestStoreErrorsQuoteNoBody` pins that the store client
  quotes no body.
- Red: assertion failures (the token in each error) against a stub cleaner;
  green; `just check` green.
- Commits: the F1 commit on `clone-identity`.

## 2026-11-21 — F2 The reset playbook keeps /tmp on disk

- `ansible/playbooks/image_reset.yml`: task "Keep /tmp on disk (mask
  tmp.mount)" runs `systemctl mask tmp.mount` (no `--now`, as in Packer)
  after the removable loader and before `apt-get clean`; changed only when
  systemd prints "Created symlink". The header names it; README's reset
  paragraph lists it (harmless on 24.04).
- `scripts/image_reset_test.go`: `TestImageResetMatchesEveryTemplate` reads
  the last provisioner of all seven templates and compares it line by line;
  `TestImageResetMaskTmpMountPlace` pins its place.
- Red: 8 commands against the templates' 9, mask task missing; green;
  `just check` and `VMTOOL_TEST_ANSIBLE_SYNTAX=1 just test ./scripts/` green.
- Commits: the F2 commit on `clone-identity`.

## 2026-11-21 — F3 Remove `packer_images/run`; `update` works from `image_dir`

- `packer_images/run` deleted (the justfile has all its commands; its
  `generate` still copied into the retired libvirt pool path). The justfile's
  header, `check` (no more `bash -n run`), `packer_images/README.md` and
  `packer_images/CLAUDE.md` no longer name it; `packer_images/docs/LOG.md`
  has an appended entry.
- `just update <template> [host]` (host from `scripts/cert.sh --local-name`)
  deletes a leftover `<host>/<template>-update`, then runs the repo's
  `./vmtool create <host>/<template>-update <template>.qcow2` as an ordinary
  clone (no `--noclone`) and says it is a throwaway smoke boot and how to
  delete it. No upgrade playbook (unchanged scope).
- Shape checks `check_no_run_script` and `check_update_clone` in the image
  gate. Red: six shape failures (run exists, three docs name it, update's
  bare name and `--noclone`); green; `just -f packer_images/justfile check`
  and `just check` green.
- Commits: the F3 commit on `clone-identity`.

## 2026-11-21 — V1 Named networks on one host

- API 2.7.0: `POST /v1/hosts/{host}/networks`, `GET` and `DELETE
  /v1/hosts/{host}/networks/{name}`, schemas `CreateNetworkRequest` and
  `NetworkInfo`; `listNetworks` unchanged (names).
- `pkg/vmtool/netdef.go` (pure): the home form of the network XML (NAT,
  bridge `vmt-<6 hex>` stp off, MTU 1450, `localOnly` domain default
  `<name>.test`, DHCP .100–.200, no `<metadata>`), its parser, the bridge
  rule (`BridgeVNI`, `BridgeName`), the name rule (VM name rule, at most 63,
  not `default`), `/proc/net/route` parsing, `PlanNetwork` (given subnet or
  the first free /24 of 10.77.0.0/16 against libvirt networks and routes,
  overlap 400; lowest free local bridge number 1–9999) and
  `CheckNetworkDeletable` (409 not made by vmtool, 409 naming attached VMs).
  `pkg/vmtool/netlibvirt.go`: define, autostart, start; destroy, undefine.
- `internal/app` `CreateNetwork`, `Network`, `DeleteNetwork`,
  `ListNetworkMarks`; server handlers; remote client; CLI `vmtool net
  create|delete|show`, `networks` marks `name (vmtool)`; TUI read-only
  network list on `N`; spec/README section. Local mode only; no store.
- Red: assertion failures against stub `netdef.go` and stub app methods;
  green; `just check` green.
- Commits: the V1 commit on `clone-identity`.

## 2026-11-21 — V2 Attach VMs to a network, with a fixed address and a name

- API (2.7.0, unchanged number): primitives `createReservation`,
  `deleteReservation` (`…/networks/{name}/reservations[/{mac}]`),
  `attachInterface`, `detachInterface` (`…/vms/{name}/interfaces[/{mac}]`);
  `VMInfo.networks` (`{network, mac, ip}`), `CreateVMRequest.networks`,
  `NetworkInfo.reservations`, `InventoryVM.networks`; store.yaml (1.1.0)
  `ReportVM.networks`, `InventoryVM.networks`.
- `pkg/vmtool/netcard.go` (pure): MACs `52:54:00` + random unique on the
  host, the reservation `<host mac name ip><lease expiry='0'/></host>`, the
  card `<interface type='network'>` with its MAC, card parsing, the next
  free address of .100–.200 or the one asked (409 taken, 400 outside).
  `netcardlibvirt.go`: virNetworkUpdate live+config, interface attach and
  detach live+config; `Info`/`List` fill `networks` from the reservations.
  `pickIP` ranks by card order, so `VMInfo.ip` stays the first card's.
- `internal/app/netattach.go`: attach = reservation then card (undo the
  reservation if the card fails), detach = card then reservation (card put
  back if the reservation stays); create `--network NAME[=IP]` checked
  before cloning, cards attached between Define and Start; `vmtool delete`
  removes the VM's reservations; the reporter carries networks.
- CLI `net attach host/vm NAME [--ip]`, `net detach host/vm NAME|MAC`,
  `info` and `net show` list cards and reservations; TUI create field and
  detail line; store migration 5 (`vms.networks`).
- Red: assertion failures against stubs (pkg/vmtool, app, store); green;
  `just check` green.
- Commits: the V2 commit on `clone-identity`.

## 2026-11-21 — V3 The network registry in the store (partial; step stays todo)

- store.yaml 1.2.0: user-token `createNetwork`, `listNetworks`,
  `getNetwork`, `deleteNetwork` (409 while members remain),
  `putNetworkMember`, `deleteNetworkMember`; host-token `GET
  /v1/hosts/{host}/networks` (only its own networks, with VNI, bridge, home,
  its own member row and the peers' LAN addresses; another host's name 403),
  added to the host-token allow-list.
- Store migration 6: `networks` (name PK, vni UNIQUE, subnet UNIQUE,
  domain, home, created) and `network_members`; allocation in one
  transaction (lowest free VNI from 10000, lowest free /24 of
  10.77.0.0/16); a 24-goroutine concurrent-create test.
- `internal/storeclient/networks.go`; `internal/app/netregistry.go`
  `CreateNetworkOnHost`: in store mode register first, then define on the
  host through the optional `VNICreator`, undo the entry if the host fails.
- Not finished: no host Manager implements `VNICreator` (the daemon's
  `createNetwork` cannot take a VNI), so a store-mode `net create` still
  numbers its bridge on the host, registers nothing and says so. The
  missing hop needs vmtool.yaml, `pkg/vmtool`, `internal/server` and
  `internal/remote`, outside V3's Touches; left for XO.
- Red: assertion failures (db, store), stub panics (storeclient, app);
  green; `just check` green.
- Commits: the V3 (partial) commit on `clone-identity`.

## 2026-11-21 — V4 The link helper `vmtool-net`

- `deploy/vmtool-net` (root helper, `/usr/local/libexec/vmtool-net`):
  `link VNI BRIDGE LOCAL_IP DEV PEER...` deletes and re-makes `vx<VNI>`
  (`ip link add … type vxlan id VNI dstport 4789 local LOCAL dev DEV`),
  masters it to the bridge, one `bridge fdb append 00:00:00:00:00:00 …
  dst PEER` per distinct peer, up; a failure after the add deletes the
  device. `unlink VNI` (absent: success). `report` prints `ip -j` and
  `bridge -j` JSON. Checks: VNI 1..16777215; bridge `vmt-%06x` of the VNI
  and a bridge in sysfs; DEV by name rule, not `lo`/`vmt-*`/`vx*`/`virbr*`/
  `vnet*`, existing; IPv4 unicast only, a peer never LOCAL.
- `deploy/vmtool-net.sudoers`; `scripts/deploy.sh` stages and installs it
  root:root 0755 beside `vmtool-thin`, sudoers 0440 after `visudo -cf`.
- `pkg/vmtool/netlink.go`: client `NetHelper` (`Link`, `Unlink`, `Report`,
  `ParseNetReport`) through `command()` with `sudo -n`; not yet wired.
- Tests: `scripts/vmtool_net_test.go` (stub `ip`/`bridge`, argv per verb, 49
  refusals), deploy tests, `pkg/vmtool/netlink_test.go`. Red: a skeleton
  helper (assertion failures); green; `just check` green.
- Commits: the V4 commit on `clone-identity`.

## 2026-11-21 — F4 Make `TestImageBlobServesAnExportWithATrailer` deterministic

- Test only (`internal/server/export_test.go`); no product change. The fake
  export reader can hold the send after a first part (`holdReader`, fields
  `holdAfter`, `hold`); the test serves 192 KiB, holds after 128 KiB (more
  than the server's write buffers), reads the headers and the first part,
  asserts no tag yet, releases, reads the rest and asserts the trailer
  digest and exactly one tag, as before.
- No other `internal/server` test had the same timing assumption.
- Red: the old test failed 1 in 500 under `-count=500` ("tagged before the
  body was read", :768). Green: `-count=500`, and `-count=500 -race`;
  `just check` green.
- Commits: the F4 commit on `clone-identity`.

## 2026-11-21 — V3 The network registry in the store (finished)

- The remaining hop to the host: vmtool.yaml 2.7.0 `CreateNetworkRequest`
  gains optional `vni` (1–16777215; bridge `vmt-%06x` of it);
  `PlanNetwork` takes a given number (409 if that bridge exists on the
  host); `pkg/vmtool.Manager.CreateNetworkVNI`; the `internal/hosts`
  wrappers forward it (`app.ForwardCreateNetworkVNI`); `internal/remote`
  sends `vni`; the daemon route passes `vni` and the subnet through
  (`Service.CreateNetworkWithVNI`, never calls the store; 400 out of range).
- Store-mode `net create` registers, then the host defines bridge
  `vmt-%06x` of the registry's VNI with its /24. An older daemon that
  ignores `vni` is detected by the returned bridge: its network and the
  registry entry are removed and the error says to redeploy the host. A
  Manager with no `CreateNetworkVNI` is refused before registering. The
  interim "not registered" line is gone; `spec/README.md` and `net create`
  help say what happens.
- Tests: an end-to-end test with the real store in httptest and a fake host
  running the real `PlanNetwork`; the old-host undo; host 409/400 keep their
  cause; `PlanNetwork` with a given number; remote sends `vni`; the server
  passes it. Red: assertion failures (app, remote, server, cli); green;
  `just check` green.
- Commits: the V3 commit on `clone-identity`.

## 2026-11-21 — V5 Networks that span hosts

- vmtool.yaml 2.7.0: `CreateNetworkRequest.member` (with `vni`), `PUT` and
  `DELETE /v1/hosts/{host}/networks/{name}/peers`, `GET
  /v1/hosts/{host}/underlay?to=` (the daemon's `ip route get`). store.yaml
  1.2.0: network reservations routes (list, put, delete); migration 7.
- `pkg/vmtool/netmember.go`: member form of the network XML (bridge
  `vmt-<VNI hex>` with stp off, MTU 1450, nothing else, no `<metadata>`),
  `ip route get` parsing, `EnsureLink` (compares through `ParseNetReport`,
  links only when different); `netlibvirt.go`: `CreateNetworkMember`,
  `SetNetworkLinks`, `RemoveNetworkLink`, `Underlay`, libvirt's event loop
  and `WatchNetworkStarted`. V4's helper is now called by the daemon.
- `internal/app/netspan.go`: `net join HOST NAME` (store mode only; a
  local-mode network is registered on its first join with its bridge number,
  or refused "recreate it with a store"; the home's and the member's LAN
  rows recorded; the member defines and links; peers pushed to every member
  concurrently, one line per one that does not answer) and `net leave`
  (refused with VMs on it and for the home; unlinks, undefines, removes the
  row, pushes the shorter list).
- `internal/app/netlinks.go` `NetLinkKeeper`, started by the daemon in the
  background: links each active vmtool network only when the registry's
  network of that VNI has the same name and lists this host; store down:
  backoff 30 s to 5 min, logged once; re-links on libvirt's network-started
  event and logs the restart-libvirtd repair.
- Attach on a member: reservation on the home, card on the member with that
  MAC, either undone on failure; detach and delete remove the home's
  reservation; reservations recorded in the registry (best effort).
- CLI `net join`, `net leave`; `networks` and the TUI mark member networks
  `(vmtool, member)`; `setup_host.yml` opens UDP 4789 from the LAN when ufw
  is active.
- Red: assertion failures for pkg/vmtool, store, app (join, cards, keeper),
  server, TUI; compile-only for cli; no recorded red for storeclient, db,
  hosts and the playbook test. Green; `just check` green; opt-in
  `VMTOOL_TEST_ANSIBLE_SYNTAX=1 just test ./scripts/` green.
- Commits: the V5 commit on `clone-identity`.

## 2026-11-21 — V6 The home host (`vmnet`)

- `config.yml` `hosts.<h>.vms: false` (`Host.HoldsVMs`, absent = true); the
  resolver carries it (`NetworkHome`); such a host is still a daemon host
  (reporter sends an empty list; networks and links unchanged).
- Create on a network home is refused in `internal/app` (409, "host H is a
  network home (vms: false in config.yml) and runs no VMs; create the VM
  on another host"), wired in CLI, TUI and the REST server.
- `vmtool hosts` shows `name (network home)`; `vmtool list` and the TUI a
  "network home: runs no VMs" row; REST `HostInfo.network_home` (optional;
  API stays 2.7.0).
- `setup_host.yml`: `vmtool_vm_host` (default true); false skips QEMU,
  OVMF, the `/dev/kvm` check, pools and the NVMe stripe, and refuses pools
  or drives given with it; `gcc` (and `libc6-dev` on Debian) added for
  `just deploy`'s build. README "A network home".
- Red: playbook structural tests before the playbook change; the create
  refusal by disabling the check (app, cli, server); some marks have no
  recorded red. Green; `just check` green; opt-in syntax check green.
- Commits: the V6 commit on `clone-identity`.

## 2026-11-21 — V7 Docs for tranche 17

- README "VM networks": one host (bridge numbers 1–9999 without a store,
  from 10000 with one, reuse of numbers and subnets, MTU 1450, `networks`
  marks `name (vmtool)` and `name (vmtool, member)`), names and fixed
  addresses, spanning networks (store, home, VXLAN unauthenticated and
  unencrypted on the LAN, start-time links), home or member down, the
  repair after a network restart, the current registry-delete limitation.
- spec/README: REST clients order the attach primitives themselves
  (`createReservation`, then `attachInterface` with its MAC); numbers;
  spanning-network notes; the stale "no CLI for members" line fixed.
- `net` help extended. `scripts/networks_docs_test.go` keyword tests.
- Red: 32 README, 9 spec README, 5 help keywords missing; green;
  `just check` green.
- Commits: the V7 commit on `clone-identity`.

## 2026-11-21 — G1 Tests cannot open a libvirt connection

- `pkg/vmtool/connect.go` `connect(uri)` is the one caller of the libvirt
  binding's connect functions; under `go test` (`testing.Testing()`) it
  returns `ErrLibvirtInTest` ("vmtool: tests must not connect to libvirt
  (uri); use a fake Manager or hosts.NewWith") before calling the binding.
  Nothing turns it off. `NewManager` and `WatchNetworkStarted` use it; the
  watch refuses before registering the event loop too.
- Shape test: every `.go` file of the module (not `.claude/`) is parsed; a
  connect function used outside `connect` fails it.
- Tests: `NewManager`, `WatchNetworkStarted`, `connect` and `hosts.New(cfg)`
  resolving the local name all return the refusal. AGENTS.md hard rule 6
  says `pkg/vmtool` refuses a libvirt connection under `go test`.
- Red: shape test (assertion: the two sites); refusal tests compile-only
  (an assertion red would have opened real libvirt). Green; `just check`
  green.
- Commits: d720542.

## 2026-11-21 — G2 `net delete` removes a registered network everywhere

- `Service.DeleteNetworkOnHost` (`internal/app/netspan.go`): in store mode,
  a registered network is deleted only on its home and only while no VM has
  a card on it (the home's libvirt and every live inventory row); members
  that answer unlink and undefine their member network (a card there stops
  the delete, saying what was done), silent members only lose their row and
  are named; then the home unlinks and undefines, its reservation records,
  its row and the entry go. Every step reruns. A configured store that does
  not answer refuses ("the store is needed to delete a network that may be
  registered; try again"). No store: V1.
- The keeper's start pass (`internal/app/netlinks.go`), once the store has
  answered, removes a member-form `vmt-` network the registry no longer
  lists for this host when no domain has a card on it; never a home form,
  nothing with the store down.
- CLI `net delete` prints members done and left; README "Deleting a
  registered network" and spec/README replace the old limitation.
- Red: assertion failures against a V1 stub (app, keeper, CLI); three guard
  tests passed on the stub. Green; `just check` green.
- Commits: the G2 commit on `clone-identity` (the G1 entry above landed
  with it too).

## 2026-11-21 — G3 Orphan reservations are removed, driven only by the store's facts

- `store.yaml` (1.2.0): reservation records gain `uuid` and `orphaned_at`;
  user-token routes `GET /v1/networks/{name}/reservations/orphaned` and
  `POST /v1/networks/{name}/reservations/{mac}/orphan`. Migration 8 adds
  the two columns; existing records take the UUID of the live inventory
  row of the same (host, name), else none.
- Marked only on a store fact: the client could not remove a reservation
  on the home (`vmtool delete`, detach, failed undo), or the sweep removed
  the VM's gone row and no other row has its UUID. Silence marks nothing;
  a VM with no row is never orphaned. A new put of the MAC clears the mark.
- Cleanup with no timer: `net attach`, `net detach`, create with
  `--network`, `vmtool delete` and `net show` first remove the network's
  orphans on the home and then their records, one line each; with the home
  down nothing changes and `net show` lists them.
- Red: assertion failures in db, store server, app and CLI against stubs;
  two absence tests (no row yet; host token) passed before the code. Green;
  `just check` green.
- Commits: the G3 commit on `clone-identity`.

## 2026-11-21 — G4 The network watch survives a libvirtd restart

- `pkg/vmtool/netwatch.go`: the network-started watch behind a small
  connector interface; its connection has keepalive (5 s, 3 probes) and a
  close callback. On close: one Warn line, reconnect with backoff 1 s
  doubling to 30 s (timer only between failed attempts, nothing while
  connected), register again, one Info line and `fn("")` once, which the
  keeper turns into one re-link pass of every network. `stop` ends it,
  also during backoff. The real connector opens through G1's `connect`.
- Tests on a fake connector and fake clock (reconnect, exactly one re-link
  pass, backoff sequence to 30 s, stop during backoff and while connected,
  first-connect error); `-race` green.
- Red: only the reconnect test, made red after the code by disabling the
  recovery call (timed out waiting for the re-link pass); the others have
  no recorded red. Green; `just check` green.
- Commits: the G4 commit on `clone-identity`.

## 2026-11-21 — G5 A network home's own daemon refuses VM-creating routes

- Daemon `config.yml` key `network_home: true` (`config write
  --network-home`); `vmtool config host-vms <host>` prints the client's
  `hosts.<h>.vms` (true/false); `just deploy` passes `--network-home` when
  it prints false (fatal if it fails for a listed host), and since deploy
  rewrites config.yml a redeploy follows the client's entry.
- Such a daemon answers 409 "HOST is a network home and runs no VMs" on
  `defineVM`, `cloneVolume`, `createVolume` and `createTemplate` before
  any libvirt call; a test walks every spec operation and pins exactly
  those four. `vmtool.yaml` documents the 409 (stays 2.7.0). README home
  section.
- Red: assertion or unknown-flag/command failures in config, CLI, server
  and deploy tests before each layer's code. Green; `just check` green.
- Commits: the G5 commit on `clone-identity`.

## 2026-11-21 — H1 The playbook's libvirt wait works on a network home

- `setup_host.yml` "Wait for the system libvirt socket" runs `virsh -c
  qemu:///system list --all` instead of `virsh … version`, which fails on
  a host without QEMU ("Cannot find suitable emulator"); same retries and
  delay, `changed_when`/`check_mode` false. README home section: a home
  needs no QEMU.
- Shape tests pin the command and refuse `virsh … version` on any line of
  `ansible/playbooks/` or `scripts/*.sh`; README sentence pinned.
- Red: assertion failures (old command, `setup_host.yml:328`, missing
  sentence). Green; `just check` and the opt-in syntax check green.
- Commits: `8c3c949` on `clone-identity`.

## 2026-11-21 — H2 The thin pool is created without `--errorwhenfull`, then set

- `nvme_stripe.yml`: `lvcreate --type thin-pool` no longer passes
  `--errorwhenfull` (LVM 2.03.16 refuses it there); the probe reads
  `lv_when_full`; new "Thin pool: queue writes when full" runs `lvchange
  --errorwhenfull n` right after a create in this run, or on an adopted
  pool not at `queue`, so a rerun runs nothing. README: the pool is
  created, then set to queue writes when full.
- Tests: no `lvcreate` carries the option; `lvchange` allow-list has four
  forms; the new task's argv, guards and place pinned; the opt-in
  expression test renders the guard (absent runs, queue skips, error runs).
- Red: assertion failures (allow-list 3 of 4, the create carrying the
  option, probe/fact/task missing). Green; `just check` and the opt-in
  syntax check green.
- Commits: the H2 commit on `clone-identity` (this entry rides in it).

## 2026-11-21 — H3 Set "queue when full" only when the pool is not already at it

- `nvme_stripe.yml`: new read-only "Thin pool: read when full after the
  create" (`lvs --reportformat json -o lv_when_full <vg>/<pool>`) right
  after the create, serving a pool made now and an adopted one; "queue
  writes when full" runs `lvchange --errorwhenfull n` only when that read
  succeeded and says not `queue` (LVM exits 5 on a pool already at queue,
  which a fresh pool is on veritech03). The first probe no longer reads
  `lv_when_full`. README: the pool queues writes when full; vmtool sets it
  only if the host's LVM made it `error`.
- Tests: the read's place, argv and flags and the lvchange's whens pinned;
  the opt-in evaluation test covers made-now/adopted x queue/error and a
  failed read.
- Red: assertion failures (no read task, first probe still reads
  `lv_when_full`, no `vars._when_full`). Green; `just check` and the
  opt-in syntax check green.
- Commits: the H3 commit on `clone-identity` (this entry rides in it).

## 2026-11-21 — K1 wait_ip waits for the first card's address only

- `pickIP` (`pkg/vmtool/ipselect.go`) considers only the first card's MAC
  (first in the domain XML) for leases, agent and ARP; every attached
  network's leases still decide whether an address is held by another MAC.
  While card 1 has no address the VM's `ip` is empty and `WaitForIP` keeps
  waiting; a vmtool network card's reservation no longer ends `wait_ip` or
  becomes `VMInfo.ip` (it shows only in `VMInfo.networks`). `Info` and
  `List` follow through `getIP`.
- `getIP` is split into `ipSources` + `pickIP`; `WaitForIP`'s loop is
  `pollIP(timeout, get, now, sleep)`, same 1 s interval and error text.
- Tests: `TestWaitIPFirstCardOnly` (card 2 leased, card 1 not yet → keeps
  polling; card 1's lease → that address), `TestWaitIPTimesOutWithoutFirstCard`,
  `TestWaitIPSourceErrorKeepsWaiting`; TestPickIP's "second card's lease is
  used" case flipped to "not used", two cases added. Single-card cases
  unchanged.
- Red: pickIP returned 10.77.1.100 / 10.77.0.100 from card 2. Green; `just
  check` green.
- Commits: the K1 commit on `clone-identity` (this entry rides in it).

## 2026-11-21 — T12 Wait for SSH without starting Ansible

- `wait_ssh` is now a direct login: `vmtool.WaitSSH(t, host, name, auth,
  timeout, SSHClock)` (`pkg/vmtool/sshwait.go`) runs `ProbeSSH`, one `ssh
  ... true` through the tunnel's ProxyCommand with create's `auth`, the
  inventory's host-key options, `ConnectTimeout=2`, and `BatchMode=yes` for
  a key login or `NumberOfPasswordPrompts=1` through `sshpass -e` for a
  password login; retried every 1 s until `ssh_wait_seconds`; the last
  attempt's output is kept. The Ansible `wait_for_connection` WaitSSH is
  removed; create makes no temporary inventory for the wait (grow_disk and
  the playbook still do). Error text and `Op: wait_ssh` unchanged.
  `Service.SSHSleep` pairs with `Service.Now` as the wait's clock.
- `testexec.Ansible` also stubs `ssh` and `sshpass` (log lines `probe
  ...`; `STUB_WAIT_FAIL` makes ssh exit 255).
- Tests: `pkg/vmtool/sshwait_test.go` (retries then succeeds, sshpass with
  the password only in SSHPASS, timeout keeps last output, no ansible
  started, bad tunnel/name refused); app tests for the password login, the
  120 s default on a fake clock, failure text and Op, one probe shared by
  grow and playbook. README timings note.
- Red: app tests failing after the create.go switch; probe tests red with
  the retry disabled (`attempts`, `sleeps = 0, want 2`). Green; `just
  check` green.
- Commits: the T12 commit on `clone-identity` (this entry rides in it).

## 2026-11-21 — K2 One-word names resolve on every image

- `packer_images/files/99-wildcard.yaml` gains `dhcp4-overrides:
  {use-domains: true}` for the `en*` match; a shape check in
  `packer_images/justfile` pins it (the one shared file all seven templates
  install). `packer_images/docs/LOG.md` entry. README (names on vmtool
  networks): one-word names need images built since 2026-11-21 or the
  hand edit it gives; `vm.<network>.test` always works.
- Red: `check: shape: files/99-wildcard.yaml lacks dhcp4-overrides:
  {use-domains: true} ...`. Green: `check: ok`; `just check` green. No
  `packer build` run.
- Commits: the K2 commit on `clone-identity` (this entry rides in it).

## 2026-11-21 — K3 Three small rollout frictions

- `vmtool config set-host H --vms=false|true`: alone, it edits only the
  `vms` key of the existing entry (`config.SetHostVMs`; false writes
  `vms: false`, true removes it; an unknown host is refused, file
  unchanged); with `--address`/`--ssh-user` the whole entry is written and
  `--vms` sets the key; left out, an existing `vms` key is carried over.
- `setup_host.yml`: in check mode on a host whose `vmtool` user does not
  exist yet (`vmtool_user_pending`), the vmtool-owned directories and the
  storage pools include are skipped, with one debug line saying check mode
  cannot show them before the user exists; a real run is unchanged.
- `scripts/deploy.sh`: an unlisted own name with no `VMTOOL_SELF_ADDRESS`
  whose `<host>.<domain>` does not resolve (`getent hosts`) uses
  `127.0.0.1`, with one stderr line naming `VMTOOL_SELF_ADDRESS`; a listed
  entry is used as it stands.
- Tests: `TestSetHostVMsKey`, `TestSetHostVMsOnly`,
  `TestConfigSetHostVMsFlag`, `TestConfigSetHostLoneVMs`,
  `TestSetupHostCheckModeBeforeUserExists`, four `TestDeployLocal...`
  cases with a stub `getent`. README for all three.
- Red: `unknown flag: --vms`, `vms: false not written`, `no check-mode note
  task`, deploy wrote the fqdn with no fallback line. Green; `just check`
  and `VMTOOL_TEST_ANSIBLE_SYNTAX=1 just test ./scripts/` green.
- Commits: the K3 commit on `clone-identity` (this entry rides in it).

## 2026-11-21 — X1 All-hosts listings do not ask a network home

- `app.ListAll` takes the network-home test (`home func(string) bool`, nil:
  none); `Service.ListAllTemplates` uses `Service.NetworkHome`. A home is
  neither opened nor asked: its entry is `{Host, NetworkHome: true}` at once,
  no error, no timeout. `HostVMs` and `HostTemplates` gain `NetworkHome`.
- `vmtool list` and `vmtool templates list` (all hosts) print
  `<host>  network home: runs no VMs` for it; a host named explicitly
  (`list home1`, `templates list --host home1`) is asked and its failure is
  the command's error. The TUI's refresh passes the resolver's test and shows
  the same row. `vmtool hosts` unchanged.
- `packer_images/CLAUDE.md` Network section names `dhcp4-overrides:
  {use-domains: true}` (K2's A4).
- Tests: `TestListAllSkipsNetworkHome`, `TestListAllTemplatesSkipsNetworkHome`,
  `TestListAllHostsDoesNotAskNetworkHome`,
  `TestTemplatesListDoesNotAskNetworkHome`, `TestTUIRefreshDoesNotAskNetworkHome`
  (through `newModel` with `hosts.NewWith`).
- Red: CLI rows `home1  ERROR: home1 unreachable`; TUI `the network home was
  opened`; app `the network home was opened` (and a compile red on the new
  `ListAll` signature). Green; `just check` green.
- Commits: the X1 commit on `clone-identity` (this entry rides in it).

## 2026-11-22 — Y1 A deployed daemon reports its real build version

- `internal/buildinfo`: package variable `stamped`, set only by `-ldflags
  -X`; `Version()` is the module version, else the VCS revision, else the
  stamp, else `unknown` (`from(bi, ok, stamp)`).
- `scripts/deploy.sh`: before any ssh, `git -C <root> rev-parse HEAD` plus
  `+dirty` when `git status --porcelain --untracked-files=no` lists a change,
  checked against `^[0-9a-f]{40}(\+dirty)?$` (a bad value stops deploy);
  the build (remote and local) runs with `-ldflags "-X
  github.com/jaredeh/vmtool/internal/buildinfo.stamped=<rev>"`. Outside a
  git checkout it builds unstamped with one stderr line. README one sentence.
- Tests: `TestFromStamp`, `TestDeployStampsBuildRevision`,
  `TestDeployStampsDirtyTree`, `TestDeployLocalStampsBuildRevision`,
  `TestDeployMalformedRevisionStopsBeforeSSH`,
  `TestDeployOutsideCheckoutBuildsUnstamped`; the stub git answers
  `rev-parse` and `status`; two tests no longer list `git.argv` as uncalled.
- Red: `stamp, no vcs: "unknown", want "deadbeef+dirty"`; deploy: no ssh
  call containing the `-ldflags` build. Green; `just check` green.
- Rollout: the daemons show their revision after their next `just deploy`.
- Commits: the Y1 commit on `clone-identity` (this entry rides in it).

## 2026-11-22 — Y2 The host's fetch from the store is not called an upload

- API 2.8.0: `ProgressEvent.stage` gains `fetch` (the host downloading from
  the store) and `relay` (this machine streaming the store's bytes to the
  host); `upload` stays this machine sending its own copy. `internal/api`
  regenerated.
- `internal/app/fetch.go`: the fetch path emits `fetch`, the relay path
  `relay`; `noteUpload` becomes `noteStage(in, stage, line)` so each notice
  sits under its own stage (store-unreachable notices under `fetch`, relay
  notices under `relay`, the "not in the store" hint under `upload`). A
  fallback shows the stages that ran, each timed under its own name.
- CLI `imageTransferProgress` prints `fetching:` / `relaying:` /
  `uploading:` (also used by `templates export --push`, still `uploading:`);
  TUI `transferTitle` likewise. Done lines unchanged. README store steps and
  `spec/README.md` name the stages. Daemons need no redeploy.
- Tests: `TestSyncStoreStageNames`, `TestTransferProgressWordsByStage`,
  `TestTimingsLineNamesTransferStages`, `TestTransferTitleWordsByStage`,
  `TestCreateStreamCarriesTransferStageNames`; version pins 2.8.0
  (`internal/server`); `internal/remote/createfetch_test.go` follows the
  rename.
- Red: `fetch: stages [upload]`, `fallback: stages [upload]`; CLI
  `"\ruploading:  25% …", want "\rfetching:  25% …"`. Green; `just check`
  green.
- Commits: the Y2 commit on `clone-identity` (this entry rides in it).

## 2026-11-22 — Z1 `net join` no longer fails when two links are made at once

- Cause: the peers route and the daemon's link keeper (woken by libvirt's
  "network started") both ran report-then-link on the member at once; the
  helper's `link` deleted and re-added `vx<VNI>`, so the second `ip link add`
  got `File exists`.
- `pkg/vmtool/netlink.go`: a process-wide lock per VNI (`lockNet`) and
  `NetHelper.EnsureLinkLocked` / `UnlinkLocked`; `Manager.SetNetworkLinks`
  and `RemoveNetworkLink` (`netlibvirt.go`) use them, so every caller in the
  daemon (peers route, keeper, join) runs one after the other per network.
- `deploy/vmtool-net`: `link` and `unlink` hold an exclusive `flock` on the
  fixed `/run/lock/vmtool-net.lock` (after argument validation); `link`
  keeps an existing `vx<VNI>` with the same VNI, local address, device,
  dstport 4789 and bridge, and only syncs its flood entries (`bridge fdb
  del` / `append`, every read-back dst validated) and sets it up; any other
  device is replaced as before. No new verb, no sudoers change.
- Tests: `TestEnsureLinkLockedSerialises`, `TestUnlinkLockedWaitsForLink`
  (stateful stub sudo); helper `TestNetLinkIdempotent`,
  `TestNetLinkIdempotentNoChange`, `TestNetLinkDiffersReplaced`,
  `TestNetLinkIdempotentBadFDBEntry`, `TestNetLockTaken`,
  `TestNetReportNoLock` (stub `ip`/`bridge`/`flock`).
- Red: `helper calls [report report link link]` with `RTNETLINK answers:
  File exists`; helper deleted and re-made a matching device; no `flock`
  call. Green; `just check` green.
- Hosts need `just deploy` (helper and daemon) for the fix to take effect.
- Commits: the Z1 commit on `clone-identity` (this entry rides in it).
