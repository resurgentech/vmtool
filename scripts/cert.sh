#!/usr/bin/env bash
# Issue a TLS certificate for a host, signed by the private CA held in OpenBao.
#
#   scripts/cert.sh <host>       tls/<host>.<domain>.{crt,key}; for a host other
#                                than this one, copy both to the host's staging
#                                dir ~/.cache/vmtool-deploy/stage/tls, drop the
#                                local key, then (one `ssh -t`, one sudo)
#                                install them into HV_DIR/tls owned by vmtool,
#                                remove the staged key and try-restart vmtool
#   scripts/cert.sh --local-name print this machine's host name (name: in
#                                config.yml, else `hostname -s`)
#
# Environment (the justfile passes these):
#   VMTOOL_ROOT     directory holding config.yml and tls/ (default: repo root)
#   VMTOOL_DOMAIN   DNS domain appended to the host name
#   BAO_CA_PATH     OpenBao kv path holding the CA: <path>/crt and <path>/key
#   CERT_DAYS       certificate lifetime in days
#   HV_DIR          daemon directory on a remote host (default /opt/vmtool)
#   CERT_STAGE_ONLY 1: stop after staging (deploy installs the files itself)
# REMOTE_DIR (the old home-directory layout) is ignored, with a warning.
#   VMTOOL_BIN      vmtool binary, used for `config host` (default <root>/vmtool)
#
# The CA key is read by `bao` straight into an openssl process substitution
# (a pipe); it is never written to disk. A missing config.yml counts as the
# default role, workstation, so `install` can run this before it writes one.
set -euo pipefail

root="${VMTOOL_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
domain="${VMTOOL_DOMAIN:-labtwo}"
bao_path="${BAO_CA_PATH:-kv/infra/labtwo/selfsigned_cert}"
days="${CERT_DAYS:-825}"
vmtool_bin="${VMTOOL_BIN:-$root/vmtool}"
cfg="$root/config.yml"

die() { echo "ERROR: $*" >&2; exit 1; }

# yaml_top <key>: value of a top-level scalar in config.yml (quotes and
# trailing comments stripped); empty if absent.
yaml_top() {
  [[ -f $cfg ]] || return 0
  awk -v k="$1" '
    $0 ~ "^" k ":" {
      v = $0; sub("^" k ":[ \t]*", "", v)
      sub(/[ \t]+#.*$/, "", v); gsub(/^["\047]|["\047]$/, "", v)
      sub(/[ \t]+$/, "", v); print v; exit
    }' "$cfg"
}

local_name() {
  local n
  n=$(yaml_top name)
  [[ -n $n ]] || n=$(hostname -s)
  printf '%s\n' "$n"
}

if [[ ${1:-} == --local-name ]]; then
  local_name
  exit 0
fi

host="${1:-}"
[[ -n $host && $# -eq 1 ]] || { echo "usage: $0 <host> | --local-name" >&2; exit 2; }
[[ $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid host name '$host'"

role=$(yaml_top role)
role="${role:-workstation}"
[[ $role == workstation ]] || die "role is '$role'; certificates are issued only on a workstation (role: workstation in config.yml)."

fqdn="$host.$domain"
tlsdir="$root/tls"
crt="$tlsdir/$fqdn.crt"
key="$tlsdir/$fqdn.key"

remote=no
if [[ $host != "$(local_name)" ]]; then
  remote=yes
  [[ -x $vmtool_bin ]] || die "$vmtool_bin not found; run 'just build' first."
  read -r addr ssh_user < <(cd "$root" && "$vmtool_bin" config host "$host") \
    || die "'vmtool config host $host' failed; is $host in config.yml?"
  [[ -n ${addr:-} && -n ${ssh_user:-} ]] || die "no address/ssh_user for host $host in config.yml."
  # Both form ssh's destination argument: nothing ssh could read as an option.
  [[ $ssh_user =~ ^[A-Za-z0-9_][A-Za-z0-9._-]*$ ]] || die "ssh_user '$ssh_user' for $host in config.yml is not a plain user name."
  [[ $addr =~ ^[A-Za-z0-9][A-Za-z0-9.:-]*$ ]] || die "address '$addr' for $host in config.yml is not a plain host name or IP."
  target="$ssh_user@$addr"
  hv="${HV_DIR:-/opt/vmtool}"
  # hv and fqdn land in a remote shell command unquoted.
  [[ $hv =~ ^/[A-Za-z0-9._/-]+$ ]] || die "HV_DIR '$hv' has characters unsafe in a shell command."
  [[ $domain =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "invalid domain '$domain'"
  [[ -z ${REMOTE_DIR:-} ]] || echo "WARNING: REMOTE_DIR is no longer used and is ignored; the certificate goes to HV_DIR ($hv)." >&2
fi

umask 077
mkdir -p "$tlsdir"
tmp=$(mktemp -d "$tlsdir/.cert.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$tmp/key" -out "$tmp/csr" -subj "/CN=$fqdn" 2>/dev/null \
  || die "openssl could not create the key and CSR."

# Random positive 128-bit serial (-CAcreateserial would write beside /dev/fd).
serial="0x1$(head -c 15 /dev/urandom | od -An -tx1 | tr -d ' \n')"

openssl x509 -req -in "$tmp/csr" -days "$days" -set_serial "$serial" \
  -CA <(bao kv get -field=string "$bao_path/crt") \
  -CAkey <(bao kv get -field=string "$bao_path/key") \
  -extfile <(printf '%s\n' \
    "subjectAltName=DNS:$fqdn,DNS:localhost,IP:127.0.0.1" \
    "extendedKeyUsage=serverAuth" \
    "keyUsage=digitalSignature" \
    "basicConstraints=CA:FALSE") \
  -out "$tmp/crt" 2>/dev/null \
  || die "signing failed; is OpenBao reachable (VAULT_ADDR / VAULT_TOKEN) and is the CA at $bao_path?"
[[ -s $tmp/crt ]] || die "signing produced no certificate."

chmod 600 "$tmp/key"
chmod 644 "$tmp/crt"
mv -f "$tmp/key" "$key"
mv -f "$tmp/crt" "$crt"
echo "Wrote $crt and $key (mode 0600)."

if [[ $remote == yes ]]; then
  # Staging dir in the login user's home; $HOME is expanded remotely.
  stage='$HOME/.cache/vmtool-deploy/stage'
  ssh "$target" "umask 077 && mkdir -p \"$stage/tls\" && chmod 700 \"\$HOME/.cache/vmtool-deploy\" \"$stage\" \"$stage/tls\"" \
    || die "ssh $target failed; the key stays at $key."
  # scp -p keeps the key's 0600; a relative path is under the remote home.
  scp -p "$crt" "$key" "$target:.cache/vmtool-deploy/stage/tls/" || die "scp to $target failed; the key stays at $key."
  rm -f "$key"
  echo "Copied to $target:~/.cache/vmtool-deploy/stage/tls/ and removed the local key."
  [[ ${CERT_STAGE_ONLY:-} != 1 ]] || exit 0

  # One sudo (run by `sh -c`, staging dir as $1; no single quotes inside).
  # -t so a password prompt works. The staged key is removed either way.
  own='install -o vmtool -g vmtool'
  script="set -e"
  script+="; test -d $hv || { echo \"$hv missing: run just prereqs $host first\" >&2; exit 1; }"
  script+="; install -d -o vmtool -g vmtool -m 0700 $hv/tls"
  script+="; $own -m 0644 \"\$1/tls/$fqdn.crt\" $hv/tls/$fqdn.crt"
  script+="; $own -m 0600 \"\$1/tls/$fqdn.key\" $hv/tls/$fqdn.key"
  script+="; systemctl try-restart vmtool"
  ssh -t "$target" "s=\"$stage\"; sudo sh -c '$script' sh \"\$s\"; rc=\$?; rm -f \"\$s/tls/$fqdn.key\"; exit \$rc" \
    || die "installing the certificate into $hv/tls on $host failed; the staged key was removed. Re-run 'just cert $host'."
  echo "Installed into $host:$hv/tls (owner vmtool); vmtool restarted if it was running."
fi
