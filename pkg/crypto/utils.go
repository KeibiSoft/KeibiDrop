// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/rand"
	"fmt"
)

func RandomBytes(size int) ([]byte, error) {
	b := make([]byte, size)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func GenerateSeed() []byte {
	res, _ := RandomBytes(seedSize)
	return res
}

// TODO: Remove it?
func safeInt(u uint64) (int, error) {
	if u > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("value too large to fit into int: %d", u)
	}
	return int(u), nil
}
