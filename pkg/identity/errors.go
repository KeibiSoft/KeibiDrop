// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package identity

import "errors"

var ErrIdentityNeedsPassphrase = errors.New("identity: passphrase required")
var ErrIdentityNewerSchema = errors.New("identity: newer schema version than this build")
