//go:build debug

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Debug-only KD_PPROF server; not built into a plain (non-debug) binary.

package main

import (
	"log/slog"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"time"
)

// maybeStartPprof starts net/http/pprof on the KD_PPROF address, loopback only.
// A dedicated mux keeps the handlers off other listeners.
func maybeStartPprof(logger *slog.Logger) {
	if addr := os.Getenv("KD_PPROF"); addr != "" {
		if isLoopbackAddr(addr) {
			logger.Warn("DEBUG pprof endpoint active (testing only)", "addr", addr)
			mux := http.NewServeMux()
			mux.HandleFunc("/debug/pprof/", httppprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
			pprofSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go func() {
				if err := pprofSrv.ListenAndServe(); err != nil {
					logger.Error("pprof server exited", "error", err)
				}
			}()
		} else {
			logger.Error("KD_PPROF refused: bind a loopback address (127.0.0.1/::1), never off-box", "addr", addr)
		}
	}
}
