#!/bin/bash
# Contact mode, the day-2 path: two peers with persistent identities save each other
# as contacts (which arms auto-connect), the laptop side restarts and comes back on
# its own, then the always-on side restarts and the session comes back again.
#   contact_mode_check.sh <mac kd binary> <kd path on timisoara> <work dir>
# A is this Mac, B is timisoara as a transient systemd unit. Sockets live under /tmp
# (the 104-byte rule). One PASS or FAIL line per check; exit 1 if anything failed.
set -u
KD=$1; TZKD=$2; W=$3
TZ="ssh -o BatchMode=yes -o ConnectTimeout=15 -i $HOME/.ssh/id_rsa_rent root@185.104.181.40"
SKA=/private/tmp/claude-501/kdx/contact-a.sock; mkdir -p "$(dirname "$SKA")" "$W/a/cfg" "$W/a/save"
B=/root/kd-bench/contact/b; SKB=/tmp/kd-contact-b.sock
FAILED=0
say() { printf '%s\n' "$*"; }
ok() { say "PASS $*"; }
bad() { say "FAIL $*"; FAILED=1; }
a() { KD_SOCKET=$SKA "$KD" "$@" 2>&1; }
b() { $TZ "KD_SOCKET=$SKB $TZKD $*" 2>&1; }
jget() { python3 -c 'import json,sys; d=json.load(sys.stdin).get("data",{}); print(d.get(sys.argv[1],"") if isinstance(d,dict) else "")' "$1" 2>/dev/null; }
start_a() { # the pid file names this daemon, so the stop never reaches another kd from the same binary
  (cd "$W/a" && KEIBIDROP_CONFIG_DIR=$W/a/cfg KD_SOCKET=$SKA KD_SAVE_PATH=$W/a/save KD_NO_FUSE=1 \
    KD_INBOUND_PORT=26493 KD_OUTBOUND_PORT=26494 KD_LOG_FILE=$W/a/kd.log nohup "$KD" start >> "$W/a/stdout" 2>&1 < /dev/null & echo $! > "$W/a/pid")
}
start_b() {
  $TZ "mkdir -p $B/cfg $B/save; rm -f $SKB; systemctl stop kd-contact-b 2>/dev/null; systemctl reset-failed kd-contact-b 2>/dev/null; \
    systemd-run --quiet --collect --unit kd-contact-b -p WorkingDirectory=$B --setenv=KEIBIDROP_CONFIG_DIR=$B/cfg --setenv=KD_SOCKET=$SKB \
    --setenv=KD_SAVE_PATH=$B/save --setenv=KD_NO_FUSE=1 --setenv=KD_INBOUND_PORT=26495 --setenv=KD_OUTBOUND_PORT=26496 \
    --setenv=KD_LOG_FILE=$B/kd.log $TZKD start" >/dev/null
}
stop_a() { a stop >/dev/null 2>&1; sleep 1; [ -f "$W/a/pid" ] && kill "$(cat "$W/a/pid")" 2>/dev/null; rm -f "$SKA"; }
stop_b() { $TZ "systemctl stop kd-contact-b 2>/dev/null; rm -f $SKB; true"; }
wait_fp() { local who=$1 fp; for _ in $(seq 1 60); do fp=$($who show fingerprint 2>/dev/null | jget fingerprint); [ -n "$fp" ] && { echo "$fp"; return 0; }; sleep 1; done; return 1; }
wait_connected() { # who seconds; the status JSON's state is idle, waiting_for_peer, connected or reconnecting
  local who=$1 t=$2 i; for i in $(seq 1 "$t"); do [ "$($who status 2>/dev/null | jget state)" = "connected" ] && { echo "$i"; return 0; }; sleep 1; done; return 1
}
pull_check() { # A pulls from-b.bin from B and compares
  local want got; want=$($TZ "sha256sum $B/from-b.bin | cut -c1-16"); rm -f "$W/a/pulled.bin"
  a pull from-b.bin "$W/a/pulled.bin" --timeout=120 >/dev/null; got=$(shasum -a 256 "$W/a/pulled.bin" 2>/dev/null | cut -c1-16)
  [ -n "$got" ] && [ "$got" = "$want" ]
}
logs() { say "  A log:"; grep -h -E 'msg="(Auto-connect|Peer (is )?present|Peer still absent|Connect supersedes|Connection lost|Reconnection|Gave up|Cipher negotiated|goodbye|DISCONNECT)' "$W/a/kd.log" | sed -E 's/^time=[^T]+T([0-9:.]+)[^ ]+ level=[A-Z]+ //' | cut -c1-150 | tail -12 | sed 's/^/    /'; }

trap 'stop_a; stop_b' EXIT
rm -rf "$W/a"; mkdir -p "$W/a/cfg" "$W/a/save"; $TZ "rm -rf $B; mkdir -p $B/cfg $B/save; head -c 1048576 /dev/urandom > $B/from-b.bin"
start_a; start_b
FPA=$(wait_fp a) && FPB=$(wait_fp b) && ok "daemons up (persistent identities)" || { bad "daemons up"; exit 1; }

a register "$FPB" >/dev/null; b register "$FPA" >/dev/null
a create --timeout=150 > "$W/a/create.out" 2>&1 &
b join --timeout=150 > "$W/a/join-b.out" 2>&1 &
wait
if [ "$(a status | jget state)" = "connected" ]; then ok "first connect by code"; else bad "first connect by code: $(head -c 200 "$W/a/create.out")"; logs; exit 1; fi

armA=$(a save-contact box | jget auto_connect); armB=$(b save-contact laptop | jget auto_connect)
[ "$armA" = "box" ] && [ "$armB" = "laptop" ] && ok "contacts saved and auto-connect armed both ways" || bad "save-contact: A=$armA B=$armB"
# Presence rides a 30 s heartbeat per contact (and one post when a contact is saved).
online=""; for i in $(seq 1 40); do
  online=$(a contacts | python3 -c 'import json,sys; d=json.load(sys.stdin).get("data",[]); print([c["online"] for c in d if c["name"]=="box"])' 2>/dev/null)
  [ "$online" = "[True]" ] && break; sleep 1
done
[ "$online" = "[True]" ] && ok "contact box shows present (after $i s)" || bad "contact presence: $online after 40 s"
b add "$B/from-b.bin" >/dev/null; sleep 2
pull_check && ok "pull over the contact session" || bad "pull over the contact session"

say "restarting the laptop side (A)"; stop_a; sleep 3; start_a
t=$(wait_connected a 180) && ok "laptop restart: session back in $t s (auto-connect)" || { bad "laptop restart: no session in 180 s"; logs; }
sleep 2; b add "$B/from-b.bin" >/dev/null; sleep 2
pull_check && ok "pull after the laptop restart" || bad "pull after the laptop restart"

say "restarting the always-on side (B)"; stop_b; sleep 3; start_b
t=$(wait_connected a 360) && ok "box restart: session back in $t s" || { bad "box restart: no session in 360 s"; logs; }
sleep 2; b add "$B/from-b.bin" >/dev/null; sleep 2
pull_check && ok "pull after the box restart" || bad "pull after the box restart"
logs
exit $FAILED
