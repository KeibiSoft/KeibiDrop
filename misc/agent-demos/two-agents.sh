#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# Two agents, one per pane, each driving its own KeibiDrop peer through MCP.
# The left machine holds a dataset. The right one reads it without copying it.
#
#   KDMCP=/path/to/kdmcp ./two-agents.sh
#
# Run it on its own to watch live, or let make-gif.sh record it.

set -u

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
KDMCP=${KDMCP:-$repo/kdmcp}
SESSION=kd-two-agents
WORK=${WORK:-/tmp/kd-two-agents}
RV=$WORK/rendezvous

command -v tmux >/dev/null || { echo "tmux is not installed"; exit 1; }
[ -x "$KDMCP" ] || { echo "no kdmcp at $KDMCP (run: make build-kdmcp)"; exit 1; }

pkill -f "$KDMCP" 2>/dev/null
tmux kill-session -t "$SESSION" 2>/dev/null
sleep 1
# Unmount before deleting: a live FUSE mount makes rm -rf fail halfway and
# leaves the next run with a half-built workspace.
for m in "$WORK/A/mnt" "$WORK/B/mnt"; do
  umount "$m" 2>/dev/null || diskutil unmount force "$m" >/dev/null 2>&1
done
rm -rf "$WORK"
mkdir -p "$RV"

pane() {
  # Each pane runs one agent. bash -lc keeps the pane open long enough for the
  # last frame to be readable.
  echo "KDMCP='$KDMCP' uv run --no-project '$here/agent_pane.py' $1 '$RV' '$WORK'; sleep 900"
}

tmux new-session  -d -s "$SESSION" -x "${COLS:-190}" -y "${ROWS:-46}" "$(pane A)"
tmux split-window -h -t "$SESSION" "$(pane B)"
# A thin full-width pane at the bottom: the clock runs for the whole demo, so
# the elapsed time is on screen in every frame.
tmux split-window -v -f -l 3 -t "$SESSION" \
  "uv run --no-project '$here/demo_timer.py' '$RV'"

tmux set  -t "$SESSION" -g status off
tmux setw -t "$SESSION" -g pane-border-status top
tmux setw -t "$SESSION" -g pane-border-format \
  '#{?#{==:#{pane_index},0}, laptop ,#{?#{==:#{pane_index},1}, gpu box , }}'
tmux setw -t "$SESSION" -g pane-border-style 'fg=colour238'
tmux setw -t "$SESSION" -g pane-active-border-style 'fg=colour238'
tmux setw -t "$SESSION" -g window-style 'bg=colour234'
tmux setw -t "$SESSION" -g pane-active-border-style 'fg=colour238'

tmux attach -t "$SESSION"
