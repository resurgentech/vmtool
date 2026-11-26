# Project rules for vmtool

**vmtool** — CLI, TUI, and REST for KVM/QEMU VMs via libvirt, plus Ansible playbooks.

## Hard rules

1. **Spec-first API.** Change `spec/openapi/vmtool.yaml` first (the store's API, `spec/openapi/store.yaml`, is handled the same way; its generated package is `internal/storeapi`). Run `go generate ./internal/api/... ./internal/storeapi/...` (or `scripts/verify-generate.sh`, which covers both). Update `internal/app`, adapters (`internal/server`, `internal/cli`, `internal/tui`), and tests in the same change. Do not hand-edit `internal/api/api.gen.go`. Do not add a route to `Handler()` that is not in the spec unless it is listed in the exclusion register in [`spec/README.md`](spec/README.md).
2. **One workflow layer.** CLI, TUI, and REST call `internal/app`. Do not reimplement create / playbook / resize / lifecycle in a surface. `pkg/vmtool` stays primitives (libvirt, ansible, SSH, inventory).
3. **Secrets.** Never commit `.machines/`, guest passwords, or SSH private keys. Do not log `ssh_pass` or request bodies.
4. **Generated artifacts.** `internal/api/api.gen.go` is produced by oapi-codegen. Regenerate; do not patch.
5. **Loopback REST.** Default bind is `127.0.0.1:9473`. Every request needs the bearer token. The server refuses to bind off loopback unless TLS and `token_sha256` are configured in `config.yml`. The one exception is the store (`cmd/vmtool-store`): it may serve plain HTTP off loopback only when its config sets `behind_proxy: true`, inside the Docker network behind a reverse proxy, which terminates TLS; it still requires a token on every route except `GET /version`.
6. **Tests.** Tests never reach real libvirt, ssh, ansible, sudo or OpenBao. External tools are stubbed only through `internal/testexec` (`testexec.Stub`, `testexec.Ansible`); under `go test`, `pkg/vmtool` refuses to start any tool outside the stub directory, refuses the test binary as the tunnel's ProxyCommand, and refuses to open a libvirt connection. The gate (`just test` / `just check`) runs in a capped systemd scope (`scripts/test-capped.sh`); run ad-hoc `go test` the same way: `scripts/test-capped.sh ./pkg/...` or `systemd-run --user --scope -q -p TasksMax=256 -p CPUQuota=800% -- go test <pkgs>`.
7. **Public and generic.** This repository is public. Code, docs, help text and config examples are generic: examples use the neutral names (`hv1`, `hv2` for hypervisors, `home1` for a network home, `ws1` for the workstation, `example.lab` for the domain, `registry.example.lab` for the registry, `https://proxy.example/vmstore` for the store behind a reverse proxy). Deployment values come from settings with no default. Test fixtures and logs are exempt.

## Spec-first change procedure

1. Edit `spec/openapi/vmtool.yaml`.
2. `go generate ./internal/api/...`
3. Implement or adjust `internal/app` and the three adapters.
4. `just check` (`scripts/verify-generate.sh` and the capped `go test ./...`) before push.

See [`spec/README.md`](spec/README.md) and [`docs/design/spec-first.md`](docs/design/spec-first.md).

## Roles (orch)

The default role of a session is **XO**: a request in chat becomes a
`ROADMAP.md` step, not an edit. XO drafts a spawn prompt, shows it, and
spawns **ARCHITECT** (plans into `ROADMAP.md`) at once, or waits for
approval and then spawns **FOREMAN** (builds from it); either runs with
a clean context, and XO reviews the report. When the user asks to run
a request unattended (`/orch run`), the roadmap is accepted in advance
and XO chains the two without stopping. Answer directly only when no
code change is needed and the context is in hand, or when the user
says to just do it.

## Orch rules

These add to the hard rules above; they do not replace them.

1. **FOREMAN builds only from `ROADMAP.md`.** No scope from chat.
2. **`docs/LOG.md` is append-only.**
3. **The gate stays green** for packages you touch before a commit.
   Default gate: `just check` (`.orch.json`).
4. **Every built step leaves a report** in `docs/reports/`, with its
   assumptions: context, question, decision, alternatives, confidence.
5. **Secrets** never go in the roadmap, the reports or the log either
   (hard rule 3).

## Where to look

| Need | Where |
|---|---|
| The queue | `ROADMAP.md` (local, gitignored; `/orch init` makes one) |
| What shipped | `docs/LOG.md` |
| What was done, and the judgment calls | `docs/reports/` (local, gitignored): one file per built step, recap plus assumptions |
| Roles and process | the orch plugin (`/orch`), or `.grok/agents/` for Grok |
| Test stubs | `internal/testexec` |
| The store's API and contract | `spec/openapi/store.yaml`, the store sections of [`spec/README.md`](spec/README.md); generated package `internal/storeapi` |
| The store's code | `internal/store` (server, images, credentials, `blob/`, `db/`), `cmd/vmtool-store`, `deploy/vmtool-store.Dockerfile`; the client side is the store pieces of `internal/app` and `internal/storeclient` |
| The VM inventory | the store: `internal/store/inventory.go`, `internal/store/db` (inventory tables); the daemon's reporter: `internal/app/inventory_reporter.go`; the read and delist: `internal/app/inventoryread.go`, `internal/app/inventorysource.go`; the CLI: `internal/cli/inventory.go` (`vmtool inventory`) |
| Using and deploying the store | the store section of [`README.md`](README.md); deploying it is one generic paragraph there (run the image behind a reverse proxy) |
| Agent definitions | `architect`, `foreman`, `implementer`, `recon` (plugin); `.grok/agents/` (Grok) |
