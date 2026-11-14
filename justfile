# vmtool — build, generate, test.
# `go generate ./internal/api/...` remains the required codegen entrypoint.

set shell := ["bash", "-euo", "pipefail", "-c"]

root := justfile_directory()
bin := "vmtool"
bao_token_path := "kv/infra/labtwo/vmtool/token"
domain := "labtwo"
bao_ca_path := "kv/infra/labtwo/selfsigned_cert"
cert_days := "825"
# vmtool directory on remote hosts; empty means /home/<ssh_user>/vmtool
remote_dir := ""

default:
    @just --list

# Generate a new API token into .env (and OpenBao if VAULT_ADDR/VAULT_TOKEN are set)
token:
    VMTOOL_ROOT="{{ root }}" BAO_TOKEN_PATH="{{ bao_token_path }}" "{{ root }}/scripts/token.sh" generate

# Fetch the API token from OpenBao into .env
token-fetch:
    VMTOOL_ROOT="{{ root }}" BAO_TOKEN_PATH="{{ bao_token_path }}" "{{ root }}/scripts/token.sh" fetch

# Issue a TLS cert for <host> from the OpenBao CA (workstation only; remote hosts get it over scp)
cert host:
    VMTOOL_ROOT="{{ root }}" VMTOOL_DOMAIN="{{ domain }}" BAO_CA_PATH="{{ bao_ca_path }}" CERT_DAYS="{{ cert_days }}" REMOTE_DIR="{{ remote_dir }}" "{{ root }}/scripts/cert.sh" "{{ host }}"

# Build, certify, write config.yml and run vmtool as a systemd service on this machine
install:
    VMTOOL_ROOT="{{ root }}" VMTOOL_DOMAIN="{{ domain }}" BAO_CA_PATH="{{ bao_ca_path }}" CERT_DAYS="{{ cert_days }}" "{{ root }}/scripts/install.sh"

# Install host prerequisites (packages, libvirt, default network + pool)
prereqs *ARGS:
    "{{ root }}/scripts/prereqs.sh" {{ ARGS }}

# Build ./vmtool
build:
    cd "{{ root }}" && go build -o {{ bin }} ./cmd/vmtool

# Regenerate internal/api from spec/openapi/vmtool.yaml
generate:
    cd "{{ root }}" && go generate ./internal/api/...

# Fail if generated API code is stale
verify:
    "{{ root }}/scripts/verify-generate.sh"

# Run all Go tests
test:
    cd "{{ root }}" && go test ./...

# Pre-push: stale-generate check + tests
check: verify test

# Remove the built binary
clean:
    rm -f "{{ root }}/{{ bin }}"
