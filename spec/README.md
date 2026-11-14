# spec/ — the API contract

vmtool is **spec-first**. `openapi/vmtool.yaml` is the source of record for every HTTP
route, payload, and status code. Code follows the spec; the spec does not follow
a handler.

## Layout

| Path | Contract |
|---|---|
| `openapi/vmtool.yaml` | Every HTTP endpoint `vmtool server` registers. oapi-codegen generates models, a stdlib `ServerInterface`, a strict wrapper, an embedded spec, and a test client (`internal/api`). |

Interactive SSH from CLI (`vmtool ssh`) and TUI (`t`) execs `ssh` locally and
is not an HTTP operation.

## Codegen workflow

```bash
go generate ./internal/api/...
scripts/verify-generate.sh   # regenerate + fail if internal/api is dirty
```

Run generate after editing the YAML and commit `internal/api/api.gen.go`.

## Coverage policy

**Every HTTP endpoint appears in `openapi/vmtool.yaml`.** Paths are literal and
`servers[0].url` is `/`, so the spec never lies about a real request path.

An exclusion requires a compelling technical reason and a row in the register
below. Enforced by `internal/server/specparity_test.go` once REST is on the
generated mux (see `docs/design/spec-first.md` PR 3).

## Exclusion register

| Route | Reason |
|---|---|
| *(empty)* | No routes are outside the spec. If oapi-codegen v2.4.1 emits a catch-all `GET /` that maps `/nope` to docs, move `getDocs` / `getOpenAPISpec` off the generated mux and record them here. |

## Security

Global `security: [bearerAuth]` (`components.securitySchemes.bearerAuth`,
HTTP bearer). Only `getDocs`, `getOpenAPISpec` and `getOpenAPIJSON` set
`security: []`; every other operation lists `401` (`components.responses.Unauthorized`).

The server derives the exempt routes from the embedded spec at startup
(`internal/server/auth.go`), not from a hand-written list: an operation is
public only if its effective `security` is empty. Every other request,
including unknown paths, gets `401` with `WWW-Authenticate: Bearer` unless it
carries exactly one `Authorization: Bearer <token>` whose SHA-256 matches
`token_sha256` in `config.yml` (constant-time compare). The check runs before
the body is read or any handler runs. Neither the header nor the token is
logged. `TestAuthEveryOperation` walks every operation in the spec.

`vmtool server` serves HTTPS only (TLS 1.2+), on loopback too: it refuses to
start unless `config.yml` sets `token_sha256`, `tls_cert` and `tls_key`. There
is no plain-HTTP or unauthenticated mode.

## URL versioning

Paths are versioned: every route except the docs routes and `GET /version`
lives under `/v1`. Everything that acts on one hypervisor is host-scoped,
`/v1/hosts/{host}/{vms,networks,bridges,images,pools}[...]`; `/v1/hosts`,
`/v1/playbooks` and `/v1/metal[...]` are global. A `{host}` the server does
not serve is `404` with a message naming the hosts it does.

`GET /version` is unversioned so a client built for another API version can
still read it. It returns `api_version` (the spec's `info.version`, read from
the embedded spec at runtime), `build_version` (module version or VCS
revision) and `host` (the server's own name). It needs the bearer token like
every non-docs route.

A breaking change bumps the major version in `info.version` and the path
prefix (`/v2/...`). Clients require an exact `api_version` match and refuse a
server that reports anything else.

## GET / pattern

PR1 records the exact std-http pattern oapi-codegen emits for the root after
the first generate. Update this line when known:

| Operation | Expected ServeMux pattern |
|---|---|
| `getDocs` | `GET /{$}` — Swagger UI (not a catch-all) |
| `getOpenAPISpec` | `GET /openapi.yaml` |
| `getOpenAPIJSON` | `GET /openapi.json` |
