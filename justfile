# vmtool — build, generate, test.
# `go generate ./internal/api/...` remains the required codegen entrypoint.

set shell := ["bash", "-euo", "pipefail", "-c"]

root := justfile_directory()
bin := "vmtool"
# The deploy settings (VMTOOL_DOMAIN, BAO_CA_PATH, BAO_STORE_HOST_TOKENS_PATH, BAO_TOKEN_PATH,
# VMTOOL_IMAGE_REGISTRY and the three key-list settings) have no value here and are not passed explicitly:
# just passes the environment through, and the scripts read each from it or, when it is not in the
# environment at all, from deploy.env in the checkout (which they parse themselves; the justfile does not
# load a dotenv file, it would read .env). `just settings` lists which are set.
cert_days := "825"
# Where the daemon lives on a hypervisor (owned by the vmtool user), and the
# directory behind its libvirt dir pool "vmtool".
hv_dir := "/opt/vmtool"
hv_pool_dir := "/var/lib/vmtool/images"
# Minimum Go on a hypervisor: the first release that honours GOTOOLCHAIN=auto,
# which then fetches the toolchain named in go.mod.
go_min := "1.21"
# Go that `just prereqs <host>` installs when the host has none (or too old):
# the `go` line of go.mod (a test keeps them equal).
go_install_version := "1.25.0"

default:
    @just --list

# Generate a new API token into .env (and OpenBao if VAULT_ADDR/VAULT_TOKEN are set)
token:
    VMTOOL_ROOT="{{ root }}" "{{ root }}/scripts/token.sh" generate

# Fetch the API token from OpenBao into .env (needs BAO_TOKEN_PATH)
token-fetch:
    VMTOOL_ROOT="{{ root }}" "{{ root }}/scripts/token.sh" fetch

# List every deploy setting as set or unset (never its value) and the recipes an unset one blocks; read-only
settings:
    VMTOOL_ROOT="{{ root }}" "{{ root }}/scripts/settings.sh"

# Issue a TLS cert for <host> from the OpenBao CA (workstation only; needs VMTOOL_DOMAIN and BAO_CA_PATH); a remote host gets it in hv_dir/tls (one sudo) and vmtool is try-restarted
cert host:
    VMTOOL_ROOT="{{ root }}" CERT_DAYS="{{ cert_days }}" HV_DIR="{{ hv_dir }}" "{{ root }}/scripts/cert.sh" "{{ host }}"

# Build, certify, write config.yml and run vmtool as a systemd service on this machine (needs VMTOOL_DOMAIN and BAO_CA_PATH)
install:
    VMTOOL_ROOT="{{ root }}" CERT_DAYS="{{ cert_days }}" HV_DIR="{{ hv_dir }}" "{{ root }}/scripts/install.sh"

# Install host prerequisites (packages, libvirt, default network + pool; on a host also the vmtool user, /opt/vmtool, the vmtool pool and the host's daemon port in ufw); `just prereqs <host>` does it over ssh, or locally (ansible_connection=local) when <host> is this machine; the port is the host's `port` in config.yml (default 9473; this machine unlisted: VMTOOL_SELF_PORT, default 9474)
[positional-arguments]
prereqs *ARGS:
    VMTOOL_ROOT="{{ root }}" HV_DIR="{{ hv_dir }}" HV_POOL_DIR="{{ hv_pool_dir }}" GO_MIN="{{ go_min }}" GO_INSTALL_VERSION="{{ go_install_version }}" "{{ root }}/scripts/prereqs.sh" "$@"

# Build vmtool on <host> over ssh (after `just prereqs <host>`), install it into hv_dir with one sudo and run it as the vmtool user, as vmtool-daemon.service on the host's port (default 9473; refuses a port something else listens on; removes an older vmtool.service run from hv_dir); on this machine it runs locally, with no ssh, and an unlisted entry (VMTOOL_SELF_ADDRESS, VMTOOL_SELF_PORT, default 9474) is written by `vmtool config set-host` once /version answers
deploy host:
    VMTOOL_ROOT="{{ root }}" CERT_DAYS="{{ cert_days }}" HV_DIR="{{ hv_dir }}" HV_POOL_DIR="{{ hv_pool_dir }}" GO_MIN="{{ go_min }}" "{{ root }}/scripts/deploy.sh" "{{ host }}"

# Build ./vmtool
build:
    cd "{{ root }}" && go build -o {{ bin }} ./cmd/vmtool

# Regenerate internal/api and internal/storeapi from spec/openapi/{vmtool,store}.yaml
generate:
    cd "{{ root }}" && go generate ./internal/api/... ./internal/storeapi/...

# Fail if generated API code is stale
verify:
    "{{ root }}/scripts/verify-generate.sh"

# Run Go tests (default ./...) in a capped systemd --user scope (scripts/test-capped.sh)
test *ARGS:
    cd "{{ root }}" && "{{ root }}/scripts/test-capped.sh" {{ ARGS }}

# Pre-push: stale-generate check + tests
check: verify test

# Build the store image and push <VMTOOL_IMAGE_REGISTRY>/vmtool-store:<tag> and :latest (run by a person; needs VMTOOL_IMAGE_REGISTRY; refuses an empty tag or `latest`; log in to the registry first)
[positional-arguments]
store-image tag:
    VMTOOL_ROOT="{{ root }}" "{{ root }}/scripts/store-image.sh" "$1"

# Remove the built binary
clean:
    rm -f "{{ root }}/{{ bin }}"
