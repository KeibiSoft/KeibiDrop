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
	"errors"
	"fmt"
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
