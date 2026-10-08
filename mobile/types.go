// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package mobile

import (
	"sync"
	"time"
)

// Operation status constants. Swift and Kotlin poll these values.
const (
	OpStatusIdle      = "idle"
	OpStatusRunning   = "running"
	OpStatusSucceeded = "succeeded"
	OpStatusFailed    = "failed"
	OpStatusTimeout   = "timeout"
)

// OpStatus is the result of GetOpStatus().
// Exported fields and simple types keep it gomobile-safe.
type OpStatus struct {
	Status  string
	Message string
}

// opState tracks async operation progress. The mutex makes it safe for concurrent use.
type opState struct {
	mu        sync.Mutex
	status    string
	message   string
	startedAt time.Time
}

func newOpState() *opState {
	return &opState{status: OpStatusIdle}
}

func (o *opState) set(status, msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status = status
	o.message = msg
	if status == OpStatusRunning {
		o.startedAt = time.Now()
	}
}

func (o *opState) get() (string, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status, o.message
}
