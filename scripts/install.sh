#!/usr/bin/env bash
# Install vmtool as a systemd system service on this (workstation) machine.
#
#   scripts/install.sh
#
# Steps: check a token exists, go build, cert for this host, config.yml (role
# workstation, TLS paths, listen 127.0.0.1:9473), render deploy/vmtool.service.in
# to the unit path with sudo, daemon-reload, enable --now, restart.
#
# Environment:
#   VMTOOL_ROOT   the repo directory (holds .env, config.yml, .machines/)
#   UNIT_PATH     where the unit goes (default /etc/systemd/system/vmtool.service)
#   HV_DIR        where a hypervisor daemon lives (default /opt/vmtool); a
#                 vmtool.service at UNIT_PATH run from there is refused
#   VMTOOL_DOMAIN, BAO_CA_PATH   required, no defaults: the environment or
#                 deploy.env in VMTOOL_ROOT (the environment wins); a missing
#                 one stops the run by name before anything is built or
#                 installed. `just settings` lists them. Passed on to cert.sh.
#   CERT_DAYS     passed on to cert.sh
#
# cert.sh runs before `config write`, so it sees no config.yml on a first
# install and treats that as the default role, workstation. If a config.yml
# already says role: hypervisor, cert.sh refuses and install stops. The token
# is only checked for presence; it is never printed or put in the unit (the
# service reads .env from its working directory). With no VMTOOL_TOKEN in the
# environment or .env it stops, before any build or restart, and says to export
# the operator token for this shell; it never writes .env.
#
# Key list (off unless set; no defaults; also read from deploy.env in
# VMTOOL_ROOT, the environment wins):
#   VMTOOL_PERSONAL_KEYS_URL   set: the pull token is read from OpenBao
#                  (`bao kv get -field=token "$BAO_KEYS_PULL_TOKEN_PATH"`) into
#                  a variable and written to keys-pull-token in VMTOOL_ROOT
#                  (0600, this user); a missing or empty one stops the install
#                  before the build; config write gets --personal-keys-url,
#                  --personal-keys-token-file keys-pull-token and
#                  --personal-keys-cache-file personal-keys.json (the cache
#                  is personal-keys.json in VMTOOL_ROOT)
#                  unset: keys-pull-token and personal-keys.json in
#                  VMTOOL_ROOT are removed and config write gets
#                  --no-personal-keys, which drops the personal_keys block
#                  first (the key request URL below is then set again)
#   VMTOOL_KEY_REQUEST_URL     set: config write gets --key-request-url
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"
unit_path="${UNIT_PATH:-/etc/systemd/system/vmtool.service}"
hv_dir="${HV_DIR:-/opt/vmtool}"
template="$here/../deploy/vmtool.service.in"
export VMTOOL_ROOT="$root"

die() { echo "ERROR: $*" >&2; exit 1; }
. "$here/deploy-env.sh"
load_deploy_env "$root"
lab_settings_check
keys_settings_check
# The lab's values: a missing one stops here, before the build, the
# certificate, any sudo or restart.
require_setting VMTOOL_DOMAIN
require_setting BAO_CA_PATH
domain="$VMTOOL_DOMAIN"
export VMTOOL_DOMAIN BAO_CA_PATH

# 1. A token must exist (env or .env) before anything is built or installed.
have_token=no
[[ -n ${VMTOOL_TOKEN:-} ]] && have_token=yes
if [[ $have_token == no && -f $root/.env ]] && grep -q '^VMTOOL_TOKEN=.' "$root/.env"; then
  have_token=yes
fi
[[ $have_token == yes ]] || die "no VMTOOL_TOKEN found; export the operator token for this shell (export VMTOOL_TOKEN=...) and run this again ('just token-fetch' puts it in .env)."

user="$(id -un)"
# The unit is written unquoted, so refuse paths systemd would split or expand.
[[ $root != *[[:space:]%\"\'\\]* ]] || die "repo path '$root' has characters unsafe in a systemd unit."
[[ -f $template ]] || die "$template not found."

# An older hypervisor daemon is a vmtool.service run from HV_DIR; overwriting it
# with this workstation unit would take the daemon down. deploy replaces it.
if grep -Fqx "WorkingDirectory=$hv_dir" "$unit_path" 2>/dev/null; then
  die "$unit_path is a hypervisor daemon's vmtool.service (WorkingDirectory=$hv_dir); run 'just deploy <host>' first, which replaces it with vmtool-daemon.service."
fi

# The pulled key list, only when VMTOOL_PERSONAL_KEYS_URL is set: the pull
# token goes from bao into a variable (never an argv) and is checked here, so a
# missing one stops the install before anything is built or installed.
keys_args=()
pull_tok=""
if [[ -n ${VMTOOL_PERSONAL_KEYS_URL:-} ]]; then
  [[ -n ${BAO_KEYS_PULL_TOKEN_PATH:-} ]] || die "VMTOOL_PERSONAL_KEYS_URL is set but BAO_KEYS_PULL_TOKEN_PATH (the OpenBao kv path of the pull token, field token) is not; nothing was installed."
  pull_tok=$(bao kv get -field=token "$BAO_KEYS_PULL_TOKEN_PATH" 2>/dev/null) || pull_tok=""
  pull_tok="${pull_tok//[$' \t\r\n']/}"
  [[ -n $pull_tok ]] || die "no pull token at $BAO_KEYS_PULL_TOKEN_PATH (field token); nothing was installed."
  keys_args=(--personal-keys-url "$VMTOOL_PERSONAL_KEYS_URL" --personal-keys-token-file keys-pull-token --personal-keys-cache-file personal-keys.json)
else
  # List off: drop the block (config.yml keeps its other settings) and the two
  # files. Removed with the config write below, after the checks above.
  keys_args=(--no-personal-keys)
  keys_off=1
fi
[[ -z ${VMTOOL_KEY_REQUEST_URL:-} ]] || keys_args+=(--key-request-url "$VMTOOL_KEY_REQUEST_URL")

# 2. Build.
(cd "$root" && go build -o "$root/vmtool" ./cmd/vmtool)

# 3. Certificate for this host.
host="$("$here/cert.sh" --local-name)"
"$here/cert.sh" "$host"

# 4. config.yml (token hash comes from .env / VMTOOL_TOKEN). The pull token
#    file is written first, 0600 for this user; config.yml names it by path.
if [[ -n $pull_tok ]]; then
  (umask 077; printf '%s\n' "$pull_tok" >"$root/keys-pull-token")
  chmod 600 "$root/keys-pull-token"
fi
[[ -z ${keys_off:-} ]] || rm -f "$root/keys-pull-token" "$root/personal-keys.json"
(cd "$root" && "$root/vmtool" config write --name "$host" --role workstation \
  --listen 127.0.0.1:9473 \
  --tls-cert "tls/$host.$domain.crt" --tls-key "tls/$host.$domain.key" "${keys_args[@]}")

# 5. Render the unit and install it.
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
content="$(<"$template")"
content="${content//@USER@/$user}"
content="${content//@DIR@/$root}"
printf '%s\n' "$content" >"$tmp"
sudo install -m 0644 "$tmp" "$unit_path"

# 6. Start it.
sudo systemctl daemon-reload
sudo systemctl enable --now vmtool
sudo systemctl restart vmtool
echo "Installed $unit_path and started vmtool (https://localhost:9473, as $user, in $root)."
