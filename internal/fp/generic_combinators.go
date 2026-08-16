// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Composition helpers for checks and collections.
// ABOUTME: Pure. This package must never import testing.

package fp

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// All reports every failed check, one per line. Use this by default.
// Arguments are evaluated before the call. Use Steps if a later check reads
// data that an earlier check proved valid.
func All(checks ...error) error {
	return errors.Join(checks...)
}

// Steps runs checks in order and stops at the first failure.
// Checks are deferred, so a failed step skips the steps after it.
func Steps(checks ...func() error) error {
	for _, f := range checks {
		if f == nil {
			continue
		}
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// Each runs check on each item. A failure gets the item index.
func Each[T any](label string, items []T, check func(T) error) error {
	if check == nil {
		return fmt.Errorf("fp: Each got a nil check for %q", label)
	}
	for i, it := range items {
		if err := check(it); err != nil {
			return fmt.Errorf("%s[%d]: %w", label, i, err)
		}
	}
	return nil
}

// EachKV runs check on each map entry, in sorted key order.
// Go randomizes map iteration. Sorting keeps failure messages stable.
func EachKV[K cmp.Ordered, V any](label string, m map[K]V, check func(K, V) error) error {
	if check == nil {
		return fmt.Errorf("fp: EachKV got a nil check for %q", label)
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if err := check(k, m[k]); err != nil {
			return fmt.Errorf("%s[%v]: %w", label, k, err)
		}
	}
	return nil
}

// Map applies f to each item and returns the results.
func Map[T, U any](in []T, f func(T) U) []U {
	if f == nil {
		return nil
	}
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

// Filter returns the items for which keep is true.
func Filter[T any](in []T, keep func(T) bool) []T {
	if keep == nil {
		return nil
	}
	var out []T
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// Fold reduces in to a single value, starting from zero.
func Fold[T, A any](in []T, zero A, step func(A, T) A) A {
	if step == nil {
		return zero
	}
	acc := zero
	for _, v := range in {
		acc = step(acc, v)
	}
	return acc
}
