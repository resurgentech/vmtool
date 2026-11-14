#!/usr/bin/env bash
# Issue a TLS certificate for a host, signed by the private CA held in OpenBao.
#
#   scripts/cert.sh <host>       tls/<host>.<domain>.{crt,key}; for a host other
#                                than this one, copy both to the host and drop
#                                the local key
#   scripts/cert.sh --local-name print this machine's host name (name: in
#                                config.yml, else `hostname -s`)
#
# Environment (the justfile passes these):
#   VMTOOL_ROOT     directory holding config.yml and tls/ (default: repo root)
#   VMTOOL_DOMAIN   DNS domain appended to the host name
#   BAO_CA_PATH     OpenBao kv path holding the CA: <path>/crt and <path>/key
#   CERT_DAYS       certificate lifetime in days
#   REMOTE_DIR      vmtool directory on the remote host
#                   (default /home/<ssh_user>/vmtool)
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
  rdir="${REMOTE_DIR:-/home/$ssh_user/vmtool}"
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
  ssh "$ssh_user@$addr" mkdir -p "$rdir/tls" || die "ssh $ssh_user@$addr failed; the key stays at $key."
  scp -p "$crt" "$key" "$ssh_user@$addr:$rdir/tls/" || die "scp to $ssh_user@$addr failed; the key stays at $key."
  ssh "$ssh_user@$addr" chmod 600 "$rdir/tls/$fqdn.key" || die "could not chmod the remote key; the key stays at $key."
  rm -f "$key"
  echo "Copied to $ssh_user@$addr:$rdir/tls/ and removed the local key."
fi
