// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Chain sequences fallible stages and carries one error.
// ABOUTME: Effects run during composition. Chain does not defer them.

package fp

import "fmt"

// Chain holds a value and an error.
// LiftResult runs its function immediately. Chain gives short-circuit
// sequencing and one observation point. It does not defer effects.
type Chain[T any] struct {
	val T
	err error
}

// Wrap starts a chain from a plain value.
func Wrap[T any](v T) Chain[T] { return Chain[T]{val: v} }

// WithError returns a copy carrying err. The value is kept for debugging.
func (c Chain[T]) WithError(err error) Chain[T] {
	c.err = err
	return c
}

// LiftResult runs fn and captures its value and error.
func LiftResult[T any](fn func() (T, error)) Chain[T] {
	if fn == nil {
		var zero T
		return Chain[T]{val: zero, err: fmt.Errorf("fp: LiftResult got a nil function")}
	}
	v, err := fn()
	return Chain[T]{val: v, err: err}
}

// Bind sequences a stage that changes the type.
// A nil stage gives an error, not a silent zero value.
func Bind[T, U any](c Chain[T], f func(T) Chain[U]) Chain[U] {
	if c.err != nil {
		return Chain[U]{err: c.err}
	}
	if f == nil {
		return Chain[U]{err: fmt.Errorf("fp: Bind got a nil stage")}
	}
	return f(c.val)
}

// Then sequences a stage that keeps the type.
// Accepts method expressions, for example Wrap(r).Then(run[S].arrange).
func (c Chain[T]) Then(f func(T) (T, error)) Chain[T] {
	if c.err != nil {
		return c
	}
	if f == nil {
		return Chain[T]{val: c.val, err: fmt.Errorf("fp: Then got a nil stage")}
	}
	v, err := f(c.val)
	return Chain[T]{val: v, err: err}
}

// Result unpacks the chain into the usual Go pair.
func (c Chain[T]) Result() (T, error) { return c.val, c.err }

// IsOK reports whether the chain holds no error.
func (c Chain[T]) IsOK() bool { return c.err == nil }

// Err returns the carried error, or nil.
func (c Chain[T]) Err() error { return c.err }

// Match calls ok with the value, or bad with the error. Nil funcs are ignored.
func (c Chain[T]) Match(ok func(T), bad func(error)) {
	if c.err != nil {
		if bad != nil {
			bad(c.err)
		}
		return
	}
	if ok != nil {
		ok(c.val)
	}
}
