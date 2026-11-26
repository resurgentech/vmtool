#!/usr/bin/env bash
# List every deploy setting as set or unset, and what an unset one blocks.
#
#   scripts/settings.sh        (`just settings`)
#
# Read-only: it changes nothing, calls no other tool and never prints a
# value, only the names, whether each is set (non-empty) and where it comes
# from (the environment, or deploy.env in VMTOOL_ROOT; an empty variable in
# the environment wins over the file). It exits 0 always, also when
# deploy.env has a bad line (it then says which line, never its content).
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"

# die only reports: load_deploy_env's messages name the file and the line
# number, never a line's content. Run in a subshell below, so exit is safe.
die() { echo "$*"; exit 1; }
. "$here/deploy-env.sh"

# What an unset setting blocks (required) or skips (optional).
declare -A needs=(
  [VMTOOL_DOMAIN]="blocks: cert, install, deploy"
  [BAO_CA_PATH]="blocks: cert, install, deploy"
  [BAO_STORE_HOST_TOKENS_PATH]="optional; unset: deploy installs no store host token (one warning)"
  [BAO_TOKEN_PATH]="blocks: token-fetch; token writes .env only and does not store the token in OpenBao"
  [VMTOOL_IMAGE_REGISTRY]="blocks: store-image"
  [VMTOOL_PERSONAL_KEYS_URL]="optional; unset: install and deploy leave the pulled key list off"
  [VMTOOL_KEY_REQUEST_URL]="optional; unset: install and deploy tell tools no key request URL"
  [BAO_KEYS_PULL_TOKEN_PATH]="optional; install and deploy need it only when VMTOOL_PERSONAL_KEYS_URL is set"
)

declare -A inenv=()
for k in "${DEPLOY_ENV_KEYS[@]}"; do
  if [[ -v $k ]]; then inenv[$k]=1; fi
done

# Load deploy.env in a subshell (a bad line stops it there, not here) and
# list, by name only, the settings that are non-empty afterwards.
bad=""
fromfile=" "
if out=$(load_deploy_env "$root"; for k in "${DEPLOY_ENV_KEYS[@]}"; do if [[ -n ${!k:-} ]]; then echo "$k"; fi; done); then
  fromfile=" ${out//$'\n'/ } "
else
  bad="deploy.env is not usable, so what it sets is not shown: $out"
fi

echo "Deploy settings (names only, never values). Set them in the environment or in deploy.env in $root."
for k in "${DEPLOY_ENV_KEYS[@]}"; do
  if [[ -v inenv[$k] ]]; then
    if [[ -n ${!k:-} ]]; then
      printf '%-28s set (environment)\n' "$k"
    else
      printf '%-28s unset (empty in the environment)  %s\n' "$k" "${needs[$k]}"
    fi
  elif [[ $fromfile == *" $k "* ]]; then
    printf '%-28s set (deploy.env)\n' "$k"
  else
    printf '%-28s unset  %s\n' "$k" "${needs[$k]}"
  fi
done
if [[ -n $bad ]]; then echo "$bad"; fi
exit 0
