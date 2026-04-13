#!/bin/bash
set -e

KD="$(cd "$(dirname "$0")/../.." && pwd)/kd"
SSH_KEY="$HOME/.ssh/id_rsa_rent"
SERVER="root@185.104.181.40"

ssh_cmd() { ssh -i "$SSH_KEY" -o ConnectTimeout=10 "$SERVER" "$@"; }
alice() { KD_SOCKET=/tmp/kd-alice.sock "$KD" "$@"; }
bob_remote() { ssh_cmd "KD_SOCKET=/tmp/kd-bob.sock /root/KeibiDrop/kd $*"; }

echo "=== WAN Benchmark: Mac <-> VPS ==="
echo ""

# Kill old processes
echo "[1/6] Cleaning up..."
alice stop 2>/dev/null || true
bob_remote stop 2>/dev/null || true
pkill -f "kd start" 2>/dev/null || true
ssh_cmd "pkill -f 'kd start' 2>/dev/null" 2>/dev/null || true
sleep 1

rm -rf /tmp/SaveAlice; mkdir -p /tmp/SaveAlice
ssh_cmd "rm -rf /tmp/SaveBob; mkdir -p /tmp/SaveBob"

# Start peers
echo "[2/6] Starting peers..."
ssh_cmd "cd /root/KeibiDrop && KD_RELAY='https://keibidroprelay.keibisoft.com/' KD_INBOUND_PORT=26431 KD_OUTBOUND_PORT=26432 KD_NO_FUSE=1 KD_SAVE_PATH=/tmp/SaveBob KD_SOCKET=/tmp/kd-bob.sock nohup ./kd start > /tmp/kd-bob.log 2>&1 &"
sleep 2

KD_RELAY='https://keibidroprelay.keibisoft.com/' KD_INBOUND_PORT=26431 KD_OUTBOUND_PORT=26432 KD_NO_FUSE=1 KD_SAVE_PATH=/tmp/SaveAlice KD_SOCKET=/tmp/kd-alice.sock "$KD" start > /tmp/kd-alice.log 2>&1 &
sleep 3

# Get fingerprints
echo "[3/6] Exchanging fingerprints..."
BOB_FP=$(bob_remote "show fingerprint" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['fingerprint'])")
ALICE_FP=$(alice show fingerprint | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['fingerprint'])")
echo "  Bob:   $BOB_FP"
echo "  Alice: $ALICE_FP"

# Register and connect
echo "[4/6] Connecting..."
alice register "$BOB_FP" > /dev/null
bob_remote "register '$ALICE_FP'" > /dev/null

alice create > /tmp/kd-create.log 2>&1 &
sleep 5
bob_remote join
sleep 2

STATUS=$(alice status | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['connection_status'])")
echo "  Connection: $STATUS"

if [ "$STATUS" = "disconnected" ]; then
    echo "FAILED: Not connected. Check logs."
    cat /tmp/kd-alice.log
    exit 1
fi

# Benchmark transfers
echo ""
echo "[5/6] Running benchmarks..."
echo ""

for SIZE in 10mb 100mb 1gb; do
    FILE="/tmp/test-${SIZE}.bin"

    if [ ! -f "$FILE" ]; then
        echo "  Skipping $SIZE (file not found)"
        continue
    fi

    # Mac -> Server (upload)
    echo "--- $SIZE: Mac -> Server (upload) ---"
    alice add "$FILE" 2>&1 || true
    sleep 1

    BASENAME="test-${SIZE}.bin"
    START=$(python3 -c "import time; print(time.time())")
    bob_remote "pull '$BASENAME' '/tmp/SaveBob/$BASENAME'"
    END=$(python3 -c "import time; print(time.time())")

    ELAPSED=$(python3 -c "print(f'{$END - $START:.1f}')")
    BYTES=$(stat -f%z "$FILE" 2>/dev/null || stat -c%s "$FILE" 2>/dev/null)
    SPEED=$(python3 -c "print(f'{$BYTES / ($END - $START) / 1048576:.1f}')")
    echo "  Time: ${ELAPSED}s, Speed: ${SPEED} MB/s"

    # Verify checksum
    LOCAL_MD5=$(md5 -q "$FILE" 2>/dev/null || md5sum "$FILE" | cut -d' ' -f1)
    REMOTE_MD5=$(ssh_cmd "md5sum /tmp/SaveBob/$BASENAME" | cut -d' ' -f1)
    if [ "$LOCAL_MD5" = "$REMOTE_MD5" ]; then
        echo "  Checksum: OK"
    else
        echo "  Checksum: MISMATCH! local=$LOCAL_MD5 remote=$REMOTE_MD5"
    fi

    # Clean up for next test
    ssh_cmd "rm -f /tmp/SaveBob/$BASENAME"
    echo ""
done

# Server -> Mac (download)
for SIZE in 10mb 100mb 1gb; do
    REMOTE_FILE="/tmp/test-${SIZE}.bin"
    BASENAME="test-${SIZE}.bin"

    echo "--- $SIZE: Server -> Mac (download) ---"
    bob_remote "add '$REMOTE_FILE'" 2>&1 || true
    sleep 1

    START=$(python3 -c "import time; print(time.time())")
    alice pull "$BASENAME" "/tmp/SaveAlice/$BASENAME"
    END=$(python3 -c "import time; print(time.time())")

    ELAPSED=$(python3 -c "print(f'{$END - $START:.1f}')")
    BYTES=$(ssh_cmd "stat -c%s $REMOTE_FILE")
    SPEED=$(python3 -c "print(f'{$BYTES / ($END - $START) / 1048576:.1f}')")
    echo "  Time: ${ELAPSED}s, Speed: ${SPEED} MB/s"

    # Verify checksum
    REMOTE_MD5=$(ssh_cmd "md5sum $REMOTE_FILE" | cut -d' ' -f1)
    LOCAL_MD5=$(md5 -q "/tmp/SaveAlice/$BASENAME" 2>/dev/null || md5sum "/tmp/SaveAlice/$BASENAME" | cut -d' ' -f1)
    if [ "$LOCAL_MD5" = "$REMOTE_MD5" ]; then
        echo "  Checksum: OK"
    else
        echo "  Checksum: MISMATCH! local=$LOCAL_MD5 remote=$REMOTE_MD5"
    fi

    rm -f "/tmp/SaveAlice/$BASENAME"
    echo ""
done

echo "[6/6] Cleaning up..."
alice stop 2>/dev/null || true
bob_remote stop 2>/dev/null || true

echo ""
echo "=== Done ==="
