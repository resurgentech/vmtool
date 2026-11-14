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
