// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Leaf checks. Each returns an error and never observes a test.
// ABOUTME: Compose them with All or Steps.

package fp

import (
	"bytes"
	"errors"
	"fmt"
)

func Equal[T comparable](what string, got, want T) error {
	if got != want {
		return fmt.Errorf("%s: got %v, want %v", what, got, want)
	}
	return nil
}

func NotEqual[T comparable](what string, got, unwanted T) error {
	if got == unwanted {
		return fmt.Errorf("%s: got %v, want anything else", what, got)
	}
	return nil
}

// BytesEqual reports a length summary, not the payload.
// Test payloads are large and dumping them hides the failure.
func BytesEqual(what string, got, want []byte) error {
	if bytes.Equal(got, want) {
		return nil
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s: got %d bytes, want %d bytes", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("%s: %d bytes, first difference at offset %d: got 0x%02x, want 0x%02x",
				what, len(got), i, got[i], want[i])
		}
	}
	return nil
}

func NoErr(what string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// WantErr fails when err is nil. Use for the error path of a test.
func WantErr(what string, err error) error {
	if err == nil {
		return fmt.Errorf("%s: got no error, want one", what)
	}
	return nil
}

func True(what string, cond bool) error {
	if !cond {
		return fmt.Errorf("%s: expected true", what)
	}
	return nil
}

func False(what string, cond bool) error {
	if cond {
		return fmt.Errorf("%s: expected false", what)
	}
	return nil
}

func ErrIs(what string, got, want error) error {
	if !errors.Is(got, want) {
		return fmt.Errorf("%s: got %v, want %v", what, got, want)
	}
	return nil
}

func Len[T any](what string, got []T, want int) error {
	if len(got) != want {
		return fmt.Errorf("%s: got len %d, want %d", what, len(got), want)
	}
	return nil
}
