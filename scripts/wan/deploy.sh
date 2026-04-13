#!/bin/bash
# Deploy KeibiDrop source to remote server and build kd binary.
set -e

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SSH_KEY="${WAN_SSH_KEY:-$HOME/.ssh/id_rsa_rent}"
SERVER="${WAN_SERVER:-root@185.104.181.40}"

ssh_cmd() { ssh -i "$SSH_KEY" -o ConnectTimeout=10 "$SERVER" "$@"; }

echo "[deploy] Building tarball..."
cd "$ROOT"
tar czf /tmp/keibidrop-src.tar.gz \
    --exclude='.git' --exclude='rust' --exclude='ios' --exclude='android' \
    --exclude='mobile' --exclude='demo-video' --exclude='design' --exclude='blog' \
    --exclude='playground' --exclude='pleas-trim' --exclude='conferences' \
    --exclude='ui-bug' --exclude='./keibidrop-cli' --exclude='*.tar.gz' \
    --exclude='MountAlice' --exclude='MountBob' --exclude='SaveAlice' --exclude='SaveBob' \
    cmd/ pkg/ go.mod go.sum Makefile rustbridge/ grpc_bindings/

echo "[deploy] Uploading ($(du -h /tmp/keibidrop-src.tar.gz | cut -f1))..."
scp -i "$SSH_KEY" /tmp/keibidrop-src.tar.gz "$SERVER":/tmp/

echo "[deploy] Building on server..."
ssh_cmd "export PATH=\$PATH:/usr/local/go/bin && \
    rm -rf /root/KeibiDrop && mkdir -p /root/KeibiDrop && \
    cd /root/KeibiDrop && tar xzf /tmp/keibidrop-src.tar.gz && \
    go build -ldflags='-s -w' -o kd ./cmd/kd/ && \
    ls -lh kd"

echo "[deploy] Done."
