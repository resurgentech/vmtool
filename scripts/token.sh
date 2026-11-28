#!/usr/bin/env bash
# Manage the vmtool REST bearer token in .env.
#
#   scripts/token.sh generate   new random token -> .env (and OpenBao if reachable)
#   scripts/token.sh fetch      OpenBao -> .env
#
# Environment:
#   VMTOOL_ROOT      directory holding .env (default: the repo root)
#   BAO_TOKEN_PATH   OpenBao kv path of the token, from the environment or
#                    deploy.env in VMTOOL_ROOT (no default). `fetch` needs it;
#                    `generate` without it writes .env only and says so
#   VAULT_ADDR, VAULT_TOKEN   OpenBao access; no defaults: the environment, or .env in
#                    VMTOOL_ROOT when run through `just` (which sources it for these recipes)
#
# The token is never printed and never appears in any process's argv: it moves
# only through bash builtins, stdin and files.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"
envfile="$root/.env"

die() { echo "ERROR: $*" >&2; exit 1; }
. "$here/deploy-env.sh"
load_deploy_env "$root"
lab_settings_check
bao_path="${BAO_TOKEN_PATH:-}"

# write_env <token>: create .env (0600) or replace its VMTOOL_TOKEN line,
# keeping every other line. Atomic: temp file in the same dir, chmod, mv.
write_env() {
  local tok=$1 tmp line
  umask 077
  tmp=$(mktemp "$root/.env.XXXXXX")
  trap 'rm -f "$tmp"' EXIT
  if [[ -f $envfile ]]; then
    while IFS= read -r line || [[ -n $line ]]; do
      [[ $line == VMTOOL_TOKEN=* ]] && continue
      printf '%s\n' "$line" >>"$tmp"
    done <"$envfile"
  fi
  printf 'VMTOOL_TOKEN=%s\n' "$tok" >>"$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$envfile"
  trap - EXIT
}

cmd_generate() {
  local tok
  tok=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  [[ ${#tok} -eq 64 ]] || die "could not generate a random token"
  write_env "$tok"

  local stored=no
  if [[ -z $bao_path ]]; then
    echo "WARNING: BAO_TOKEN_PATH is not set (settings file or environment); token not stored in OpenBao." >&2
  elif [[ -z ${VAULT_ADDR:-} || -z ${VAULT_TOKEN:-} ]]; then
    echo "WARNING: VAULT_ADDR / VAULT_TOKEN not set; token not stored in OpenBao." >&2
  elif ! command -v bao >/dev/null 2>&1; then
    echo "WARNING: bao not found; token not stored in OpenBao." >&2
  elif ! bao status >/dev/null 2>&1; then
    echo "WARNING: OpenBao did not answer 'bao status'; token not stored there." >&2
  elif printf '%s' "$tok" | bao kv put "$bao_path" string=- >/dev/null; then
    stored=yes
  else
    echo "WARNING: 'bao kv put $bao_path' failed; token not stored in OpenBao." >&2
  fi

  echo "Wrote a new VMTOOL_TOKEN to $envfile (mode 0600)."
  if [[ $stored == yes ]]; then
    echo "Stored it in OpenBao at $bao_path."
  else
    echo "It is NOT in OpenBao; other workstations cannot 'just token-fetch' it."
  fi
  echo "Re-run 'just install' on this machine and 'just deploy <host>' for every host, so they all use the new token."
}

cmd_fetch() {
  require_setting BAO_TOKEN_PATH
  require_bao_session "$root"
  command -v bao >/dev/null 2>&1 || die "bao not found on PATH."
  local tok
  tok=$(bao kv get -field=string "$bao_path") || die "bao kv get $bao_path failed."
  [[ -n $tok ]] || die "OpenBao returned an empty value at $bao_path; run 'just token' first."
  write_env "$tok"
  echo "Wrote VMTOOL_TOKEN from OpenBao ($bao_path) to $envfile (mode 0600)."
}

case "${1:-}" in
  generate) cmd_generate ;;
  fetch) cmd_fetch ;;
  *) echo "usage: $0 generate|fetch" >&2; exit 2 ;;
esac
