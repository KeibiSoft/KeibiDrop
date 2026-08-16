// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: The lift layer. Turns an error into a test failure.
// ABOUTME: This package is the only one that imports testing outside a test file.

package testkit

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"runtime"
	"runtime/debug"
	"testing"
)

// Must returns v, or aborts the enclosing Run with err.
//
//	ln := testkit.Must(quic.ListenAddr("127.0.0.1:0", cfg, nil))
//
// Must takes no *testing.T on purpose. The Go spec allows a multi-value call to
// fill an argument list only when that call is the sole argument, so
// Must(t, f()) is not valid Go. Dropping t makes Must(f()) legal and lets T
// infer, including the two-value form.
//
// Call Must only inside a Run body. Run turns the abort into an ordinary test
// failure. Outside Run the panic escapes and ends the test binary.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(at(err, 2))
	}
	return v
}

// Must2 is Must for a call that returns two values and an error.
func Must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(at(err, 2))
	}
	return a, b
}

// at prefixes err with the caller location.
// Must cannot call t.Helper, so the location is carried in the message.
func at(err error, skip int) error {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return err
	}
	return fmt.Errorf("%s:%d: %w", trimPath(file), line, err)
}

// trimPath keeps the last two path elements, which is enough to locate a file.
func trimPath(p string) string {
	slash := -1
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if slash >= 0 {
				return p[i+1:]
			}
			slash = i
		}
	}
	return p
}

// Run joins the error-returning code to testify.
// This is the only point that observes a test failure. Put the whole test body
// inside it, so Must is safe everywhere in the test.
func Run(t *testing.T, body func() error) {
	t.Helper()
	require.NotNil(t, body, "testkit.Run got a nil body")
	require.NoError(t, recovering(body))
}

// recovering runs body and turns a panic into an error.
//
// There is no type assertion and no type switch here. recover returns any by
// language design, but the value is only formatted, never inspected by type.
// A panic from the code under test therefore becomes a readable test failure
// with its stack, and the rest of the package still runs.
func recovering(body func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n\n%s", r, debug.Stack())
		}
	}()
	return body()
}
