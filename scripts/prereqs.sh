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
# That runs the playbook against the host over ssh with a temporary inventory
# as a hypervisor (-e vmtool_role=hypervisor): the vmtool user, HV_DIR, the
# vmtool pool at HV_POOL_DIR, Go only when missing or older than GO_MIN
# (GO_INSTALL_VERSION is what gets installed), and the daemon port in ufw
# (-e vmtool_daemon_port=<port>: the host's `port` in config.yml, default
# 9473; `vmtool config host <host>` prints "address ssh_user port"). It always
# asks for the become password. When <host> is this machine (the name that
# scripts/cert.sh --local-name prints) the inventory uses
# ansible_connection=local, with no ssh; if config.yml does not list it, the
# port is VMTOOL_SELF_PORT (default 9474).
# Opt-in NVMe stripe (destructive by nature, runs only with the drives named,
# and refuses non-blank drives): pass the drives as extra arguments, e.g.
#   just prereqs veritech02 -e '{"vmtool_nvme_drives":["/dev/disk/by-id/nvme-A","/dev/disk/by-id/nvme-B"]}'
# (scripts/prereqs.sh takes the same arguments directly). It builds an md RAID 0 array, a volume group and a libvirt logical pool
# `nvme` (ansible/playbooks/tasks/nvme_stripe.yml). Without it nothing happens.
# VMTOOL_ROOT (default: this checkout) is where config.yml and vmtool live;
# VMTOOL_BIN overrides the binary. The justfile sets HV_DIR (/opt/vmtool),
# HV_POOL_DIR (/var/lib/vmtool/images), GO_MIN (1.21) and GO_INSTALL_VERSION
# (the go line of go.mod); the defaults here match.
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
  # This machine? The same test as cert.sh and deploy.sh.
  local_name=$(VMTOOL_ROOT="$root" "$repo/scripts/cert.sh" --local-name) || { echo "ERROR: could not work out this machine's name." >&2; exit 1; }
  is_local=no
  [[ $host != "$local_name" ]] || is_local=yes
  # "address ssh_user port" from config.yml; this machine, when not listed,
  # uses VMTOOL_SELF_PORT (default 9474), as deploy.sh does.
  rc=0
  out=$(cd "$root" && "$vmtool_bin" config host "$host" 2>&1) || rc=$?
  if [[ $rc -eq 0 ]]; then
    read -r addr ssh_user port <<<"$out" || true
    port="${port:-9473}"
    [[ -n ${addr:-} && -n ${ssh_user:-} ]] || { echo "ERROR: no address/ssh_user for host $host in config.yml." >&2; exit 1; }
  elif [[ $is_local == yes && $out == *"unknown host"* ]]; then
    addr=
    ssh_user=
    port="${VMTOOL_SELF_PORT:-9474}"
  else
    echo "ERROR: 'vmtool config host $host' failed (is $host in config.yml?): $out" >&2
    exit 1
  fi
  [[ $port =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || { echo "ERROR: port '$port' for $host is not a port number." >&2; exit 1; }
  port=$((10#$port))
  inv=$(mktemp)
  trap 'rm -f "$inv"' EXIT
  if [[ $is_local == yes ]]; then
    printf '%s ansible_connection=local\n' "$host" >"$inv"
    where="this machine"
  else
    printf '%s ansible_host=%s ansible_user=%s\n' "$host" "$addr" "$ssh_user" >"$inv"
    where="$ssh_user@$addr"
  fi
  become=()
  if [[ ! " $* " == *" -K "* && ! " $* " == *" --ask-become-pass "* ]]; then
    become=(--ask-become-pass)
  fi
  hv_dir="${HV_DIR:-/opt/vmtool}"
  hv_pool_dir="${HV_POOL_DIR:-/var/lib/vmtool/images}"
  go_min="${GO_MIN:-1.21}"
  go_install="${GO_INSTALL_VERSION:-}"
  if [[ -z $go_install ]]; then
    go_install=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "$repo/go.mod" | head -n1)
  fi
  [[ -n $go_install ]] || { echo "ERROR: no Go version for GO_INSTALL_VERSION (go.mod has no go line)." >&2; exit 1; }
  echo "Running $playbook on $host ($where)"
  rc=0
  ansible-playbook -i "$inv" "$playbook" -e "vmtool_hosts=$host" -e vmtool_role=hypervisor \
    -e "vmtool_hv_dir=$hv_dir" -e "vmtool_hv_pool_dir=$hv_pool_dir" \
    -e "vmtool_go_min=$go_min" -e "vmtool_go_install_version=$go_install" \
    -e vmtool_install_go=true -e "vmtool_daemon_port=$port" "${become[@]}" "$@" || rc=$?
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
