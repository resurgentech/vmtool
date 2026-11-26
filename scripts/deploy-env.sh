# Sourced by cert.sh, deploy.sh, install.sh, store-image.sh, daemon-dist.sh, daemon-image.sh, token.sh and
# settings.sh (not run on its own).
#
# load_deploy_env <root> reads the optional settings file <root>/deploy.env
# (gitignored) for exactly these nine settings, none with a default except the last:
#
#   VMTOOL_DOMAIN              DNS domain of the host certificates (cert, install, deploy)
#   BAO_CA_PATH                the OpenBao kv path of the CA: <path>/crt and <path>/key
#                              (cert, install, deploy)
#   BAO_STORE_HOST_TOKENS_PATH the OpenBao kv path whose fields are the hosts'
#                              store tokens (deploy; optional)
#   BAO_TOKEN_PATH             the OpenBao kv path of the operator token
#                              (token-fetch; token stores it there when set)
#   VMTOOL_IMAGE_REGISTRY      host[:port][/path] the store image is pushed to (store-image)
#   VMTOOL_PERSONAL_KEYS_URL   the pulled key list's URL
#   VMTOOL_KEY_REQUEST_URL     the URL a daemon tells tools to file key requests at
#   BAO_KEYS_PULL_TOKEN_PATH   the OpenBao kv path of the list's pull token
#                              (the field is always "token")
#   VMTOOL_DAEMON_BUILD_BASE   the distro image `daemon-dist` and `daemon-image` build in; optional, the
#                              script falls back to a generic default (daemon-build-lib.sh)
#
# The parser is strict and never executes the file: a line is blank, a
# comment (# first), or KEY=value for one of those keys (the value may be
# wrapped in one pair of matching quotes). Anything else stops the run with an
# error naming the line number, never its content. A variable already in the
# environment (even an empty one) wins over the file. The values are exported.
# The caller defines die().

DEPLOY_ENV_KEYS=(VMTOOL_DOMAIN BAO_CA_PATH BAO_STORE_HOST_TOKENS_PATH BAO_TOKEN_PATH VMTOOL_IMAGE_REGISTRY
  VMTOOL_PERSONAL_KEYS_URL VMTOOL_KEY_REQUEST_URL BAO_KEYS_PULL_TOKEN_PATH VMTOOL_DAEMON_BUILD_BASE)

load_deploy_env() {
  local f="$1/deploy.env" line n=0 key val k
  local -A found=()
  [[ -e $f ]] || return 0
  [[ -f $f && -r $f ]] || die "$f is not a readable file."
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    line="${line%$'\r'}"
    [[ $line =~ ^[[:space:]]*(#.*)?$ ]] && continue
    if [[ $line =~ ^(VMTOOL_DOMAIN|BAO_CA_PATH|BAO_STORE_HOST_TOKENS_PATH|BAO_TOKEN_PATH|VMTOOL_IMAGE_REGISTRY|VMTOOL_PERSONAL_KEYS_URL|VMTOOL_KEY_REQUEST_URL|BAO_KEYS_PULL_TOKEN_PATH|VMTOOL_DAEMON_BUILD_BASE)=(.*)$ ]]; then
      key="${BASH_REMATCH[1]}"
      val="${BASH_REMATCH[2]}"
      if [[ $val =~ ^\"(.*)\"$ || $val =~ ^\'(.*)\'$ ]]; then val="${BASH_REMATCH[1]}"; fi
      found[$key]="$val"
    else
      die "$f line $n is not KEY=value for one of the deploy settings (VMTOOL_DOMAIN, BAO_CA_PATH, BAO_STORE_HOST_TOKENS_PATH, BAO_TOKEN_PATH, VMTOOL_IMAGE_REGISTRY, VMTOOL_PERSONAL_KEYS_URL, VMTOOL_KEY_REQUEST_URL, BAO_KEYS_PULL_TOKEN_PATH, VMTOOL_DAEMON_BUILD_BASE), nor blank, nor a # comment."
    fi
  done <"$f"
  for k in "${!found[@]}"; do
    [[ -v $k ]] || { printf -v "$k" %s "${found[$k]}"; export "${k?}"; }
  done
}

# require_setting <NAME>: stops, by name, when the setting is unset or empty.
require_setting() {
  [[ -n ${!1:-} ]] || die "set $1 in the settings file (deploy.env) or the environment."
}

# lab_settings_check validates the lab settings once loaded: each value lands
# in a remote shell command, a bao argument or a docker argument, so only
# plain characters pass. Unset settings are fine here; the recipe that needs
# one calls require_setting.
lab_settings_check() {
  local p
  [[ -z ${VMTOOL_DOMAIN:-} || ${VMTOOL_DOMAIN} =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] \
    || die "VMTOOL_DOMAIN is not a plain DNS domain (letters, digits, . and - only)."
  for p in BAO_CA_PATH BAO_STORE_HOST_TOKENS_PATH BAO_TOKEN_PATH; do
    [[ -z ${!p:-} || ${!p} =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ ]] \
      || die "$p is not a plain OpenBao kv path (like kv/example/vmtool/token)."
  done
  [[ -z ${VMTOOL_IMAGE_REGISTRY:-} || ${VMTOOL_IMAGE_REGISTRY} =~ ^[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?(/[A-Za-z0-9._/-]+)?$ ]] \
    || die "VMTOOL_IMAGE_REGISTRY is not a plain registry (host[:port][/path], like hub.example.lab)."
}

# keys_settings_check validates the three key-list settings once loaded: each
# value lands in a remote shell command (URLs) or a bao argument (the path), so
# only plain characters pass. Unset settings are fine here; what a set URL
# needs is the caller's business.
keys_settings_check() {
  local u
  for u in VMTOOL_PERSONAL_KEYS_URL VMTOOL_KEY_REQUEST_URL; do
    [[ -z ${!u:-} || ${!u} =~ ^https?://[A-Za-z0-9][A-Za-z0-9.:-]*(/[A-Za-z0-9._~/-]*)?$ ]] \
      || die "$u is not a plain URL (http(s)://host[:port]/path with letters, digits and . _ ~ / - only)."
  done
  [[ -z ${BAO_KEYS_PULL_TOKEN_PATH:-} || ${BAO_KEYS_PULL_TOKEN_PATH} =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ ]] \
    || die "BAO_KEYS_PULL_TOKEN_PATH is not a plain OpenBao kv path (like kv/example/keys/pull/vmtool)."
}

# The one setting with a default: the distro image the daemon is built in. An
# old, long-supported glibc, so the binary runs on anything newer.
DAEMON_BUILD_BASE_DEFAULT="ubuntu:22.04"
