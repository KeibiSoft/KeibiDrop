#!/usr/bin/env bash
# ABOUTME: Connects two running kd daemons (Alice + Bob) and shares a test file.
# ABOUTME: Run example_run_kd_alice.sh and example_run_kd_bob.sh first in separate terminals.
set -e

ALICE="KD_SOCKET=/tmp/kd-alice.sock ./kd"
BOB="KD_SOCKET=/tmp/kd-bob.sock ./kd"

echo "=== Getting fingerprints ==="
ALICE_FP=$(KD_SOCKET=/tmp/kd-alice.sock ./kd show fingerprint | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['fingerprint'])" 2>/dev/null) || {
  # Fallback: parse JSON without python
  ALICE_FP=$(KD_SOCKET=/tmp/kd-alice.sock ./kd show fingerprint | sed 's/.*"fingerprint":"\([^"]*\)".*/\1/')
}
BOB_FP=$(KD_SOCKET=/tmp/kd-bob.sock ./kd show fingerprint | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['fingerprint'])" 2>/dev/null) || {
  BOB_FP=$(KD_SOCKET=/tmp/kd-bob.sock ./kd show fingerprint | sed 's/.*"fingerprint":"\([^"]*\)".*/\1/')
}

echo "Alice fingerprint: $ALICE_FP"
echo "Bob fingerprint:   $BOB_FP"

echo ""
echo "=== Registering fingerprints ==="
KD_SOCKET=/tmp/kd-alice.sock ./kd register "$BOB_FP"
KD_SOCKET=/tmp/kd-bob.sock ./kd register "$ALICE_FP"

echo ""
echo "=== Alice creates room, Bob joins ==="
KD_SOCKET=/tmp/kd-alice.sock ./kd create &
CREATE_PID=$!
sleep 1
KD_SOCKET=/tmp/kd-bob.sock ./kd join
wait $CREATE_PID

echo ""
echo "=== Connected! Checking status ==="
KD_SOCKET=/tmp/kd-alice.sock ./kd status
KD_SOCKET=/tmp/kd-bob.sock ./kd status

echo ""
echo "=== Sharing a test file from Alice ==="
echo "Hello from Alice via kd!" > /tmp/kd-test-hello.txt
KD_SOCKET=/tmp/kd-alice.sock ./kd add /tmp/kd-test-hello.txt

echo ""
echo "=== Listing files ==="
KD_SOCKET=/tmp/kd-alice.sock ./kd list
KD_SOCKET=/tmp/kd-bob.sock ./kd list

echo ""
echo "=== Done! Both peers connected and file shared. ==="
echo "To pull from Bob:  KD_SOCKET=/tmp/kd-bob.sock ./kd pull kd-test-hello.txt ./received.txt"
echo "To disconnect:     KD_SOCKET=/tmp/kd-alice.sock ./kd disconnect"
echo "To stop daemons:   KD_SOCKET=/tmp/kd-alice.sock ./kd stop && KD_SOCKET=/tmp/kd-bob.sock ./kd stop"
