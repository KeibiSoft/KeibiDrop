#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# The laptop side of the demo: pair once, connect, use the folder.
# Env: NAS_CODE (the box's code), KD (kd binary), KD_SOCKET, MOUNT.
set -u
KD=${KD:-kd}
MOUNT=${MOUNT:-$HOME/KeibiDrop/Mount}
DIM=$'\033[38;5;243m'; B=$'\033[1m'; G=$'\033[38;5;114m'; N=$'\033[0m'
export KD_SOCKET
say() { printf '%s$ %s%s\n' "$B" "$*" "$N"; }
# Print the command as a user would type it, with the binary shown as kd.
run() { say kd "${@:2}"; "$@"; }
pause() { sleep "${1:-1.2}"; }

sleep 2
if ! $KD contacts 2>/dev/null | grep -q -F "$NAS_CODE"; then
  run $KD add-contact nas "$NAS_CODE"
  pause
fi
run $KD quick-connect "$NAS_CODE"
$KD wait-connected >/dev/null 2>&1
printf '%sconnected%s\n' "$G" "$N"
pause 1.5
sleep 6   # let the box announce its folder
say ls "$MOUNT/"
ls "$MOUNT/"
pause
say "ls $MOUNT/photos | wc -l"
ls "$MOUNT/photos/" | wc -l
pause
f=$(ls "$MOUNT/photos" | head -1)
say "sha256sum $MOUNT/photos/$f | cut -c1-64"
{ shasum -a 256 "$MOUNT/photos/$f" 2>/dev/null || sha256sum "$MOUNT/photos/$f"; } | cut -c1-64
pause 1.5
printf '\n%sThe box serves. The laptop mounts. Only the bytes a program reads cross the wire.%s\n' "$B" "$N"
printf '%skeibidrop.com/docs/how-to/run-on-a-nas.html%s\n' "$DIM" "$N"
