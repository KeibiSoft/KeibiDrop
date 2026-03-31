#!/usr/bin/env bash
# ABOUTME: Starts kd daemon as Bob (no-FUSE mode, for agents).
# ABOUTME: Uses project-relative paths so it works on any machine.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveBob"
KD_LOG_FILE="Log_Bob.txt" \
  KD_NO_FUSE=1 \
  KD_SAVE_PATH="$ROOT/SaveBob" \
  KD_RELAY="http://127.0.0.1:54321" \
  KD_INBOUND_PORT=26003 \
  KD_OUTBOUND_PORT=26004 \
  KD_SOCKET=/tmp/kd-bob.sock \
  "$ROOT/kd" start
