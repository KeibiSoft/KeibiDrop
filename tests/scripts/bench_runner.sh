#!/usr/bin/env bash
# ABOUTME: McGeoch-style benchmark runner for KeibiDrop throughput regression tests.
# ABOUTME: Orchestrates SCP baseline (T0) + 3 treatment binaries × 3 sizes × 4 reps with Latin-square interleaving.

set -euo pipefail

BENCH_DATA_DIR="${BENCH_DATA_DIR:-/tmp/kd-bench-data}"
RAW_TSV="${RAW_TSV:-/tmp/kd-bench-raw.tsv}"
TREATMENTS=(T1 T2 T3)
SIZES=(10MB 100MB 1GB)
SIZE_BYTES=(10485760 104857600 1073741824)
REPS=4  # rep 1 = warmup (discarded in analysis)

# Latin-square order per rep (index into TREATMENTS array)
# Rep 1 (warmup): T1, T2, T3  → orders=(0 1 2)
# Rep 2:          T2, T3, T1  → orders=(1 2 0)
# Rep 3:          T3, T1, T2  → orders=(2 0 1)
# Rep 4:          T1, T3, T2  → orders=(0 2 1)
declare -A LATIN_SQUARE
LATIN_SQUARE[1]="0 1 2"
LATIN_SQUARE[2]="1 2 0"
LATIN_SQUARE[3]="2 0 1"
LATIN_SQUARE[4]="0 2 1"

# ---------------------------------------------------------------------------
# Wait for load < 1.0, polling every 5 seconds
# ---------------------------------------------------------------------------
_wait_for_low_load() {
    while true; do
        local load
        load=$(awk '{print $1}' /proc/loadavg)
        local load_int
        load_int=$(echo "$load" | awk -F. '{print $1}')
        if [ "$load_int" -lt 1 ]; then
            return 0
        fi
        echo "waiting for load... (current: ${load})"
        sleep 5
    done
}

# ---------------------------------------------------------------------------
# Drop page cache
# ---------------------------------------------------------------------------
_drop_caches() {
    sync
    echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# run_one treatment size_idx rep
# ---------------------------------------------------------------------------
run_one() {
    local treatment="$1"
    local size_idx="$2"
    local rep="$3"

    local treatment_lower
    treatment_lower=$(echo "$treatment" | tr '[:upper:]' '[:lower:]')
    local size="${SIZES[$size_idx]}"
    local binary="/tmp/kd-bench-${treatment_lower}.test"
    local ts
    ts=$(date -Iseconds)

    _wait_for_low_load
    _drop_caches

    printf "[Rep %d/%d] %s %s... " "$rep" "$REPS" "$treatment" "$size"

    local output
    output=$(GOGC=off "$binary" -test.run "TestPullFileThroughput/${size}" -test.v -test.count=1 2>&1)

    local bench_line
    bench_line=$(echo "$output" | grep '^BENCH	' | head -1)

    if [ -z "$bench_line" ]; then
        echo "FAILED (no BENCH line in output)"
        echo "--- output ---" >&2
        echo "$output" >&2
        echo "--------------" >&2
        return 1
    fi

    local mbps wall_sec
    mbps=$(echo "$bench_line" | awk -F'\t' '{print $3}')
    wall_sec=$(echo "$bench_line" | awk -F'\t' '{print $4}')

    printf "%s\t%s\t%s\t%s\t%d\t%s\t%s\n" \
        "$ts" "$treatment" "no-FUSE" "$size" "$rep" "$mbps" "$wall_sec" \
        >> "$RAW_TSV"

    echo "done (${mbps} MB/s)"
}

# ---------------------------------------------------------------------------
# run_cp_baseline — T0: raw disk I/O ceiling, 3 sizes × 3 reps using cp
# ---------------------------------------------------------------------------
run_cp_baseline() {
    echo "=== T0 cp baseline (raw I/O ceiling) ==="
    local cp_reps=3
    local i s_idx
    for s_idx in 0 1 2; do
        local size="${SIZES[$s_idx]}"
        local size_lower
        size_lower=$(echo "$size" | tr '[:upper:]' '[:lower:]')
        local size_bytes="${SIZE_BYTES[$s_idx]}"
        local src="${BENCH_DATA_DIR}/payload-${size_lower}.bin"
        local dst="/tmp/kd-bench-cp-dest.bin"

        for (( i=1; i<=cp_reps; i++ )); do
            _wait_for_low_load
            _drop_caches

            printf "[T0 cp rep %d/%d] %s... " "$i" "$cp_reps" "$size"

            local start end wall_sec mbps
            start=$(date +%s%N)
            cp "$src" "$dst"
            end=$(date +%s%N)

            wall_sec=$(awk "BEGIN{printf \"%.3f\", ($end - $start)/1e9}")
            mbps=$(awk "BEGIN{printf \"%.2f\", $size_bytes / $wall_sec / 1048576}")

            rm -f "$dst"

            printf "%s\tT0\tcp\t%s\t%d\t%s\t%s\n" \
                "$(date -Iseconds)" "$size" "$i" "$mbps" "$wall_sec" \
                >> "$RAW_TSV"

            echo "done (${mbps} MB/s)"
        done
    done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
if [ ! -f "$RAW_TSV" ]; then
    printf "timestamp\ttreatment\tmode\tsize\trep\tmbps\twall_sec\n" > "$RAW_TSV"
fi

echo "=== KeibiDrop McGeoch Benchmark Runner ==="
echo "Raw TSV: $RAW_TSV"
echo ""

run_cp_baseline

echo ""
echo "=== Treatment runs ==="

for (( rep=1; rep<=REPS; rep++ )); do
    read -ra orders <<< "${LATIN_SQUARE[$rep]}"
    for treatment_idx in "${orders[@]}"; do
        for (( s_idx=0; s_idx<${#SIZES[@]}; s_idx++ )); do
            run_one "${TREATMENTS[$treatment_idx]}" "$s_idx" "$rep"
        done
    done
done

echo ""
echo "DONE. Results in $RAW_TSV"
