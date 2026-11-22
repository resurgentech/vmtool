# spec/ — the API contract

vmtool is **spec-first**. `openapi/vmtool.yaml` is the source of record for every HTTP
route, payload, and status code. Code follows the spec; the spec does not follow
a handler.

## Layout

| Path | Contract |
|---|---|
| `openapi/vmtool.yaml` | Every HTTP endpoint `vmtool server` registers. oapi-codegen generates models, a stdlib `ServerInterface`, a strict wrapper, an embedded spec, and a test client (`internal/api`). |
| `openapi/store.yaml` | The image and credential store's HTTP API (`vmtool-store`, 1.4.0), served by `cmd/vmtool-store`, not by `vmtool server`. Its own generated package, `internal/storeapi`; handled as `vmtool.yaml` is (see [The store](#the-store)). |

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

## Errors (API 2.9.0)

Every error body is the `Error` schema: `error` (text, for people), and
`code`, a machine-readable reason. A client branches on `code`, never on the
text of `error`, which may change. `code` is optional in the schema because
an older server does not send it; this server sends it on every `Error` body.

| `code` | Status | Meaning |
|---|---|---|
| `bad_request` | 400 and other 4xx refusals of the request | the request is wrong |
| `unauthorized` | 401 | missing or invalid bearer token (the text is unchanged) |
| `workstation_only` | 403 | a hypervisor refusing an operation the workstation serves |
| `not_found` | 404 | no such host, VM, image or other object |
| `conflict` | 409 | the object is in a state that refuses the operation |
| `store_unavailable` | 503 | the store this operation needs did not answer or is not configured |
| `host_unreachable` | 502 | the workstation could not reach the configured host's daemon it forwarded to (connection refused, no route, TLS failure, a connection that broke) |
| `host_timeout` | 504 | the host's daemon did not answer in time; the operation may or may not have run |
| `bad_gateway` | 502 | a service behind the server failed or answered badly (a guest over ssh in `execVM`) |
| `internal` | 500 | anything else |

`502` and `504` are documented on every per-host operation a workstation
forwards to a host's daemon (`/v1/hosts/{host}/...`, except `openVMTunnel` and
`openVMConsole`, which the workstation does not forward, `uploadImage`, which
only a hypervisor serves, and `setVMComment`, which writes to the store).
They are produced by the strict handler's response error handler
(`internal/server/errors.go`): a handler returns the error it has no status of
its own for, and one function maps it. `internal/remote` marks a call that got
no answer as `app.KindHostUnreachable` or `app.KindHostTimeout`; a daemon that
answered with a failure is still `app.KindBadGateway` and reaches the caller
as before. The CLI runs `internal/app` directly and prints the error text.
`TestForwardedOperationsReportAHostThatIsDown` drives every forwarded
operation against a host that refuses and a host that does not answer.

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

**Base URL and `path_prefix` (API 1.3.0).** The store's base URL may carry a
path. With `path_prefix: /vmstore` in the store's `config.yaml` (empty by
default: serve at the root), every route below, `GET /version` included, is
served under the prefix, for example `https://proxy.example/vmstore/version`
and `https://proxy.example/vmstore/v1/whoami`, behind a proxy that passes the
full path on unchanged. One wrapper in front of the request log checks the
prefix and removes it from the path (decoded and escaped) before anything
else, so routing, auth, the public `GET /version`, the ticket check (which
still refuses an encoded path) and the log's route are as without a prefix.
Any other path, `/vmstore` alone, `/vmstorex/...` and an unclean path below
the prefix, is `404` with the store's error body, before auth: no token is
asked for, nothing changes and no redirect is sent. The value is one or more
`/segment` of `A-Z a-z 0-9 . _ ~ -`, with no empty or `.` segment, no `..` and
no trailing `/`; the store refuses to start with any other, naming
`path_prefix`. Clients (`internal/storeclient`, the daemon's image fetch from
`store_url`) keep the base URL's path and append the routes to it, so they
need only the address with the path.

**Auth.** Every route needs `Authorization: Bearer <token>` except `GET
/version`. The store holds a SHA-256 per named caller (`tokens.yaml`), never a
token, and logs requests by caller name; it logs no token, ticket, credential
or request body. One more scheme exists, `Authorization: Ticket <t>`, accepted
on exactly `GET /v1/images/{name}/blob`; on any other route it is 401, like a
bad token. **A store token opens every guest's credentials.** A **host
token** is a limited bearer token, one per host, kept as `{host: sha256}` in
`host_tokens.yaml` (0600; `host_tokens_file` in the store's config; a missing
file means none). It is accepted only on `GET /v1/whoami` (answers
`host/<host>`, kind `host`), `PUT /v1/hosts/{host}/inventory` and
`GET /v1/hosts/{host}/networks` (its own host's network memberships, see
[Networks](#networks-api-120)); on every
other route, image blob, credentials and images included, it is 401. A hash
may not appear in both token files. The log's caller for it is `host/<host>`.

| Group | Operations |
|---|---|
| Probe | `getVersion` (public); `whoami` (the caller's name; the client's mode check) |
| Images | `listImages`, `getImage`, `putImage`, `deleteImage`, `getImageBlob`, `createImageTicket` |
| VM credentials, keyed by (host, name) and the domain UUID | `getVMCredential`, `putVMCredential`, `deleteVMCredential` |
| Metal records, keyed by name | `listMachineCredentials`, `getMachineCredential`, `putMachineCredential`, `deleteMachineCredential` |
| Inventory, keyed by (host, libvirt UUID) | `putHostInventory` (host token for that host only; a user token or another host's token is 403), `getInventory` (`?host=`, `?gone=true`), `delistVM`, `deleteInventoryRow` (user tokens only) |
| Network registry, keyed by name | `createNetwork`, `listNetworks`, `getNetwork`, `deleteNetwork`, `putNetworkMember`, `deleteNetworkMember` (user tokens only); `getHostNetworks` (a host token for that host only; a user token may name any host) |

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

**Inventory.** A record of what each host daemon last said, not a live view:
no up/down state, and nothing is marked gone by age or silence; the store never
decides whether a host is up, and a sleeping host's VMs stay listed. A host's
`putHostInventory` body is its whole VM list (JSON, at most 1 MiB and 4096 VMs,
no origin and no comment: those are the VM's notes, written only through
`putVMNotes`, see [A VM's notes in the store](#a-vms-notes-in-the-store-api-260));
a listed VM is inserted or updated,
an omitted live row of that host goes gone. Reports are ordered by the store's
own clock: a report received before the last one applied is answered 200 and
ignored; `generated_at` is required and stored, and orders nothing, so a host
with a wrong clock is not silenced. A VM reported with `disks_unreadable: true`
(optional; its disks could not be read on the host) is updated in every other
field and keeps its last known disks; the next report without the flag clears
it. A gone row keeps its login for 7 days (`credentials_kept_until`); the
sweep, at store start and every 24 hours, removes the row and the login of the
same (host, name) only when its UUID matches. `delistVM` marks the live row
gone (a client calls it when a host answers "no such VM" for a VM it named);
`deleteInventoryRow` removes a row at once and leaves the credential record
alone (`vmtool delete` calls it together with the credential `DELETE`, so
login and row go at once). Hosts and VMs reach the store only through the
daemons' reports, and are corrected at the next report (daemon start, or a
change made through vmtool, plus one follow-up report when a new VM's address
appears) or on touch; a change made outside vmtool waits for one of those.

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

Stage names (API 2.8.0). In a create's `ProgressEvent`s and `timings`, the
host downloading from the store is stage `fetch`, this machine streaming the
store's bytes to the host is `relay`, and this machine sending its own copy is
`upload`. A fetch that falls back shows the stages that ran, each under its
own name. Only the workstation client changed; daemons need no redeploy.

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
lost or replaced key loses every stored credential.

The store backs itself up. A backup is a `VACUUM INTO` copy of the database,
uploaded to the bucket as `backups/store-<UTC yyyymmddThhmmssZ>.db` every 24
hours, and once 5 minutes after start when the newest backup is older than 24
hours or there is none; one runs at a time, and after each the 14 newest
`backups/store-*.db` are kept and the rest deleted (only keys of exactly that
shape are ever deleted). `docker compose exec vmtool-store vmtool-store backup`
takes one on demand through its own SQLite connection (a read transaction,
safe beside the running store), prints one line with the object name and exits
0, else non-zero. It adds no HTTP route and no spec change. A failed backup is
logged with the operation, object name, error class and duration, never a
credential, token, key, request body or S3 key.

**The encryption key is not in the backup.** The copy holds the credential
columns as AES-256-GCM ciphertext; the key is kept separately (OpenBao and
`enc.key`), and a backup restored without the key it was written under is
unreadable. Image blobs are not in the backup; they stay in the bucket.

Restore: stop the container; fetch the chosen object (`mc cp` or the console)
to `data/store.db` with mode 0600 in the 0700 directory; use the same
`enc.key`; start; check `GET /version` and a credential read. vmtool adds no
restore command. See the `vmtool_store` role's README in `ansible_labtwo` and
the root `README.md`.

### Networks (API 1.2.0)

The registry of networks that span hosts (locked decision "VM networks";
tranche 17). It records what hosts cannot agree on alone: each network's name,
VNI, subnet, domain and home, and its members. `store.yaml` went from 1.1.0 to
1.2.0 for it (1.1.0 had been left unreleased by the inventory work; a minor
bump costs nothing if it was).

- **Tables** (migration 6): `networks` (`name` primary key, `vni` unique,
  `subnet` unique, `domain`, `home`, `created`) and `network_members`
  (`network`, `host`, `lan_ip`, `lan_dev`, `joined`; primary key
  (network, host)). Nothing is secret and nothing records whether a host is up.
- **User-token routes.** `createNetwork` (`POST /v1/networks`: `name`, `home`,
  optional `domain`, `subnet`, `vni`), `listNetworks`, `getNetwork`,
  `deleteNetwork` (409 while the network has members), `putNetworkMember` and
  `deleteNetworkMember` (`/v1/networks/{name}/members/{host}`). A host token
  gets 401 on every one of them.
- **Allocation.** With no `vni` the store takes the lowest free one from 10000
  (the first bridge is `vmt-002710`; a bridge is `vmt-` plus the VNI as six
  lowercase hex digits); with no `subnet` it takes the lowest free /24 of
  10.77.0.0/16. A given `vni` (1 to 16777215) or `subnet` (an IPv4 /24 written
  as its network address) is used if free and is a 409 if not. The check, the
  allocation and the insert are one transaction on the store's one database
  connection, so concurrent creates never share a VNI or a subnet. A deleted
  network's VNI and subnet are handed out again. `domain` defaults to
  `<name>.test`; `name` follows the VM name rule, at most 63 characters, never
  `default`. The store does not check a subnet against a host's own libvirt
  networks or routes: the host does when it defines the network.
- **`getHostNetworks`** (`GET /v1/hosts/{host}/networks`) is the host token's
  one read besides its report, for its own `{host}` only (another host's name
  is 403; a user token may name any host). It lists only the networks the host
  is a member of or the home of, each with `vni`, `bridge`, `subnet`, `domain`,
  `home`, the host's own `lan_ip` and `lan_dev` (`member`, absent for a home
  that is not a member) and `peers`: the other members' host names and LAN
  addresses, nothing else. A host with none gets an empty list.
- **The home is not a member by being the home.** A member row is the host's
  LAN address and interface for its VXLAN link. `createNetwork` adds none, so
  a one-host network has no members and delete works. The first `vmtool net
  join` of a network adds the home's row too (found by the home's daemon,
  `getUnderlay`), so every host's peers are the other rows; the registry
  refuses to delete a network that still has members.
- **Reservations** (migration 7, `network_reservations`: `network`, `mac`,
  `ip`, `host`, `vm`, `created`; primary key (network, mac), unique (network,
  ip)). User-token routes `listNetworkReservations` (`GET
  /v1/networks/{name}/reservations`, in address order, `404` for an unknown
  network), `putNetworkReservation` (`PUT …/reservations/{mac}`: `ip`, `host`,
  `vm`; idempotent per MAC; `409` when another MAC holds the address; `404`
  for an unknown network) and `deleteNetworkReservation` (`DELETE
  …/reservations/{mac}`, `404` when none). The store keeps a copy of what the
  home's daemon made, written by the client that made or removed the
  reservation (never by a daemon, whose host token has none of these routes), so
  a client can answer without reaching the home; the home's own libvirt DHCP
  configuration stays the truth. A network that is registered has its
  reservations recorded, a network that is not (made without a store) has none.
  Deleting a network deletes its records. Version stays 1.2.0 (deployed
  nowhere).
  **Orphans** (migration 8: `uuid`, `orphaned_at`). `putNetworkReservation`
  takes an optional `uuid` (the VM's libvirt UUID; a put without one keeps the
  recorded one, and a put clears `orphaned_at`); records and lists carry `uuid`
  and `orphaned_at` when set. User-token routes `listOrphanedNetworkReservations`
  (`GET …/reservations/orphaned`, address order, `404` for an unknown network)
  and `markNetworkReservationOrphaned` (`POST …/reservations/{mac}/orphan`, no
  body, no content type needed; keeps the first time; `404` when none). A
  reservation is orphaned only on a positive fact the store holds: a client
  marks it (its `delete` or a failed undo could not remove it on the home), or
  the inventory sweep marks every reservation of the UUID of the gone row it
  removes after 7 days (`orphaned_reservations` in the sweep's log line). A
  record with no UUID (made before migration 8 with no live row of the same
  host and name) is never orphaned by the sweep; a host's silence marks
  nothing. The store removes nothing and reaches no host: a client that
  reaches the home removes the orphans on the home, then deletes their
  records, whenever it makes, lists, removes or shows a reservation on the
  network (`net attach`, `net detach`, `create --network`, `delete`, `net
  show`); a silent home changes nothing. Host tokens have neither route.
- **Store mode and local mode.** `vmtool net create HOST NAME` in store mode
  has the registry allocate the VNI and subnet (home = HOST) and then has the
  host define the network on bridge `vmt-<VNI>`; if the host refuses, the
  registry entry is deleted again. In local mode (no store, an unreachable
  one, or a host token) nothing is registered and the host numbers its own
  bridge from 1 to 9999 and picks its own subnet. In store mode the host is
  handed both the VNI and the subnet (`vni` and `subnet` of the daemon's
  `createNetwork`), so the bridge is `vmt-%06x` of the VNI (10000 and up) and
  the subnet is the allocated /24. A bridge of that number already on the host
  is its 409, an overlapping subnet its 400; the entry is removed in both
  cases and the message names the cause. **An older daemon** ignores the
  unknown `vni` field and numbers the bridge itself: the app compares the
  bridge it gets back with `vmt-%06x` of the VNI, deletes the network the
  daemon just made and the registry entry, and fails with "host H runs a
  vmtool that cannot take a network number; redeploy it (just deploy H)". The
  daemon's own route stays a primitive: it never calls the store. Members are
  joined and left by `vmtool net join` and `net leave` (see "Networks that span
  hosts" under API 2.7.0).
- **Numbers.** Without a store the host numbers bridges from 1 to 9999; the
  registry gives out 10000 and up, so the ranges never meet (`vmt-002711` is
  10001). A number and a subnet freed by a delete are handed out again.

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
workstation), `getInventory` (the store's inventory, read through the
workstation's store; API 2.6.0) and every `/v1/metal` operation. `openVMTunnel` is not marked: a
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
| `Info` | `getVM` | `VMInfo` carries every field of `vmtool.VMInfo` (no origin and no comment: those are in the store, see [A VM's notes in the store](#a-vms-notes-in-the-store-api-260)). |
| `List` | `listVMs` | The UUID and firmware are filled; the other XML fields stay empty. |
| `Delete(name, noclone)` | `deleteVMDefinition` | Raw. `deleteVM` is the workstation workflow: it also forgets `.machines/<host>/<vm>/` credentials. |
| `MigrateDisk` | `migrateDisk` | Method, then `Info`. |
| `AddDisk` | `addDisk` | Method, then `Info`; result carries `path` and `target` (`DiskDevice`). On a thin pool the volume is the thin LV `<vm>-N`; a pool with no room is 409 with the numbers (see [Thin pools and templates](#thin-pools-and-templates-api-240)). |
| `ResizeDisk` | `resizeVMVolume` | New. libvirt, and a thin boot volume (extend, then `BlockResize` when running); a thick `logical` disk is refused. A thin pool with no room is 409 with the numbers. `resizeDisk` is the workstation workflow that also grows the guest. |
| `WaitForIP` | `waitVMIP` | New. `timeout_seconds` 1..3600, default 120. |
| `Define` | `defineVM` | New. `PUT vms/{name}/definition`; no `ssh_user`/`ssh_pass`. The domain carries nothing of vmtool's own (no `<metadata>`). |
| `CreateVolume` | `createVolume` | New. Returns `{path}`. |
| `CloneImage` | `cloneVolume` | New. `base_path` must be inside a pool. Optional `size_bytes` (2.2.0): a `logical` target gets an LV of max(image, size), converted to raw; dir pools ignore it. Optional `template` (2.4.0): the image name as asked, informational. When `target_pool` is a thin pool the clone is a thin snapshot of the template of the cached image (by file name and manifest SHA-256; built first if missing; 409 when the pool has no room); there `base_path` may also be a saved template's `/dev/<vg>/<lv>`. Returns `{path}`. |
| `ResizeVolume` | `resizeVolume` | New. `path` must be inside a pool. |
| `ImagePath` | `getImagePath` | New. 404 if no pool has it. On a hypervisor with thin pools, a name no pool has but a saved template has answers that template's `/dev/<vg>/<lv>` (2.4.0 behaviour, no new field). |
| `ListNetworks` / `ListBridges` | `listNetworks` / `listBridges` | |
| `ListImagesByPool` | `listImages` | `{pool: [image]}`. |
| `DeleteImage` | `deleteImage` | `pool` query, default `default`. |
| `ListPools` | `listPools` | `PoolInfo{name,path,active,type?,size_bytes?,used_bytes?,virtual_bytes?,data_percent?,metadata_percent?}`; `type` (2.2.0) is libvirt's pool type (`dir`, `logical`, …), or `thin` (2.4.0) for an LVM thin pool a hypervisor daemon lists itself. The usage fields (2.4.0) are omitted when unknown; libvirt pools fill size and used, thin pools all five. `GET` VM adds `disk_backing` (`file`/`block`) the same release, and `uuid` (the domain UUID) in 2.3.0. |
| `CreatePool` | `createPool` | |
| `ListTemplates` (`app.TemplateManager`) | `listTemplates` | New (2.4.0). Optional interface: a hypervisor daemon with thin pools, and the remote client for one. `TemplateInfo{name,origin,image?,sha256?,pool,lv,size_bytes,used_bytes,clones,stale,created?,export?}` (`export` 2.5.0: `{bytes, sha256?, expires?}` of the template's finished export volume); `stale` is an image template whose (image, SHA-256) is not the daemon's current manifest entry. A host with no thin pools answers `[]`. `extra_disks` is set only by a save. |
| `PrepareTemplate` | `createTemplate` | New (2.4.0). `POST templates` `{image, pool?}` builds the image template ahead through the same path the first create into the pool uses (room check, building LV, import, tag, rename, per-(VG, template LV) lock); an existing one is returned, nothing written. `from_vm`, `name` and `replace` select the save form (next row). 404 no such image on the host, 400 not a thin pool / not in the manifest / no thin pools, 409 no room. A workstation syncs the image to a listed host first, as `createVM` does. |
| `DeleteTemplate` | `deleteTemplate` | New (2.4.0). `DELETE templates/{name}?pool=`; `{name}` is the template's name or its LV name (`vmtpl-<12hex>`). Removes only a volume the daemon's report lists as a template; anything else is 404. A name matching several templates is 409 listing them. Clones are never touched. Returns the removed `TemplateInfo`. `templates prune` is the workstation workflow: it lists, then deletes each stale template by LV name and pool. |
| `SaveTemplate` (`app.TemplateSaver`) | `createTemplate` | New (2.4.0), the same route as `PrepareTemplate` with `{from_vm, name, replace?}` instead of `{image, pool}`: exactly one of `image` and `from_vm` (both or neither 400; with `from_vm`, `name` is required and `pool` refused; with `image`, `name` and `replace` refused). The host snapshots the shut-off VM's boot LV (a `vmtool_vol` thin LV) into a new saved template in the VM's own pool, in one helper write, tagged `vmtool_template`, `vmtool_origin=saved`, `vmtool_name=<name>`; nothing is booted, mounted or read. VM not shut off 409; disk not on a thin pool or bad name 400; VM unknown 404; name used by a template or a cached image 409 (`replace` replaces only a saved template, new one made first); no room 409. The 201 `TemplateInfo` carries `extra_disks` (the VM's targets other than `vda`, not part of the template). No image is synced. |
| `LookupTemplate` / `ExportTemplateVolume` / `OpenExport` / `RecordTemplateSHA` (`app.TemplateVolumeExporter`); client `app.TemplateExporter` | `exportTemplate` | 2.4.0, export volumes 2.5.0. `POST templates/{name}/export?pool=&hash=`, `application/x-ndjson`, one `ExportTemplateEvent` per line: `progress` (stage `export` from the converter's percent of the template's size, stage `hash` while the volume is read for its digest), then one `done` or one `error`. `{name}` is a template's name or LV name (404 none, 409 several). The template is converted into a temporary thick `vmexp-<12 hex>` volume in the array's volume group, kept 24 hours; nothing goes to the image cache (no file, no `.part`, no manifest entry). `done.image` carries `name` (the template's) and `size` (the qcow2's byte count); `sha256` only when `hash=true` (one read of the volume unless its digest is already tagged, then the tags). No room on the array is a plain 409 before the stream starts. Same name conflicts as before (foreign volume 409, one at a time 409; a manifest entry that is not this template's 409; an image template only as the cached image it came from: one `done` with the manifest entry). Served by `tunnelMux` (raw handler, `internal/server/export.go`). A workstation forwards to a listed host. |
| `OpenImageBlob` (`app.ImageBlobSource`) | `getImageBlob` | 2.4.0, export volumes 2.5.0. `GET images/{name}/blob`, `application/octet-stream`. A manifest entry is served as before (`Content-Length`, `X-Image-SHA256`; not listed and no export 404, 409 if a foreign volume has the name, symlink / non-regular / size differs 409, bad name 400). Otherwise the template export volume of that name is served through `OpenExport` and hashed while sent: digest tagged, `Content-Length` and `X-Image-SHA256`; not yet, `X-Image-Size`, a declared `X-Image-SHA256` trailer after the last byte, chunked body; the tag is recorded only after the whole body was written, a short send or a client that left records nothing. The 404 names `templates export`. `forwardBlob` relays header or trailer unchanged. Served by `tunnelMux` (`internal/server/export.go`). `vmtool images pull <host>/<name>` writes it to `image_dir` / `--to` (`.part`, size and digest checked, a trailer digest compared after the last byte, nothing kept on a mismatch or a missing trailer, linked in, never overwrites). |
| `ImageManifest` (`app.ImageStore`) | `getImageManifest` | New. Optional interface, remote only. `{name, sha256?, size?, foreign, path?}`. |
| `UploadImage` (`app.ImageStore`) | `uploadImage` | New. `PUT images/{name}`, `application/octet-stream`, `X-Image-SHA256`, `Content-Length`; streamed. |

`Create` is not on `app.Manager`; the workstation's `app.Service.CreateVM`
composes `cloneVolume`, `resizeVolume`, `defineVM` and `startVM`.

Path inputs (`base_path`, `path`, `disk_path`, `extra_disks[].path`) must be
absolute and, after `filepath.Clean` and symlink resolution of the existing
prefix, strictly inside the path of a libvirt storage pool; otherwise `400`.
A pool of type `logical` is the exception: its volumes are `/dev/<vg>/<lv>`,
symlinks to `/dev/dm-N`, so there the cleaned path is accepted unresolved
when it is exactly `<pool path>/<one name>` and exists. A thin volume is the
other exception: exactly `/dev/<vg>/<lv>` (no `..`, no `/dev/mapper`, no
symlink resolution) for a thin LV the daemon's helper report lists as a
`vmtool_vol` of a vmtool thin pool; a template is accepted only as
`cloneVolume`'s `base_path`. (In a volume group that also holds a `logical`
pool, as on the hosts host setup builds, the logical rule above accepts any LV
path of it too; there the daemon's own `defineVM` and `resizeVolume` refuse
template and building LVs, and a clone's base must be a template.)
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

## Thin pools and templates (API 2.4.0)

One additive bump, 2.3.0 to 2.4.0, carries everything below; no existing
route, field or status changed meaning. User-facing description, setup and the
rollout order are in the root `README.md` ("Thin pools"); this section is the
contract. A thick `logical` pool and the VMs on it are not affected by any of
it, and a template belongs to one host's daemon.

**A thin pool in the API.** A hypervisor daemon lists each LVM thin pool it
manages (an LV tagged `vmtool_thinpool`) as a `PoolInfo` named after the
thin-pool LV (default `nvme-thin`), `type: thin`, `path: /dev/<vg>`, beside
libvirt's pools; a name that clashes with a libvirt pool is left out. `PoolInfo`
gains optional `size_bytes`, `used_bytes`, `virtual_bytes` (the sum of the sizes
of every thin volume: what the pool has promised), `data_percent` and
`metadata_percent`; an absent field is unknown, not 0. libvirt pools fill size
and used when active. A pool name is used anywhere a pool is (`pool` in
`createVM`, `addDisk`, `cloneVolume`...), including through `default_pool`. A
daemon older than 2.4.0 lists no thin pool and answers 404 for the new routes.
LVM is reached by the daemon only through one root helper, not libvirt (root
README, "The root helper"); the helper's tags are `vmtool_vol` (a VM's disk),
`vmtool_template`, `vmtool_building`, `vmtool_name`, `vmtool_sha256` and
`vmtool_origin`, and nothing without them is touched.

**Routes** (all in `openapi/vmtool.yaml`, rows in the table above):

| Route | Operation | Use |
|---|---|---|
| `GET /v1/hosts/{host}/templates` | `listTemplates` | every template of every thin pool; `[]` without thin pools |
| `POST /v1/hosts/{host}/templates` | `createTemplate` | `{image, pool?}` builds an image template ahead; `{from_vm, name, replace?}` saves a shut-off VM as a saved template; 201 `TemplateInfo` |
| `DELETE /v1/hosts/{host}/templates/{name}?pool=` | `deleteTemplate` | removes one template; 404 for anything that is not a template, 409 listing the candidates when the name is ambiguous |
| `POST /v1/hosts/{host}/templates/{name}/export?pool=&hash=` | `exportTemplate` | NDJSON, below |
| `GET /v1/hosts/{host}/images/{name}/blob` | `getImageBlob` | the bytes of an image the daemon created or of a template's export volume, below |
| `POST /v1/hosts/{host}/volumes/clone` | `cloneVolume` | gains `template` (the image name as asked, informational) and, for a thin target pool, `size_bytes` |

`{name}` of a template is its name (`vmtool_name`) or its LV name
(`vmtpl-<12 hex>`). An image template is named after its image and keyed by
(image, SHA-256); a saved template carries a random id in its LV name. Pruning
stale templates, `images pull host/<name>` and the `templates export --push`
stream to the store have no route of their own: the client composes them from
the routes above and the store's (`PullHostImage`, `PruneTemplates` and
`ExportTemplateToStore` in `internal/app`).

**No room is 409.** Every operation that adds or grows a thin volume is refused
before any LVM call, with the numbers in the message, when the pool's data or
metadata use is at or above 80 %, or when the sum of every thin volume's size
plus the new one would exceed 4 times the pool's size (exactly 4x passes). The
routes that declare it: `cloneVolume` and `createVM` (a clone, a template
build), `createTemplate` (both forms), `addDisk`, `resizeVMVolume` and
`resizeDisk`. A client maps a 409 from `addDisk` and `resizeVMVolume` to
`vmtool.ErrThinNoRoom`. On a resize the room check counts the growth, not the
new size.

**`exportTemplate` NDJSON.** `200 application/x-ndjson`, one
`ExportTemplateEvent` per line, no trailing text. The refusals that come before
any line (400, 403, 404, 409) are ordinary JSON errors. A run is `progress`
lines, then exactly one `done` or `error`:

```
{"event":"progress","stage":"export","received":0,"total":10737418240}
{"event":"progress","stage":"export","received":5368709120,"total":10737418240}
{"event":"progress","stage":"hash","received":2147483648,"total":2147483648}
{"event":"done","image":{"name":"t1-tpl","sha256":"<64 hex>","size":2147483648,"foreign":false}}
```

`export` is the converter's percent of the template's size (`total`, an upper
bound of the qcow2 file); `hash` counts the bytes read for the digest and
appears only with `hash=true` and an unknown digest. `done.image.name` is the
template's name (what `images pull` asks for) and `size` the qcow2's byte
count; `sha256` is there only with `hash=true`. An `error` line says what
failed.

**`getImageBlob`.** Streams an image for `vmtool images pull host/<name>`,
`application/octet-stream`. A name the daemon's manifest lists is served as
before, with `Content-Length` and `X-Image-SHA256` (400 bad name, 404 the file
is missing, 409 a foreign volume has the name, or the file is a symlink or not
a regular file of the listed size, opened without following a symlink). A name
it does not list is looked up among the template export volumes (2.5.0) and
served from the volume, hashed while sent: with the digest already tagged the
answer is as above; without it the answer is chunked with `X-Image-Size`, a
declared `Trailer: X-Image-SHA256`, and the digest after the last byte, which
the client compares with what it wrote. The daemon tags the volume and its
template only after the whole body was written. 404 names `templates export`.
The body is never logged. A workstation daemon forwards both this route
(header or trailer relayed unchanged) and `exportTemplate` to a listed host and
answers 403 for its own unlisted name.

**Export volumes (2.5.0).** What changed for a caller of `exportTemplate`:
nothing is written to the image cache any more, so `listImages` and `getImage`
never show an export and `deleteImage` does not apply to one; the export is a
thick LV `vmexp-<12 hex>` in the array's volume group, tagged `vmtool_export`,
one per template (reused; a replaced template gets a fresh one), removed 24
hours after it was made (checked at daemon start and hourly), and with its
template (`deleteTemplate`, which `templates prune` uses, removes the
template's export too), and `TemplateInfo`
carries an optional `export` (size, whether the digest is known, expiry).
*Room rule:* the volume's size is `qemu-img measure` "fully allocated" for the
template, rounded to the extent; the call is refused with 409, before the
stream starts and before anything is written, when the group's free space
outside the thin pool, less that size, would fall below 10 % of the thin
pool's size: `<host>: export of <t> needs X GB in volume group <vg>; Y GB is
free outside the thin pool after keeping R GB for it to grow; nothing was
written`. There is no fallback to the boot drive. A VM name starting `vmexp-`
is refused on every pool. `virsh vol-list nvme` lists these volumes: they are
vmtool's. An old client pulling an export whose digest is not yet known gets
"did not send a SHA-256 digest".

## Streamed create (API 2.6.0)

`createVM` (`POST /v1/hosts/{host}/vms`) is a synchronous JSON call, as it
always was, and streams when asked: a request whose `Accept` header names
`application/x-ndjson` gets progress as the workflow emits it. Without that
value the answers are unchanged (201 `VMInfo`, 400/404/409, 500 with the VM in
the body). A wildcard (`*/*`) or `application/x-ndjson;q=0` does not ask.

- **Route.** The same route and body, still in the spec and served by the
  strict handler: `tunnelMux.CreateVM` (`internal/server/createstream.go`) only
  marks a request that asks for the stream and hands every request to the
  strict adapter, so the exclusion register stays empty. A hypervisor answers
  403 before the body is read, as before.
- **Refusals before any work** (a bad request, a name that is taken, an unknown
  host, an image that is not found) are the same JSON statuses as without the
  header. The 200 starts with the first progress event.
- **Lines.** `200 application/x-ndjson`, one `CreateVMEvent` per line, each
  flushed as written, `event` being `progress`, `done` or `error`:

```
{"event":"progress","progress":{"stage":"clone","status":"start","detail":"cloning image","elapsed_ms":4}}
{"event":"progress","progress":{"stage":"clone","status":"done","detail":"cloned → web.qcow2","elapsed_ms":1630}}
{"event":"done","vm":{"name":"web","state":"running","ip":"192.0.2.10",...},"timings":{"stages":[{"stage":"clone","ms":1626},...],"total_ms":41200}}
{"event":"error","error":"...got no address within 120s...","kind":"internal","vm":{...},"timings":{"stages":[...],"total_ms":125000}}
```

  `progress` carries a `ProgressEvent` (`stage`, `status`, `detail`, `sent` and
  `total` for an upload, `output` on a failed or finished step, and
  `elapsed_ms`). A run ends with exactly one `done` (the `VMInfo` and the
  `timings`) or one `error` (`error`, `kind` of `invalid`, `not_found`,
  `conflict` or `internal`, `output` when a tool's output was captured, the
  `vm` when the domain exists, and the `timings` so far). The status is 200
  either way: read the last line. A stream with no `done` and no `error` was
  cut off.
- **Timings** are milliseconds on the workstation's clock, from the start of
  the workflow: `elapsed_ms` stamps each event, `timings.stages` has the
  stages that ran with the time from each one's first event to its last, and
  `total_ms` is the last stamp. They are the numbers the CLI's closing
  `timings:` line prints (`app.CreateTimings`).
- **Output.** `output` (on a progress line or on `error`) can hold a
  playbook's output, so it may be sensitive; it is never logged.
- **Cancel.** A dropped connection ends the call as it does for the JSON
  response (the request's context). No event line is logged, like no request
  body.

```python
import json, os
import requests

BASE = "https://127.0.0.1:9473"
headers = {
    "Authorization": "Bearer " + os.environ["VMTOOL_TOKEN"],
    "Accept": "application/x-ndjson",
}
body = {"name": "vm1", "image": "img1"}
with requests.post(f"{BASE}/v1/hosts/hv1/vms", headers=headers, json=body,
                   stream=True, timeout=(10, 600), verify=os.environ.get("VMTOOL_CA", True)) as r:
    if r.status_code != 200:            # a refusal before any work: plain JSON
        raise SystemExit(f"{r.status_code}: {r.json()['error']}")
    for line in r.iter_lines():         # one JSON object per line, as it is written
        if not line:
            continue
        ev = json.loads(line)
        if ev["event"] == "progress":
            p = ev["progress"]
            print(f"{p['elapsed_ms']:>7} ms  {p['stage']} {p['status']}  {p.get('detail', '')}")
        elif ev["event"] == "done":
            print("created", ev["vm"]["name"], ev["vm"]["ip"], ev["timings"]["total_ms"], "ms")
        else:
            raise SystemExit(f"{ev['kind']}: {ev['error']}")
```

## Named networks on one host (API 2.7.0)

`POST /v1/hosts/{host}/networks` (`createNetwork`), `GET` and `DELETE
/v1/hosts/{host}/networks/{name}` (`getNetwork`, `deleteNetwork`). They need no
store and no ssh: the host's own libvirt does the work. `GET
/v1/hosts/{host}/networks` (`listNetworks`) is unchanged, a list of names; the
CLI's `vmtool networks` marks vmtool's by asking `getNetwork` for each name.

- **What a vmtool network is.** A libvirt network whose bridge is `vmt-` plus
  six lowercase hex digits; those digits are its VNI (`NetworkInfo.vni`, set
  when `vmtool` is true). vmtool writes no annotation and no `<metadata>` into
  the definition, so a network is vmtool's by its bridge name and by nothing
  else. A network without such a bridge, `default` included, is never touched
  by delete.
- **Create** makes the home form: `<forward mode='nat'/>`, `<bridge
  name='vmt-NNNNNN' stp='off'/>`, `<mtu size='1450'/>`, `<domain
  name='<domain>' localOnly='yes'/>`, an `<ip>` of .1 of the /24 with DHCP over
  .100 to .200, set to autostart and started. `name` follows the VM name rule
  (letters, digits, `.`, `_`, `-`, at most 63) and is never `default`; `domain`
  defaults to `<name>.test`; `subnet` is an IPv4 /24 written as its network
  address, or omitted for the first /24 of 10.77.0.0/16 that no libvirt network
  and no route on the host uses. `vni` (1 to 16777215, optional) is the network number: the
  bridge becomes `vmt-%06x` of it, and a bridge of that number already on the
  host is `409`; the store's registry allocates it from 10000 up, and the
  daemon never calls the store itself. Omitted, the bridge number is the lowest
  free one from 1 (so it never collides with a registry number). A daemon that
  predates the field ignores it. Answers: `201` with the `NetworkInfo`; `400` for a bad
  name, subnet or domain and for a subnet that overlaps a libvirt network or a
  route (the message names it); `409` for a name a libvirt network already has
  and for no free /24; `404` for an unknown host.
- **Delete** answers `204`; `409` for a network that is not vmtool's ("not made
  by vmtool") and for one with VMs attached (the message names them); `404` for
  a network the host does not have.
- **VMs are not changed by these three.** A VM keeps its own NAT card first;
  cards are the next section.

### A host that runs no VMs (API 2.7.0, same version)

`HostInfo.network_home` (`GET /v1/hosts`) is true for a host whose `config.yml`
entry says `vms: false` (a network home); it is omitted otherwise. `createVM`
on such a host is 409 with a plain message, before anything reaches the host.
No other operation changes: the host is a full daemon host (its inventory
report is an empty list; networks and links work).

### VMs on a vmtool network (API 2.7.0, same version)

A VM gets an extra card on a vmtool network, with a fixed address and a name.
The fixed address is the network's DHCP reservation for the card's MAC
(`<host mac ip name>` with `<lease expiry='0'/>`, added with `virNetworkUpdate`
live and in the config), which is libvirt's own configuration; vmtool writes
nothing of its own into the network or the VM, and the VM's definition gets the
card and nothing else.

- **Primitives on the daemon**, each acting on one host's libvirt:
  `POST /v1/hosts/{host}/networks/{name}/reservations` (`createReservation`:
  `vm`, optional `mac` and `ip`), `DELETE …/reservations/{mac}`
  (`deleteReservation`), `POST /v1/hosts/{host}/vms/{name}/interfaces`
  (`attachInterface`: `network`, `mac`) and `DELETE …/interfaces/{mac}`
  (`detachInterface`). Only a vmtool network takes them (409 otherwise); a
  detach never removes a card that is not on a vmtool network, so the VM's own
  NAT card is safe. An omitted `mac` is `52:54:00` plus three random bytes that
  no VM on the host and no reservation uses; an omitted `ip` is the lowest free
  address of the DHCP range (.100 to .200) that no reservation and no lease
  holds. An `ip` given is `400` outside the subnet (or the network or broadcast
  address) and `409` when it is the gateway or already held; a `mac` or a VM
  name the network already has is `409`. The reservation carries the VM's name
  (`_` becomes `-`), so `vm` and `vm.<domain>` resolve on the network from the
  first boot.
- **The workflow is `internal/app`'s**, not a route: `vmtool net attach host/vm
  NAME [--ip IP]` reserves, then attaches (a failed card removes the
  reservation); `net detach` removes the card, then the reservation (a failed
  reservation removal puts the card back); `create`'s `networks` (`[{network,
  ip?}]`, the CLI's repeatable `--network NAME[=IP]`, the TUI's Networks field)
  checks every network and address before anything is created, then attaches
  each between define and start (a failure takes back the cards already made
  and leaves the VM defined, not started); `vmtool delete` removes the VM's
  reservations after the VM. REST clients order the attach primitives
  themselves: `createReservation`, then `attachInterface` with its MAC (the
  one `createReservation` answered, or the one the client gave), and on the way
  out `detachInterface`, then `deleteReservation`. A failed second step is
  the client's to undo by calling the first one's opposite. No route does the
  ordering for it.
- **`VMInfo.networks`** lists each card on a vmtool network as `{network, mac,
  ip}`; `VMInfo.ip` stays the first card's address (the VM's own NAT card),
  whatever a later card's lease says. `NetworkInfo.reservations` lists a
  network's DHCP host entries. The inventory report and the store's
  `InventoryVM.networks` carry the same `{network, mac, ip}` (store migration
  5; store.yaml stays 1.1.0 while nothing is deployed).

### Networks that span hosts (API 2.7.0, same version)

A network's *home* (the host whose libvirt network does the NAT, DHCP and DNS)
keeps its full definition; another host is a *member* of it: it defines the
same name with the same bridge `vmt-NNNNNN` and the MTU and nothing else (no
`<forward>`, no `<ip>`, no `<domain>`, no `<metadata>`), and a VXLAN device
`vx<VNI>` (UDP 4789, bound to the host's LAN address, mastered to the bridge)
floods to every other host's LAN address. The overlay is neither authenticated
nor encrypted. The registry (store.yaml, "Networks") holds the members and
their LAN addresses, so joining needs the store.

- **Daemon primitives** (none calls the store; the client app orders them):
  - `createNetwork` with `member: true` makes the member form: `vni` is
    required, `subnet` and `domain` are `400`, a name or a bridge number the
    host already has is `409`. A daemon that predates `member` would make a
    home-form network; the client compares what comes back.
  - `PUT /v1/hosts/{host}/networks/{name}/peers` (`setNetworkPeers`: `lan_ip`,
    `lan_dev`, `peers`) makes the host's VXLAN device flood to exactly those
    addresses, through the root helper `vmtool-net` (`NetHelper.EnsureLink`: it
    reads the helper's report and links only when the device differs, because
    the helper re-makes a device whose settings differ, which drops the
    network's cross-host traffic for a moment; one that matches is kept, Z1).
    The helper's `report` lists the VXLAN devices' names without `-d`, asks
    each `vx<N>` on its own (`ip -j -d link show dev vx<N>`; a device gone
    in between is skipped) and adds `bridge -j fdb show`, whose entries name
    their device in `ifname`; it never runs ip's detailed dump, which
    iproute2 6.1 crashes on whenever a VXLAN device exists (Z3).
    `404` for an unknown network, `409` for one that is not vmtool's, `400` for an argument the helper would refuse; `DELETE` of
    the same path (`deleteNetworkPeers`) removes the device.
  - `GET /v1/hosts/{host}/underlay?to=IP` (`getUnderlay`) runs `ip route get`
    (no root) and answers the host's source address and interface toward `to`.
    `409` for a route to the host's own address or an unusable answer.
- **Join** (`vmtool net join HOST NAME [--home H]`, `internal/app`
  `JoinNetwork`; store mode only, else it refuses with the reason): reads the
  registry; the member asks its route to the home (the home's registered LAN
  address, else the address of the home in `config.yml hosts:`: the *client*
  resolves the home's address, the member's daemon looks the route up); on the
  first join the home answers the same lookup toward the member and is
  recorded as a member too; the member defines the bridge-only network, is
  recorded, and links to every other row; then every other member that answers
  (the home included) gets its new peer list; one that does not is skipped
  with one line (it reads the registry when its daemon starts). A network made
  without a store (bridge number 1 to 9999) is registered by its first join
  with its bridge number as its VNI and its subnet and domain from the home
  (`--home` names the home), its reservations recorded as the home's VMs'; a
  number the registry already has is a `409` ("recreate it with a store"). A
  step that fails takes back the ones before it.
- **Leave** (`vmtool net leave HOST NAME`): refused while a VM of the host has a
  card on the network and for the home; unlinks, undefines, removes the member,
  pushes the shorter list to the others.
- **The daemon keeps links up** (`internal/app` `NetLinkKeeper`, started by
  `vmtool server` on a hypervisor after the bind): for each vmtool network
  active on the host (bridge `vmt-` plus six hex digits, whose digits are the
  VNI) it reads the host's own networks from the store with the host token
  (`getHostNetworks`) and links, but only when the registry's network of that
  VNI has the same name and lists this host (VNIs of deleted networks are
  reused); otherwise it leaves the bridge unlinked and logs one line. With the
  store unreachable it links nothing, logs once and retries on the inventory
  reporter's backoff (30 s doubling to 5 min). It never delays serving. libvirt's
  "network started" event links that network again and logs how to repair the
  cards (restart libvirtd; vmtool never restarts it).
- **A card on a member** (`attach`, `detach`, `create --network`, `delete`): the
  reservation is made on the home (`createReservation` picks the MAC and the
  address; the home must be awake and the store must answer, to find it), then
  the card is attached on the VM's host with that MAC (`attachInterface`); a
  failure undoes the other step. `detach` removes the card, then the home's
  reservation. `delete` removes the reservations on the home after the VM (the
  VM is deleted even if the home does not answer, and the error names what is
  left). Each reservation is also recorded in the registry when made and
  removed. The address and the name reach the guest from the home's DHCP over
  the link. `VMInfo.networks` for a card on a member has no address (the
  reservation is on the home).
- **Firewall.** `just prereqs` opens UDP 4789 from the LAN of the host's
  default route when ufw is active.
- **MTU 1450.** Home and member forms both carry `<mtu size='1450'/>`, so a
  frame still fits the LAN with the VXLAN header added.
- **`networks` output.** The CLI's `vmtool networks` writes `name (vmtool)` for
  a vmtool network in the home form (and for a one-host network), `name
  (vmtool, member)` for the member form (a vmtool bridge with no gateway), and
  the bare name for any other libvirt network.
- **When hosts are down.** Nothing in the daemon or the registry waits for a
  member: the store never records whether a host is up. With the home down,
  VM-to-VM traffic by address keeps working (the links are between hosts);
  names, DHCP for new addresses and NAT through the overlay fail, and
  reservations never expire, so addresses survive. A member that is asleep
  takes the peer list from the registry when its daemon starts.
- **Deleting a registered network.** `vmtool net delete HOME NAME` (a CLI
  workflow over the existing routes, no new route) removes the member rows,
  the reservation records, the home's row and then the entry, so the store's
  409 on `DELETE /v1/networks/{name}` while members remain is what a REST
  client of the registry sees if it skips the rows. A member that was asleep
  has its row removed and its libvirt network removed by its own daemon at
  start, when the registry does not list the host for it.
- **Known limits (current).**
  - The "network started" watch is one libvirt connection, opened once: after a
    libvirtd restart, events are not seen until the vmtool daemon restarts.
    After a manual `net-destroy` and `net-start` the VMs' cards stay detached
    from the bridge; restart libvirtd (vmtool never does).
  - The overlay is neither authenticated nor encrypted: anyone on the LAN can
    read it and, knowing a VNI, inject frames into that network.

## GET / pattern

PR1 records the exact std-http pattern oapi-codegen emits for the root after
the first generate. Update this line when known:

| Operation | Expected ServeMux pattern |
|---|---|
| `getDocs` | `GET /{$}` — Swagger UI (not a catch-all) |
| `getOpenAPISpec` | `GET /openapi.yaml` |
| `getOpenAPIJSON` | `GET /openapi.json` |

## A VM's notes in the store (API 2.6.0)

vmtool writes nothing of its own into a VM's libvirt definition: no
`<metadata>`, no element of its own. What it records about a VM besides what
the host reports, its **origin** and its **comment** (together, the VM's
*notes*), lives in the store only, in plain text, in the store's `vm_notes`
table keyed (host, libvirt UUID). No daemon report carries or touches them.

- **Origin** (`image`, `image_sha256`, `template`, `clone`
  `thin|block|file|none`, `pool`, `created` RFC 3339) is written once, by the
  workstation's create in `internal/app`, right after define, when it knows
  host, name and the UUID the host gave the domain, and only with a store in
  use. The store accepts it only while (host, uuid) has none; the same origin
  again is 200 and changes nothing, a different one is 409. `image_sha256` is
  the host's manifest digest after the image sync (empty when the create did
  not know it); for a thin snapshot the create asks the host's template list
  for the template it was made from (and takes its digest when the image's was
  unknown). No store in use: no origin, and no extra line.
- **Comment**: at most 512 bytes of UTF-8, one line, no control characters
  (`vmnotes.ValidateComment`), checked by the workstation and again by the
  store. `CreateVMRequest.comment` is written with the origin; with no store
  in use it is refused (503) before anything is created.
  `PUT /v1/hosts/{host}/vms/{name}/comment` (`setVMComment`, workstation-only;
  a hypervisor answers 403), body `{"comment": "..."}`, writes the store
  without asking the host (it may sleep) and answers the VM's notes
  (`VMNotes`). Empty clears. 400 naming `comment` for a bad one, 404 when the
  store's inventory has no row for the VM, 409 when the row is gone, 503 with
  no store in use (the message says comments are kept in the store).
- **The store fails after define** (store mode at the start): the create
  succeeds; one line (stderr, `X-Vmtool-Warnings`, and a progress notice on a
  streamed create) says the origin (and the comment) was not saved and how to
  set the comment later.
- **Reading them**: `GET /v1/inventory` (`InventoryVM.origin`,
  `InventoryVM.comment`). `VMInfo` carries neither; `vmtool info` reads the
  store once and shows them in store mode only (nothing on a store failure).
- **The store side**: `PUT /v1/inventory/{host}/{name}/notes` (`putVMNotes`,
  user tokens only; a host token gets 401), body `{uuid?, origin?, comment?}`.
  With `uuid` (create's case) the notes are stored before the host's first
  report makes the row; without it the route needs a live row of (host, name).
  Notes go with their row (`deleteInventoryRow`, the sweep); notes whose row
  never appears are swept 7 days after they were last written.
- A domain that still carries the element an earlier 2.6.0 build wrote
  (`urn:vmtool:vm:1`; none exists outside tests) defines, lists and reports
  as any other: vmtool neither reads nor removes it.

## Reading the inventory (API 2.6.0)

`GET /v1/inventory` (`getInventory`, workstation-only) is the workstation's
read of the store's `GET /v1/inventory` (see [The store](#the-store)), through
the one `internal/app` call `Service.Inventory` that `vmtool inventory` and the
TUI's `I` view also use. Query `host` limits hosts and VMs to one host;
`gone=true` adds the VMs their host no longer has (each with `gone_since` and
`credentials_kept_until`). Machines (metal and alien names and kinds) are never
filtered by `host`. The answer is a record of what each daemon last said: no
up/down state and no staleness mark, `last_report` a plain time.
`InventoryVM.disks_unreadable` marks a VM whose disks the host could not read
(its `disks` are the last known ones); `vmtool inventory` and the TUI show it
as `*` with a footnote.

API 2.10.0 adds `Inventory.templates` (optional in the schema, always sent by
the workstation) and `InventoryHost.templates_reported`. A template row is
`{host, name, kind: image|saved, pool, source_image?, size_bytes, created?}`,
the last list each host's daemon sent with its VMs (store API 1.4.0:
`HostReport.templates`, `InventoryTemplate`); `host` limits them too. A host
that stops reporting keeps its rows. `templates_reported` is true once the host
has sent a list (an empty one counts) and false for a daemon that never has; a
report without a list changes neither the rows nor the flag.

A workflow that names a VM as `host/vm` and gets "no such VM" from a host that
confirms it (an `Info` that is also a not-found) delists the VM in the store and
says so in its error, with the date the login is kept until; the status stays
404. A down store adds no line. The REST console route does not delist.

| Status | When |
|---|---|
| 200 | `{hosts, vms, machines}` |
| 401 | missing or invalid bearer |
| 403 | the server's role is hypervisor: `getInventory is served by the workstation, not by hypervisor <name>` |
| 503 | no store configured, or it did not answer: `inventory: <why>; the inventory is kept by the store, and there is no local copy`. The CLI prints the same text and exits 1. There is no local mode, so no `X-Vmtool-Warnings` fallback line rides on it. |
| 500 | anything else |
