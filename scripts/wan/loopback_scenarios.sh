#!/bin/bash
# Loopback scenarios on one host: two kd daemons, production relay and bridge.
#   loopback_scenarios.sh <kd binary> <work dir> [fuse|nofuse]
# One PASS, FAIL or SKIP line per check; exit 1 if anything failed. The daemons
# are started in plain statements, never inside $(...): a background daemon
# keeps a substitution's pipe open and the script never gets past it.
set -u
KD=$(cd "$(dirname "$1")" && pwd)/$(basename "$1"); W=$2; FUSE=${3:-fuse}
PA=26471; PB=26491   # inbound ports; outbound is +1
# Unix socket paths must stay under 104 bytes, so they live under /tmp, not the work dir.
SK=${KD_LOOP_SOCK:-/tmp/kdloop.$$}; mkdir -p "$SK" "$W"; fails=0
ok(){ echo "PASS $*"; }; bad(){ echo "FAIL $*"; fails=$((fails+1)); }; skip(){ echo "SKIP $*"; }
fpof(){ python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("data",{}).get("fingerprint",""))
except Exception: print("")'; }
sha(){ if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d" " -f1; else shasum -a 256 "$1" | cut -d" " -f1; fi; }
kdc(){ local p=$1; shift; KD_SOCKET=$SK/$p.sock "$KD" "$@" 2>&1; }
start(){ local p=$1 port=$2 fuse=$3; local d=$W/$p nofuse=1; [ "$fuse" = fuse ] && nofuse=0
  mkdir -p "$d/save" "$d/config" "$d/mount"; rm -f "$SK/$p.sock"
  (cd "$d" && KEIBIDROP_CONFIG_DIR=$d/config KD_SOCKET=$SK/$p.sock KD_SAVE_PATH=$d/save KD_MOUNT_PATH=$d/mount KD_NO_FUSE=$nofuse KD_INCOGNITO=1 KD_INBOUND_PORT=$port KD_OUTBOUND_PORT=$((port+1)) KD_LOG_FILE=$d/kd.log nohup "$KD" start > "$d/stdout" 2>&1 < /dev/null &) ; }
fp_of(){ local fp=""; for _ in $(seq 1 60); do fp=$(kdc "$1" show fingerprint | fpof); [ -n "$fp" ] && break; sleep 1; done; echo "$fp"; }
connected(){ kdc "$1" wait-connected --timeout="${2:-90}" | grep -q '"connected":true'; }
t0=$(date +%s); ts(){ echo "[$(( $(date +%s) - t0 ))s] $*"; }

# A shares its save dir on start, with a trash folder and a bitmap sidecar planted in it.
mkdir -p "$W/A/config" "$W/A/save/.Trashes/0"
printf 'scan_shared_on_start = true\nrescan_shared_seconds = 5\n' > "$W/A/config/config.toml"
head -c 300000 /dev/urandom > "$W/A/save/from-a.bin"
head -c 1000 /dev/urandom > "$W/A/save/.Trashes/0/trashed.bin"
head -c 16 /dev/urandom > "$W/A/save/part.bin.kdbitmap"
start A $PA nofuse; start B $PB "$FUSE"
fpA=$(fp_of A); fpB=$(fp_of B)
[ -n "$fpA" ] && [ -n "$fpB" ] || { bad "daemons up (A=$fpA B=$fpB)"; cat "$W/A/stdout" "$W/B/stdout" 2>/dev/null | tail -4; exit 1; }
ok "daemons up"
kdc A register "$fpB" >/dev/null; kdc B register "$fpA" >/dev/null

# 1. Cancel, then connect again: the joiner first, a cancel at 6 s, then the creator.
kdc B join --timeout=120 > "$W/join1.out" & J1=$!
sleep 6; kdc B disconnect > /dev/null; wait $J1
grep -q cancelled "$W/join1.out" && ok "cancel ends the wait: $(head -c 80 "$W/join1.out")" || bad "cancel: $(head -c 120 "$W/join1.out")"
fpB2=$(kdc B show fingerprint | fpof); [ "$fpB2" = "$fpB" ] && ok "code unchanged after cancel" || { bad "code rotated after cancel"; kdc A register "$fpB2" >/dev/null; }
kdc B join --timeout=120 > "$W/join2.out" & J2=$!
sleep 2; kdc A create --timeout=120 > "$W/create1.out" & C1=$!
if connected A 90 && connected B 90; then ok "connect after cancel ($(( $(date +%s) - t0 )) s)"; else bad "connect after cancel"; fi
wait $J2 $C1 2>/dev/null
mode=$(kdc B status | grep -oE '"connection_mode":"[a-z]+"' | head -1); ts "B $mode"

# 2. Shared-folder scan: the planted file is offered, the trash and the sidecar are not.
sleep 3; L=$(kdc B list)
echo "$L" | grep -q 'from-a.bin' && ok "scan shares from-a.bin" || bad "scan: from-a.bin missing: $(echo "$L" | head -c 200)"
echo "$L" | grep -qE 'Trashes|kdbitmap' && bad "scan leaks trash or sidecar: $(echo "$L" | grep -oE '[^"]*(Trashes|kdbitmap)[^"]*' | head -3)" || ok "scan hides the trash folder and the sidecar"

# 3. Transfer both ways, sha256 compared.
kdc B pull from-a.bin "$W/B/got-a.bin" --timeout=120 > /dev/null
[ "$(sha "$W/B/got-a.bin")" = "$(sha "$W/A/save/from-a.bin")" ] && ok "pull A->B" || bad "pull A->B"
head -c 300000 /dev/urandom > "$W/B/from-b.bin"; kdc B add "$W/B/from-b.bin" > /dev/null; sleep 2
kdc A pull from-b.bin "$W/A/got-b.bin" --timeout=120 > /dev/null
[ "$(sha "$W/A/got-b.bin")" = "$(sha "$W/B/from-b.bin")" ] && ok "pull B->A" || bad "pull B->A"

# 4. FUSE on B: a trash folder is refused, a plain folder is not, a move into an existing trash folder fails.
if [ "$FUSE" = fuse ] && kdc B wait-mount --timeout=30 | grep -q '"ready":true'; then
  M=$W/B/mount
  mkdir "$M/.Trashes" 2>/dev/null && bad "mkdir .Trashes succeeded on the mount" || ok "mkdir .Trashes refused"
  mkdir "$M/.Trash-0" 2>/dev/null && bad "mkdir .Trash-0 succeeded on the mount" || ok "mkdir .Trash-0 refused"
  mkdir "$M/plain" 2>/dev/null && { ok "mkdir plain allowed"; rmdir "$M/plain" 2>/dev/null; } || bad "mkdir plain refused"
  mkdir -p "$W/B/save/.Trashes/0"   # the folder already exists on disk, as after an older build
  if mv "$M/from-a.bin" "$M/.Trashes/0/from-a.bin" 2>/dev/null; then bad "move into an existing trash folder succeeded"; else ok "move into an existing trash folder refused"; fi
  [ -e "$M/from-a.bin" ] && ok "file still in place" || bad "file gone after the refused move"
else
  skip "FUSE checks (no mount)"
fi

# 5. The relay poll while waiting is not an error line.
grep -q 'level=ERROR msg="Failed to fetch"' "$W/B/kd.log" && bad "404 poll logged at ERROR" || ok "404 poll is quiet"
grep -q 'QUIC control lane verified' "$W/B/kd.log" && ok "QUIC lane up" || skip "QUIC lane not verified on B (see kd.log)"

# 6. Always-on: A stays up, B disconnects and connects again, the files are still there.
# A disconnect resets the session, so both sides register the peer again first.
kdc B disconnect > /dev/null; sleep 2
fpA=$(fp_of A); fpB=$(fp_of B); kdc A register "$fpB" >/dev/null; kdc B register "$fpA" >/dev/null
kdc B join --timeout=120 > "$W/join3.out" & J3=$!
sleep 1; kdc A create --timeout=120 > "$W/create2.out" & C2=$!
if connected A 90 && connected B 90; then ok "second connect to the same peer"; else bad "second connect"; fi
wait $J3 $C2 2>/dev/null; sleep 2
kdc B list | grep -q 'from-a.bin' && ok "files listed again after reconnect" || bad "files missing after reconnect"

kdc A disconnect >/dev/null; kdc B disconnect >/dev/null; kdc A stop >/dev/null; kdc B stop >/dev/null; sleep 1
rm -rf "$SK"
echo "done: $fails failure(s); logs in $W/A/kd.log and $W/B/kd.log"
[ $fails -eq 0 ]
