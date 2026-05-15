#!/usr/bin/env bash
# ABOUTME: E2E test runner that orchestrates desktop kd CLI + Maestro Android flows.
# ABOUTME: Usage: ./run-e2e.sh [flow_number] — runs all flows or a single one.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
KD="$PROJECT_ROOT/kd"
MAESTRO="${MAESTRO_BIN:-$HOME/.maestro/bin/maestro}"
ADB="${ADB_BIN:-$HOME/android-sdk/platform-tools/adb}"
SAVE_PATH="$PROJECT_ROOT/SaveAlice"
SCREENSHOTS_DIR="$SCRIPT_DIR/screenshots"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

mkdir -p "$SAVE_PATH" "$SCREENSHOTS_DIR"

passed=0
failed=0
skipped=0

log()  { echo -e "${GREEN}[E2E]${NC} $*"; }
warn() { echo -e "${YELLOW}[E2E]${NC} $*"; }
err()  { echo -e "${RED}[E2E]${NC} $*"; }

cleanup() {
    log "Cleaning up..."
    "$KD" stop 2>/dev/null || true
    "$ADB" shell am force-stop com.keibisoft.keibidrop 2>/dev/null || true
}
trap cleanup EXIT

preflight() {
    log "Preflight checks..."
    [ -x "$KD" ] || { err "kd binary not found at $KD"; exit 1; }
    [ -x "$MAESTRO" ] || { err "maestro not found at $MAESTRO"; exit 1; }
    "$ADB" devices | grep -q "device$" || { err "No Android device connected"; exit 1; }
    "$ADB" shell pm list packages | grep -q "com.keibisoft.keibidrop" || { err "KeibiDrop not installed on device"; exit 1; }
    log "All preflight checks passed"
}

start_desktop() {
    "$KD" stop 2>/dev/null || true
    sleep 1
    KD_SAVE_PATH="$SAVE_PATH" KD_NO_FUSE=1 "$KD" start >/dev/null 2>&1 &
    sleep 3

    DESKTOP_FP=$("$KD" show fingerprint 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['fingerprint'])")
    [ -n "$DESKTOP_FP" ] || { err "Failed to get desktop fingerprint"; exit 1; }
    export DESKTOP_FINGERPRINT="$DESKTOP_FP"
    log "Desktop started, fingerprint: ${DESKTOP_FP:0:12}..."
}

get_android_fingerprint() {
    "$ADB" shell uiautomator dump /sdcard/ui.xml 2>/dev/null
    "$ADB" pull /sdcard/ui.xml /tmp/android-ui.xml 2>/dev/null
    ANDROID_FP=$(grep -oP 'text="[A-Za-z0-9_-]{80,}"' /tmp/android-ui.xml | head -1 | sed 's/text="//;s/"//')
    echo "$ANDROID_FP"
}

register_and_connect_desktop() {
    local android_fp="$1"
    "$KD" register "$android_fp" 2>/dev/null
    "$KD" connect 2>/dev/null
}

run_flow() {
    local flow_file="$1"
    local flow_name
    flow_name=$(basename "$flow_file" .yaml)

    log "Running flow: $flow_name"
    if MAESTRO_CLI_NO_ANALYTICS=1 "$MAESTRO" test \
        --env "DESKTOP_FINGERPRINT=$DESKTOP_FINGERPRINT" \
        "$flow_file" 2>&1; then
        log "${GREEN}PASS${NC}: $flow_name"
        ((passed++)) || true
        return 0
    else
        err "${RED}FAIL${NC}: $flow_name"
        ((failed++)) || true
        return 1
    fi
}

# --- Flow orchestration ---

run_01_connect() {
    log "=== Scenario 1: Code exchange and connection ==="
    start_desktop

    # Launch app first to get the Android fingerprint.
    "$ADB" shell am start -n com.keibisoft.keibidrop/.MainActivity
    sleep 3
    local android_fp
    android_fp=$(get_android_fingerprint)
    [ -n "$android_fp" ] || { err "Failed to get Android fingerprint"; return 1; }
    log "Android fingerprint: ${android_fp:0:12}..."

    # Register Android on desktop.
    "$KD" register "$android_fp" 2>/dev/null

    # Force-stop app so Maestro gets a clean launch.
    "$ADB" shell am force-stop com.keibisoft.keibidrop 2>/dev/null
    sleep 1

    # Desktop connect retry loop in background.
    # Keeps trying every 3s until connected or 60s elapsed.
    (
        for attempt in $(seq 1 20); do
            result=$("$KD" connect 2>&1)
            if echo "$result" | grep -q '"status":"connected"'; then
                break
            fi
            sleep 3
        done
    ) &
    local kd_pid=$!

    run_flow "$SCRIPT_DIR/01_connect.yaml"
    local result=$?

    wait "$kd_pid" 2>/dev/null || true

    # Verify desktop side is connected.
    local status
    status=$("$KD" status 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['connection_status'])" 2>/dev/null || echo "unknown")
    log "Desktop status: $status"

    return $result
}

run_02_file_visible() {
    log "=== Scenario 2: File visibility ==="

    # Share test files from desktop.
    echo "KeibiDrop test $(date)" > "$SAVE_PATH/test-small.txt"
    dd if=/dev/urandom of="$PROJECT_ROOT/test-10mb.bin" bs=1M count=10 2>/dev/null

    "$KD" add "$SAVE_PATH/test-small.txt" 2>/dev/null
    "$KD" add "$PROJECT_ROOT/test-10mb.bin" 2>/dev/null
    sleep 2

    run_flow "$SCRIPT_DIR/02_file_visible.yaml"
}

run_03_save_bug() {
    log "=== Scenario 3: Save-kills-connection bug ==="
    run_flow "$SCRIPT_DIR/03_save_disconnect_bug.yaml"
}

run_04_status_bug() {
    log "=== Scenario 4: Reconnecting status bug ==="

    # Need a fresh connection for this test.
    "$KD" disconnect 2>/dev/null || true
    sleep 2
    "$ADB" shell am force-stop com.keibisoft.keibidrop 2>/dev/null
    sleep 1

    "$ADB" shell am start -n com.keibisoft.keibidrop/.MainActivity
    sleep 3
    local android_fp
    android_fp=$(get_android_fingerprint)
    "$KD" register "$android_fp" 2>/dev/null

    "$KD" connect 2>/dev/null &
    run_flow "$SCRIPT_DIR/04_reconnect_status_bug.yaml"
    wait 2>/dev/null || true
}

run_05_disconnect_reconnect() {
    log "=== Scenario 5: Disconnect/Reconnect ==="

    # Disconnect from desktop (Android should detect and return to Screen 0).
    "$KD" disconnect 2>/dev/null
    sleep 2

    # Re-register for reconnection.
    "$ADB" shell am start -n com.keibisoft.keibidrop/.MainActivity 2>/dev/null
    sleep 3
    local android_fp
    android_fp=$(get_android_fingerprint)
    "$KD" register "$android_fp" 2>/dev/null

    # Desktop connect in background — Maestro will tap Connect on Android.
    "$KD" connect 2>/dev/null &

    run_flow "$SCRIPT_DIR/05_disconnect_reconnect.yaml"
    wait 2>/dev/null || true
}

# --- Main ---

preflight

FLOW_NUM="${1:-all}"

if [ "$FLOW_NUM" = "all" ]; then
    run_01_connect
    run_02_file_visible
    run_03_save_bug
    run_04_status_bug
    run_05_disconnect_reconnect
else
    case "$FLOW_NUM" in
        1) run_01_connect ;;
        2) run_02_file_visible ;;
        3) run_03_save_bug ;;
        4) run_04_status_bug ;;
        5) run_05_disconnect_reconnect ;;
        *) err "Unknown flow: $FLOW_NUM"; exit 1 ;;
    esac
fi

echo ""
log "========================================="
log "Results: ${GREEN}$passed passed${NC}, ${RED}$failed failed${NC}, ${YELLOW}$skipped skipped${NC}"
log "Screenshots: $SCREENSHOTS_DIR/"
log "========================================="

[ "$failed" -eq 0 ]
