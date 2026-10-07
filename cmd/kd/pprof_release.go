//go:build !debug

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: No-op stub for maybeStartPprof; the pprof server is debug-only.

package main

import "log/slog"

// maybeStartPprof is a no-op in a non-debug build; KD_PPROF has no effect.
func maybeStartPprof(*slog.Logger) {}
