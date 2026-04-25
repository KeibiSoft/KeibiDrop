#!/bin/bash
set -e

MOUNT_FUSE="${1:?Usage: $0 <mount-fuse-bin>}"

MOUNT_DIR=$(mktemp -d)
SAVE_DIR=$(mktemp -d)

cleanup() {
    cd /
    kill "$FUSE_PID" 2>/dev/null || true
    sleep 2
    if [ "$(uname)" = "Linux" ]; then
        fusermount -u "$MOUNT_DIR" 2>/dev/null || true
    else
        umount "$MOUNT_DIR" 2>/dev/null || diskutil unmount "$MOUNT_DIR" 2>/dev/null || true
    fi
    sleep 1
    rm -rf "$MOUNT_DIR" "$SAVE_DIR" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Git Clone on FUSE Test ==="
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

# Clone a small public repo
echo ""
echo "--- Step 1: git clone ---"
git clone https://github.com/KeibiSoft/go-fp.git "$MOUNT_DIR/go-fp" 2>&1
echo "Clone completed"

# Verify git log works
echo ""
echo "--- Step 2: git log ---"
cd "$MOUNT_DIR/go-fp"
GIT_LOG=$(git log --oneline 2>&1)
if echo "$GIT_LOG" | grep -qi "broken\|fatal\|error"; then
    echo "FAIL: git log broken"
    echo "$GIT_LOG"
    exit 1
fi
echo "$GIT_LOG" | head -5
echo "PASS: git log works"

# Verify git status
echo ""
echo "--- Step 3: git status ---"
GIT_STATUS=$(git status 2>&1)
if echo "$GIT_STATUS" | grep -qi "broken\|fatal"; then
    echo "FAIL: git status broken"
    echo "$GIT_STATUS"
    exit 1
fi
echo "$GIT_STATUS" | head -3
echo "PASS: git status works"

# Verify HEAD
echo ""
echo "--- Step 4: HEAD integrity ---"
HEAD_CONTENT=$(cat .git/HEAD)
if echo "$HEAD_CONTENT" | grep -q "invalid"; then
    echo "FAIL: HEAD contains .invalid placeholder"
    echo "HEAD: $HEAD_CONTENT"
    exit 1
fi
echo "HEAD: $HEAD_CONTENT"
echo "PASS: HEAD is valid"

# Verify file content
echo ""
echo "--- Step 5: File integrity ---"
FILE_COUNT=$(find . -name '*.go' | wc -l | tr -d ' ')
echo "Go files: $FILE_COUNT"
if [ "$FILE_COUNT" -lt 4 ]; then
    echo "FAIL: expected at least 4 .go files"
    exit 1
fi
echo "PASS: files present"

echo ""
echo "=== Git Clone on FUSE: ALL PASSED ==="
