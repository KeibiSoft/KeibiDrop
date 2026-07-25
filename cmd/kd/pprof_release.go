//go:build !debug

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: No-op stub for maybeStartPprof; the pprof server is debug-only.

package main

import "log/slog"

// maybeStartPprof is a no-op in a plain (non-debug) build; KD_PPROF is unreachable.
func maybeStartPprof(*slog.Logger) {}
