# The vmtool daemon, built to run on any recent glibc. Build from the repo root:
#   docker build -f deploy/vmtool-daemon.Dockerfile --target dist --output type=local,dest=dist .
# (`just daemon-dist` does this and checks the result; `just daemon-image <tag>`
# builds and pushes the `image` target.)
#
# The binary is built with cgo and the libvirt_dlopen tag: it needs no libvirt
# headers to build and loads libvirt.so.0 when it first opens a libvirt
# connection, so it starts on a host without libvirt and runs on the host's.
# cgo stays, so the glibc of the base is the oldest the binary runs on. The base
# is a plain Debian or Ubuntu family image (it installs gcc with apt-get); pick
# an old, long-supported one. The Go toolchain comes from the golang image.
#
# Stages: `dist` holds the files at / (for --output), `image` the same under
# /dist in a FROM scratch image. No secret goes in; the two build arguments are
# the base image and the commit to stamp.

ARG VMTOOL_DAEMON_BUILD_BASE=ubuntu:22.04

FROM golang:1.25.14-bookworm AS go

FROM ${VMTOOL_DAEMON_BUILD_BASE} AS build
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends gcc libc6-dev ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY cmd/vmtool ./cmd/vmtool
COPY internal ./internal
COPY pkg ./pkg
COPY spec ./spec
ARG VMTOOL_COMMIT
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -tags libvirt_dlopen \
      -ldflags "-X github.com/resurgentech/vmtool/internal/buildinfo.stamped=${VMTOOL_COMMIT}" \
      -o /out/vmtool ./cmd/vmtool
COPY deploy/vmtool-thin deploy/vmtool-net deploy/vmtool.service.in deploy/vmtool-thin.sudoers deploy/vmtool-net.sudoers /out/
RUN cd /out && sha256sum * > /tmp/SHA256SUMS && mv /tmp/SHA256SUMS /out/SHA256SUMS

FROM scratch AS dist
COPY --from=build /out/ /

FROM scratch AS image
COPY --from=build /out/ /dist/
