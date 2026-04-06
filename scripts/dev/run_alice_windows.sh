#!/usr/bin/env bash
# Alice – no-FUSE kd daemon on Windows (MinGW bash).
# Start relay first: cd KeibiDrop-Relay && bash run-local.sh
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveAlice"

KD_NO_FUSE=1 \
  KD_RELAY="${KD_RELAY:-http://localhost:54321}" \
  KD_INBOUND_PORT="${KD_INBOUND_PORT:-26001}" \
  KD_OUTBOUND_PORT="${KD_OUTBOUND_PORT:-26002}" \
  KD_SAVE_PATH="$ROOT/SaveAlice" \
  KD_LOG_FILE="$ROOT/Log_Alice.txt" \
  KD_SOCKET="$TEMP/kd-alice.sock" \
  "$ROOT/kd.exe" start
