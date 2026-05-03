// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package identity

import "errors"

var ErrIdentityNeedsPassphrase = errors.New("identity: passphrase required")
var ErrIdentityNewerSchema = errors.New("identity: newer schema version than this build")
