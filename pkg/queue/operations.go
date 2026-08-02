// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package queue

import (
	"time"
)

// OperationType defines the type of filesystem operation being queued.
type OperationType int

const (
	OpWrite OperationType = iota
	OpCreate
	OpDelete
	OpRename
	OpMkdir
	OpRmdir
)

// QueuedOperation is a pending filesystem operation to send to the peer.
type QueuedOperation struct {
	ID        uint64 // Monotonic operation ID
	Type      OperationType
	Path      string    // Relative path in the mounted filesystem
	OldPath   string    // Source path for RENAME
	Data      []byte    // Data to write, for WRITE
	Offset    int64     // File offset, for WRITE
	Size      int64     // File size, for CREATE/EDIT
	Mode      uint32    // File mode and permissions
	Mtime     int64     // Modification time (unix nano)
	Checksum  uint64    // xxHash3 of Data, for integrity checks
	CreatedAt time.Time // Queue time
	Retries   int       // Replay attempt count
}

// FileVersion tracks file state for conflict detection.
type FileVersion struct {
	Path       string
	Size       int64
	Mtime      int64  // unix nano
	Checksum   uint64 // xxHash3
	SourcePeer string // "local" or peer fingerprint prefix
}

// Conflict represents a detected file conflict.
type Conflict struct {
	Path          string
	LocalVersion  *FileVersion
	RemoteVersion *FileVersion
	DetectedAt    time.Time
}

// ConflictResolution defines how to resolve a file conflict.
type ConflictResolution int

const (
	KeepBoth      ConflictResolution = iota // Default: rename local as .conflict.TIMESTAMP
	LastWriteWins                           // Higher mtime wins
	AskUser                                 // UI prompt for choice
)

// String returns the operation type as a string.
func (t OperationType) String() string {
	switch t {
	case OpWrite:
		return "WRITE"
	case OpCreate:
		return "CREATE"
	case OpDelete:
		return "DELETE"
	case OpRename:
		return "RENAME"
	case OpMkdir:
		return "MKDIR"
	case OpRmdir:
		return "RMDIR"
	default:
		return "UNKNOWN"
	}
}
