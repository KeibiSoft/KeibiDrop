// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Sentinel errors and structured error types for the identity package.
// ABOUTME: Downstream callers use errors.Is / errors.As to distinguish failure modes.

package identity

import (
	"errors"
	"fmt"
)

// IdentityCorruptedError is returned when the on-disk identity file cannot be
// parsed or decrypted. Path holds the location of the offending file.
type IdentityCorruptedError struct {
	Path     string // path of the corrupted file
	Original error
}

func (e *IdentityCorruptedError) Error() string {
	return fmt.Sprintf(
		"Your saved identity at %s could not be read. "+
			"It may be corrupted, or its master key is no longer accessible. "+
			"To start over, delete %s and the contacts file alongside it, then re-launch — "+
			"this will also wipe your saved contacts. "+
			"Original error: %v",
		e.Path, e.Path, e.Original)
}

func (e *IdentityCorruptedError) Unwrap() error { return e.Original }

// ErrIdentityNeedsPassphrase is returned when the envelope was written with a
// passphrase KDF but no passphrase was supplied at load time.
var ErrIdentityNeedsPassphrase = errors.New("identity: passphrase required")

// ErrIdentityNewerSchema is returned when the envelope's format byte is higher
// than what this build understands.
var ErrIdentityNewerSchema = errors.New(
	"identity: file written by a newer KeibiDrop version (schema version too high)",
)
