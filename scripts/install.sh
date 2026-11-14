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
#   VMTOOL_DOMAIN, BAO_CA_PATH, CERT_DAYS   passed on to cert.sh
#
# cert.sh runs before `config write`, so it sees no config.yml on a first
# install and treats that as the default role, workstation. If a config.yml
# already says role: hypervisor, cert.sh refuses and install stops. The token
# is only checked for presence; it is never printed or put in the unit (the
# service reads .env from its working directory).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"
domain="${VMTOOL_DOMAIN:-labtwo}"
unit_path="${UNIT_PATH:-/etc/systemd/system/vmtool.service}"
template="$here/../deploy/vmtool.service.in"
export VMTOOL_ROOT="$root" VMTOOL_DOMAIN="$domain"

die() { echo "ERROR: $*" >&2; exit 1; }

# 1. A token must exist (env or .env) before anything is built or installed.
have_token=no
[[ -n ${VMTOOL_TOKEN:-} ]] && have_token=yes
if [[ $have_token == no && -f $root/.env ]] && grep -q '^VMTOOL_TOKEN=.' "$root/.env"; then
  have_token=yes
fi
[[ $have_token == yes ]] || die "no VMTOOL_TOKEN found; run 'just token' or 'just token-fetch' first."

user="$(id -un)"
# The unit is written unquoted, so refuse paths systemd would split or expand.
[[ $root != *[[:space:]%\"\'\\]* ]] || die "repo path '$root' has characters unsafe in a systemd unit."
[[ -f $template ]] || die "$template not found."

# 2. Build.
(cd "$root" && go build -o "$root/vmtool" ./cmd/vmtool)

# 3. Certificate for this host.
host="$("$here/cert.sh" --local-name)"
"$here/cert.sh" "$host"

# 4. config.yml (token hash comes from .env / VMTOOL_TOKEN).
(cd "$root" && "$root/vmtool" config write --name "$host" --role workstation \
  --listen 127.0.0.1:9473 \
  --tls-cert "tls/$host.$domain.crt" --tls-key "tls/$host.$domain.key")

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
