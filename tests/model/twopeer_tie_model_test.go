// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// Models the exact version tie (observed once in CI). Both shipped acceptance
// shapes diverge on a tie: ADD rejects on both sides, EDIT swaps contents.
// The checked fix rule breaks the tie by peer rank: one winner on both sides,
// the loser preserved.
package model

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/stretchr/testify/require"
)

type tieMode int

const (
	// tieModeAddStrict: shipped ADD predicate, a tie rejects on both sides.
	tieModeAddStrict tieMode = iota
	// tieModeEditGE: shipped EDIT predicate, a tie passes on both sides.
	tieModeEditGE
	// tieModeRanked: the fix rule, peer 1 wins an exact tie on both sides.
	tieModeRanked
)

type tiePeer struct {
	content   int // id of the write this peer holds (0 = initial)
	heldVer   int // version stamp of that content
	watermark int // highest accepted announced version
	own       int // version stamp of this peer's own newest local write
}

type tieMsg struct{ content, ver int }

type tieState struct {
	p          [2]tiePeer
	q          [2][]tieMsg
	nextVer    int
	nextCid    int
	writesLeft [2]int // unique-stamp writes
	tiesLeft   int    // paired equal-stamp writes
	tieCid     [2]int // content ids of the tie pair, 0 until the tie fires
	preserved  int    // content id preserved on tie acceptance, ranked mode
}

func tieKey(s tieState) string {
	b := make([]byte, 0, 48)
	enc := func(v int) { b = append(b, byte(v), byte(v>>8)) }
	for i := 0; i < 2; i++ {
		enc(s.p[i].content)
		enc(s.p[i].heldVer)
		enc(s.p[i].watermark)
		enc(s.p[i].own)
		enc(s.writesLeft[i])
		enc(s.tieCid[i])
		enc(len(s.q[i]))
		for _, m := range s.q[i] {
			enc(m.content)
			enc(m.ver)
		}
	}
	enc(s.nextVer)
	enc(s.nextCid)
	enc(s.tiesLeft)
	enc(s.preserved)
	return string(b)
}

func cloneTie(s tieState) tieState {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]tieMsg(nil), s.q[i]...)
	}
	return n
}

// tieAccept is the acceptance predicate under the chosen mode.
func tieAccept(mode tieMode, i int, p tiePeer, m tieMsg) bool {
	switch mode {
	case tieModeAddStrict:
		return m.ver > p.watermark && m.ver > p.own
	case tieModeEditGE:
		return m.ver >= p.watermark && m.ver >= p.own
	case tieModeRanked:
		if m.ver > p.watermark && m.ver > p.own {
			return true
		}
		// Exact tie with our own write: the higher-ranked peer wins.
		// Peer 1 outranks peer 0, so only peer 0 accepts the tie.
		return m.ver == p.own && m.ver >= p.watermark && i == 0
	}
	return false
}

// Actions: a unique-stamp write, the paired tie write (one shared stamp,
// atomic pairing), or delivery of the head announcement.
func tieSuccessors(s tieState, mode tieMode) []tieState {
	var out []tieState
	for i := 0; i < 2; i++ {
		if s.writesLeft[i] > 0 {
			n := cloneTie(s)
			n.nextVer++
			n.nextCid++
			n.p[i].content = n.nextCid
			n.p[i].heldVer = n.nextVer
			n.p[i].own = n.nextVer
			n.writesLeft[i]--
			other := 1 - i
			n.q[other] = append(n.q[other], tieMsg{content: n.nextCid, ver: n.nextVer})
			out = append(out, n)
		}
		if len(s.q[i]) > 0 {
			n := cloneTie(s)
			m := n.q[i][0]
			n.q[i] = append([]tieMsg(nil), n.q[i][1:]...)
			if tieAccept(mode, i, n.p[i], m) {
				if mode == tieModeRanked && m.ver == n.p[i].own && n.p[i].content != m.content {
					// The loser adopts the winner and preserves its own write.
					n.preserved = n.p[i].content
				}
				n.p[i].watermark = m.ver
				n.p[i].content = m.content
				n.p[i].heldVer = m.ver
			}
			out = append(out, n)
		}
	}
	if s.tiesLeft > 0 {
		n := cloneTie(s)
		n.nextVer++
		ver := n.nextVer
		for i := 0; i < 2; i++ {
			n.nextCid++
			n.p[i].content = n.nextCid
			n.p[i].heldVer = ver
			n.p[i].own = ver
			n.tieCid[i] = n.nextCid
			n.q[1-i] = append(n.q[1-i], tieMsg{content: n.nextCid, ver: ver})
		}
		n.tiesLeft--
		out = append(out, n)
	}
	return out
}

func tieQuiescent(s tieState) bool {
	return s.writesLeft[0] == 0 && s.writesLeft[1] == 0 && s.tiesLeft == 0 &&
		len(s.q[0]) == 0 && len(s.q[1]) == 0
}

// checkTie explores one scope. Counting mode demonstrates, fail mode gates.
func checkTie(t *testing.T, mode tieMode, writesA, writesB, ties int, failOnDiverge bool) (terminals, divergent, preservedTerminals int) {
	t.Helper()

	m := Model[tieState]{
		Key:        tieKey,
		Successors: func(s tieState) []tieState { return tieSuccessors(s, mode) },
		Quiescent:  tieQuiescent,
		CheckTerminal: func(s tieState) error {
			if s.p[0].content != s.p[1].content {
				divergent++
				if failOnDiverge {
					return fp.Equal("CONVERGENCE VIOLATION: peers diverged at quiescence",
						s.p[0].content, s.p[1].content)
				}
			}
			if s.preserved != 0 {
				preservedTerminals++
			}
			return nil
		},
	}

	rep, err := Explore(m, tieState{writesLeft: [2]int{writesA, writesB}, tiesLeft: ties})
	require.NoError(t, err)

	t.Logf("mode=%d writes=%d+%d ties=%d: states=%d terminals=%d divergent=%d preservedTerminals=%d",
		mode, writesA, writesB, ties, rep.Explored, rep.Terminals, divergent, preservedTerminals)
	return rep.Terminals, divergent, preservedTerminals
}

// The shipped ADD predicate diverges permanently on a tie: both reject.
func TestTie_AddStrictPredicateDiverges(t *testing.T) {
	for _, tc := range []struct{ a, b int }{{0, 0}, {1, 0}, {1, 1}} {
		_, divergent, _ := checkTie(t, tieModeAddStrict, tc.a, tc.b, 1, false)
		if divergent == 0 {
			t.Fatalf("writes=%d+%d ties=1: expected divergent terminals under the strict ADD predicate; found none", tc.a, tc.b)
		}
	}
}

// The shipped EDIT predicate diverges worse: both accept, contents swap.
func TestTie_EditPredicateSwapsContents(t *testing.T) {
	_, divergent, _ := checkTie(t, tieModeEditGE, 0, 0, 1, false)
	if divergent == 0 {
		t.Fatal("ties=1: expected divergent (swapped) terminals under the EDIT predicate; found none")
	}
}

// Without a tie every interleaving converges: the tie alone diverges.
func TestTie_NoTieNoDivergence(t *testing.T) {
	for _, mode := range []tieMode{tieModeAddStrict, tieModeRanked} {
		_, divergent, _ := checkTie(t, mode, 2, 1, 0, false)
		if divergent != 0 {
			t.Fatalf("mode=%d without ties: %d divergent terminals", mode, divergent)
		}
	}
}

// The fix rule: one winner on both sides, every interleaving converges,
// the losing write is preserved.
func TestTie_RankedTieBreakConverges(t *testing.T) {
	for _, tc := range []struct{ a, b int }{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		checkTie(t, tieModeRanked, tc.a, tc.b, 1, true)
	}

	terminals, _, preservedTerminals := checkTie(t, tieModeRanked, 0, 0, 1, true)
	if preservedTerminals != terminals {
		t.Fatalf("pure tie scope: loser preserved in %d of %d terminals; want all", preservedTerminals, terminals)
	}
}

// The winner is the higher-ranked peer's write, on both peers.
func TestTie_RankedWinnerIsHigherRank(t *testing.T) {
	m := Model[tieState]{
		Key:        tieKey,
		Successors: func(s tieState) []tieState { return tieSuccessors(s, tieModeRanked) },
		Quiescent:  tieQuiescent,
		CheckTerminal: func(s tieState) error {
			return fp.All(
				fp.Equal("winner on peer 0", s.p[0].content, s.tieCid[1]),
				fp.Equal("winner on peer 1", s.p[1].content, s.tieCid[1]),
			)
		},
	}
	rep, err := Explore(m, tieState{tiesLeft: 1})
	require.NoError(t, err)
	t.Logf("pure tie scope: states=%d terminals=%d, winner is peer 1's write in all", rep.Explored, rep.Terminals)
}
