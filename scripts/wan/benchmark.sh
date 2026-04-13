#!/bin/bash
# Run WAN benchmark: timed file transfers with checksum verification.
set -e

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KD="$ROOT/kd"
SSH_KEY="${WAN_SSH_KEY:-$HOME/.ssh/id_rsa_rent}"
SERVER="${WAN_SERVER:-root@185.104.181.40}"

ssh_cmd() { ssh -i "$SSH_KEY" -o ConnectTimeout=10 "$SERVER" "$@"; }
alice() { KD_SOCKET=/tmp/kd-alice.sock "$KD" "$@"; }
bob() { ssh_cmd "KD_SOCKET=/tmp/kd-bob.sock /root/KeibiDrop/kd $*"; }
now() { uv run python3 -c "import time;print(time.time())"; }

echo "=========================================="
echo "  KeibiDrop WAN Benchmark (no-FUSE)"
echo "=========================================="

# Create test files if missing
for SIZE_MB in 10 100 1024; do
    F="/tmp/test-${SIZE_MB}mb.bin"
    if [ ! -f "$F" ]; then
        echo "[setup] Creating ${SIZE_MB}MB test file..."
        dd if=/dev/urandom of="$F" bs=1m count=$SIZE_MB 2>/dev/null
    fi
done
ssh_cmd "for S in 10 100 1024; do F=/tmp/test-\${S}mb.bin; [ -f \$F ] || dd if=/dev/urandom of=\$F bs=1M count=\$S 2>/dev/null; done"

# Clean save dirs
rm -rf /tmp/SaveAlice/*
ssh_cmd "rm -rf /tmp/SaveBob/*"

echo ""
echo "=== UPLOAD (Mac -> Server) ==="

for PAIR in "10:10mb" "100:100mb" "1024:1gb"; do
    SIZE_MB="${PAIR%%:*}"
    LABEL="${PAIR##*:}"
    FILE="/tmp/test-${SIZE_MB}mb.bin"
    BYTES=$(stat -f%z "$FILE" 2>/dev/null || stat -c%s "$FILE")

    echo ""
    echo "--- ${LABEL} upload ---"
    alice add "$FILE" 2>&1 | grep -v '"ok":true' || true
    sleep 1

    T1=$(now)
    bob "pull test-${SIZE_MB}mb.bin /tmp/SaveBob/test-${SIZE_MB}mb.bin" 2>&1
    T2=$(now)

    uv run python3 -c "e=$T2-$T1; print(f'  Time: {e:.1f}s, Speed: {$BYTES/e/1048576:.1f} MB/s')"

    LOCAL_MD5=$(md5 -q "$FILE" 2>/dev/null || md5sum "$FILE" | cut -d' ' -f1)
    REMOTE_MD5=$(ssh_cmd "md5sum /tmp/SaveBob/test-${SIZE_MB}mb.bin 2>/dev/null" | cut -d' ' -f1)
    if [ "$LOCAL_MD5" = "$REMOTE_MD5" ]; then
        echo "  Checksum: OK"
    else
        echo "  Checksum: MISMATCH local=$LOCAL_MD5 remote=$REMOTE_MD5"
    fi
done

echo ""
echo "=== DOWNLOAD (Server -> Mac) ==="

for PAIR in "10:10mb" "100:100mb" "1024:1gb"; do
    SIZE_MB="${PAIR%%:*}"
    LABEL="${PAIR##*:}"
    REMOTE_FILE="/tmp/test-${SIZE_MB}mb.bin"
    BYTES=$(ssh_cmd "stat -c%s $REMOTE_FILE")

    echo ""
    echo "--- ${LABEL} download ---"
    bob "add $REMOTE_FILE" 2>&1 | grep -v '"ok":true' || true
    sleep 1

    T1=$(now)
    alice pull "test-${SIZE_MB}mb.bin" "/tmp/SaveAlice/test-${SIZE_MB}mb.bin" 2>&1
    T2=$(now)

    uv run python3 -c "e=$T2-$T1; print(f'  Time: {e:.1f}s, Speed: {$BYTES/e/1048576:.1f} MB/s')"

    REMOTE_MD5=$(ssh_cmd "md5sum $REMOTE_FILE" | cut -d' ' -f1)
    LOCAL_MD5=$(md5 -q "/tmp/SaveAlice/test-${SIZE_MB}mb.bin" 2>/dev/null || echo "MISSING")
    if [ "$LOCAL_MD5" = "$REMOTE_MD5" ]; then
        echo "  Checksum: OK"
    else
        echo "  Checksum: MISMATCH local=$LOCAL_MD5 remote=$REMOTE_MD5"
    fi
done

echo ""
echo "=== DONE ==="
