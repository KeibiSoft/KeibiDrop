#!/usr/bin/env bash
# Bob – no-FUSE kd daemon on Windows (MinGW bash).
# Start relay first: cd KeibiDrop-Relay && bash run-local.sh
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveBob"

KD_NO_FUSE=1 \
  KD_RELAY="${KD_RELAY:-http://localhost:54321}" \
  KD_INBOUND_PORT="${KD_INBOUND_PORT:-26003}" \
  KD_OUTBOUND_PORT="${KD_OUTBOUND_PORT:-26004}" \
  KD_SAVE_PATH="$ROOT/SaveBob" \
  KD_LOG_FILE="$ROOT/Log_Bob.txt" \
  KD_SOCKET="$TEMP/kd-bob.sock" \
  "$ROOT/kd.exe" start
