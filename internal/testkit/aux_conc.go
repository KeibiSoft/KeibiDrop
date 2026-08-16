// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Goroutine start and join, replacing the chan error pattern.
// ABOUTME: The join function returns error, so it drops into fp.All unwrapped.

package testkit

import "fmt"

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
