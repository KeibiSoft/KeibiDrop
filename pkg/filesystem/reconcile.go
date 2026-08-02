// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: pure partial-invalidation decision function for rsync-style re-fetch on edit.
// ABOUTME: no locks beyond ChunkBitmap methods, no I/O, no goroutines, no RPC.

package filesystem

// reconcileBitmap builds the post-edit bitmap. It keeps only chunks proven
// unchanged (stored hash == peer hash for the same chunk span) and leaves the
// rest unset for on-demand re-fetch. It returns (newBitmap, true) on success,
// or (nil, false) when the caller must fall back to a full reset.
func reconcileBitmap(old *ChunkBitmap, oldSize, newSize int64, peerHashes map[int]uint64) (*ChunkBitmap, bool) {
	if old == nil || newSize <= 0 || !old.HasHashes() || len(peerHashes) == 0 {
		return nil, false
	}

	cs := int64(old.ChunkSizeBytes())
	nb := NewChunkBitmapWithSize(newSize, int(cs))

	commonEnd := min(oldSize, newSize)

	// Iterate only over chunks fully inside the common prefix.
	for c := 0; (int64(c)+1)*cs <= commonEnd; c++ {
		h, hasHash := old.Hash(c)
		if !hasHash {
			// Chunk absent in old or no fingerprint: skip.
			continue
		}
		if peerHashes[c] == h {
			nb.Set(c)
			nb.SetHash(c, h)
		}
		// Differing hash or peer omitted it: leave unset.
	}

	return nb, true
}
