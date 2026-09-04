#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# Two KeibiDrop peers pair and move a file with zero prompts and no TTY.
# Every kd invocation reads from /dev/null and is checked by exit code, never
# by parsing the message text.
#
# Usage: ./demo-two-machines.sh [workdir]
# Run it twice against the same workdir to check idempotency.

set -u -o pipefail

KD=${KD:-kd}
WORK=${1:-/tmp/kd-agent-demo}
TIMEOUT=${TIMEOUT:-90}

A_SOCK=/tmp/kd-demo-a.sock
B_SOCK=/tmp/kd-demo-b.sock

pass=0
fail=0
ok()   { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }
step() { printf '\n== %s\n' "$1"; }

# a/b run one verb against one daemon with stdin closed.
a() { KD_SOCKET=$A_SOCK "$KD" "$@" </dev/null 2>/dev/null; }
b() { KD_SOCKET=$B_SOCK "$KD" "$@" </dev/null 2>/dev/null; }

jfield() { python3 -c "import json,sys;print(json.load(sys.stdin)['data']['$1'])" 2>/dev/null; }

cleanup() {
  a stop  >/dev/null 2>&1
  b stop  >/dev/null 2>&1
  sleep 1
  rm -f "$A_SOCK" "$B_SOCK"
}
trap cleanup EXIT

step "Workspace $WORK"
rm -rf "$WORK"
mkdir -p "$WORK"/A/{cfg,recv,mnt} "$WORK"/B/{cfg,recv,mnt} "$WORK"/src
cleanup

# A read-only source directory: an agent's build output is often not writable.
head -c 2000000 /dev/urandom > "$WORK/src/artifact.bin"
sha_src=$(shasum -a 256 "$WORK/src/artifact.bin" | cut -d' ' -f1)
chmod 555 "$WORK/src"

step "Start two daemons, stdin closed"
# Each daemon needs its own config dir, or both load the same identity file and
# end up with identical fingerprints, which connect rejects.
KEIBIDROP_CONFIG_DIR=$WORK/A/cfg KD_SOCKET=$A_SOCK KD_SAVE_PATH=$WORK/A/recv \
  KD_MOUNT_PATH=$WORK/A/mnt KD_NO_FUSE=1 KD_INCOGNITO=1 \
  KD_INBOUND_PORT=26501 KD_OUTBOUND_PORT=26502 KD_LOG_FILE=$WORK/A/kd.log \
  "$KD" start </dev/null >"$WORK/A/start.json" 2>"$WORK/A/start.err" &

KEIBIDROP_CONFIG_DIR=$WORK/B/cfg KD_SOCKET=$B_SOCK KD_SAVE_PATH=$WORK/B/recv \
  KD_MOUNT_PATH=$WORK/B/mnt KD_NO_FUSE=1 KD_INCOGNITO=1 \
  KD_INBOUND_PORT=26511 KD_OUTBOUND_PORT=26512 KD_LOG_FILE=$WORK/B/kd.log \
  "$KD" start </dev/null >"$WORK/B/start.json" 2>"$WORK/B/start.err" &

for _ in $(seq 1 30); do
  sleep 1
  [ -S "$A_SOCK" ] && [ -S "$B_SOCK" ] && break
done

a version >/dev/null && ok "daemon A answers" || bad "daemon A did not start"
b version >/dev/null && ok "daemon B answers" || bad "daemon B did not start"

AFP=$(jfield fingerprint <"$WORK/A/start.json")
BFP=$(jfield fingerprint <"$WORK/B/start.json")
[ -n "$AFP" ] && [ -n "$BFP" ] && [ "$AFP" != "$BFP" ] \
  && ok "both codes printed as JSON and differ" \
  || bad "codes missing or identical (A=$AFP B=$BFP)"

step "Exchange codes"
# The exchange is symmetric: each side must know the other's code before the
# handshake completes. There is no trust-on-first-use over the relay.
a register "$BFP" >/dev/null && ok "A accepted B's code" || bad "A register"
b register "$AFP" >/dev/null && ok "B accepted A's code" || bad "B register"

step "Connect, capped at ${TIMEOUT}s"
a connect-timeout --timeout="$TIMEOUT" >"$WORK/A/connect.json" 2>&1 &
apid=$!
b connect-timeout --timeout="$TIMEOUT" >"$WORK/B/connect.json" 2>&1 &
bpid=$!
wait $apid; arc=$?
wait $bpid; brc=$?
[ $arc -eq 0 ] && ok "A connected (exit 0)" || bad "A connect exit $arc: $(cat "$WORK/A/connect.json")"
[ $brc -eq 0 ] && ok "B connected (exit 0)" || bad "B connect exit $brc: $(cat "$WORK/B/connect.json")"

a wait-connected --timeout=30 >/dev/null && ok "A reports healthy" || bad "A not healthy"
b wait-connected --timeout=30 >/dev/null && ok "B reports healthy" || bad "B not healthy"

step "A shares a file from a read-only directory"
a add "$WORK/src/artifact.bin" >/dev/null && ok "add from a 555 directory" || bad "add"
a add "$WORK/src/artifact.bin" >/dev/null && ok "add is idempotent" || bad "second add"

step "B pulls it without blocking"
for _ in $(seq 1 20); do
  b list | grep -q artifact.bin && break
  sleep 1
done
b list | grep -q artifact.bin && ok "B sees the file" || bad "B never saw the file"

tid=$(b pull-async artifact.bin "$WORK/B/recv/artifact.bin" | jfield transfer_id)
[ -n "$tid" ] && ok "pull-async returned $tid at once" || bad "pull-async gave no transfer_id"

done_ok=0
for _ in $(seq 1 "$TIMEOUT"); do
  st=$(b transfer-status "$tid")
  echo "$st" | grep -q '"done":true' && { done_ok=1; break; }
  sleep 1
done
[ $done_ok -eq 1 ] && ok "transfer reported done" || bad "transfer never finished: $st"

step "Verify the bytes"
sha_dst=$(shasum -a 256 "$WORK/B/recv/artifact.bin" 2>/dev/null | cut -d' ' -f1)
[ -n "$sha_dst" ] && [ "$sha_src" = "$sha_dst" ] \
  && ok "sha256 matches ($sha_src)" \
  || bad "checksum mismatch: src=$sha_src dst=${sha_dst:-missing}"

step "Exit codes are usable"
a file-size definitely-not-here >/dev/null 2>&1
[ $? -eq 4 ] && ok "missing file exits 4 (not_found)" || bad "missing file exit was $?"
a bogus-verb >/dev/null 2>&1
[ $? -eq 8 ] && ok "unknown verb exits 8 (unsupported)" || bad "unknown verb exit was $?"

step "Disconnect is idempotent"
a disconnect >/dev/null && ok "A disconnect" || bad "A disconnect"
a disconnect >/dev/null && ok "A disconnect again" || bad "second disconnect failed"
b disconnect >/dev/null && ok "B disconnect" || bad "B disconnect"

printf '\n== %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
