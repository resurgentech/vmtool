# Sourced by deploy.sh and install.sh (not run on its own).
#
# load_deploy_env <root> reads the optional settings file <root>/deploy.env
# (gitignored) for exactly three settings, none with a default:
#
#   VMTOOL_PERSONAL_KEYS_URL   the pulled key list's URL
#   VMTOOL_KEY_REQUEST_URL     the URL a daemon tells tools to file key requests at
#   BAO_KEYS_PULL_TOKEN_PATH   the OpenBao kv path of the list's pull token
#                              (the field is always "token")
#
# The parser is strict and never executes the file: a line is blank, a
# comment (# first), or KEY=value for one of those three keys (the value may be
# wrapped in one pair of matching quotes). Anything else stops the run with an
# error naming the line number, never its content. A variable already in the
# environment (even an empty one) wins over the file. The values are exported.
# The caller defines die().
load_deploy_env() {
  local f="$1/deploy.env" line n=0 key val k
  local -A found=()
  [[ -e $f ]] || return 0
  [[ -f $f && -r $f ]] || die "$f is not a readable file."
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    line="${line%$'\r'}"
    [[ $line =~ ^[[:space:]]*(#.*)?$ ]] && continue
    if [[ $line =~ ^(VMTOOL_PERSONAL_KEYS_URL|VMTOOL_KEY_REQUEST_URL|BAO_KEYS_PULL_TOKEN_PATH)=(.*)$ ]]; then
      key="${BASH_REMATCH[1]}"
      val="${BASH_REMATCH[2]}"
      if [[ $val =~ ^\"(.*)\"$ || $val =~ ^\'(.*)\'$ ]]; then val="${BASH_REMATCH[1]}"; fi
      found[$key]="$val"
    else
      die "$f line $n is not KEY=value for VMTOOL_PERSONAL_KEYS_URL, VMTOOL_KEY_REQUEST_URL or BAO_KEYS_PULL_TOKEN_PATH (nor blank, nor a # comment)."
    fi
  done <"$f"
  for k in "${!found[@]}"; do
    [[ -v $k ]] || { printf -v "$k" %s "${found[$k]}"; export "${k?}"; }
  done
}

# keys_settings_check validates the three settings once loaded: each value
# lands in a remote shell command (URLs) or a bao argument (the path), so only
# plain characters pass. Unset settings are fine here; what a set URL needs is
# the caller's business.
keys_settings_check() {
  local u
  for u in VMTOOL_PERSONAL_KEYS_URL VMTOOL_KEY_REQUEST_URL; do
    [[ -z ${!u:-} || ${!u} =~ ^https?://[A-Za-z0-9][A-Za-z0-9.:-]*(/[A-Za-z0-9._~/-]*)?$ ]] \
      || die "$u is not a plain URL (http(s)://host[:port]/path with letters, digits and . _ ~ / - only)."
  done
  [[ -z ${BAO_KEYS_PULL_TOKEN_PATH:-} || ${BAO_KEYS_PULL_TOKEN_PATH} =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ ]] \
    || die "BAO_KEYS_PULL_TOKEN_PATH is not a plain OpenBao kv path (like kv/example/keys/pull/vmtool)."
}
