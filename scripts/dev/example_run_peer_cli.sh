#!/usr/bin/env bash
# ABOUTME: Launches Bob peer via Go CLI (FUSE mode).
# ABOUTME: Uses project-relative paths so it works on any machine.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveBob" "$ROOT/MountBob"
LOG_FILE="Log_Bob.txt" \
  TO_SAVE_PATH="$ROOT/SaveBob" \
  TO_MOUNT_PATH="$ROOT/MountBob" \
  KEIBIDROP_RELAY="http://127.0.0.1:54321" \
  INBOUND_PORT=26003 \
  OUTBOUND_PORT=26004 \
  go run "$ROOT/cmd/cli/keibidrop-cli.go"
