#!/usr/bin/env bash
# Build vmtool on a hypervisor and run it there as a systemd service, as the
# system user vmtool, from HV_DIR (default /opt/vmtool).
#
#   scripts/deploy.sh <host>
#
# `just prereqs <host>` must have run first (it makes the vmtool user, HV_DIR
# and the vmtool pool). The daemon is the unit vmtool-daemon.service,
# listening on 0.0.0.0:<port>; <port> is the host's `port` in config.yml
# (default 9473; `vmtool config host <host>` prints "address ssh_user port").
#
# This machine: when <host> is this machine's name (scripts/cert.sh
# --local-name: `name:` in config.yml, else `hostname -s`), every step runs
# locally, with no ssh and one sudo for the install. If config.yml has no
# hosts entry for it, address and port come from VMTOOL_SELF_ADDRESS (default
# <host>.<domain>) and VMTOOL_SELF_PORT (default 9474), and after a good
# /version `vmtool config set-host` writes the entry. A listed entry is used
# as it stands and never rewritten.
#
# Steps, in order; each stops the run if it fails:
#   1. go check    `go env GOVERSION` over ssh, /usr/local/go/bin first on PATH,
#                  must be >= GO_MIN (the first Go that honours
#                  GOTOOLCHAIN=auto, which fetches the go.mod one)
#   2. prereqs     `id vmtool` and `test -d HV_DIR` over ssh
#   3. copy        git ls-files | tar over ssh into a fresh
#                  ~/.cache/vmtool-deploy/src (tracked files only: no .env,
#                  .machines, config.yml or tls/); stage/ is emptied too
#   4. build       go build there as the login user into stage/vmtool
#   5. cert        CERT_STAGE_ONLY=1 scripts/cert.sh <host> (into stage/tls;
#                  on this machine cert.sh keeps tls/ in the checkout and the
#                  pair is copied into stage/tls, the checkout's copy stays)
#   6. config      `vmtool config write --token-stdin` run in stage/; the token
#                  rides stdin of an ssh call with no tty and no sudo, so it
#                  never meets a sudo prompt; config.yml holds only its hash.
#                  When this machine's config.yml has server.url (`vmtool
#                  config server-url` prints it), `--store-url <url>` is added:
#                  the daemon's store_url, the one address fetchImage dials.
#                  With a store_url, the host's field of
#                  BAO_STORE_HOST_TOKENS_PATH (default
#                  kv/infra/labtwo/vmtool_store/host_tokens) is read with
#                  `bao kv get -field=<host>` into a variable, sent over the
#                  stdin of one ssh call into stage/store-host-token, and
#                  `--store-token-file store-host-token` is added. No field:
#                  one warning, no flag, and an old HV_DIR/store-host-token is
#                  removed at install
#   7. install     the rendered unit, the rendered root helper vmtool-thin
#                  (deploy/vmtool-thin with HV_POOL_DIR written into it) and
#                  its sudoers rule (deploy/vmtool-thin.sudoers) go to stage/
#                  on stdin; then one `ssh -t` call runs one `sudo sh -c`,
#                  which in this order: refuses
#                  if the port is listened on by anything but our daemon
#                  (vmtool-daemon, or a vmtool.service whose WorkingDirectory
#                  is HV_DIR), before changing anything; disables, stops and
#                  removes such an older vmtool.service (any other
#                  vmtool.service is left alone); installs the helper as
#                  root:root 0755 into LIBEXEC_DIR (never owned by vmtool, so
#                  the daemon cannot rewrite what it runs as root) and the
#                  sudoers rule root 0440 under a temporary name with a dot
#                  (sudo ignores such files) in SUDOERS_DIR, which `visudo -cf`
#                  must pass before it is moved to SUDOERS_DIR/vmtool-thin (a
#                  failed check removes it and stops the install, before the
#                  daemon's files are touched); then installs binary,
#                  config.yml, crt and key (owner vmtool) into HV_DIR and the
#                  unit into UNIT_DIR; then daemon-reload, enable, restart
#                  vmtool-daemon; the host token goes in vmtool:vmtool 0600 as
#                  HV_DIR/store-host-token. The staged config.yml, key and token are removed
#                  either way.
#   8. check       GET /version over HTTPS (public: no token sent)
#
# Environment (the justfile passes these):
#   VMTOOL_ROOT, VMTOOL_DOMAIN, BAO_CA_PATH, CERT_DAYS   as for cert.sh
#   HV_DIR         daemon directory on the host (default /opt/vmtool)
#   GO_MIN         minimum Go as major.minor (default 1.21)
#   VMTOOL_BIN     local vmtool, used for `config host` (default <root>/vmtool)
#   DEPLOY_RETRIES seconds to wait for /version after restart (default 15)
#   VMTOOL_SELF_ADDRESS, VMTOOL_SELF_PORT   this machine, when unlisted
#   HV_POOL_DIR    the image directory the helper vmtool-thin may read and
#                  write files in (default /var/lib/vmtool/images; the
#                  justfile passes the same value `just prereqs` uses)
#   UNIT_DIR       systemd unit directory on the host (default
#                  /etc/systemd/system; a test seam)
#   LIBEXEC_DIR    where the helper is installed (default /usr/local/libexec;
#                  a test seam: the sudoers rule names the default path)
#   SUDOERS_DIR    sudoers drop-in directory (default /etc/sudoers.d; a test
#                  seam)
#   GO_BIN_DIR     where a prereqs-installed Go lives (default
#                  /usr/local/go/bin; a test seam)
# REMOTE_DIR (the old home-directory layout) is ignored, with a warning.
# The token comes from VMTOOL_TOKEN, else .env in VMTOOL_ROOT, read with bash
# builtins so it is never in any argv.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"
domain="${VMTOOL_DOMAIN:-labtwo}"
go_min="${GO_MIN:-1.21}"
retries="${DEPLOY_RETRIES:-15}"
vmtool_bin="${VMTOOL_BIN:-$root/vmtool}"
hv="${HV_DIR:-/opt/vmtool}"
template="$here/../deploy/vmtool.service.in"
ud="${UNIT_DIR:-/etc/systemd/system}"
pool_dir="${HV_POOL_DIR:-/var/lib/vmtool/images}"
libexec="${LIBEXEC_DIR:-/usr/local/libexec}"
sudoers_dir="${SUDOERS_DIR:-/etc/sudoers.d}"
thin_src="$here/../deploy/vmtool-thin"
sudoers_src="$here/../deploy/vmtool-thin.sudoers"
net_src="$here/../deploy/vmtool-net"
net_sudoers_src="$here/../deploy/vmtool-net.sudoers"
gobin="${GO_BIN_DIR:-/usr/local/go/bin}"
export VMTOOL_ROOT="$root" VMTOOL_DOMAIN="$domain" VMTOOL_BIN="$vmtool_bin"

die() { echo "ERROR: $*" >&2; exit 1; }

host="${1:-}"
[[ -n $host && $# -eq 1 ]] || { echo "usage: $0 <host>" >&2; exit 2; }
[[ $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid host name '$host'"
[[ $domain =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid domain '$domain'"
# hv lands in remote shell commands and a systemd unit unquoted.
[[ $hv =~ ^/[A-Za-z0-9._/-]+$ ]] || die "HV_DIR '$hv' has characters unsafe in a shell command or systemd unit."
[[ $ud =~ ^/[A-Za-z0-9._/-]+$ ]] || die "UNIT_DIR '$ud' has characters unsafe in a shell command."
[[ $pool_dir =~ ^/[A-Za-z0-9._/-]+$ ]] || die "HV_POOL_DIR '$pool_dir' has characters unsafe in a shell command or the vmtool-thin helper."
[[ $libexec =~ ^/[A-Za-z0-9._/-]+$ ]] || die "LIBEXEC_DIR '$libexec' has characters unsafe in a shell command."
[[ $sudoers_dir =~ ^/[A-Za-z0-9._/-]+$ ]] || die "SUDOERS_DIR '$sudoers_dir' has characters unsafe in a shell command."
[[ $gobin =~ ^/[A-Za-z0-9._/-]+$ ]] || die "GO_BIN_DIR '$gobin' has characters unsafe in a shell command."
[[ -z ${REMOTE_DIR:-} ]] || echo "WARNING: REMOTE_DIR is no longer used and is ignored; the daemon goes to HV_DIR ($hv)." >&2

# Token: env, else .env. Builtins only.
tok="${VMTOOL_TOKEN:-}"
if [[ -z $tok && -f $root/.env ]]; then
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line == VMTOOL_TOKEN=* ]]; then
      tok="${line#VMTOOL_TOKEN=}"
      tok="${tok%$'\r'}"
      tok="${tok#[\"\']}"
      tok="${tok%[\"\']}"
    fi
  done <"$root/.env"
fi
[[ -n $tok ]] || die "no VMTOOL_TOKEN found; run 'just token' or 'just token-fetch' first."

[[ -x $vmtool_bin ]] || die "$vmtool_bin not found; run 'just build' first."
[[ -f $template ]] || die "$template not found."
[[ -f $thin_src && -f $sudoers_src ]] || die "deploy/vmtool-thin or deploy/vmtool-thin.sudoers not found."
[[ -f $net_src && -f $net_sudoers_src ]] || die "deploy/vmtool-net or deploy/vmtool-net.sudoers not found."
fqdn="$host.$domain"

# This machine? The same test as cert.sh, which keeps tls/ here for it.
local_name=$("$here/cert.sh" --local-name) || die "could not work out this machine's name."
is_local=no
[[ $host != "$local_name" ]] || is_local=yes

# Host entry: "address ssh_user port". An own name that is not listed uses
# VMTOOL_SELF_ADDRESS / VMTOOL_SELF_PORT (the address falls back to 127.0.0.1
# when VMTOOL_SELF_ADDRESS is unset and <host>.<domain> does not resolve); any
# other failure is fatal.
listed=yes
rc=0
out=$(cd "$root" && "$vmtool_bin" config host "$host" 2>&1) || rc=$?
if [[ $rc -eq 0 ]]; then
  read -r addr ssh_user port <<<"$out" || true
  port="${port:-9473}"
  [[ -n ${addr:-} && -n ${ssh_user:-} ]] || die "no address/ssh_user for host $host in config.yml."
elif [[ $is_local == yes && $out == *"unknown host"* ]]; then
  listed=no
  addr="${VMTOOL_SELF_ADDRESS:-$fqdn}"
  # No VMTOOL_SELF_ADDRESS and a name nothing resolves: this machine is
  # reached on loopback (the check and the hosts: entry use it).
  if [[ -z ${VMTOOL_SELF_ADDRESS:-} ]] && ! getent hosts "$fqdn" >/dev/null 2>&1; then
    addr=127.0.0.1
    echo "deploy: $fqdn has no DNS record and $host has no hosts: entry; using 127.0.0.1 as its address (set VMTOOL_SELF_ADDRESS to use another)." >&2
  fi
  port="${VMTOOL_SELF_PORT:-9474}"
  ssh_user="$(id -un)"
else
  die "'vmtool config host $host' failed (is $host in config.yml?): $out"
fi
# addr and ssh_user form ssh's destination argument: nothing ssh could read as an option.
[[ $ssh_user =~ ^[A-Za-z0-9_][A-Za-z0-9._-]*$ ]] || die "ssh_user '$ssh_user' for $host is not a plain user name."
[[ $addr =~ ^[A-Za-z0-9][A-Za-z0-9.:-]*$ ]] || die "address '$addr' for $host is not a plain host name or IP."
[[ $port =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || die "port '$port' for $host is not a port number."
port=$((10#$port))

# The client's store, if it has one: `vmtool config server-url` prints
# server.url (nothing when absent). It becomes the daemon's store_url, the one
# address its fetchImage dials. It lands in a remote shell command, so only a
# plain https URL passes.
store_url=$(cd "$root" && "$vmtool_bin" config server-url) || die "'vmtool config server-url' failed; fix config.yml."
store_arg=""
if [[ -n $store_url ]]; then
  [[ $store_url =~ ^https://[A-Za-z0-9][A-Za-z0-9.:-]*(/[A-Za-z0-9._~/-]*)?$ ]] \
    || die "server.url in config.yml is not a plain https URL (like https://vmtool-store.labtwo); it becomes the daemon's store_url."
  store_arg=" --store-url $store_url"
fi

# The host's store token, only when there is a store: read from bao by field
# name (the host) into a shell variable with builtins, never in any argv. A
# missing field, an empty one or a bao failure is not fatal: one warning, and
# the daemon is installed without a token file (and any old one removed).
host_tok=""
if [[ -n $store_url ]]; then
  host_tok=$(bao kv get -field="$host" "${BAO_STORE_HOST_TOKENS_PATH:-kv/infra/labtwo/vmtool_store/host_tokens}" 2>/dev/null) || host_tok=""
  host_tok="${host_tok//[$' \t\r\n']/}"
  [[ -n $host_tok ]] || echo "WARNING: $host has no store host token; its daemon will not report VMs; see make_secrets.sh hosttokens" >&2
fi
token_arg=""
[[ -z $host_tok ]] || token_arg=" --store-token-file store-host-token"

# The client's hosts.<host>.vms: `vmtool config host-vms` prints false for a
# network home. Its daemon is told (network_home: true) and refuses the
# VM-creating routes; the key is config.yml, rewritten on every deploy, so a
# change of the client's entry is picked up by the next one. A machine that is
# not listed has no entry and holds VMs.
home_arg=""
if [[ $listed == yes ]]; then
  holds=$(cd "$root" && "$vmtool_bin" config host-vms "$host") || die "'vmtool config host-vms $host' failed; fix config.yml."
  [[ $holds != false ]] || home_arg=" --network-home"
fi

# run <command>: on the host over ssh, or here in bash when it is this machine.
# runt is the install call: `ssh -t` so a sudo prompt works.
if [[ $is_local == yes ]]; then
  where="$host (this machine)"
  lsh='bash -c'
  run() { bash -c "$1"; }
  runt() { bash -c "$1"; }
else
  target="$ssh_user@$addr"
  where="$target"
  lsh='bash -lc'
  run() { ssh "$target" "$1"; }
  runt() { ssh -t "$target" "$1"; }
fi

# Staging area in the login user's home. Single-quoted: $HOME is expanded by
# the shell that runs the command, never here.
base='$HOME/.cache/vmtool-deploy'

# 1. Go check, before anything is copied or built.
rc=0
found=$(run "$lsh 'PATH=$gobin:\$PATH; go env GOVERSION'") || rc=$?
[[ $rc -ne 255 ]] || die "ssh to $where failed."
found="${found//[$' \t\r\n']/}"
[[ $rc -eq 0 && -n $found ]] || found="not installed"

go_ok=no
if [[ $found =~ ^go([0-9]+)(\.([0-9]+))? ]]; then
  fmaj=${BASH_REMATCH[1]}
  fmin=${BASH_REMATCH[3]:-0}
  rmaj=${go_min%%.*}
  rmin=${go_min#*.}
  [[ $rmin != "$go_min" ]] || rmin=0
  if ((10#$fmaj > 10#$rmaj || (10#$fmaj == 10#$rmaj && 10#$fmin >= 10#$rmin))); then
    go_ok=yes
  fi
fi
if [[ $go_ok == no ]]; then
  mod=$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' "$root/go.mod" 2>/dev/null | head -n1)
  die "$host: found $found, need go >= $go_min (go.mod asks for ${mod:-?}; GOTOOLCHAIN=auto fetches it). Ubuntu 22.04's golang-go is too old; install a newer Go and re-run."
fi

# 2. Prereqs: the vmtool user and HV_DIR, before anything is copied.
rc=0
run "id vmtool >/dev/null 2>&1 && test -d $hv" || rc=$?
[[ $rc -ne 255 ]] || die "ssh to $where failed."
[[ $rc -eq 0 ]] || die "$host has no vmtool user or no $hv; run 'just prereqs $host' first."

# 3. Copy tracked files into a fresh src/; empty stage/. umask 077: the
#    staging area will hold config.yml and the TLS key.
echo "Copying tracked files to $where:~/.cache/vmtool-deploy/src"
git -C "$root" ls-files -z | tar -C "$root" --null -T - -czf - \
  | run "umask 077 && rm -rf \"$base/src\" \"$base/stage\" && mkdir -p \"$base/src\" \"$base/stage/tls\" && chmod 700 \"$base\" \"$base/src\" \"$base/stage\" \"$base/stage/tls\" && tar -xzf - -C \"$base/src\""

# 4. Build there as the login user.
echo "Building vmtool on $host"
run "$lsh 'export PATH=$gobin:\$PATH; cd \"$base/src\" && GOTOOLCHAIN=auto go build -o \"$base/stage/vmtool\" ./cmd/vmtool'"

# 5. Certificate into stage/tls (cert.sh drops the local key; no sudo there).
#    On this machine cert.sh leaves the pair in tls/ of the checkout (the
#    loopback `just install` uses it too); copy it into stage/tls.
CERT_STAGE_ONLY=1 HV_DIR="$hv" "$here/cert.sh" "$host"
if [[ $is_local == yes ]]; then
  stage_dir="$HOME/.cache/vmtool-deploy/stage/tls"
  cp -p "$root/tls/$fqdn.crt" "$root/tls/$fqdn.key" "$stage_dir/"
  chmod 600 "$stage_dir/$fqdn.key"
fi

# 6. config.yml in stage/, from scratch; the token rides stdin of this call
#    alone (no -t, no sudo). The cert paths are relative to the working
#    directory, so they hold in stage/ and in HV_DIR alike.
printf '%s\n' "$tok" | run "cd \"$base/stage\" && umask 077 && rm -f config.yml && ./vmtool config write --token-stdin --role hypervisor --name $host --listen 0.0.0.0:$port --tls-cert tls/$fqdn.crt --tls-key tls/$fqdn.key$store_arg$token_arg$home_arg"
# The host token, when there is one: stdin of this one call, into stage/ with
# umask 077 (the install below sets owner and mode).
if [[ -n $host_tok ]]; then
  printf '%s\n' "$host_tok" | run "umask 077 && cat > \"$base/stage/store-host-token\""
fi

# 7. Unit (rendered here) to stage/, then one sudo for the whole install.
content="$(<"$template")"
content="${content//@USER@/vmtool}"
content="${content//@DIR@/$hv}"
printf '%s\n' "$content" | run "cat > \"$base/stage/vmtool-daemon.service\""
# The root helper, its image directory rendered in (the installed copy has no
# other way to learn it), and the sudoers rule that lets vmtool run it.
thin="$(<"$thin_src")"
thin="${thin//@POOL_DIR@/$pool_dir}"
printf '%s\n' "$thin" | run "cat > \"$base/stage/vmtool-thin\""
run "cat > \"$base/stage/vmtool-thin.sudoers\"" <"$sudoers_src"
# The link helper takes no rendering: staged as it is, with its own rule.
run "cat > \"$base/stage/vmtool-net\"" <"$net_src"
run "cat > \"$base/stage/vmtool-net.sudoers\"" <"$net_sudoers_src"

# Run as root by `sh -c`, with the staging dir as $1. No single quotes inside.
# (a) refuse a port that anything but our daemon listens on, before any
# change; ours: vmtool-daemon, and an older vmtool.service run from HV_DIR.
# (b) remove that older unit. (c) install. (d) reload, enable, restart.
own='install -o vmtool -g vmtool'
script="set -e"
script+="; test -d $hv || { echo \"$hv missing: run just prereqs $host first\" >&2; exit 1; }"
script+="; ours=\" \$(systemctl show -p MainPID --value vmtool-daemon 2>/dev/null || true) \"; old=no"
script+="; if grep -Fqx \"WorkingDirectory=$hv\" $ud/vmtool.service 2>/dev/null; then old=yes; ours=\"\$ours\$(systemctl show -p MainPID --value vmtool 2>/dev/null || true) \"; fi"
script+="; l=\$(ss -Htlnp \"sport = :$port\")"
script+="; pids=\$(printf \"%s\\n\" \"\$l\" | grep -o \"pid=[0-9]*\" | cut -d= -f2 || true)"
script+="; if [ -n \"\$l\" ] && [ -z \"\$pids\" ]; then echo \"port $port is listened on by a process that cannot be identified; nothing was changed\" >&2; exit 1; fi"
script+="; for p in \$pids; do case \"\$ours\" in *\" \$p \"*) ;; *) echo \"port $port is already listened on by pid \$p, which is not vmtool; nothing was changed\" >&2; exit 1 ;; esac; done"
script+="; if [ \$old = yes ]; then systemctl disable --now vmtool; rm -f $ud/vmtool.service; fi"
script+="; install -d -m 0755 $libexec"
script+="; install -o root -g root -m 0755 \"\$1/vmtool-thin\" $libexec/vmtool-thin"
script+="; install -o root -g root -m 0440 \"\$1/vmtool-thin.sudoers\" $sudoers_dir/vmtool-thin.tmp"
script+="; visudo -cf $sudoers_dir/vmtool-thin.tmp >/dev/null || { rm -f $sudoers_dir/vmtool-thin.tmp; echo \"the sudoers rule failed visudo; it was not installed and the daemon was not updated\" >&2; exit 1; }"
script+="; mv $sudoers_dir/vmtool-thin.tmp $sudoers_dir/vmtool-thin"
script+="; install -o root -g root -m 0755 \"\$1/vmtool-net\" $libexec/vmtool-net"
script+="; install -o root -g root -m 0440 \"\$1/vmtool-net.sudoers\" $sudoers_dir/vmtool-net.tmp"
script+="; visudo -cf $sudoers_dir/vmtool-net.tmp >/dev/null || { rm -f $sudoers_dir/vmtool-net.tmp; echo \"the vmtool-net sudoers rule failed visudo; it was not installed and the daemon was not updated\" >&2; exit 1; }"
script+="; mv $sudoers_dir/vmtool-net.tmp $sudoers_dir/vmtool-net"
script+="; install -d -o vmtool -g vmtool -m 0700 $hv/tls"
script+="; $own -m 0755 \"\$1/vmtool\" $hv/vmtool"
script+="; $own -m 0600 \"\$1/config.yml\" $hv/config.yml"
script+="; $own -m 0644 \"\$1/tls/$fqdn.crt\" $hv/tls/$fqdn.crt"
script+="; $own -m 0600 \"\$1/tls/$fqdn.key\" $hv/tls/$fqdn.key"
script+="; if [ -f \"\$1/store-host-token\" ]; then $own -m 0600 \"\$1/store-host-token\" $hv/store-host-token; else rm -f $hv/store-host-token; fi"
script+="; install -m 0644 \"\$1/vmtool-daemon.service\" $ud/vmtool-daemon.service"
script+="; systemctl daemon-reload; systemctl enable --now vmtool-daemon; systemctl restart vmtool-daemon"
# -t so a sudo password prompt works; the token never rides this call.
runt "s=\"$base/stage\"; sudo sh -c '$script' sh \"\$s\"; rc=\$?; rm -f \"\$s/config.yml\" \"\$s/tls/$fqdn.key\" \"\$s/store-host-token\"; exit \$rc" \
  || die "installing on $host failed (a refused port, or the install itself); the staged config.yml, key and token were removed. Re-run 'just deploy $host'."

# 8. GET /version (public; verified against the system trust store).
# The cert names <host>.<domain>, so that is the URL. curl's --resolve takes
# only a numeric address: pin it when config.yml gives an IP, else use DNS.
resolve=()
if [[ $addr =~ ^[0-9.]+$ || $addr == *:* ]]; then
  resolve=(--resolve "$fqdn:$port:$addr")
fi
n=0
while :; do
  n=$((n + 1))
  if curl --fail -sS "${resolve[@]}" "https://$fqdn:$port/version" </dev/null; then
    echo
    break
  fi
  ((n < retries)) || die "GET https://$fqdn:$port/version did not succeed; check 'systemctl status vmtool-daemon' on $host."
  sleep 1
done

# This machine, not listed: record it now that the daemon answers.
if [[ $is_local == yes && $listed == no ]]; then
  (cd "$root" && "$vmtool_bin" config set-host "$host" --address "$addr" --ssh-user "$ssh_user" --port "$port") \
    || die "vmtool is deployed, but 'vmtool config set-host $host' failed; add the hosts: entry by hand."
fi
echo "Deployed vmtool to $where:$hv (runs as vmtool) and it answers on https://$fqdn:$port."
