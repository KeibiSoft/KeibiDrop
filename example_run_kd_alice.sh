#!/usr/bin/env bash
# ABOUTME: Starts kd daemon as Alice (no-FUSE mode, for agents).
# ABOUTME: Uses project-relative paths so it works on any machine.
ROOT="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$ROOT/SaveAlice"
KD_LOG_FILE="Log_Alice.txt" \
  KD_NO_FUSE=1 \
  KD_SAVE_PATH="$ROOT/SaveAlice" \
  KD_RELAY="http://127.0.0.1:54321" \
  KD_INBOUND_PORT=26001 \
  KD_OUTBOUND_PORT=26002 \
  KD_SOCKET=/tmp/kd-alice.sock \
  "$ROOT/kd" start
