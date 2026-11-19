# vmtool-store image. Build from the repo root:
#   docker build -f deploy/vmtool-store.Dockerfile .
# (`just store-image <tag>` builds and pushes it.) No secret goes in: the
# config, token file and encryption key are mounted at /etc/vmtool-store and
# the S3 credentials come from the environment at run time. In a container
# config.yaml must set `listen: 0.0.0.0:8095` (the default is loopback);
# Caddy fronts it.

FROM golang:1.24.0-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/vmtool-store ./cmd/vmtool-store
COPY internal ./internal
COPY pkg ./pkg
COPY spec ./spec
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vmtool-store ./cmd/vmtool-store
# distroless has no shell: make the data and config directories here and copy
# them with their owner. 65532 is the image's nonroot user.
RUN mkdir -p /out/var/lib/vmtool-store /out/etc/vmtool-store \
 && chown -R 65532:65532 /out/var/lib/vmtool-store /out/etc/vmtool-store \
 && chmod 0700 /out/var/lib/vmtool-store /out/etc/vmtool-store

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vmtool-store /usr/local/bin/vmtool-store
COPY --from=build --chown=65532:65532 /out/var/lib/vmtool-store /var/lib/vmtool-store
COPY --from=build --chown=65532:65532 /out/etc/vmtool-store /etc/vmtool-store
USER 65532:65532
EXPOSE 8095
VOLUME ["/var/lib/vmtool-store", "/etc/vmtool-store"]
ENTRYPOINT ["/usr/local/bin/vmtool-store", "-config", "/etc/vmtool-store/config.yaml"]
