#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
# The box's pane: stream its container log, minus the engine's start-up noise.
set -u
: "${BOX_LOGS:=docker compose logs -f}"
bash -c "$BOX_LOGS" 2>&1 | grep --line-buffered -v -E 'WARN FUSE|^\{|Tier selected' | sed -u 's/ Version: .*//'
