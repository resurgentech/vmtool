#!/usr/bin/env bash
# Build the daemon image and push <registry>/vmtool-daemon:<tag>.
#
#   scripts/daemon-image.sh <tag>        (`just daemon-image <tag>`)
#
# The image is FROM scratch with the daemon-dist files at /dist (nothing to
# run: take them out with `docker create` and `docker cp`, or add it as a
# build stage). It is built from this checkout in the base image, as
# daemon-dist.sh does. Run by a person; log in to the registry first. Refuses
# an empty tag, `latest` and a tag docker would not take (exit 2).
#
# Environment:
#   VMTOOL_ROOT              the repo directory (default: the repo root)
#   VMTOOL_IMAGE_REGISTRY    host[:port][/path] to push to (required, no
#                            default): the environment, else deploy.env. A
#                            missing one stops the run by name before docker
#                            is called.
#   VMTOOL_DAEMON_BUILD_BASE, VMTOOL_ALLOW_DIRTY   as in daemon-dist.sh
#
# No credential is passed to docker.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"

die() { echo "ERROR: $*" >&2; exit 1; }
. "$here/deploy-env.sh"
. "$here/daemon-build-lib.sh"

tag="${1-}"
if [[ -z $tag || $tag == latest || ! $tag =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
  echo "daemon-image: give a version tag (not empty, not latest; letters, digits, . _ - only)" >&2
  exit 2
fi

load_deploy_env "$root"
lab_settings_check
require_setting VMTOOL_IMAGE_REGISTRY
daemon_build_args

image="$VMTOOL_IMAGE_REGISTRY/vmtool-daemon"
cd "$root"
docker build -f deploy/vmtool-daemon.Dockerfile --target image \
  --build-arg "VMTOOL_DAEMON_BUILD_BASE=$base" --build-arg "VMTOOL_COMMIT=$rev" \
  -t "$image:$tag" .
docker push "$image:$tag"
