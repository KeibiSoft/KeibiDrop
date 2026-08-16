// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Goroutine start and join, replacing the chan error pattern.
// ABOUTME: The join function returns error, so it drops into fp.All unwrapped.

package testkit

import (
	"context"
	"fmt"
	"time"
)

// Within runs body and reports an error if body does not return inside d.
// It replaces the hand-rolled shape that starts a goroutine, reports through a
// bool channel and races a time.After.
//
// A panic in body becomes an error, so Must is safe inside body. A bare
// goroutine cannot do this: only the goroutine that panics can recover it, so a
// Must in a plain go func ends the test binary instead of failing the test.
//
// After a timeout body keeps running. Do not call t.Error or t.Fatal in body. A
// late call panics after the test ends. Return an error instead.
func Within(d time.Duration, what string, body func() error) error {
	if body == nil {
		return fmt.Errorf("testkit: Within got a nil body")
	}
	done := make(chan error, 1)
	go func() { done <- recovering(body) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return fmt.Errorf("%s: did not finish in %v", what, d)
	}
}

// WithinCtx runs body and reports an error if body does not return before ctx
// ends. Use it where the deadline is held by a context and is shared by more
// than one wait. Within starts a new duration at each call, so it gives each
// wait its own budget, which is a different test.
//
// A panic in body becomes an error, the same as Within. The same rule applies
// after a timeout: body keeps running, so return an error from it, never t.Error.
func WithinCtx(ctx context.Context, what string, body func() error) error {
	if ctx == nil {
		return fmt.Errorf("testkit: WithinCtx got a nil context for %s", what)
	}
	if body == nil {
		return fmt.Errorf("testkit: WithinCtx got a nil body for %s", what)
	}
	done := make(chan error, 1)
	go func() { done <- recovering(body) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", what, ctx.Err())
	}
}

// Go starts fn and returns a join function.
// Call the join function to wait for fn and get its error. The join function
// has the shape func() error, so it composes into fp.All with no wrapper.
func Go(fn func() error) func() error {
	if fn == nil {
		return func() error { return fmt.Errorf("testkit: Go got a nil function") }
	}
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return func() error { return <-ch }
}

// GoN runs fn for i in [0,n) and returns a join function.
// The join function drains every result before it returns, so no goroutine
// leaks when one of them fails early. It reports the first error by index.
func GoN(n int, fn func(i int) error) func() error {
	if fn == nil {
		return func() error { return fmt.Errorf("testkit: GoN got a nil function") }
	}
	if n <= 0 {
		return func() error { return nil }
	}
	type res struct {
		i   int
		err error
	}
	ch := make(chan res, n)
	for i := 0; i < n; i++ {
		go func(i int) { ch <- res{i: i, err: fn(i)} }(i)
	}
	return func() error {
		first := -1
		var firstErr error
		for k := 0; k < n; k++ {
			r := <-ch
			if r.err != nil && (first == -1 || r.i < first) {
				first, firstErr = r.i, r.err
			}
		}
		if firstErr != nil {
			return fmt.Errorf("worker %d: %w", first, firstErr)
		}
		return nil
	}
}
