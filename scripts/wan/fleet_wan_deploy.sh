#!/bin/bash
# Sync the CURRENT working tree to Timisoara and Singapore and build kd on both.
# Build only. It starts no daemon and touches no running process.
#
#   scripts/wan/fleet_wan_deploy.sh              both hosts
#   scripts/wan/fleet_wan_deploy.sh timisoara    one host
#
# Sync mechanism copied from fleet/run.sh, which already encodes each host's
# quirks. Do not hand-roll it again:
#   - go is NOT on the non-interactive PATH on Linux, it is /usr/local/go/bin/go
#   - GOFLAGS=-buildvcs=false, because the synced dir is not a git checkout
#   - WinFsp lives under "C:\Program Files (x86)", whose space breaks cgo. The
#     8.3 short path C:/PROGRA~2/WinFsp is the fix, and it must be set BEFORE
#     the build.
#   - /root/KeibiDrop on Timisoara is a STALE clone. The real source dir is
#     /root/kd-bench/src.
set -u
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
. "$REPO/fleet/hosts.sh"
TARGETS="${1:-timisoara singapore}"
rc=0

for e in $TARGETS; do
  "host_$e" || { echo "unknown host $e"; rc=1; continue; }
  echo
  echo "=============================================================="
  echo ">>> DEPLOY: $e  ($OS, $TGT)"
  echo "=============================================================="
  # Under `set -u` bash rejects "${arr[@]}" when arr is EMPTY, which is the
  # Timisoara case (no -i flag). Give the array a harmless first element
  # instead of special-casing every expansion.
  KEYOPT=(-o StrictHostKeyChecking=accept-new)
  [ -n "$KEY" ] && KEYOPT+=(-i "$KEY")
  SSH=(ssh "${KEYOPT[@]}" -o ConnectTimeout=20 "$TGT")

  if [ "$OS" = "windows" ]; then
    ( cd "$REPO" && find . \( -name '.Trashes' -o -path './.git' -o -path './node_modules' -o -path './Save*' \) -prune \
        -o -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' -o -name '*.proto' \) -print0 \
      | tar czf /tmp/kd-wan-src.tgz --null -T - ) || { rc=1; continue; }
    echo "--- sync $(du -h /tmp/kd-wan-src.tgz | cut -f1) ---"
    scp "${KEYOPT[@]}" -q /tmp/kd-wan-src.tgz "$TGT:C:/Windows/Temp/kd-wan-src.tgz" || { rc=1; continue; }
    scp "${KEYOPT[@]}" -q "$REPO/scripts/wan/wan_build.ps1" "$TGT:C:/Windows/Temp/wan_build.ps1" || { rc=1; continue; }
    "${SSH[@]}" "powershell -NoProfile -ExecutionPolicy Bypass -File C:/Windows/Temp/wan_build.ps1 -Src '$SRC'" || rc=1
  else
    echo "--- sync (rsync .go/.mod/.sum/.proto) ---"
    rsync -az -e "ssh ${KEYOPT[*]} -o ConnectTimeout=20" \
      --exclude='.Trashes' --exclude='/Save*' --exclude='/.git' --exclude='/node_modules' \
      --include='*/' --include='*.go' --include='go.mod' --include='go.sum' --include='*.proto' --exclude='*' \
      "$REPO/" "$TGT:$SRC/" || { rc=1; continue; }
    "${SSH[@]}" "set -e
      export GOFLAGS=-buildvcs=false
      cd '$SRC'
      $GO build -o kd ./cmd/kd/
      ls -lh kd
      ./kd version || true" || rc=1
  fi
done

echo
[ "$rc" = "0" ] && echo "DEPLOY OK on: $TARGETS" || echo "DEPLOY had failures"
exit $rc
