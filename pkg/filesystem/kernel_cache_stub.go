// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

//go:build !android && (!(darwin || linux) || !cgo)

package filesystem

// Windows (WinFsp) and builds without cgo: no per-path kernel cache drop.
// The Windows read path serves an accepted update already.

func (d *Dir) captureKernelCache()            {}
func (d *Dir) releaseKernelCache()            {}
func (d *Dir) invalidateKernelCache(_ string) {}
