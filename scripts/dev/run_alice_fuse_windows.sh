#!/usr/bin/env bash
# Alice – FUSE kd daemon on Windows (MinGW bash). Requires WinFSP installed.
# Mounts peer files to drive letter Z: (or set KD_MOUNT_PATH=Z:).
# Start relay first: cd KeibiDrop-Relay && bash run-local.sh
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/SaveAlice"

KD_RELAY="${KD_RELAY:-http://localhost:54321}" \
  KD_INBOUND_PORT="${KD_INBOUND_PORT:-26001}" \
  KD_OUTBOUND_PORT="${KD_OUTBOUND_PORT:-26002}" \
  KD_SAVE_PATH="$ROOT/SaveAlice" \
  KD_MOUNT_PATH="${KD_MOUNT_PATH:-Z:}" \
  KD_LOG_FILE="$ROOT/Log_Alice_FUSE.txt" \
  KD_SOCKET="$TEMP/kd-alice.sock" \
  "$ROOT/kd.exe" start
