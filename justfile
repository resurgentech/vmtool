# vmtool — build, generate, test.
# `go generate ./internal/api/...` remains the required codegen entrypoint.

set shell := ["bash", "-euo", "pipefail", "-c"]

root := justfile_directory()
bin := "vmtool"
bao_token_path := "kv/infra/labtwo/vmtool/token"
domain := "labtwo"
bao_ca_path := "kv/infra/labtwo/selfsigned_cert"
bao_store_host_tokens_path := "kv/infra/labtwo/vmtool_store/host_tokens"
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
go_install_version := "1.24.0"

default:
    @just --list

# Generate a new API token into .env (and OpenBao if VAULT_ADDR/VAULT_TOKEN are set)
token:
    VMTOOL_ROOT="{{ root }}" BAO_TOKEN_PATH="{{ bao_token_path }}" "{{ root }}/scripts/token.sh" generate

# Fetch the API token from OpenBao into .env
token-fetch:
    VMTOOL_ROOT="{{ root }}" BAO_TOKEN_PATH="{{ bao_token_path }}" "{{ root }}/scripts/token.sh" fetch

# Issue a TLS cert for <host> from the OpenBao CA (workstation only); a remote host gets it in hv_dir/tls (one sudo) and vmtool is try-restarted
cert host:
    VMTOOL_ROOT="{{ root }}" VMTOOL_DOMAIN="{{ domain }}" BAO_CA_PATH="{{ bao_ca_path }}" CERT_DAYS="{{ cert_days }}" HV_DIR="{{ hv_dir }}" "{{ root }}/scripts/cert.sh" "{{ host }}"

# Build, certify, write config.yml and run vmtool as a systemd service on this machine
install:
    VMTOOL_ROOT="{{ root }}" VMTOOL_DOMAIN="{{ domain }}" BAO_CA_PATH="{{ bao_ca_path }}" CERT_DAYS="{{ cert_days }}" HV_DIR="{{ hv_dir }}" "{{ root }}/scripts/install.sh"

# Install host prerequisites (packages, libvirt, default network + pool; on a host also the vmtool user, /opt/vmtool, the vmtool pool and the host's daemon port in ufw); `just prereqs <host>` does it over ssh, or locally (ansible_connection=local) when <host> is this machine; the port is the host's `port` in config.yml (default 9473; this machine unlisted: VMTOOL_SELF_PORT, default 9474)
[positional-arguments]
prereqs *ARGS:
    VMTOOL_ROOT="{{ root }}" HV_DIR="{{ hv_dir }}" HV_POOL_DIR="{{ hv_pool_dir }}" GO_MIN="{{ go_min }}" GO_INSTALL_VERSION="{{ go_install_version }}" "{{ root }}/scripts/prereqs.sh" "$@"

# Build vmtool on <host> over ssh (after `just prereqs <host>`), install it into hv_dir with one sudo and run it as the vmtool user, as vmtool-daemon.service on the host's port (default 9473; refuses a port something else listens on; removes an older vmtool.service run from hv_dir); on this machine it runs locally, with no ssh, and an unlisted entry (VMTOOL_SELF_ADDRESS, VMTOOL_SELF_PORT, default 9474) is written by `vmtool config set-host` once /version answers
deploy host:
    VMTOOL_ROOT="{{ root }}" VMTOOL_DOMAIN="{{ domain }}" BAO_CA_PATH="{{ bao_ca_path }}" CERT_DAYS="{{ cert_days }}" BAO_STORE_HOST_TOKENS_PATH="{{ bao_store_host_tokens_path }}" HV_DIR="{{ hv_dir }}" HV_POOL_DIR="{{ hv_pool_dir }}" GO_MIN="{{ go_min }}" "{{ root }}/scripts/deploy.sh" "{{ host }}"

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

# Build the store image and push registry.labtwo/vmtool-store:<tag> and :latest (run by a person; refuses an empty tag or `latest`; log in to the registry first)
store-image tag:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "{{ tag }}" || "{{ tag }}" == "latest" ]]; then echo "store-image: give a version tag (not empty, not latest)" >&2; exit 2; fi
    cd "{{ root }}"
    docker build -f deploy/vmtool-store.Dockerfile -t registry.labtwo/vmtool-store:{{ tag }} -t registry.labtwo/vmtool-store:latest .
    docker push registry.labtwo/vmtool-store:{{ tag }}
    docker push registry.labtwo/vmtool-store:latest

# Remove the built binary
clean:
    rm -f "{{ root }}/{{ bin }}"
