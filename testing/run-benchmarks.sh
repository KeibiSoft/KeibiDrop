#!/bin/bash
set -e

cd "$(dirname "$0")/.."

echo "=== KeibiDrop Benchmark Suite ==="
echo "Machine: $(uname -m) $(uname -s)"
echo "CPU: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || lscpu 2>/dev/null | grep 'Model name' | sed 's/.*: *//')"
echo "RAM: $(sysctl -n hw.memsize 2>/dev/null | awk '{printf "%.0f GB", $1/1073741824}' || free -h 2>/dev/null | awk '/Mem/{print $2}')"
echo ""

FILTER="${1:-Throughput|Profile|Sweep|Latency|Overhead|Baseline|RoundTrip|SecureConn|Cipher}"

echo "Running tests matching: $FILTER"
echo ""

go test -v -count=1 -timeout 300s ./tests/ -run "$FILTER" 2>&1 | \
    grep -E '(throughput|MB/s|BENCH|SWEEP|SecureConn|Latency|block=|workers=|Write MB|Sync MB|Total MB|Read MB|Size|Layer|PASS|FAIL|^ok)' | \
    grep -v 'level='

echo ""
echo "=== Done ==="
