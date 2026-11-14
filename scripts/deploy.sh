#!/usr/bin/env bash
# Build vmtool on a hypervisor and run it there as a systemd service.
#
#   scripts/deploy.sh <host>
#
# Steps, in order (nothing is copied, built or restarted if the first fails):
#   1. go check    `go env GOVERSION` over ssh must be >= GO_MIN (the first Go
#                  that honours GOTOOLCHAIN=auto, which fetches the go.mod one)
#   2. copy        git ls-files | tar over ssh into <remote_dir> (tracked files
#                  only: no .env, .machines, config.yml or tls/)
#   3. build       go build on the host (cgo libvirt), GOTOOLCHAIN=auto
#   4. cert        scripts/cert.sh <host> (issues, copies to <remote_dir>/tls)
#   5. config      remote `vmtool config write --token-stdin`; the token goes on
#                  ssh stdin, never in argv, and no plain token is stored there
#   6. unit        rendered deploy/vmtool.service.in, sent on stdin to a temp
#                  file, then `ssh -t sudo install` + systemctl (-t so a sudo
#                  password prompt works; the token never rides that call)
#   7. check       GET /version over HTTPS, header read from stdin
#
# Environment (the justfile passes these):
#   VMTOOL_ROOT, VMTOOL_DOMAIN, BAO_CA_PATH, CERT_DAYS   as for cert.sh
#   REMOTE_DIR     default /home/<ssh_user>/vmtool
#   GO_MIN         minimum Go as major.minor (default 1.21)
#   VMTOOL_BIN     local vmtool, used for `config host` (default <root>/vmtool)
#   DEPLOY_RETRIES seconds to wait for /version after restart (default 15)
# The token comes from VMTOOL_TOKEN, else .env in VMTOOL_ROOT, read with bash
# builtins so it is never in any argv.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"
domain="${VMTOOL_DOMAIN:-labtwo}"
go_min="${GO_MIN:-1.21}"
retries="${DEPLOY_RETRIES:-15}"
vmtool_bin="${VMTOOL_BIN:-$root/vmtool}"
template="$here/../deploy/vmtool.service.in"
port=9473
export VMTOOL_ROOT="$root" VMTOOL_DOMAIN="$domain" VMTOOL_BIN="$vmtool_bin"

die() { echo "ERROR: $*" >&2; exit 1; }

host="${1:-}"
[[ -n $host && $# -eq 1 ]] || { echo "usage: $0 <host>" >&2; exit 2; }
[[ $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid host name '$host'"

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
target="$ssh_user@$addr"
rdir="${REMOTE_DIR:-/home/$ssh_user/vmtool}"
# rdir lands in remote shell commands and a systemd unit unquoted.
[[ $rdir =~ ^/[A-Za-z0-9._/-]+$ ]] || die "remote dir '$rdir' has characters unsafe in a shell command or systemd unit."
[[ -f $template ]] || die "$template not found."
fqdn="$host.$domain"

# 1. Go check, before anything is copied or built.
gopath='PATH=$PATH:/usr/local/go/bin'
rc=0
found=$(ssh "$target" "bash -lc '$gopath; go env GOVERSION'") || rc=$?
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

# 2. Copy tracked files.
echo "Copying tracked files to $target:$rdir"
git -C "$root" ls-files -z | tar -C "$root" --null -T - -czf - \
  | ssh "$target" "mkdir -p $rdir && tar -xzf - -C $rdir"

# 3. Build on the host.
echo "Building vmtool on $host"
ssh "$target" "bash -lc 'cd $rdir && $gopath GOTOOLCHAIN=auto go build -o vmtool ./cmd/vmtool'"

# 4. Certificate (cert.sh copies it to <rdir>/tls and drops the local key).
REMOTE_DIR="$rdir" "$here/cert.sh" "$host"

# 5. config.yml on the host; the token rides stdin of its own ssh call.
printf '%s\n' "$tok" | ssh "$target" "cd $rdir && ./vmtool config write --token-stdin --role hypervisor --name $host --listen 0.0.0.0:$port --tls-cert tls/$fqdn.crt --tls-key tls/$fqdn.key"

# 6. Unit: send the rendered file, then install and restart with sudo.
content="$(<"$template")"
content="${content//@USER@/$ssh_user}"
content="${content//@DIR@/$rdir}"
printf '%s\n' "$content" | ssh "$target" 'cat > "$HOME/.vmtool.service.new"'
ssh -t "$target" "sudo install -m 0644 \$HOME/.vmtool.service.new /etc/systemd/system/vmtool.service && rm -f \$HOME/.vmtool.service.new && sudo systemctl daemon-reload && sudo systemctl enable --now vmtool && sudo systemctl restart vmtool"

# 7. GET /version (header from stdin; verified against the system trust store).
# The cert names <host>.<domain>, so that is the URL. curl's --resolve takes
# only a numeric address: pin it when config.yml gives an IP, else use DNS.
resolve=()
if [[ $addr =~ ^[0-9.]+$ || $addr == *:* ]]; then
  resolve=(--resolve "$fqdn:$port:$addr")
fi
n=0
while :; do
  n=$((n + 1))
  if printf 'Authorization: Bearer %s\n' "$tok" \
    | curl --fail -sS "${resolve[@]}" -H @- "https://$fqdn:$port/version"; then
    echo
    echo "Deployed vmtool to $target:$rdir and it answers on https://$fqdn:$port."
    exit 0
  fi
  ((n < retries)) || die "GET https://$fqdn:$port/version did not succeed; check 'systemctl status vmtool' on $host."
  sleep 1
done
