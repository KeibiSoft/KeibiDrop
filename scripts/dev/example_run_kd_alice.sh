#!/usr/bin/env bash
# Starts kd daemon as Alice.
# Env vars from Makefile take precedence; defaults below for standalone use.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveAlice"

KD_LOG_FILE="${KD_LOG_FILE:-Log_Alice_kd.txt}" \
  KD_NO_FUSE="${KD_NO_FUSE-1}" \
  KD_SAVE_PATH="${KD_SAVE_PATH:-$ROOT/SaveAlice}" \
  KD_RELAY="${KD_RELAY:-http://127.0.0.1:54321}" \
  KD_INBOUND_PORT="${KD_INBOUND_PORT:-26001}" \
  KD_OUTBOUND_PORT="${KD_OUTBOUND_PORT:-26002}" \
  KD_SOCKET="${KD_SOCKET:-/tmp/kd-alice.sock}" \
  "$ROOT/kd" start
