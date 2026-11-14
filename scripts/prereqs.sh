#!/usr/bin/env bash
# Install vmtool's host prerequisites by running ansible/playbooks/setup_host.yml.
# Ansible itself is the one thing this cannot bootstrap, so it is checked first.
#
# Extra arguments are passed straight through to ansible-playbook:
#   scripts/prereqs.sh -e vmtool_install_go=true
#   scripts/prereqs.sh --check --diff
#
# A first argument that does not start with '-' is a host from config.yml
# (needs a built ./vmtool and a valid token, as 'vmtool config host' does):
#   scripts/prereqs.sh veritech01 [ansible-playbook options]
# That runs the playbook against the host over ssh with a temporary inventory,
# installing Go (-e vmtool_install_go=true) and opening the daemon port in ufw
# (-e vmtool_daemon_port=9473), and always asks for the become password.
# VMTOOL_ROOT (default: this checkout) is where config.yml and vmtool live;
# VMTOOL_BIN overrides the binary.
set -euo pipefail
cd "$(dirname "$0")/.."
repo="$PWD"

playbook="ansible/playbooks/setup_host.yml"

if ! command -v ansible-playbook >/dev/null 2>&1; then
  printf >&2 '%s\n' \
    "ERROR: ansible-playbook not found; install ansible first." \
    "" \
    "  Ubuntu / Debian:  sudo apt install ansible" \
    "  Omarchy / Arch:   sudo pacman -S --needed ansible"
  exit 1
fi

if [[ ! -f $playbook ]]; then
  echo "ERROR: $playbook not found (run this from a vmtool checkout)." >&2
  exit 1
fi

host=
if [[ ${1:-} != "" && ${1:-} != -* ]]; then
  host="$1"
  shift
  [[ $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || { echo "ERROR: invalid host name '$host'" >&2; exit 1; }
fi

if [[ -n $host ]]; then
  root="${VMTOOL_ROOT:-$repo}"
  vmtool_bin="${VMTOOL_BIN:-$root/vmtool}"
  [[ -x $vmtool_bin ]] || { echo "ERROR: $vmtool_bin not found; run 'just build' first." >&2; exit 1; }
  read -r addr ssh_user < <(cd "$root" && "$vmtool_bin" config host "$host") \
    || { echo "ERROR: 'vmtool config host $host' failed; is $host in config.yml?" >&2; exit 1; }
  [[ -n ${addr:-} && -n ${ssh_user:-} ]] || { echo "ERROR: no address/ssh_user for host $host in config.yml." >&2; exit 1; }
  inv=$(mktemp)
  trap 'rm -f "$inv"' EXIT
  printf '%s ansible_host=%s ansible_user=%s\n' "$host" "$addr" "$ssh_user" >"$inv"
  become=()
  if [[ ! " $* " == *" -K "* && ! " $* " == *" --ask-become-pass "* ]]; then
    become=(--ask-become-pass)
  fi
  echo "Running $playbook on $host ($ssh_user@$addr)"
  rc=0
  ansible-playbook -i "$inv" "$playbook" -e "vmtool_hosts=$host" -e vmtool_install_go=true \
    -e vmtool_daemon_port=9473 "${become[@]}" "$@" || rc=$?
  exit "$rc"
fi

# The playbook needs root. Ask for the sudo password unless sudo is passwordless
# or the caller already passed a --ask-become-pass/-K of their own.
become=()
if ! sudo -n true 2>/dev/null && [[ ! " $* " == *" -K "* && ! " $* " == *" --ask-become-pass "* ]]; then
  become=(--ask-become-pass)
fi

echo "Running $playbook"
exec ansible-playbook "$playbook" "${become[@]}" "$@"
