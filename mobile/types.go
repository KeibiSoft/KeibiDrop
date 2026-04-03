// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"sync"
	"time"
)

// Operation status constants (polled from Swift/Kotlin).
const (
	OpStatusIdle      = "idle"
	OpStatusRunning   = "running"
	OpStatusSucceeded = "succeeded"
	OpStatusFailed    = "failed"
	OpStatusTimeout   = "timeout"
)

// OpStatus is the result of GetOpStatus(). gomobile-safe (exported fields, simple types).
type OpStatus struct {
	Status  string
	Message string
}

// opState tracks async operation progress (thread-safe).
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
