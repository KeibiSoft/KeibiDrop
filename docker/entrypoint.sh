#!/bin/sh
# ABOUTME: Entrypoint for the KeibiDrop serving-peer container.
# ABOUTME: Runs the kd daemon headless, prints pairing steps, keeps the saved peer connected.
#
# Environment (all optional):
#   KD_PEER               The other machine's 86-character code. Saved as a
#                         contact on first start; the box then reconnects to it
#                         on its own for as long as the container runs.
#   KD_PEER_NAME          Contact name for KD_PEER. Default: laptop
#   KD_SHARE_READ_ONLY    true refuses every change the peer sends. Default: false
#   KD_INBOUND_PORT       TCP port peers dial. Default: 26431 (range 26000-27000)
#   KD_OUTBOUND_PORT      TCP port this box dials from. Default: 26432
#   KD_STRICT             true never falls back to the bridge. Default: false
#   KD_RELAY, KD_BRIDGE   Self-hosted relay and bridge, if you run them.
#   KD_LOG_FILE           Engine log. Default: /tmp/keibidrop.log (not persisted)
#   KD_LOG_MAX_MB         Truncate the engine log past this size. Default: 64
#
# Anything else from the configuration reference works through /config/config.toml.
# Environment variables win over the file.

set -u

CONFIG_DIR="${KEIBIDROP_CONFIG_DIR:-/config}"
SHARES="${KD_SAVE_PATH:-/shares}"
POLL="${KD_POLL_SECONDS:-5}"
LOG_MAX_MB="${KD_LOG_MAX_MB:-64}"

export KEIBIDROP_CONFIG_DIR="$CONFIG_DIR"
export KD_SAVE_PATH="$SHARES"
export KD_SOCKET="${KD_SOCKET:-/tmp/kd.sock}"
export KD_MOUNT_PATH="${KD_MOUNT_PATH:-/tmp/keibidrop-mount}"
export KD_LOG_FILE="${KD_LOG_FILE:-/tmp/keibidrop.log}"
# The serving side never mounts. Announce what is already in /shares.
export KD_NO_FUSE="${KD_NO_FUSE:-true}"
export KD_SCAN_SHARED_ON_START="${KD_SCAN_SHARED_ON_START:-true}"
# Files that land in /shares mid-session are announced on the next pass.
export KD_RESCAN_SHARED_SECONDS="${KD_RESCAN_SHARED_SECONDS:-30}"

say() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

# json_field NAME: first string value of "NAME" in stdin.
json_field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -n 1; }
# json_bool NAME: true/false value of "NAME" in stdin.
json_bool() { sed -n "s/.*\"$1\":\(true\|false\).*/\1/p" | head -n 1; }

KD_PID=""

stop_daemon() {
  [ -n "$KD_PID" ] || return 0
  kd stop >/dev/null 2>&1 || kill "$KD_PID" 2>/dev/null
  wait "$KD_PID" 2>/dev/null
  KD_PID=""
}

on_signal() {
  say "Stopping."
  stop_daemon
  exit 0
}
trap on_signal TERM INT

start_daemon() {
  # $1: contact to keep connected, or empty.
  if [ -n "$1" ]; then
    KD_AUTO_CONNECT_PEER="$1" kd start &
  else
    kd start &
  fi
  KD_PID=$!
  i=0
  while [ "$i" -lt 60 ]; do
    if kd status >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$KD_PID" 2>/dev/null; then
      say "The daemon exited during start. Engine log: $KD_LOG_FILE"
      exit 1
    fi
    i=$((i + 1))
    sleep 1
  done
  say "The daemon did not answer within 60 s. Engine log: $KD_LOG_FILE"
  exit 1
}

has_contact() {
  kd contacts 2>/dev/null | grep -F -q "\"fingerprint\":\"$1\""
}

# The saved contact the box keeps connected: KD_PEER, else the only contact.
auto_target() {
  if [ -n "${KD_PEER:-}" ]; then
    printf '%s' "$KD_PEER"
    return
  fi
  n=$(kd contacts 2>/dev/null | grep -o '"fingerprint":"[^"]*"' | wc -l | tr -d ' ')
  if [ "$n" = "1" ]; then
    kd contacts 2>/dev/null | json_field fingerprint
  fi
}

# --- /config must be private: it holds this box's identity key. ---
mkdir -p "$CONFIG_DIR" "$SHARES" 2>/dev/null || true
chmod 700 "$CONFIG_DIR" 2>/dev/null || true
mode=$(stat -c %a "$CONFIG_DIR" 2>/dev/null || echo 700)
case "$mode" in
  *00) ;;
  *)
    say "Refusing to start: $CONFIG_DIR is readable by other users (mode $mode)."
    say "It holds this box's private identity. Fix on the host: chmod 700 <the folder mapped to $CONFIG_DIR>"
    exit 1
    ;;
esac
if [ ! -w "$CONFIG_DIR" ]; then
  say "Refusing to start: $CONFIG_DIR is not writable by uid $(id -u). Map a folder this user owns, or run the container with user: <uid>:<gid> of the owner."
  exit 1
fi

KD_PEER=$(printf '%s' "${KD_PEER:-}" | tr -d '[:space:]')

# --- Start armed with the saved peer when there is one. A KD_PEER that is
# not yet a contact makes the daemon come up once unarmed: save it, restart
# armed. Later starts find the contact on disk and arm at once. ---
armed=""
target=""
if [ -n "$KD_PEER" ]; then
  start_daemon "$KD_PEER"
  if has_contact "$KD_PEER"; then
    armed="$KD_PEER"
  elif kd add-contact "${KD_PEER_NAME:-laptop}" "$KD_PEER" >/dev/null 2>&1; then
    say "Saved ${KD_PEER_NAME:-laptop} as a contact."
    stop_daemon
    start_daemon "$KD_PEER"
    armed="$KD_PEER"
  else
    say "Could not save KD_PEER as a contact. Check that it is the full 86-character code."
  fi
else
  start_daemon ""
  target=$(auto_target)
  if [ -n "$target" ]; then
    stop_daemon
    start_daemon "$target"
    armed="$target"
  fi
fi

FP=$(kd show fingerprint 2>/dev/null | json_field fingerprint)
nfiles=$(find "$SHARES" -type f 2>/dev/null | wc -l | tr -d ' ')
mode_text="read and write"
case "$(printf '%s' "${KD_SHARE_READ_ONLY:-false}" | tr 'A-Z' 'a-z')" in
  1|true|yes|on) mode_text="read only" ;;
esac

echo
echo "KeibiDrop serving peer is up. Version: $(kd version 2>/dev/null | json_field version)"
echo "Sharing: $SHARES ($nfiles files, $mode_text)"
echo
echo "This box's code:"
echo
echo "  $FP"
echo
if [ -n "$armed" ]; then
  echo "Keeping the saved contact connected. Connect to this box from the other"
  echo "machine whenever you like; after a drop the box reconnects on its own."
else
  echo "Pair it once:"
  echo "  1. On your computer, add this code as a contact (the app's Contacts,"
  echo "     or: kd add-contact nas $FP)."
  echo "  2. Give this box your computer's code:"
  echo "       docker exec keibidrop kd add-contact laptop <your 86-character code>"
  echo "     The box starts connecting to it within $POLL s and keeps doing so."
  echo "  3. Connect to the contact from your computer."
fi
echo
echo "The folder mapped to $CONFIG_DIR holds this box's private identity."
echo "Anyone who can read it can act as this box. Keep it out of shared backups."
echo

# --- Event stream for docker logs. ---
prev_running=""
prev_mode=""
while :; do
  sleep "$POLL" &
  wait $! 2>/dev/null || true

  if ! kill -0 "$KD_PID" 2>/dev/null; then
    say "The daemon exited. Engine log: $KD_LOG_FILE"
    exit 1
  fi

  st=$(kd status 2>/dev/null) || continue
  running=$(printf '%s' "$st" | json_bool running)
  mode=$(printf '%s' "$st" | json_field connection_mode)
  peer=$(printf '%s' "$st" | json_field peer_fingerprint)
  if [ "$running" != "$prev_running" ]; then
    if [ "$running" = "true" ]; then
      say "Peer connected: ${peer%"${peer#????????}"}... path: ${mode:-unknown}"
    elif [ -n "$prev_running" ]; then
      say "Peer disconnected. Waiting for it to come back."
    fi
    prev_running="$running"
    prev_mode="$mode"
  elif [ "$running" = "true" ] && [ "$mode" != "$prev_mode" ]; then
    say "Connection path changed: ${prev_mode:-unknown} to ${mode:-unknown}"
    prev_mode="$mode"
  fi

  k=0
  while [ "$k" -lt 8 ]; do
    ev=$(kd poll-event 2>/dev/null | json_field event)
    [ -n "$ev" ] || break
    case "$ev" in
      connect_status:*|connection_mode:*) ;;  # the running line above covers these
      *) say "event: $ev" ;;
    esac
    k=$((k + 1))
  done

  # A contact saved through docker exec arms the reconnect loop on the fly.
  if [ -z "$armed" ]; then
    target=$(auto_target)
    if [ -n "$target" ]; then
      say "Contact saved. Restarting the daemon to keep it connected."
      stop_daemon
      start_daemon "$target"
      armed="$target"
      prev_running=""
    fi
  fi

  # Engine log cap. The daemon appends, so truncation is safe.
  if [ -f "$KD_LOG_FILE" ]; then
    sz=$(stat -c %s "$KD_LOG_FILE" 2>/dev/null || echo 0)
    if [ "$sz" -gt $((LOG_MAX_MB * 1024 * 1024)) ]; then
      tail -c $((8 * 1024 * 1024)) "$KD_LOG_FILE" > "$KD_LOG_FILE.1" 2>/dev/null
      : > "$KD_LOG_FILE"
      say "Engine log passed ${LOG_MAX_MB} MB; kept the last 8 MB in $KD_LOG_FILE.1"
    fi
  fi
done
