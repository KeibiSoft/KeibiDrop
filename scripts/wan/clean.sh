#!/bin/bash
# Kill kd processes and clean save directories on both Mac and server.
set -e

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KD="$ROOT/kd"
SSH_KEY="${WAN_SSH_KEY:-$HOME/.ssh/id_rsa_rent}"
SERVER="${WAN_SERVER:-root@185.104.181.40}"

ssh_cmd() { ssh -i "$SSH_KEY" -o ConnectTimeout=10 "$SERVER" "$@"; }

echo "[clean] Stopping peers..."
KD_SOCKET=/tmp/kd-alice.sock "$KD" stop 2>/dev/null || true
ssh_cmd "KD_SOCKET=/tmp/kd-bob.sock /root/KeibiDrop/kd stop 2>/dev/null" 2>/dev/null || true
pkill -f "kd start" 2>/dev/null || true
ssh_cmd "pkill -f 'kd start' 2>/dev/null" 2>/dev/null || true
sleep 1

echo "[clean] Removing save directories..."
rm -rf /tmp/SaveAlice /tmp/kd-alice.sock /tmp/kd-create.log
mkdir -p /tmp/SaveAlice
ssh_cmd "rm -rf /tmp/SaveBob /tmp/kd-bob.sock; mkdir -p /tmp/SaveBob"

echo "[clean] Done."
