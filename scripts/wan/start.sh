#!/bin/bash
# Start both peers, exchange fingerprints, and connect.
set -e

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KD="$ROOT/kd"
SSH_KEY="${WAN_SSH_KEY:-$HOME/.ssh/id_rsa_rent}"
SERVER="${WAN_SERVER:-root@185.104.181.40}"
RELAY="${WAN_RELAY:-https://keibidroprelay.keibisoft.com/}"

ssh_cmd() { ssh -i "$SSH_KEY" -o ConnectTimeout=10 "$SERVER" "$@"; }
alice() { KD_SOCKET=/tmp/kd-alice.sock "$KD" "$@"; }

echo "[start] Starting Bob on server..."
ssh -f -i "$SSH_KEY" "$SERVER" "cd /root/KeibiDrop && \
    KD_RELAY='$RELAY' KD_INBOUND_PORT=26431 KD_OUTBOUND_PORT=26432 \
    KD_NO_FUSE=1 KD_SAVE_PATH=/tmp/SaveBob KD_SOCKET=/tmp/kd-bob.sock \
    ./kd start > /tmp/kd-bob.log 2>&1"
sleep 3

echo "[start] Starting Alice on Mac..."
KD_RELAY="$RELAY" KD_INBOUND_PORT=26431 KD_OUTBOUND_PORT=26432 \
    KD_NO_FUSE=1 KD_SAVE_PATH=/tmp/SaveAlice KD_SOCKET=/tmp/kd-alice.sock \
    "$KD" start > /tmp/kd-alice.log 2>&1 &
sleep 3

echo "[start] Exchanging fingerprints..."
BOB_FP=$(ssh_cmd "KD_SOCKET=/tmp/kd-bob.sock /root/KeibiDrop/kd show fingerprint" \
    | uv run python3 -c "import sys,json;print(json.load(sys.stdin)['data']['fingerprint'])")
ALICE_FP=$(alice show fingerprint \
    | uv run python3 -c "import sys,json;print(json.load(sys.stdin)['data']['fingerprint'])")

echo "  Bob:   ${BOB_FP:0:20}..."
echo "  Alice: ${ALICE_FP:0:20}..."

alice register "$BOB_FP" > /dev/null
ssh_cmd "KD_SOCKET=/tmp/kd-bob.sock /root/KeibiDrop/kd register '$ALICE_FP'" > /dev/null

echo "[start] Connecting..."
alice create > /tmp/kd-create.log 2>&1 &
sleep 6
ssh_cmd "KD_SOCKET=/tmp/kd-bob.sock /root/KeibiDrop/kd join"
sleep 3

STATUS=$(alice status | uv run python3 -c "import sys,json;d=json.load(sys.stdin)['data'];print(d['connection_status'])")
echo "[start] Connection: $STATUS"

if [ "$STATUS" = "disconnected" ] || [ "$STATUS" = "unknown" ]; then
    echo "[start] FAILED - check /tmp/kd-alice.log"
    exit 1
fi

echo "[start] Done."
