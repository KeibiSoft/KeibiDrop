#!/usr/bin/env bash
# ABOUTME: Launches Alice peer via Go CLI (FUSE mode).
# ABOUTME: Uses project-relative paths so it works on any machine.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveAlice" "$ROOT/MountAlice"
LOG_FILE="Log_Alice.txt" \
  TO_SAVE_PATH="$ROOT/SaveAlice" \
  TO_MOUNT_PATH="$ROOT/MountAlice" \
  KEIBIDROP_RELAY="http://127.0.0.1:54321" \
  INBOUND_PORT=26001 \
  OUTBOUND_PORT=26002 \
  go run "$ROOT/cmd/cli/keibidrop-cli.go"
