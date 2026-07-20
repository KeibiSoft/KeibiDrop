// ABOUTME: White-box tests for the epoch-partitioned nonce generator: layout, epoch
// ABOUTME: bumps, counter reset, epoch-0 wire compatibility, and the 48-bit guard.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"encoding/binary"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// At epoch 0 the nonce must stay byte-identical to the pre-epoch
// [4B prefix][8B BE counter] layout, so a ratchet-unaware peer interoperates.
func TestNonceGenerator_Epoch0WireCompat(t *testing.T) {
	ng := NewNonceGenerator(0x01020304)
	for want := uint64(1); want <= 4; want++ {
		n, err := ng.Next()
		require.NoError(t, err)
		require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, n[:4], "prefix stays in bytes 0:4")
		require.Equal(t, want, binary.BigEndian.Uint64(n[4:]),
			"at epoch 0 the low 8 bytes decode as the plain counter")
	}
}

func TestNonceGenerator_EpochBytesAndReset(t *testing.T) {
	ng := NewNonceGenerator(0)
	_, err := ng.Next() // counter 1
	require.NoError(t, err)
	_, err = ng.Next() // counter 2
	require.NoError(t, err)
	require.Equal(t, uint64(2), ng.Count())

	ng.SetEpoch(1)
	require.Equal(t, uint64(0), ng.Count(), "SetEpoch resets the counter")

	n, err := ng.Next()
	require.NoError(t, err)
	require.Equal(t, uint16(1), binary.BigEndian.Uint16(n[4:6]), "epoch is the high 2 nonce bytes")
	require.Equal(t, uint64(1), binary.BigEndian.Uint64(n[4:])&nonceCounterMask,
		"counter resumes at 1 under the new epoch")
	require.Equal(t, uint64(1), ng.Count())

	ng.SetEpoch(0xABCD)
	n, err = ng.Next()
	require.NoError(t, err)
	require.Equal(t, []byte{0xAB, 0xCD}, n[4:6], "an arbitrary epoch lands in bytes 4:6")
}

// The 48-bit counter must never wrap within an epoch: a wrap would carry into the
// epoch bytes and reuse a (key, nonce) pair, so Next must fail closed with an error
// (which the writer turns into a dropped connection) rather than crash the daemon.
func TestNonceGenerator_CounterOverflowFailsClosed(t *testing.T) {
	ng := NewNonceGenerator(0)
	ng.state.Store((uint64(7) << nonceCounterBits) | (nonceCounterMask - 1))

	n, err := ng.Next() // counter -> 2^48-1, still the last valid value
	require.NoError(t, err)
	require.Equal(t, uint16(7), binary.BigEndian.Uint16(n[4:6]))
	require.Equal(t, nonceCounterMask, binary.BigEndian.Uint64(n[4:])&nonceCounterMask)

	_, err = ng.Next() // this one would wrap the counter into the epoch bytes
	require.ErrorIs(t, err, ErrNonceOverflow, "a counter wrap into the epoch bytes must fail closed")
}

// The packed epoch:counter word must give every concurrent Next a distinct nonce.
func TestNonceGenerator_ConcurrentNextUnique(t *testing.T) {
	ng := NewNonceGenerator(0x11223344)
	const goroutines, per = 16, 500

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[[NonceSize]byte]struct{}, goroutines*per)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				n, err := ng.Next()
				require.NoError(t, err)
				mu.Lock()
				seen[n] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, seen, goroutines*per, "no nonce repeats under concurrent Next()")
}
