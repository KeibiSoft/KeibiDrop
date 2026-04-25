#!/bin/bash
set -e

MOUNT_FUSE="${1:?Usage: $0 <mount-fuse-bin> <fstest-dir>}"
FSTEST_DIR="${2:?}"

if [ ! -d "$FSTEST_DIR/tests" ]; then
    echo "ERROR: fstest not found at $FSTEST_DIR"
    echo "Clone it: git clone https://github.com/pjd/pjdfstest.git $FSTEST_DIR"
    echo "Then: cd $FSTEST_DIR && make"
    exit 1
fi

MOUNT_DIR=$(mktemp -d)
SAVE_DIR=$(mktemp -d)

cleanup() {
    if [ "$(uname)" = "Linux" ]; then
        fusermount -u "$MOUNT_DIR" 2>/dev/null || true
    else
        umount "$MOUNT_DIR" 2>/dev/null || true
    fi
    sleep 1
    rm -rf "$MOUNT_DIR" "$SAVE_DIR"
}
trap cleanup EXIT

echo "=== fstest POSIX Compliance on FUSE ==="
echo "Mount: $MOUNT_DIR"
echo "Save:  $SAVE_DIR"
echo "fstest: $FSTEST_DIR"

# Start FUSE mount
"$MOUNT_FUSE" "$MOUNT_DIR" "$SAVE_DIR" &
FUSE_PID=$!
sleep 3

# Verify mount
if ! mount | grep -q "$MOUNT_DIR"; then
    echo "FAIL: FUSE mount not active"
    exit 1
fi

cd "$MOUNT_DIR"

# Run fstest excluding symlink and hardlink tests (intentionally unsupported)
echo ""
echo "--- Running fstest (excluding symlink, link) ---"
RESULTS=$(mktemp)
prove -rv \
    "$FSTEST_DIR/tests/chmod" \
    "$FSTEST_DIR/tests/chown" \
    "$FSTEST_DIR/tests/mkdir" \
    "$FSTEST_DIR/tests/mkfifo" \
    "$FSTEST_DIR/tests/open" \
    "$FSTEST_DIR/tests/rename" \
    "$FSTEST_DIR/tests/rmdir" \
    "$FSTEST_DIR/tests/truncate" \
    "$FSTEST_DIR/tests/unlink" \
    2>/dev/null | tee "$RESULTS" | tail -5

echo ""
PASSED=$(grep -cE "^ok " "$RESULTS" 2>/dev/null || echo "0")
FAILED=$(grep -cE "^not ok " "$RESULTS" 2>/dev/null || echo "0")
TOTAL=$((PASSED + FAILED))
if [ "$TOTAL" -gt 0 ]; then
    RATE=$(echo "scale=1; $PASSED * 100 / $TOTAL" | bc)
    echo "=== fstest Results: $PASSED/$TOTAL passed ($RATE%) ==="
else
    echo "=== fstest: no results ==="
fi
rm -f "$RESULTS"
