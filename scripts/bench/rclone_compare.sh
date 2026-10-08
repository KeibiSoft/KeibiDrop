#!/usr/bin/env bash
# ABOUTME: Benchmarks rclone mount (--vfs-cache-mode full) over local SFTP on
# ABOUTME: the two claims KeibiDrop measures: 10k-tree enumeration and the
# ABOUTME: bytes re-transferred after a 2 MB in-place edit of a 256 MB file.
#
# Usage: [RCLONE=/path/to/rclone] scripts/bench/rclone_compare.sh [workdir]
# Needs: rclone, a FUSE driver (macFUSE / fuse3 / WinFsp), dd, find.
# On macOS the Homebrew rclone build refuses `rclone mount`; use the official
# binary from rclone.org/downloads and pass it via RCLONE=.
# Byte counts come from the mount's rc endpoint (core/stats), so they are the
# backend (SFTP) bytes the VFS actually moved.

set -euo pipefail

RCLONE="${RCLONE:-rclone}"
WORK="${1:-$(mktemp -d)}"
DATA="$WORK/data"
MNT="$WORK/mnt"
CACHE="$WORK/cache"
RC="localhost:5573"
SFTP_PORT=2223
FILES=10000
BIGMB=256
EDITMB=2

mkdir -p "$DATA" "$MNT" "$CACHE"
echo "workdir: $WORK"
"$RCLONE" version | head -1

cleanup() {
  umount "$MNT" 2>/dev/null || diskutil unmount "$MNT" 2>/dev/null || fusermount -u "$MNT" 2>/dev/null || true
  kill "${MOUNT_PID:-0}" "${SERVE_PID:-0}" 2>/dev/null || true
}
trap cleanup EXIT

echo "== seeding $FILES x 4KB files + one ${BIGMB}MB file =="
seed_start=$(date +%s)
for d in $(seq 0 99); do
  mkdir -p "$DATA/d$d"
done
python3 - "$DATA" "$FILES" <<'EOF'
import os, sys
data_dir, n = sys.argv[1], int(sys.argv[2])
payload = os.urandom(4096)
for i in range(n):
    with open(os.path.join(data_dir, f"d{i//100}", f"f{i:05d}.bin"), "wb") as f:
        f.write(payload)
EOF
dd if=/dev/urandom of="$DATA/vmimage.bin" bs=1m count="$BIGMB" 2>/dev/null
echo "seeded in $(( $(date +%s) - seed_start ))s"

echo "== rclone serve sftp + mount (--vfs-cache-mode full) =="
"$RCLONE" serve sftp "$DATA" --addr "localhost:$SFTP_PORT" --user u --pass p >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
sleep 1

OBSCURED=$("$RCLONE" obscure p)
"$RCLONE" mount ":sftp:/" "$MNT" \
  --sftp-host localhost --sftp-port "$SFTP_PORT" --sftp-user u --sftp-pass "$OBSCURED" \
  --vfs-cache-mode full --cache-dir "$CACHE" \
  --dir-cache-time 3s --attr-timeout 1s \
  --rc --rc-addr "$RC" --rc-no-auth >"$WORK/mount.log" 2>&1 &
MOUNT_PID=$!
for i in $(seq 1 30); do
  [ -e "$MNT/vmimage.bin" ] && break
  sleep 0.5
done
[ -e "$MNT/vmimage.bin" ] || { echo "mount did not come up"; exit 1; }

stats_bytes() { "$RCLONE" rc --rc-addr "$RC" --rc-no-auth core/stats 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("bytes",0))'; }

echo "== phase 1: enumerate $FILES files (cold dir tree) =="
t0=$(python3 -c 'import time; print(time.time())')
COUNT=$(find "$MNT" -type f | wc -l | tr -d ' ')
t1=$(python3 -c 'import time; print(time.time())')
ENUM_S=$(python3 -c "print(f'{$t1-$t0:.2f}')")
echo "rclone_enumerate files=$COUNT wall=${ENUM_S}s"

echo "== phase 2: cold full read of ${BIGMB}MB =="
b0=$(stats_bytes)
t0=$(python3 -c 'import time; print(time.time())')
cksum_before=$(shasum -a 256 "$MNT/vmimage.bin" | cut -d' ' -f1)
t1=$(python3 -c 'import time; print(time.time())')
b1=$(stats_bytes)
READ_S=$(python3 -c "print(f'{$t1-$t0:.2f}')")
echo "rclone_cold_read bytes_on_wire=$((b1-b0)) wall=${READ_S}s sha=$cksum_before"

echo "== phase 3: 2MB in-place edit at origin, then re-read =="
dd if=/dev/urandom of="$DATA/vmimage.bin" bs=1m seek=128 count="$EDITMB" conv=notrunc 2>/dev/null
sha_origin=$(shasum -a 256 "$DATA/vmimage.bin" | cut -d' ' -f1)
sleep 6  # outlive --dir-cache-time and --attr-timeout so the change is visible

b2=$(stats_bytes)
t0=$(python3 -c 'import time; print(time.time())')
sha_reread=$(shasum -a 256 "$MNT/vmimage.bin" | cut -d' ' -f1)
t1=$(python3 -c 'import time; print(time.time())')
b3=$(stats_bytes)
REREAD_S=$(python3 -c "print(f'{$t1-$t0:.2f}')")
EDIT_BYTES=$((b3-b2))
echo "rclone_edit_resync edit_mb=$EDITMB bytes_on_wire=$EDIT_BYTES wall=${REREAD_S}s"
if [ "$sha_reread" = "$sha_origin" ]; then
  echo "rclone_edit_correctness byte_exact=yes"
else
  echo "rclone_edit_correctness byte_exact=NO stale_served (sha origin=$sha_origin reread=$sha_reread)"

  echo "== phase 3b: force revalidation (ls -la, wait, re-read) =="
  ls -la "$MNT" >/dev/null
  sleep 5
  b4=$(stats_bytes)
  sha_ls=$(shasum -a 256 "$MNT/vmimage.bin" | cut -d' ' -f1)
  b5=$(stats_bytes)
  if [ "$sha_ls" = "$sha_origin" ]; then
    echo "rclone_after_ls byte_exact=yes bytes_on_wire=$((b5-b4))"
  else
    echo "rclone_after_ls byte_exact=NO bytes_on_wire=$((b5-b4))"
  fi

  echo "== phase 3c: rc vfs/refresh, then re-read =="
  "$RCLONE" rc --rc-addr "$RC" --rc-no-auth vfs/refresh recursive=true >/dev/null 2>&1 || true
  sleep 2
  b6=$(stats_bytes)
  sha_refresh=$(shasum -a 256 "$MNT/vmimage.bin" | cut -d' ' -f1)
  b7=$(stats_bytes)
  if [ "$sha_refresh" = "$sha_origin" ]; then
    echo "rclone_after_vfs_refresh byte_exact=yes bytes_on_wire=$((b7-b6))"
  else
    echo "rclone_after_vfs_refresh byte_exact=NO bytes_on_wire=$((b7-b6))"
  fi
fi

echo "== done =="
