#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# Record the NAS demo and put the KeibiDrop wordmark in the corner.
#
#   BOX_LOGS=... NAS_CODE=... KD=... KD_SOCKET=... ./make-gif.sh
#
# Needs vhs (with ttyd), rsvg-convert, ffmpeg and magick.
#
# The panes hold their final screen rather than exiting, so the recording ends
# on the finished result and the tape's own length decides the duration.

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
logo=$repo/logo.svg
raw=$here/nas-demo-raw.gif
out=$here/nas-demo.gif

for tool in vhs rsvg-convert ffmpeg magick; do
  command -v "$tool" >/dev/null || { echo "missing $tool"; exit 1; }
done
command -v tmux >/dev/null || { echo "missing tmux"; exit 1; }

echo "Recording..."
( cd "$here" && vhs demo.tape )

echo "Rendering the wordmark..."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# The source is a 146x24 wordmark; 3x keeps it crisp once scaled into place.
rsvg-convert -w 438 -h 72 "$logo" -o "$tmp/logo-raw.png"
# The wordmark ships dark, for a light page. The terminal theme is dark, so
# repaint every pixel white and keep the original alpha as the shape.
magick "$tmp/logo-raw.png" -fill white -colorize 100 "$tmp/logo.png"

echo "Compositing..."
# Bottom right, inset from the edge, at 80% opacity: readable, and out of the
# way of the text. The palette is generated from the composited frames so the
# wordmark does not pick up colour banding.
ffmpeg -y -loglevel error \
  -i "$raw" -i "$tmp/logo.png" \
  -filter_complex "\
    [1:v]scale=219:36,format=rgba,colorchannelmixer=aa=0.80[wm];\
    [0:v][wm]overlay=W-w-24:H-h-16[v];\
    [v]split[a][b];\
    [a]palettegen=stats_mode=diff[p];\
    [b][p]paletteuse=dither=bayer:bayer_scale=4" \
  "$out"

# The panes are left running by design; clear them up so the next run starts cold.
tmux kill-session -t kd-nas-demo 2>/dev/null || true

ls -lh "$out"
echo "Wrote $out"
