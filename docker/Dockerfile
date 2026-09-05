# KeibiDrop serving peer for always-on boxes (NAS, home server, VPS).
#
# The image holds the kd daemon only. The serving side never mounts, so the
# container needs no FUSE, no /dev/fuse and no extra capabilities. The machine
# that reads the files runs the desktop app or kd with its own FUSE driver.
#
# Build from the repository root:
#   docker build -f docker/Dockerfile -t keibidrop:dev .
#
# cgofuse is compiled in like every other KeibiDrop binary and loads libfuse
# with dlopen at mount time only, so the runtime stage ships no fuse library.

FROM golang:1.26-bookworm AS build

RUN apt-get update \
 && apt-get install -y --no-install-recommends libfuse-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg
COPY grpc_bindings ./grpc_bindings

ARG VERSION=dev
ARG COMMIT=unknown
ENV CGO_ENABLED=1
RUN go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=${VERSION} -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=${COMMIT}" \
      -o /out/kd ./cmd/kd

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 1000 keibidrop \
 && useradd --uid 1000 --gid 1000 --home-dir /config --no-create-home --shell /usr/sbin/nologin keibidrop \
 && mkdir -p /config /shares \
 && chown keibidrop:keibidrop /config /shares \
 && chmod 700 /config

COPY --from=build /out/kd /usr/local/bin/kd
COPY docker/entrypoint.sh /usr/local/bin/keibidrop-entrypoint
RUN chmod 755 /usr/local/bin/keibidrop-entrypoint

# Every setting is an environment variable; the entrypoint documents them.
# /config: identity (private key), contacts, config.toml. Treat it like ~/.ssh.
# /shares: the folder this box serves.
ENV HOME=/config \
    KEIBIDROP_CONFIG_DIR=/config \
    KD_SAVE_PATH=/shares \
    KD_SOCKET=/tmp/kd.sock

VOLUME ["/config", "/shares"]

USER keibidrop

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD kd status >/dev/null 2>&1 || exit 1

ENTRYPOINT ["keibidrop-entrypoint"]
