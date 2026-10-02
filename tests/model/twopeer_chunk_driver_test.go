// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// The driver of model v5 (CHUNK-LEDGER-DESIGN.md, stage 3): the code's
// JudgeChunk takes the place of the model's own rule, and every case of the
// model must still hold. A divergence here is the code's rule disagreeing
// with the model over some interleaving.
package model

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/stretchr/testify/require"
)

func withCodeJudge(t *testing.T) {
	t.Helper()
	prev := chJudge
	chJudge = func(own chRec, ownDirty bool, in chRec, base int, peerWins bool) (bool, bool) {
		return filesystem.JudgeChunk(
			filesystem.ChunkRecord{Hash: uint64(own.id), Stamp: int64(own.stamp), Author: uint8(own.author), Present: true, Dirty: ownDirty}, // #nosec G115
			filesystem.ChunkRecord{Hash: uint64(in.id), Stamp: int64(in.stamp), Author: uint8(in.author), BaseHash: uint64(base)},            // #nosec G115
			peerWins)
	}
	t.Cleanup(func() { chJudge = prev })
}

// Every model v5 case, judged by the code.
func TestChunk_CodeJudgeMatchesModel(t *testing.T) {
	withCodeJudge(t)
	TestChunk_ConvergesAndPreservesOnlyTrueConflicts(t)
	TestChunk_DisjointEditsMergeWithoutCopies(t)
	TestChunk_SingleWriterNeverCopies(t)
	TestChunk_IdenticalRewriteChangesNothing(t)
	TestChunk_SameChunkEditsAlwaysPreserve(t)
}

// The notify worker can deliver a later message before an earlier one for
// the same path (VERIFY.md A4). Under the ledger a message carries whole
// records, so the order of delivery changes nothing: the ledgers converge,
// no version is lost, and no copy appears outside a true conflict. With the
// model's rule and with the code's.
func TestChunk_ReorderedDeliveryIsHarmless(t *testing.T) {
	check := func(t *testing.T) {
		for _, pr := range []struct{ a, b int }{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
			for _, rank := range []bool{false, true} {
				r := checkChunk(t, pr.a, pr.b, chCfg{aWinsTie: rank, sets: chBoth(chAllSets), reorder: true})
				require.Zero(t, r.divergent, "divergence: %s", r.example)
				require.Zero(t, r.lossy, "a chunk version was lost")
				require.Zero(t, r.copiesOutsideBoth, "a copy for a chunk only one side changed")
			}
		}
	}
	t.Run("model rule", check)
	t.Run("code rule", func(t *testing.T) {
		withCodeJudge(t)
		check(t)
	})
}
