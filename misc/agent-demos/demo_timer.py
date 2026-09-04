#!/usr/bin/env python3
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# The clock pane. It runs for the whole demo so the elapsed time is on screen
# at every moment, and it names the stage the two agents have reached by
# watching the rendezvous directory they already use.
#
#   demo_timer.py <rendezvous-dir>

import os
import sys
import time

RV = sys.argv[1]

DIM = "\033[38;5;243m"
FG = "\033[38;5;253m"
ACC = "\033[38;5;80m"
R = "\033[0m"

# Each stage is named by the marker file the agents write when they reach it.
STAGES = [
    ("A.code", "codes created"),
    ("B.listed", "dataset listed on the gpu box"),
    ("B.done", "shards read through the mount"),
    ("A.done", "done"),
]


def main():
    start = time.time()
    frozen = None
    while True:
        stage = "starting up"
        for marker, label in STAGES:
            if os.path.exists(os.path.join(RV, marker)):
                stage = label

        if os.path.exists(os.path.join(RV, "A.done")) and frozen is None:
            # Stop the clock and hold the finishing time on screen. The last
            # frame of a recording has to carry the total, not a number that
            # kept climbing after the work was over.
            frozen = time.time() - start

        elapsed = frozen if frozen is not None else time.time() - start
        label = f"{ACC}{elapsed:6.1f}s{R}"
        if frozen is not None:
            label = f"{ACC}{elapsed:6.1f}s{R}  {DIM}total{R}"

        sys.stdout.write(f"\r\033[K  {DIM}elapsed{R}  {label}   {DIM}·{R}   {FG}{stage}{R}")
        sys.stdout.flush()
        time.sleep(0.1)


if __name__ == "__main__":
    sys.exit(main())
