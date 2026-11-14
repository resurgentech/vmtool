#!/usr/bin/env bash
# Build vmtool on a hypervisor and run it there as a systemd service, as the
# system user vmtool, from HV_DIR (default /opt/vmtool).
#
#   scripts/deploy.sh <host>
#
# `just prereqs <host>` must have run first (it makes the vmtool user, HV_DIR
# and the vmtool pool). Steps, in order; each stops the run if it fails:
#   1. go check    `go env GOVERSION` over ssh, /usr/local/go/bin first on PATH,
#                  must be >= GO_MIN (the first Go that honours
#                  GOTOOLCHAIN=auto, which fetches the go.mod one)
#   2. prereqs     `id vmtool` and `test -d HV_DIR` over ssh
#   3. copy        git ls-files | tar over ssh into a fresh
#                  ~/.cache/vmtool-deploy/src (tracked files only: no .env,
#                  .machines, config.yml or tls/); stage/ is emptied too
#   4. build       go build there as the login user into stage/vmtool
#   5. cert        CERT_STAGE_ONLY=1 scripts/cert.sh <host> (into stage/tls)
#   6. config      `vmtool config write --token-stdin` run in stage/; the token
#                  rides stdin of an ssh call with no tty and no sudo, so it
#                  never meets a sudo prompt; config.yml holds only its hash
#   7. install     the rendered unit goes to stage/ on stdin; then one `ssh -t`
#                  call runs one `sudo sh -c`: install binary, config.yml,
#                  crt and key (owner vmtool) into HV_DIR and the unit into
#                  /etc/systemd/system, then daemon-reload, enable, restart.
#                  The staged config.yml and key are removed either way.
#   8. check       GET /version over HTTPS (public: no token sent)
#
# Environment (the justfile passes these):
#   VMTOOL_ROOT, VMTOOL_DOMAIN, BAO_CA_PATH, CERT_DAYS   as for cert.sh
#   HV_DIR         daemon directory on the host (default /opt/vmtool)
#   GO_MIN         minimum Go as major.minor (default 1.21)
#   VMTOOL_BIN     local vmtool, used for `config host` (default <root>/vmtool)
#   DEPLOY_RETRIES seconds to wait for /version after restart (default 15)
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
port=9473
export VMTOOL_ROOT="$root" VMTOOL_DOMAIN="$domain" VMTOOL_BIN="$vmtool_bin"

die() { echo "ERROR: $*" >&2; exit 1; }

host="${1:-}"
[[ -n $host && $# -eq 1 ]] || { echo "usage: $0 <host>" >&2; exit 2; }
[[ $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid host name '$host'"
[[ $domain =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid domain '$domain'"
# hv lands in remote shell commands and a systemd unit unquoted.
[[ $hv =~ ^/[A-Za-z0-9._/-]+$ ]] || die "HV_DIR '$hv' has characters unsafe in a shell command or systemd unit."
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
read -r addr ssh_user < <(cd "$root" && "$vmtool_bin" config host "$host") \
  || die "'vmtool config host $host' failed; is $host in config.yml?"
[[ -n ${addr:-} && -n ${ssh_user:-} ]] || die "no address/ssh_user for host $host in config.yml."
# Both form ssh's destination argument: nothing ssh could read as an option.
[[ $ssh_user =~ ^[A-Za-z0-9_][A-Za-z0-9._-]*$ ]] || die "ssh_user '$ssh_user' for $host in config.yml is not a plain user name."
[[ $addr =~ ^[A-Za-z0-9][A-Za-z0-9.:-]*$ ]] || die "address '$addr' for $host in config.yml is not a plain host name or IP."
target="$ssh_user@$addr"
[[ -f $template ]] || die "$template not found."
fqdn="$host.$domain"

# Staging area in the login user's home. Single-quoted: $HOME is expanded by
# the remote shell, never here.
base='$HOME/.cache/vmtool-deploy'
gobin=/usr/local/go/bin

# 1. Go check, before anything is copied or built.
rc=0
found=$(ssh "$target" "bash -lc 'PATH=$gobin:\$PATH; go env GOVERSION'") || rc=$?
[[ $rc -ne 255 ]] || die "ssh to $target failed."
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
ssh "$target" "id vmtool >/dev/null 2>&1 && test -d $hv" || rc=$?
[[ $rc -ne 255 ]] || die "ssh to $target failed."
[[ $rc -eq 0 ]] || die "$host has no vmtool user or no $hv; run 'just prereqs $host' first."

# 3. Copy tracked files into a fresh src/; empty stage/. umask 077: the
#    staging area will hold config.yml and the TLS key.
echo "Copying tracked files to $target:~/.cache/vmtool-deploy/src"
git -C "$root" ls-files -z | tar -C "$root" --null -T - -czf - \
  | ssh "$target" "umask 077 && rm -rf \"$base/src\" \"$base/stage\" && mkdir -p \"$base/src\" \"$base/stage/tls\" && chmod 700 \"$base\" \"$base/src\" \"$base/stage\" \"$base/stage/tls\" && tar -xzf - -C \"$base/src\""

# 4. Build there as the login user.
echo "Building vmtool on $host"
ssh "$target" "bash -lc 'export PATH=$gobin:\$PATH; cd \"$base/src\" && GOTOOLCHAIN=auto go build -o \"$base/stage/vmtool\" ./cmd/vmtool'"

# 5. Certificate into stage/tls (cert.sh drops the local key; no sudo there).
CERT_STAGE_ONLY=1 HV_DIR="$hv" "$here/cert.sh" "$host"

# 6. config.yml in stage/, from scratch; the token rides stdin of this call
#    alone (no -t, no sudo). The cert paths are relative to the working
#    directory, so they hold in stage/ and in HV_DIR alike.
printf '%s\n' "$tok" | ssh "$target" "cd \"$base/stage\" && umask 077 && rm -f config.yml && ./vmtool config write --token-stdin --role hypervisor --name $host --listen 0.0.0.0:$port --tls-cert tls/$fqdn.crt --tls-key tls/$fqdn.key"

# 7. Unit (rendered here) to stage/, then one sudo for the whole install.
content="$(<"$template")"
content="${content//@USER@/vmtool}"
content="${content//@DIR@/$hv}"
printf '%s\n' "$content" | ssh "$target" "cat > \"$base/stage/vmtool.service\""

# Run as root by `sh -c`, with the staging dir as $1. No single quotes inside.
own='install -o vmtool -g vmtool'
script="set -e"
script+="; test -d $hv || { echo \"$hv missing: run just prereqs $host first\" >&2; exit 1; }"
script+="; install -d -o vmtool -g vmtool -m 0700 $hv/tls"
script+="; $own -m 0755 \"\$1/vmtool\" $hv/vmtool"
script+="; $own -m 0600 \"\$1/config.yml\" $hv/config.yml"
script+="; $own -m 0644 \"\$1/tls/$fqdn.crt\" $hv/tls/$fqdn.crt"
script+="; $own -m 0600 \"\$1/tls/$fqdn.key\" $hv/tls/$fqdn.key"
script+="; install -m 0644 \"\$1/vmtool.service\" /etc/systemd/system/vmtool.service"
script+="; systemctl daemon-reload; systemctl enable --now vmtool; systemctl restart vmtool"
# -t so a sudo password prompt works; the token never rides this call.
ssh -t "$target" "s=\"$base/stage\"; sudo sh -c '$script' sh \"\$s\"; rc=\$?; rm -f \"\$s/config.yml\" \"\$s/tls/$fqdn.key\"; exit \$rc" \
  || die "installing on $host failed; the staged config.yml and key were removed. Re-run 'just deploy $host'."

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
    echo "Deployed vmtool to $target:$hv (runs as vmtool) and it answers on https://$fqdn:$port."
    exit 0
  fi
  ((n < retries)) || die "GET https://$fqdn:$port/version did not succeed; check 'systemctl status vmtool' on $host."
  sleep 1
done
