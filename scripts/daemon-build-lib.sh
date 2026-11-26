# Sourced by daemon-dist.sh and daemon-image.sh (not run on its own): what
# both pass to docker. The caller has sourced deploy-env.sh, defined die(),
# set `root` and run load_deploy_env and lab_settings_check.
#
# daemon_build_args sets, for the caller:
#   base    the build base image: VMTOOL_DAEMON_BUILD_BASE (environment, else
#           deploy.env), else the generic default in deploy-env.sh; checked
#           as a plain image reference before it reaches docker
#   rev     the commit stamp, as deploy.sh makes it: the full 40-digit HEAD,
#           with +dirty for a changed tree. A changed tree (tracked or not,
#           ignored files aside) is refused unless VMTOOL_ALLOW_DIRTY=1,
#           because the image build copies the working tree.
daemon_build_args() {
  base="${VMTOOL_DAEMON_BUILD_BASE:-$DAEMON_BUILD_BASE_DEFAULT}"
  [[ $base =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]] \
    || die "VMTOOL_DAEMON_BUILD_BASE is not a plain image reference (letters, digits and . _ / : @ - only, like ubuntu:22.04)."
  command -v git >/dev/null 2>&1 || die "git is needed to stamp the build with its commit."
  rev=$(git -C "$root" rev-parse HEAD 2>/dev/null) || die "$root is not a git checkout; the build is stamped with its commit."
  if [[ -n $(git -C "$root" status --porcelain 2>/dev/null) ]]; then
    [[ ${VMTOOL_ALLOW_DIRTY:-} == 1 ]] \
      || die "the tree has uncommitted changes; commit them, or set VMTOOL_ALLOW_DIRTY=1 to build anyway (stamped +dirty)."
    rev+="+dirty"
  fi
  [[ $rev =~ ^[0-9a-f]{40}(\+dirty)?$ ]] || die "git revision '$rev' of $root is not a 40-digit hash with an optional +dirty; not building with it."
}
