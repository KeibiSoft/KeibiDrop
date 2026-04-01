#!/usr/bin/env bash
# Launches the KeibiDrop Rust UI as Bob.
# Env vars from Makefile take precedence; defaults below for standalone use.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveBob" "$ROOT/MountBob"

LOG_FILE="${LOG_FILE:-Log_Bob.txt}" \
  NO_FUSE="${NO_FUSE-1}" \
  TO_SAVE_PATH="${TO_SAVE_PATH:-$ROOT/SaveBob}" \
  TO_MOUNT_PATH="${TO_MOUNT_PATH:-$ROOT/MountBob}" \
  KEIBIDROP_RELAY="${KEIBIDROP_RELAY:-http://localhost:54321}" \
  INBOUND_PORT="${INBOUND_PORT:-26003}" \
  OUTBOUND_PORT="${OUTBOUND_PORT:-26004}" \
  "$ROOT/rust/target/release/keibidrop-rust"
