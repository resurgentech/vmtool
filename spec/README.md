# spec/ — the API contract

vmtool is **spec-first**. `openapi/vmtool.yaml` is the source of record for every HTTP
route, payload, and status code. Code follows the spec; the spec does not follow
a handler.

## Layout

| Path | Contract |
|---|---|
| `openapi/vmtool.yaml` | Every HTTP endpoint `vmtool server` registers. oapi-codegen generates models, a stdlib `ServerInterface`, a strict wrapper, an embedded spec, and a test client (`internal/api`). |
| `openapi/store.yaml` | The image and credential store's HTTP API (`vmtool-store`, 1.0.0), served by `cmd/vmtool-store`, not by `vmtool server`. Its own generated package, `internal/storeapi`; handled as `vmtool.yaml` is (see [The store](#the-store)). |

Interactive SSH from CLI (`vmtool ssh`) and TUI (`t`) execs `ssh` locally and
is not an HTTP operation.

## Codegen workflow

```bash
go generate ./internal/api/... ./internal/storeapi/...
scripts/verify-generate.sh   # regenerate both + fail if internal/api or internal/storeapi is dirty
```

Run generate after editing a YAML and commit the generated files
(`internal/api/api.gen.go`, and the generated file in `internal/storeapi`).

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
HTTP bearer). Only `getDocs`, `getOpenAPISpec`, `getOpenAPIJSON` and
`getVersion` set `security: []`; every other operation lists `401`
(`components.responses.Unauthorized`).

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

## Console (WebSocket)

`GET /v1/hosts/{host}/vms/{name}/console` (`openVMConsole`) and
`GET /v1/metal/{name}/console` (`openMachineConsole`) upgrade to a WebSocket
(RFC 6455) that is an interactive login shell on the target: `ssh -tt` with
no remote command, on a pty on the server with `TERM=xterm-256color`. There is
no command parameter (for one command use `exec`). The code is
`internal/server/console.go`, wired through `tunnelMux`; both operations are
workstation-only. Nothing in the stream is logged: no output byte, no typed
input, no control message, and none goes into an error.

### Handshake

- URL: `ws[s]://<server>/v1/hosts/{host}/vms/{name}/console` or
  `.../v1/metal/{name}/console`, with optional `?cols=N&rows=N` (integers
  1-65535; default 80x24).
- `Authorization: Bearer <token>` on the handshake, as for every route.
- No `Origin` header: any `Origin` is refused (403), so a browser cannot open
  a console. Most non-browser clients send none unless told to.
- No subprotocol is offered or required.
- HTTP/1.1 only. A client that negotiated HTTP/2 gets 426.

The server checks in this order, and every refusal is an ordinary JSON
`{"error": "..."}` response sent before the upgrade. Success is
`101 Switching Protocols`.

| # | Check | Status |
|---|---|---|
| 1 | bearer token | 401 |
| 2 | server role is not workstation | 403 |
| 3 | an `Origin` header is present | 403 |
| 4 | not HTTP/1.1 (the response carries `Upgrade: websocket`) | 426 |
| 5 | not a WebSocket upgrade (`Connection`, `Upgrade`, `Sec-WebSocket-Version: 13`, `Sec-WebSocket-Key`) | 400 |
| 6 | invalid name, or `cols`/`rows` not an integer in 1-65535 | 400 |
| 7 | the target: unknown host, VM or machine | 404 |
| 8 | a VM with no IP address (a machine has no such case) | 409 |
| 9 | 16 consoles already open, or the server is shutting down | 429 |
| 10 | ssh or its pty could not be started | 502 |

An error while resolving the target (steps 7 and 8) is mapped by kind (400,
404, 409, 502; anything else 500), as for the other operations.

### Messages

| Direction | Type | Meaning |
|---|---|---|
| client to server | binary | typed input, written to the terminal as is |
| client to server | text | a control message; only `{"event":"resize","cols":N,"rows":N}` (N an integer 1-65535) resizes the pty |
| server to client | binary | terminal output, at most 32 KiB per message |

Typed input must be a binary message. A browser terminal's `ws.send(string)`
is a text message and is treated as a control message: it is ignored, not
typed. A text message that is not valid JSON, is another event, or has a size
outside 1-65535 is ignored too: it is not written to the terminal and the
socket stays open. The server sends no text messages.

### Close codes

| Code | Reason | When |
|---|---|---|
| 4000 | `exit N` | the shell ended; `N` is ssh's exit status (255 for an ssh connection failure). The remaining output is sent first |
| 1009 | `message too big` | an inbound message over 64 KiB |
| 1011 | `ping timeout` | no pong within 30 s of a ping |
| 1011 | `console failed` | the terminal could not be written, or the shell was killed by a signal so it has no exit status |
| 4503 | `unavailable` | the server is shutting down |

The reason of an exit matches `^exit [0-9]+$`. If the client closes first or
the connection drops, the server sends nothing. In every case the server kills
the shell's whole process group (ssh and the tunnel's ProxyCommand) before it
returns, so a dropped console leaves no process behind.

### Limits

- Inbound messages: 64 KiB. Outbound messages: 32 KiB.
- The server pings every 30 s and ends a console that does not pong within
  30 s; a client library that answers pings needs nothing more.
- At most 16 consoles at once, across both operations (429 above that).
- The server's own read and write deadlines do not apply to a console.
- Compression (permessage-deflate) is not offered.

### Example (Python, `websockets`)

Starts `sudo`, waits for its password prompt, answers it, resizes, and leaves. The token and the CA path
come from the environment; the secret is a placeholder.

```python
import asyncio
import json
import os
import re
import ssl

from websockets.asyncio.client import connect
from websockets.exceptions import ConnectionClosed

URL = os.environ["VMTOOL_URL"]  # e.g. wss://workstation:9473
TOKEN = os.environ["VMTOOL_TOKEN"]
CTX = ssl.create_default_context(cafile=os.environ["VMTOOL_CA"])  # the hulbert.local CA


async def main() -> int:
    uri = URL + "/v1/hosts/jetfire/vms/web/console?cols=120&rows=40"
    try:
        # No origin= : any Origin header is refused with 403.
        async with connect(
            uri, additional_headers={"Authorization": "Bearer " + TOKEN}, ssl=CTX
        ) as ws:
            # Typed input must be bytes: a str is a text message and is ignored.
            await ws.send(b"sudo -k; sudo true\n")
            seen = b""
            while b"password" not in seen.lower():  # terminal output is binary
                seen += await ws.recv()
            await ws.send(b"PLACEHOLDER-not-a-secret-1\n")  # the answer
            # A resize is a text message holding JSON.
            await ws.send(json.dumps({"event": "resize", "cols": 200, "rows": 50}))
            await ws.send(b"exit 0\n")
            while True:  # drain the output until the server closes
                await ws.recv()
    except ConnectionClosed as closed:
        rcvd = closed.rcvd  # the server's close frame, or None
        if rcvd is not None and rcvd.code == 4000:
            m = re.fullmatch(r"exit ([0-9]+)", rcvd.reason)
            if m:
                return int(m.group(1))
        raise RuntimeError(f"the console did not end with an exit: {rcvd}")


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
```

### Compared with interloc's terminal contract

This is the same data path and vocabulary as interloc's: binary terminal bytes
in both directions, the `{"event":"resize",...}` control message, and close
code 4000 when the process exits. The differences: one socket instead of
interloc's two (`ws/frontend` for data, `ws/control` for controls); no
terminal resource to create and no `?terminal=` id; no reattach and no ring
replay, so a dropped socket is a finished console; no closing trailer, only
the close frame's `exit N`; a bearer header instead of a session cookie; and
no browser, because an `Origin` is refused.

### Porting hermes_setup's `send_secret`

`send_secret` was written for the old console (removed in `0ee7231`); it
does not work against this one as it is. What to change:

- The paths are `/v1/hosts/{host}/vms/{name}/console` and
  `/v1/metal/{name}/console`; the host is a path segment of the VM path.
- Send the bearer header (`additional_headers`) and no `origin`.
- Send input as bytes. `socket.send(str)` is a text message and is ignored
  here, so the prelude, the heredoc and the exit query would silently go
  nowhere. Use `.encode()`.
- Read the exit code from the close (`rcvd.code == 4000`, parse
  `rcvd.reason`), not from a marker echoed in the output.
- Better, leave the console for this job: `send_secret` becomes
  `POST /v1/hosts/{host}/vms/{name}/exec` (API 2.1.0) with
  `{"command": ["sh", "-c", "umask 077; cat > /p/.env.new && mv /p/.env.new /p/.env"], "stdin": "KEY=PLACEHOLDER\n"}`.
  The reply carries `exit_code`; the secret is on stdin, in no echo and in no
  argv. `POST /v1/metal/{name}/exec` is the same for a machine.

## The store

`openapi/store.yaml` is the contract of `vmtool-store`, a separate command
(`cmd/vmtool-store`, built with `CGO_ENABLED=0`; it imports neither
`pkg/vmtool` nor `internal/config`). It keeps base images (bytes in an S3
bucket, an index in SQLite) and guest credentials (AES-256-GCM columns in
SQLite). The spec-first rule applies as for `vmtool.yaml`, and every route the
store serves is in its spec (the exclusion register above is for vmtool's
server). Setup, modes and deployment are in the root `README.md`; this section
is the contract.

**Auth.** Every route needs `Authorization: Bearer <token>` except `GET
/version`. The store holds a SHA-256 per named caller (`tokens.yaml`), never a
token, and logs requests by caller name; it logs no token, ticket, credential
or request body. One more scheme exists, `Authorization: Ticket <t>`, accepted
on exactly `GET /v1/images/{name}/blob`; on any other route it is 401, like a
bad token. **A store token opens every guest's credentials.**

| Group | Operations |
|---|---|
| Probe | `getVersion` (public); `whoami` (the caller's name; the client's mode check) |
| Images | `listImages`, `getImage`, `putImage`, `deleteImage`, `getImageBlob`, `createImageTicket` |
| VM credentials, keyed by (host, name) and the domain UUID | `getVMCredential`, `putVMCredential`, `deleteVMCredential` |
| Metal records, keyed by name | `listMachineCredentials`, `getMachineCredential`, `putMachineCredential`, `deleteMachineCredential` |

**Images.** `putImage` streams the body to the bucket and hashes it as it is
written; the image is indexed only when the SHA-256 equals `X-Image-SHA256` and
the length equals `Content-Length` (400 otherwise; 411 without a length). A push
of an existing name with a new digest replaces it, and the old image is served
until the new one is indexed (the object key contains the digest). A second
push of a name already in flight is 409.

**Tickets.** `createImageTicket` returns a random ticket for one image at its
current digest: read-only, 15 minutes (`store.TicketLifetime`), spent by the
first successful download, kept by the store only as a hash and only in memory
(a store restart voids every ticket). A ticket opens that image's blob and
nothing else: another image, a changed digest, an expired or spent ticket, or
any other route all answer 401.

**Credentials.** The `GET` of a VM record carries the domain UUID the caller
holds. A record for another UUID is 404 with `"stale": true` and no credential
field, so a record for a recreated VM is never used. That `GET` and the machine
`GET` are the only responses that carry a secret; a `PUT` does not echo one.
`vmtool delete` and `create`'s replace call the `DELETE`.

### The client's two modes

`config.yml` `server: {url: https://...}` (https only; no credentials, query or
fragment in the URL) makes a client "configured". The token is
`VMTOOL_SERVER_TOKEN` from the environment, else `.env`; never `config.yml`. At
the start of a command that uses credentials or images
(`app.StoreSession.Mode`), one authenticated `GET /v1/whoami`, limited to 3
seconds, decides, once for the whole command:

- answered: **store mode**. Credentials, metal records and images come from the
  store; `.machines/` is a cache.
- not configured, no token, or no answer: **local mode**. When configured, one
  stderr line: `<host> unreachable: using this machine's .machines/ and
  image_dir` (`<host>: <reason>: using ...` when the token is missing).

A store that fails inside a store-mode command does not change the mode:
credential writes land in `.machines/` marked pending, the command succeeds,
one line says the credentials are only on this machine, and reads fall back to
the cache. There is no queue.

### Cache markers and reconciliation

A credentials folder `.machines/<vm>.<host>.vmtool/` holds a `.vmtool-source`
marker (`pkg/vmtool/credcache.go`): `store <uuid>` is a cache of the store's
record for that domain UUID; `pending <uuid>` was written here and is not in the
store; a folder with no marker (local mode, or from before the store) counts as
pending with the UUID unknown. In store mode the first command that uses the VM
(`Service.PrepareVMCreds`) compares with the domain's current UUID
(`VMInfo.uuid`, API 2.3.0, from its daemon):

| Folder | Store | Result |
|---|---|---|
| pending, UUID matches (or unknown) | no record, or the same UUID | `PUT`; marker becomes `store`; one notice |
| pending, another UUID | any | stale: moved to `.machines/.stale/<folder>-<date>/`, never pushed, never deleted; one notice |
| `store` | record lost | `PUT` back; one notice |

After a playbook run (success or failure) the folder is pushed when it changed;
the record is fetched when the folder is missing, and once more after `ssh`
exits 255. A daemon that reports no UUID (older than 2.3.0) gets no sync and one
line saying to redeploy it.

### `fetchImage` and the relay

`fetchImage` (`POST /v1/hosts/{host}/images/{name}/fetch`, in `vmtool.yaml`,
API 2.3.0) has a hypervisor daemon download an image from the store instead of
receiving it from the client. The client first gets a ticket
(`createImageTicket`) and passes `{sha256, size, ticket}`; the daemon dials only
the `store_url` in its own `config.yml` (https, the system trust store, no
redirect, no proxy), sends `Authorization: Ticket`, and writes through the
receive path of `uploadImage`. It answers NDJSON `progress` lines, then one
`done` or `error` (reasons `store_unreachable`, `store_refused`, `mismatch`,
`other`). The ticket is in no log, error or event line. Daemons hold no store
token and no S3 key.

The client **relays** (streams the store's blob into the daemon's
`uploadImage`, no local file, one line saying so) when the daemon cannot fetch:
no `store_url` (409 `no store configured on <host>; ...`), a daemon that
predates the operation (404 with a plain-text body), or `store_unreachable`. An
expired or spent ticket (`store_refused`) and a digest or size `mismatch` are
not relayed: `create` fails.

### Backups and the key

`enc.key` (file mode 0600, mounted at `/etc/vmtool-store/enc.key`) encrypts
every credential column; the store cannot read what another key wrote, so a
lost or replaced key loses every stored credential. The design calls for a
daily `VACUUM INTO` snapshot to `backups/` in the bucket (14 kept) and a
`vmtool-store backup` command. **Neither is built**: `db.Snapshot`
(`internal/store/db`) exists and nothing calls it, and the command takes only
`-config`. Until then a backup is a copy of `data/store.db` taken with the
container stopped, kept with the key (see the `vmtool_store` role's README in
`ansible_labtwo`, and the root `README.md`).

## URL versioning

Paths are versioned: every route except the docs routes and `GET /version`
lives under `/v1`. Everything that acts on one hypervisor is host-scoped,
`/v1/hosts/{host}/{vms,networks,bridges,images,pools}[...]`; `/v1/hosts`,
`/v1/playbooks` and `/v1/metal[...]` are global. A `{host}` the server does
not serve is `404` with a message naming the hosts it does.

`GET /version` is public (no `Authorization` header needed), unversioned and a
reachability probe. It returns `api_version` (the spec's `info.version`, read
from the embedded spec at runtime), `build_version` (module version or VCS
revision) and `host` (the server's own name). `api_version` is informational:
clients do not compare versions, neither before an operation nor in
`vmtool hosts` / `GET /v1/hosts`.

A breaking change bumps the major version in `info.version` and the path
prefix (`/v2/...`).

## Hypervisor role and daemon primitives

A server whose `config.yml` has `role: hypervisor` is a remote daemon: it does
libvirt work only and holds no guest secrets. Every operation the spec marks
`x-vmtool-workstation-only: true` answers `403` with
`{"error":"<operationId> is served by the workstation, not by hypervisor <name>"}`.
The check runs once, in `internal/server` (`gateWorkstationOnly`), after the
bearer check and before the body is read, driven by that extension read from
the spec, so the set cannot drift. The marked operations are `createVM`,
`execVM`, `runPlaybook`, `resizeDisk` (the workflow form that also grows
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
| `CloneImage` | `cloneVolume` | New. `base_path` must be inside a pool. Optional `size_bytes` (2.2.0): a `logical` target gets an LV of max(image, size), converted to raw; dir pools ignore it. Optional `template` (2.4.0): the image name as asked, informational. When `target_pool` is a thin pool the clone is a thin snapshot of the template of the cached image (by file name and manifest SHA-256; built first if missing; 409 when the pool has no room); there `base_path` may also be a saved template's `/dev/<vg>/<lv>`. Returns `{path}`. |
| `ResizeVolume` | `resizeVolume` | New. `path` must be inside a pool. |
| `ImagePath` | `getImagePath` | New. 404 if no pool has it. On a hypervisor with thin pools, a name no pool has but a saved template has answers that template's `/dev/<vg>/<lv>` (2.4.0 behaviour, no new field). |
| `ListNetworks` / `ListBridges` | `listNetworks` / `listBridges` | |
| `ListImagesByPool` | `listImages` | `{pool: [image]}`. |
| `DeleteImage` | `deleteImage` | `pool` query, default `default`. |
| `ListPools` | `listPools` | `PoolInfo{name,path,active,type?,size_bytes?,used_bytes?,virtual_bytes?,data_percent?,metadata_percent?}`; `type` (2.2.0) is libvirt's pool type (`dir`, `logical`, …), or `thin` (2.4.0) for an LVM thin pool a hypervisor daemon lists itself. The usage fields (2.4.0) are omitted when unknown; libvirt pools fill size and used, thin pools all five. `GET` VM adds `disk_backing` (`file`/`block`) the same release, and `uuid` (the domain UUID) in 2.3.0. |
| `CreatePool` | `createPool` | |
| `ListTemplates` (`app.TemplateManager`) | `listTemplates` | New (2.4.0). Optional interface: a hypervisor daemon with thin pools, and the remote client for one. `TemplateInfo{name,origin,image?,sha256?,pool,lv,size_bytes,used_bytes,clones,stale,created?}`; `stale` is an image template whose (image, SHA-256) is not the daemon's current manifest entry. A host with no thin pools answers `[]`. |
| `PrepareTemplate` | `createTemplate` | New (2.4.0). `POST templates` `{image, pool?}` builds the image template ahead through the same path the first create into the pool uses (room check, building LV, import, tag, rename, per-(VG, template LV) lock); an existing one is returned, nothing written. Only `image` and `pool` are accepted in 2.4.0; other sources are added as optional fields later. 404 no such image on the host, 400 not a thin pool / not in the manifest / no thin pools, 409 no room. A workstation syncs the image to a listed host first, as `createVM` does. |
| `DeleteTemplate` | `deleteTemplate` | New (2.4.0). `DELETE templates/{name}?pool=`; `{name}` is the template's name or its LV name (`vmtpl-<12hex>`). Removes only a volume the daemon's report lists as a template; anything else is 404. A name matching several templates is 409 listing them. Clones are never touched. Returns the removed `TemplateInfo`. `templates prune` is the workstation workflow: it lists, then deletes each stale template by LV name and pool. |

| `LookupTemplate` / `ExportTemplateVolume` / `RecordTemplateSHA` (`app.TemplateVolumeExporter`); client `app.TemplateExporter` | `exportTemplate` | New (2.4.0). `POST templates/{name}/export?pool=`, `application/x-ndjson`, one `ExportTemplateEvent` per line: `progress` (stage `export` while the helper writes `.<name>.part`, stage `hash`), then one `done` (the manifest entry) or one `error`. `{name}` is a template's name or LV name (404 none, 409 several); the image takes the template's name. Same receive-path refusals as `uploadImage` (foreign 409, one at a time 409); a name the manifest lists is 409 unless it is the same template re-exported (recorded `sha256` equals the entry's and the file is in place: one `done`, no rewrite). The digest is written to the template's tags, so a later create of that image on the host clones the template: no upload, no rebuild. Served by `tunnelMux` (raw handler, `internal/server/export.go`). A workstation forwards to a listed host. |
| `OpenImageBlob` (`app.ImageBlobSource`) | `getImageBlob` | New (2.4.0). `GET images/{name}/blob`, `application/octet-stream` with `Content-Length` and `X-Image-SHA256`. Only a file the daemon's manifest lists: not listed 404 (409 if a foreign volume has the name), symlink / non-regular / size differs from the manifest 409, name must be a valid image name (400). Served by `tunnelMux` (`internal/server/export.go`). `vmtool images pull <host>/<name>` writes it to `image_dir` / `--to` (`.part`, size and digest checked, linked in, never overwrites). |
| `ImageManifest` (`app.ImageStore`) | `getImageManifest` | New. Optional interface, remote only. `{name, sha256?, size?, foreign, path?}`. |
| `UploadImage` (`app.ImageStore`) | `uploadImage` | New. `PUT images/{name}`, `application/octet-stream`, `X-Image-SHA256`, `Content-Length`; streamed. |

`Create` is not on `app.Manager`; the workstation's `app.Service.CreateVM`
composes `cloneVolume`, `resizeVolume`, `defineVM` and `startVM`.

Path inputs (`base_path`, `path`, `disk_path`, `extra_disks[].path`) must be
absolute and, after `filepath.Clean` and symlink resolution of the existing
prefix, strictly inside the path of a libvirt storage pool; otherwise `400`.
A pool of type `logical` is the exception: its volumes are `/dev/<vg>/<lv>`,
symlinks to `/dev/dm-N`, so there the cleaned path is accepted unresolved
when it is exactly `<pool path>/<one name>` and exists.
The cleaned path is what reaches libvirt. Names in bodies (`new_name`, volume
`name`, the VM name in `.../vms/{name}/definition`) must be plain names. The
check is point-in-time; it is defence in depth for a loopback, token-guarded
API, not a sandbox.

## Daemon address, unit and the own name

A daemon is one `vmtool server` with `role: hypervisor`, run as the unit
`vmtool-daemon.service` on the host's `port` in the workstation's
`config.yml` (`hosts.<h>.port`, default 9473; 0 or absent is the default,
otherwise 1 to 65535). `just deploy <host>` listens on `0.0.0.0:<port>`;
`vmtool config host <host>` prints `address ssh_user port`. The workstation's
loopback server from `just install` is a different unit, `vmtool.service`
on `127.0.0.1:9473`, and can sit beside a daemon on the same machine.
A machine whose own name is not in `hosts:` and is deployed with `just deploy
<own name>` gets port 9474 unless `VMTOOL_SELF_PORT` says otherwise, and the
entry is written afterwards.

From an API client's side, `{host}` is the same for every served name. A
workstation that lists its own name under `hosts:` serves it like any other
host, through that daemon: `GET /v1/hosts` shows it as a hypervisor row (not
`local`), `GET /v1/hosts/{own}/...` is proxied to the daemon, and the server
never opens its own libvirt for it. When the own name is not listed it is the
local host, served by local libvirt, opened only when a request names it. A
machine with neither a listing nor libvirt answers requests for its own
name with an error whose text starts `no daemon listed for <name> and no
local libvirt`; other hosts are unaffected. Only a workstation may list its
own name; a hypervisor's `config.yml` that does is refused at load.

Base images that `createVM` uploads come from the workstation's local libvirt
pools (only when local libvirt is in use) and then from `image_dir` in its
`config.yml` (a plain file `<image>`, then `<image>.qcow2`).

## Image cache

Hypervisors cache base images in their `vmtool` libvirt pool
(`vmtool.HypervisorPool`, made by `just prereqs <host>`); there is no
fallback to `default`, and a hypervisor without the pool answers 500
`no storage pool "vmtool" on <host> ... (run just prereqs <host>)`. Each daemon keeps
`image-manifest.yml` (name -> sha256, size) in its working directory and owns
a volume only if that manifest lists the name. `getImageManifest` reports an
entry, or `foreign: true` when a same-named volume exists (any pool, or a
file in the `vmtool` pool's directory) that the manifest does not list.
`uploadImage` refuses a foreign name with 409 before reading the body, writes
`.<name>.part`, verifies the SHA-256, deletes an old volume only if the
manifest lists it, and moves the file in with a hard link (never an
overwrite). One upload of a name runs at a time per daemon: a second PUT of
a name already uploading is 409 before its body is read. Otherwise a
`.<name>.part` left by an interrupted upload is deleted (a symlink as a
link; a directory or other non-file is 409 and stays) before the new one is
created `O_EXCL`; one another process creates meanwhile is 409. At start,
after binding its port, a hypervisor daemon deletes every regular file or
symlink named `.<name>.part` (with `<name>` a valid image name) in the
`vmtool` pool's directory and nothing else (no recursion, no other names or
pools); without libvirt or the pool it logs one line and serves anyway. A
workstation deletes nothing at start. It is the only route exempt from the 1 MiB body cap
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

An omitted or empty pool, in any request (`createVM`, `addDisk`,
`createVolume`, `cloneVolume`, `defineVM`, `deleteImage`), is the host's
default pool: `default` on a workstation, `vmtool` on a hypervisor. Surfaces
and `internal/app` never fill one in; the host that owns libvirt resolves
it. The workstation's libvirt `vmtool.Manager` maps empty to `default`; a
hypervisor's resolver (`internal/hosts`) wraps its local Manager so empty is
`vmtool`. So a VM created on a hypervisor with no `--pool` has its clone and
extra disk in the `vmtool` pool, next to the cached image. The workstation's
`config.yml` may set `default_pool` (local host) or `hosts.<h>.default_pool`;
the workstation's resolver then fills that pool in for new VM disks only
(clone target, `createVolume`, `addDisk`, `defineVM`'s pool) before any
request, so a daemon receives it as an explicit pool. Images keep the cache
pool.

## GET / pattern

PR1 records the exact std-http pattern oapi-codegen emits for the root after
the first generate. Update this line when known:

| Operation | Expected ServeMux pattern |
|---|---|
| `getDocs` | `GET /{$}` — Swagger UI (not a catch-all) |
| `getOpenAPISpec` | `GET /openapi.yaml` |
| `getOpenAPIJSON` | `GET /openapi.json` |
