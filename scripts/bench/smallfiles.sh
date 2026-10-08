#!/bin/sh
# Small-file extraction walk + announce benchmarks, both warm settings.
# Usage: scripts/bench/smallfiles.sh [count]
# KD_SMALLFILES_COUNT and KD_ANNOUNCE_COUNT scale the shapes.
set -eu
cd "$(dirname "$0")/../.."

COUNT="${1:-400}"
OUT="${TMPDIR:-/tmp}/kd-smallfiles-bench.log"

KD_SMALLFILES_COUNT="$COUNT" go test ./tests/ -run 'TestSmallFilesWalk|TestAnnounceNestedTree' \
  -count=1 -v -timeout 30m 2>&1 | tee "$OUT"

echo
echo "BENCH lines:"
grep "BENCH" "$OUT" || true
