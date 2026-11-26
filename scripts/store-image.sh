#!/usr/bin/env bash
# Build the store image and push <registry>/vmtool-store:<tag> and :latest.
#
#   scripts/store-image.sh <tag>
#
# Run by a person (`just store-image <tag>`); log in to the registry first.
# Refuses an empty tag, `latest` and a tag docker would not take (exit 2).
#
# Environment:
#   VMTOOL_ROOT              the repo directory (default: the repo root)
#   VMTOOL_IMAGE_REGISTRY    host[:port][/path] to push to (required, no
#                            default): the environment, else deploy.env in
#                            VMTOOL_ROOT, the environment winning. A missing
#                            one stops the run by name before docker is called.
#
# No build argument and no credential is passed to docker.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${VMTOOL_ROOT:-$(cd "$here/.." && pwd)}"

die() { echo "ERROR: $*" >&2; exit 1; }
. "$here/deploy-env.sh"

tag="${1-}"
if [[ -z $tag || $tag == latest || ! $tag =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
  echo "store-image: give a version tag (not empty, not latest; letters, digits, . _ - only)" >&2
  exit 2
fi

load_deploy_env "$root"
lab_settings_check
require_setting VMTOOL_IMAGE_REGISTRY

image="$VMTOOL_IMAGE_REGISTRY/vmtool-store"
cd "$root"
docker build -f deploy/vmtool-store.Dockerfile -t "$image:$tag" -t "$image:latest" .
docker push "$image:$tag"
docker push "$image:latest"
