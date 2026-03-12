#!/usr/bin/env bash
# ABOUTME: Launches Alice peer via Go GUI (no-FUSE mode).
# ABOUTME: Uses project-relative paths so it works on any machine.
ROOT="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$ROOT/SaveAlice" "$ROOT/MountAlice"
NO_FUSE="" \
  TO_SAVE_PATH="$ROOT/SaveAlice" \
  TO_MOUNT_PATH="$ROOT/MountAlice" \
  KEIBIDROP_RELAY="http://127.0.0.1:54321" \
  INBOUND_PORT=26001 \
  OUTBOUND_PORT=26002 \
  go run "$ROOT/cmd/keibidrop.go"
