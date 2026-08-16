// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Bounded waiting. Replaces hand-rolled deadline loops and sleeps.
// ABOUTME: Poll and Stays return error, so they compose and are goroutine safe.

package testkit

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// defaultInterval is used when a caller passes a non-positive interval.
const defaultInterval = 2 * time.Millisecond

// Poll waits for cond, or reports a timeout.
// It returns error, so it composes with fp.All and is safe off the test
// goroutine, where t.Fatal is a Go bug.
func Poll(timeout, interval time.Duration, cond func() bool, what string) error {
	if cond == nil {
		return fmt.Errorf("testkit: Poll got a nil condition for %s", what)
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(interval)
	}
	// One more check. The last sleep can cross the deadline.
	if cond() {
		return nil
	}
	return fmt.Errorf("timed out after %v waiting for %s", timeout, what)
}

// Eventually fails the test if cond does not become true inside timeout.
func Eventually(t testing.TB, timeout, interval time.Duration, cond func() bool, what string) {
	t.Helper()
	require.NoError(t, Poll(timeout, interval, cond, what))
}

// EventuallyVal polls until probe reports ready, then returns the value.
func EventuallyVal[T any](t testing.TB, timeout, interval time.Duration, probe func() (T, bool), what string) T {
	t.Helper()
	var last T
	require.NotNil(t, probe, "testkit: EventuallyVal got a nil probe")
	Eventually(t, timeout, interval, func() bool {
		v, ok := probe()
		if ok {
			last = v
		}
		return ok
	}, what)
	return last
}

// Stays reports an error if cond becomes true inside the window.
// Use it to prove something does not happen.
func Stays(window, interval time.Duration, cond func() bool, what string) error {
	if cond == nil {
		return fmt.Errorf("testkit: Stays got a nil condition for %s", what)
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if cond() {
			return fmt.Errorf("%s became true inside %v", what, window)
		}
		time.Sleep(interval)
	}
	return nil
}

// Never fails the test if cond becomes true inside the window.
func Never(t testing.TB, window, interval time.Duration, cond func() bool, what string) {
	t.Helper()
	require.NoError(t, Stays(window, interval, cond, what))
}
