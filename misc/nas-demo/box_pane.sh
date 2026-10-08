#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2025 KeibiSoft S.R.L.
# The box's pane: stream its container log, minus the engine's start-up noise.
set -u
: "${BOX_LOGS:=docker compose logs -f}"
bash -c "$BOX_LOGS" 2>&1 | grep --line-buffered -v -E 'WARN FUSE|^\{|Tier selected' | sed -u 's/ Version: .*//'
