#!/usr/bin/env bash
# ABOUTME: Launches the KeibiDrop Rust UI (no-FUSE mode, Alice peer).
# ABOUTME: Uses project-relative paths so it works on any machine.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveAlice" "$ROOT/MountAlice"
LOG_FILE="Log_Alice.txt" \
  NO_FUSE=1 \
  TO_SAVE_PATH="$ROOT/SaveAlice" \
  TO_MOUNT_PATH="$ROOT/MountAlice" \
  KEIBIDROP_RELAY="http://127.0.0.1:54321" \
  INBOUND_PORT=26001 \
  OUTBOUND_PORT=26002 \
  "$ROOT/rust/target/release/keibidrop-rust"
