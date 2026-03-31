#!/usr/bin/env bash
# Starts kd daemon as Bob.
# Env vars from Makefile take precedence; defaults below for standalone use.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveBob"

KD_LOG_FILE="${KD_LOG_FILE:-Log_Bob_kd.txt}" \
  KD_NO_FUSE="${KD_NO_FUSE-1}" \
  KD_SAVE_PATH="${KD_SAVE_PATH:-$ROOT/SaveBob}" \
  KD_RELAY="${KD_RELAY:-http://127.0.0.1:54321}" \
  KD_INBOUND_PORT="${KD_INBOUND_PORT:-26003}" \
  KD_OUTBOUND_PORT="${KD_OUTBOUND_PORT:-26004}" \
  KD_SOCKET="${KD_SOCKET:-/tmp/kd-bob.sock}" \
  "$ROOT/kd" start
