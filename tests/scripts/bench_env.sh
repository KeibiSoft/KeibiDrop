# ABOUTME: Benchmark environment setup for KeibiDrop McGeoch-style regression tests.
# ABOUTME: Sources or runs standalone; idempotent — safe to call before each experiment run.

set -euo pipefail

BENCH_DATA_DIR="${BENCH_DATA_DIR:-/tmp/kd-bench-data}"

# ---------------------------------------------------------------------------
# Load average guard — abort if system is busy
# ---------------------------------------------------------------------------
_load=$(awk '{print $1}' /proc/loadavg)
_load_int=$(echo "$_load" | awk -F. '{print $1}')
if [ "$_load_int" -ge 1 ]; then
    echo "ERROR: load average ${_load} >= 1.0 — system too busy for benchmarks." >&2
    exit 1
fi
echo "INFO: load average ${_load} — OK"

# ---------------------------------------------------------------------------
# Drop page cache
# ---------------------------------------------------------------------------
sync
echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null 2>&1 || echo "INFO: page cache drop skipped (no sudo)"
echo "INFO: page cache drop done"

# ---------------------------------------------------------------------------
# Generate test payloads (skip if file exists at correct size)
# ---------------------------------------------------------------------------
mkdir -p "$BENCH_DATA_DIR"

_gen_file() {
    local path="$1"
    local size_bytes="$2"
    local label="$3"

    if [ -f "$path" ]; then
        local actual
        actual=$(stat -c '%s' "$path")
        if [ "$actual" -eq "$size_bytes" ]; then
            echo "INFO: ${label} payload already exists at ${path} — skipping"
            return
        fi
        echo "INFO: ${label} payload exists but wrong size (${actual} vs ${size_bytes}) — regenerating"
        rm -f "$path"
    fi

    echo "INFO: generating ${label} payload at ${path} ..."
    local bs=1048576  # 1 MiB blocks
    local count=$(( size_bytes / bs ))
    dd if=/dev/urandom of="$path" bs=$bs count=$count status=none
    echo "INFO: ${label} payload generated"
}

_gen_file "${BENCH_DATA_DIR}/payload-10mb.bin"  $((  10 * 1024 * 1024)) "10MB"
_gen_file "${BENCH_DATA_DIR}/payload-100mb.bin" $(( 100 * 1024 * 1024)) "100MB"
_gen_file "${BENCH_DATA_DIR}/payload-1gb.bin"   $((1024 * 1024 * 1024)) "1GB"

# ---------------------------------------------------------------------------
# Record system info
# ---------------------------------------------------------------------------
echo "INFO: kernel   = $(uname -r)"
echo "INFO: cpu      = $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs)"
echo "INFO: go       = $(go version 2>/dev/null || echo 'not found')"
echo "INFO: git      = $(git -C "$(dirname "$0")/../.." rev-parse --short HEAD 2>/dev/null || echo 'unknown')"

# ---------------------------------------------------------------------------
# Disable GC for benchmark processes (caller should eval this or export it)
# ---------------------------------------------------------------------------
export GOGC=off
echo "export GOGC=off"

# ---------------------------------------------------------------------------
# TSV header for raw.tsv
# ---------------------------------------------------------------------------
echo -e "timestamp\ttreatment\tmode\tsize\trep\tmbps\twall_sec"
