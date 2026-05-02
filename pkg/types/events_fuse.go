// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: FUSE-specific type conversions between fuse.Stat_t and protobuf Attr.
// ABOUTME: Excluded on Android where FUSE is unavailable.

package types

import (
	"time"

	keibidrop "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/winfsp/cgofuse/fuse"
)

// timespecToNano converts a fuse.Timespec to total nanoseconds since Unix epoch.
func timespecToNano(ts fuse.Timespec) uint64 {
	return uint64(ts.Sec)*uint64(time.Second) + uint64(ts.Nsec) //nolint:gosec // G115: FUSE timespec values are positive
}

// StatToAttr converts a FUSE Stat_t to a protobuf Attr.
func StatToAttr(stat *fuse.Stat_t) *keibidrop.Attr {
	if stat == nil {
		return nil
	}

	return &keibidrop.Attr{
		Dev:              stat.Dev,
		Ino:              stat.Ino,
		Mode:             stat.Mode,
		Size:             stat.Size,
		AccessTime:       timespecToNano(stat.Atim),
		ModificationTime: timespecToNano(stat.Mtim),
		ChangeTime:       timespecToNano(stat.Ctim),
		BirthTime:        timespecToNano(stat.Birthtim),
		Flags:            stat.Flags,
	}
}

// AttrToStat converts a protobuf Attr back to a FUSE Stat_t.
func AttrToStat(attr *keibidrop.Attr) *fuse.Stat_t {
	if attr == nil {
		return nil
	}

	return &fuse.Stat_t{
		Dev:      attr.Dev,
		Ino:      attr.Ino,
		Mode:     attr.Mode,
		Size:     attr.Size,
		Atim:     NanoToTimespec(attr.AccessTime),
		Mtim:     NanoToTimespec(attr.ModificationTime),
		Ctim:     NanoToTimespec(attr.ChangeTime),
		Birthtim: NanoToTimespec(attr.BirthTime),
		Flags:    attr.Flags,
	}
}

// NanoToTimespec converts nanoseconds since Unix epoch to a fuse.Timespec.
func NanoToTimespec(ns uint64) fuse.Timespec {
	return fuse.NewTimespec(time.Unix(0, int64(ns))) //nolint:gosec // G115: nanoseconds fit int64 until year 2262
}
