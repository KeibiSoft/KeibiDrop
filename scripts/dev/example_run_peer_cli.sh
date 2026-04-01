#!/usr/bin/env bash
# Launches Bob peer via Go CLI.
# Env vars from Makefile take precedence; defaults below for standalone use.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveBob" "$ROOT/MountBob"

LOG_FILE="${LOG_FILE:-Log_Bob.txt}" \
  NO_FUSE="${NO_FUSE-1}" \
  TO_SAVE_PATH="${TO_SAVE_PATH:-$ROOT/SaveBob}" \
  TO_MOUNT_PATH="${TO_MOUNT_PATH:-$ROOT/MountBob}" \
  KEIBIDROP_RELAY="${KEIBIDROP_RELAY:-http://127.0.0.1:54321}" \
  INBOUND_PORT="${INBOUND_PORT:-26003}" \
  OUTBOUND_PORT="${OUTBOUND_PORT:-26004}" \
  go run "$ROOT/cmd/cli/keibidrop-cli.go"
