#!/usr/bin/env bash
# Launches the KeibiDrop Rust UI as Alice.
# Env vars from Makefile take precedence; defaults below for standalone use.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveAlice" "$ROOT/MountAlice"

LOG_FILE="${LOG_FILE:-Log_Alice.txt}" \
  NO_FUSE="${NO_FUSE-1}" \
  TO_SAVE_PATH="${TO_SAVE_PATH:-$ROOT/SaveAlice}" \
  TO_MOUNT_PATH="${TO_MOUNT_PATH:-$ROOT/MountAlice}" \
  KEIBIDROP_RELAY="${KEIBIDROP_RELAY:-http://127.0.0.1:54321}" \
  INBOUND_PORT="${INBOUND_PORT:-26001}" \
  OUTBOUND_PORT="${OUTBOUND_PORT:-26002}" \
  "$ROOT/rust/target/release/keibidrop-rust"
