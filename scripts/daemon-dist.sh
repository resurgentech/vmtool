#!/usr/bin/env bash
# Build the daemon in a container, for any recent glibc, into dist/.
#
#   scripts/daemon-dist.sh        (`just daemon-dist`)
#
# Builds ./cmd/vmtool with cgo and the libvirt_dlopen tag in a container of the
# base image (deploy/vmtool-daemon.Dockerfile), so the binary loads libvirt at
# run time and its glibc floor is the base's. Writes, in VMTOOL_ROOT/dist:
#   vmtool  vmtool-thin  vmtool-net  vmtool.service.in  SHA256SUMS
# and the two sudoers rules (vmtool-thin.sudoers, vmtool-net.sudoers).
# vmtool-thin and vmtool.service.in are the templates as they are in deploy/;
# whoever installs them renders them, as deploy.sh does.
#
# Environment:
#   VMTOOL_ROOT              the repo directory (default: the repo root)
#   VMTOOL_DAEMON_BUILD_BASE the base image (default ubuntu:22.04; the
#                            environment, else deploy.env)
#   VMTOOL_ALLOW_DIRTY=1     build a tree with uncommitted changes (stamped +dirty)
#
# The base must be Debian or Ubuntu family (it installs gcc with apt-get).
# No credential is passed to docker. A failed build leaves no dist/ behind
# (an older dist/ stays).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"

die() { echo "ERROR: $*" >&2; exit 1; }
. "$here/deploy-env.sh"
. "$here/daemon-build-lib.sh"

load_deploy_env "$root"
daemon_build_args

files=(vmtool vmtool-thin vmtool-net vmtool.service.in vmtool-thin.sudoers vmtool-net.sudoers)

cd "$root"
out="dist.new.$$"
trap 'rm -rf "$root/$out"' EXIT
rm -rf "$out"
docker build -f deploy/vmtool-daemon.Dockerfile --target dist \
  --build-arg "VMTOOL_DAEMON_BUILD_BASE=$base" --build-arg "VMTOOL_COMMIT=$rev" \
  --output "type=local,dest=$root/$out" .

# What came out: every expected file, and a SHA256SUMS that lists every file
# there is and matches it.
for f in "${files[@]}" SHA256SUMS; do
  [[ -f $out/$f ]] || die "the build did not produce $f."
done
n=0
for f in "$out"/*; do
  f="${f##*/}"
  [[ $f == SHA256SUMS ]] && continue
  n=$((n + 1))
  grep -Eq "^[0-9a-f]{64}  $f\$" "$out/SHA256SUMS" || die "SHA256SUMS does not list $f."
done
[[ $(wc -l <"$out/SHA256SUMS") -eq $n ]] || die "SHA256SUMS lists a file that is not in dist/."
(cd "$out" && sha256sum --check --quiet SHA256SUMS) || die "SHA256SUMS does not match the files."

rm -rf dist
mv "$out" dist
echo "dist/ built from $rev on $base:"
cat dist/SHA256SUMS
