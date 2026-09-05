#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# Two panes: the box's container log on the left, the laptop on the right.
#
#   BOX_LOGS="docker compose logs -f" NAS_CODE=<the box's code> ./nas-demo.sh
#
# BOX_LOGS is any command that streams the box's log (a local compose, or
# `ssh box docker logs -f keibidrop`). The laptop pane drives a running kd
# daemon through KD_SOCKET (see laptop_pane.sh). Needs tmux.

set -u
here=$(cd "$(dirname "$0")" && pwd)
SESSION=kd-nas-demo
: "${BOX_LOGS:=docker compose logs -f}"
: "${NAS_CODE:?set NAS_CODE to the code the box printed}"
: "${KD:=kd}"
: "${KD_SOCKET:=}"
: "${MOUNT:=$HOME/KeibiDrop/Mount}"

command -v tmux >/dev/null || { echo "tmux is not installed"; exit 1; }
tmux kill-session -t "$SESSION" 2>/dev/null
sleep 1

export BOX_LOGS NAS_CODE KD KD_SOCKET MOUNT
# bash -lc keeps a pane open long enough for the last frame to be readable.
tmux new-session  -d -s "$SESSION" -x "${COLS:-190}" -y "${ROWS:-40}" \
  "bash -lc '$here/box_pane.sh; sleep 900'"
tmux split-window -h -t "$SESSION" \
  "bash -lc '$here/laptop_pane.sh; sleep 900'"
tmux set  -t "$SESSION" -g status off
tmux setw -t "$SESSION" -g pane-border-status top
tmux setw -t "$SESSION" -g pane-border-format \
  '#{?#{==:#{pane_index},0}, the box: docker compose logs -f ,#{?#{==:#{pane_index},1}, the laptop , }}'
tmux setw -t "$SESSION" -g pane-border-style 'fg=colour238'
tmux setw -t "$SESSION" -g pane-active-border-style 'fg=colour238'
tmux setw -t "$SESSION" -g window-style 'bg=colour234'
tmux attach -t "$SESSION"
