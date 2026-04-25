#!/bin/bash
set -e

MOUNT_FUSE="${1:?Usage: $0 <mount-fuse-bin> <initdb> <pg_ctl> <psql>}"
INITDB="${2:?}"
PG_CTL="${3:?}"
PSQL="${4:?}"

MOUNT_DIR=$(mktemp -d)
SAVE_DIR=$(mktemp -d)
PG_PORT=5599
PG_LOG=$(mktemp)

cleanup() {
    "$PG_CTL" -D "$MOUNT_DIR/pgdata" stop 2>/dev/null || true
    sleep 1
    if [ "$(uname)" = "Linux" ]; then
        fusermount -u "$MOUNT_DIR" 2>/dev/null || true
    else
        umount "$MOUNT_DIR" 2>/dev/null || true
    fi
    sleep 1
    rm -rf "$MOUNT_DIR" "$SAVE_DIR" "$PG_LOG"
}
trap cleanup EXIT

echo "=== PostgreSQL on FUSE Test ==="
echo "Mount: $MOUNT_DIR"
echo "Save:  $SAVE_DIR"

# Start FUSE mount
"$MOUNT_FUSE" "$MOUNT_DIR" "$SAVE_DIR" &
FUSE_PID=$!
sleep 3

# Verify mount
if ! mount | grep -q "$MOUNT_DIR"; then
    echo "FAIL: FUSE mount not active"
    exit 1
fi

# Determine user for initdb (cannot run as root)
if [ "$(id -u)" = "0" ]; then
    PG_USER="postgres"
    RUN="su -s /bin/bash $PG_USER -c"
    chmod 777 "$MOUNT_DIR" "$SAVE_DIR"
else
    RUN="bash -c"
fi

# Step 1: initdb
echo ""
echo "--- Step 1: initdb ---"
$RUN "$INITDB -D $MOUNT_DIR/pgdata --no-locale -E UTF8" 2>&1 | tail -3

# Count files
FUSE_FILES=$(find "$MOUNT_DIR/pgdata" -type f | wc -l | tr -d ' ')
echo "Files on FUSE mount: $FUSE_FILES"

# Compare with native
NATIVE_DIR=$(mktemp -d)
$RUN "$INITDB -D $NATIVE_DIR/pgdata --no-locale -E UTF8" > /dev/null 2>&1
NATIVE_FILES=$(find "$NATIVE_DIR/pgdata" -type f | wc -l | tr -d ' ')
rm -rf "$NATIVE_DIR"
echo "Files from native initdb: $NATIVE_FILES"

if [ "$FUSE_FILES" != "$NATIVE_FILES" ]; then
    echo "FAIL: file count mismatch (FUSE=$FUSE_FILES, native=$NATIVE_FILES)"
    exit 1
fi
echo "PASS: file counts match"

# Step 2: Start PostgreSQL
echo ""
echo "--- Step 2: Start PostgreSQL ---"
chmod 700 "$MOUNT_DIR/pgdata" 2>/dev/null || true
$RUN "$PG_CTL -D $MOUNT_DIR/pgdata -o '-p $PG_PORT' -l $PG_LOG start"
sleep 2

# Step 3: CRUD operations
echo ""
echo "--- Step 3: CRUD ---"
$RUN "$PSQL -h /tmp -p $PG_PORT -d postgres -c \"
CREATE TABLE test_data (id SERIAL PRIMARY KEY, name TEXT, value FLOAT);
INSERT INTO test_data (name, value) SELECT md5(i::text), random() FROM generate_series(1, 1000) AS i;
SELECT count(*) FROM test_data;
\""

ROWS=$($RUN "$PSQL -h /tmp -p $PG_PORT -d postgres -t -c 'SELECT count(*) FROM test_data'" | tr -d ' ')
if [ "$ROWS" != "1000" ]; then
    echo "FAIL: expected 1000 rows, got $ROWS"
    exit 1
fi
echo "PASS: 1000 rows inserted and verified"

# Step 4: Shutdown
echo ""
echo "--- Step 4: Shutdown ---"
$RUN "$PG_CTL -D $MOUNT_DIR/pgdata stop"

echo ""
echo "=== PostgreSQL on FUSE: ALL PASSED ==="
