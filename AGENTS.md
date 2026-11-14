# Project rules for vmtool

**vmtool** — CLI, TUI, and REST for KVM/QEMU VMs via libvirt, plus Ansible playbooks.

## Hard rules

1. **Spec-first API.** Change `spec/openapi/vmtool.yaml` first. Run `go generate ./internal/api/...` (or `scripts/verify-generate.sh`). Update `internal/app`, adapters (`internal/server`, `internal/cli`, `internal/tui`), and tests in the same change. Do not hand-edit `internal/api/api.gen.go`. Do not add a route to `Handler()` that is not in the spec unless it is listed in the exclusion register in [`spec/README.md`](spec/README.md).
2. **One workflow layer.** CLI, TUI, and REST call `internal/app`. Do not reimplement create / playbook / resize / lifecycle in a surface. `pkg/vmtool` stays primitives (libvirt, ansible, SSH, inventory).
3. **Secrets.** Never commit `.machines/`, guest passwords, or SSH private keys. Do not log `ssh_pass` or request bodies.
4. **Generated artifacts.** `internal/api/api.gen.go` is produced by oapi-codegen. Regenerate; do not patch.
5. **Loopback REST.** Default bind is `127.0.0.1:9473`. Every request needs the bearer token. The server refuses to bind off loopback unless TLS and `token_sha256` are configured in `config.yml`.

## Spec-first change procedure

1. Edit `spec/openapi/vmtool.yaml`.
2. `go generate ./internal/api/...`
3. Implement or adjust `internal/app` and the three adapters.
4. `scripts/verify-generate.sh` and `go test ./...` before push.

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
| Agent definitions | `architect`, `foreman`, `implementer`, `recon` (plugin); `.grok/agents/` (Grok) |
