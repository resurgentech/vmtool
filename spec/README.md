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
is no plain-HTTP or unauthenticated mode. ALPN offers only `http/1.1`.

`GET /v1/hosts/{host}/vms/{name}/tunnel` (`openVMTunnel`) is the one operation
not served by the strict handler: an HTTP/1.1 `Upgrade: vmtool-tunnel` that,
after the bearer check, a VM lookup and a dial to the libvirt-reported IP on
port 22, hijacks the connection and relays raw bytes (`internal/server/tunnel.go`,
wired through `tunnelMux`). It is still in the spec, so the exclusion register
stays empty.

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

## Hypervisor role and daemon primitives

A server whose `config.yml` has `role: hypervisor` is a remote daemon: it does
libvirt work only and holds no guest secrets. Every operation the spec marks
`x-vmtool-workstation-only: true` answers `403` with
`{"error":"<operationId> is served by the workstation, not by hypervisor <name>"}`.
The check runs once, in `internal/server` (`gateWorkstationOnly`), after the
bearer check and before the body is read, driven by that extension read from
the spec, so the set cannot drift. The marked operations are `createVM`,
`runVMCommand`, `runPlaybook`, `resizeDisk` (the workflow form that also grows
the guest and uses credentials), `listPlaybooks` (playbooks live on the
workstation) and every `/v1/metal` operation. `openVMTunnel` is not marked: a
hypervisor exists to serve it. `TestWorkstationOnlySet` pins the exact set.

`remote.Manager` (an HTTP client implementing `app.Manager`) calls these
routes. Every Manager method maps onto one route; the lifecycle routes run
`Manager.X` then `Manager.Info` and return the `VMInfo`, so the client drops
the body and keeps the error.

| `app.Manager` method | operationId | Notes |
|---|---|---|
| `Start` / `Stop` / `Resume` / `Reboot` | `startVM` / `stopVM` / `resumeVM` / `rebootVM` | Method, then `Info`. Returns `VMInfo`. |
| `Destroy` | `poweroffVM` | Same shape. |
| `SetAutostart` | `setAutostart` | Same shape. |
| `Info` | `getVM` | `VMInfo` carries every field of `vmtool.VMInfo`. |
| `List` | `listVMs` | |
| `Delete(name, noclone)` | `deleteVMDefinition` | Raw. `deleteVM` is the workstation workflow: it also forgets `.machines/<host>/<vm>/` credentials. |
| `MigrateDisk` | `migrateDisk` | Method, then `Info`. |
| `AddDisk` | `addDisk` | Method, then `Info`; result carries `path` and `target` (`DiskDevice`). |
| `ResizeDisk` | `resizeVMVolume` | New. libvirt only. `resizeDisk` is the workstation workflow that also grows the guest. |
| `WaitForIP` | `waitVMIP` | New. `timeout_seconds` 1..3600, default 120. |
| `Define` | `defineVM` | New. `PUT vms/{name}/definition`; no `ssh_user`/`ssh_pass`. |
| `CreateVolume` | `createVolume` | New. Returns `{path}`. |
| `CloneImage` | `cloneVolume` | New. `base_path` must be inside a pool. Returns `{path}`. |
| `ResizeVolume` | `resizeVolume` | New. `path` must be inside a pool. |
| `ImagePath` | `getImagePath` | New. 404 if no pool has it. |
| `ListNetworks` / `ListBridges` | `listNetworks` / `listBridges` | |
| `ListImagesByPool` | `listImages` | `{pool: [image]}`. |
| `DeleteImage` | `deleteImage` | `pool` query, default `default`. |
| `ListPools` | `listPools` | `PoolInfo{name,path,active}`. |
| `CreatePool` | `createPool` | |

| `ImageManifest` (`app.ImageStore`) | `getImageManifest` | New. Optional interface, remote only. `{name, sha256?, size?, foreign, path?}`. |
| `UploadImage` (`app.ImageStore`) | `uploadImage` | New. `PUT images/{name}`, `application/octet-stream`, `X-Image-SHA256`, `Content-Length`; streamed. |

`Create` is not on `app.Manager`; the workstation's `app.Service.CreateVM`
composes `cloneVolume`, `resizeVolume`, `defineVM` and `startVM`.

Path inputs (`base_path`, `path`, `disk_path`, `extra_disks[].path`) must be
absolute and, after `filepath.Clean` and symlink resolution of the existing
prefix, strictly inside the path of a libvirt storage pool; otherwise `400`.
The cleaned path is what reaches libvirt. Names in bodies (`new_name`, volume
`name`, the VM name in `.../vms/{name}/definition`) must be plain names. The
check is point-in-time; it is defence in depth for a loopback, token-guarded
API, not a sandbox.

## Image cache

Hypervisors cache base images in their `default` pool. Each daemon keeps
`image-manifest.yml` (name -> sha256, size) in its working directory and owns
a volume only if that manifest lists the name. `getImageManifest` reports an
entry, or `foreign: true` when a same-named volume exists (any pool, or a
file in the `default` pool's directory) that the manifest does not list.
`uploadImage` refuses a foreign name with 409 before reading the body, writes
`.<name>.part`, verifies the SHA-256, deletes an old volume only if the
manifest lists it, and moves the file in with a hard link (never an
overwrite). It is the only route exempt from the 1 MiB body cap
(`withMaxBytes`); the server sets no overall read timeout, and an upload that
sends nothing for 2 minutes fails. Both operations are served by hypervisors
only: a workstation answers 403 for itself, refuses `uploadImage` for every
host (its `app.CreateVM` uploads to the daemon directly through
`remote.Manager`) and answers `getImageManifest` for a configured host by
asking that host's daemon. On the workstation the same file name is a hash
cache (adds `path` and `mtime`), so an unchanged image is not re-hashed. The
handler is `uploadImage` in `internal/server/images.go`, served by
`tunnelMux` (not the strict handler) so it can stream and refuse before the
body is read.

## GET / pattern

PR1 records the exact std-http pattern oapi-codegen emits for the root after
the first generate. Update this line when known:

| Operation | Expected ServeMux pattern |
|---|---|
| `getDocs` | `GET /{$}` — Swagger UI (not a catch-all) |
| `getOpenAPISpec` | `GET /openapi.yaml` |
| `getOpenAPIJSON` | `GET /openapi.json` |
