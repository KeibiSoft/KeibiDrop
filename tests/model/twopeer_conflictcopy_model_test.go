// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// Models a conflict sibling across a session drop. Shipped rules: a drop
// between the preserve and its delivery strands the sibling on one peer
// forever. The fix rule (re-announce local siblings on reconnect) closes
// every such interleaving.
package model

import (
	"sort"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/stretchr/testify/require"
)

const (
	ccEdit = iota // announcement of a write: content, ver, base
	ccSib         // announcement of a minted conflict sibling: id
)

type ccMsg struct {
	kind    int
	content int // edit: content id (equals its version)
	ver     int
	base    int // version the writer held when it wrote
	sib     int // sibling id (the preserved version)
}

type ccPeer struct {
	content   int
	watermark int
	own       int
	sibs      map[int]bool // conflict siblings present on this peer
}

type ccState struct {
	p          [2]ccPeer
	q          [2][]ccMsg
	nextVer    int
	writesLeft [2]int
	dropsLeft  int
}

func cloneCC(s ccState) ccState {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]ccMsg(nil), s.q[i]...)
		n.p[i].sibs = map[int]bool{}
		for k := range s.p[i].sibs {
			n.p[i].sibs[k] = true
		}
	}
	return n
}

func sortedSibs(sibs map[int]bool) []int {
	out := make([]int, 0, len(sibs))
	for k := range sibs {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func ccKey(s ccState) string {
	b := make([]byte, 0, 64)
	enc := func(v int) { b = append(b, byte(v), byte(v>>8)) }
	for i := 0; i < 2; i++ {
		enc(s.p[i].content)
		enc(s.p[i].watermark)
		enc(s.p[i].own)
		for _, id := range sortedSibs(s.p[i].sibs) {
			enc(id)
		}
		b = append(b, 0xFF)
		enc(s.writesLeft[i])
		enc(len(s.q[i]))
		for _, m := range s.q[i] {
			enc(m.kind)
			enc(m.content)
			enc(m.ver)
			enc(m.base)
			enc(m.sib)
		}
	}
	enc(s.nextVer)
	enc(s.dropsLeft)
	return string(b)
}

// Actions: a write with its base, delivery (a stale-base winner preserves
// and announces a sibling), and a drop that clears both queues.
func ccSuccessors(s ccState, reannounce bool) []ccState {
	var out []ccState
	for i := 0; i < 2; i++ {
		if s.writesLeft[i] > 0 {
			n := cloneCC(s)
			n.nextVer++
			base := 0
			if n.p[i].content != 0 {
				base = n.p[i].content // content ids are their versions
			}
			n.p[i].content = n.nextVer
			n.p[i].own = n.nextVer
			n.writesLeft[i]--
			other := 1 - i
			n.q[other] = append(n.q[other], ccMsg{kind: ccEdit, content: n.nextVer, ver: n.nextVer, base: base})
			out = append(out, n)
		}
		if len(s.q[i]) > 0 {
			n := cloneCC(s)
			m := n.q[i][0]
			n.q[i] = append([]ccMsg(nil), n.q[i][1:]...)
			switch m.kind {
			case ccEdit:
				if m.ver > n.p[i].watermark && m.ver > n.p[i].own {
					if old := n.p[i].content; old != 0 && old != m.base {
						// Stale base: preserve the loser and announce the sibling.
						n.p[i].sibs[old] = true
						n.q[1-i] = append(n.q[1-i], ccMsg{kind: ccSib, sib: old})
					}
					n.p[i].watermark = m.ver
					n.p[i].content = m.content
				}
			case ccSib:
				n.p[i].sibs[m.sib] = true
			}
			out = append(out, n)
		}
	}
	if s.dropsLeft > 0 && (len(s.q[0]) > 0 || len(s.q[1]) > 0) {
		n := cloneCC(s)
		n.q[0] = nil
		n.q[1] = nil
		n.dropsLeft--
		if reannounce {
			// The fix: on reconnect each peer re-announces its local siblings.
			for i := 0; i < 2; i++ {
				for _, id := range sortedSibs(n.p[i].sibs) {
					n.q[1-i] = append(n.q[1-i], ccMsg{kind: ccSib, sib: id})
				}
			}
		}
		out = append(out, n)
	}
	return out
}

func ccQuiescent(s ccState) bool {
	return s.writesLeft[0] == 0 && s.writesLeft[1] == 0 &&
		len(s.q[0]) == 0 && len(s.q[1]) == 0
}

func sibsEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func checkCC(t *testing.T, writesA, writesB, drops int, reannounce, failOnAsymmetry bool) (terminals, asymmetric, withSibs int) {
	t.Helper()

	m := Model[ccState]{
		Key:        ccKey,
		Successors: func(s ccState) []ccState { return ccSuccessors(s, reannounce) },
		Quiescent:  ccQuiescent,
		CheckTerminal: func(s ccState) error {
			if len(s.p[0].sibs) > 0 || len(s.p[1].sibs) > 0 {
				withSibs++
			}
			if !sibsEqual(s.p[0].sibs, s.p[1].sibs) {
				asymmetric++
				if failOnAsymmetry {
					return fp.Equal("SIBLING LOSS: conflict copy exists on one peer only",
						len(s.p[0].sibs), len(s.p[1].sibs))
				}
			}
			return nil
		},
	}

	init := ccState{writesLeft: [2]int{writesA, writesB}, dropsLeft: drops}
	init.p[0].sibs = map[int]bool{}
	init.p[1].sibs = map[int]bool{}
	rep, err := Explore(m, init)
	require.NoError(t, err)

	t.Logf("writes=%d+%d drops=%d reannounce=%v: states=%d terminals=%d asymmetric=%d withSibs=%d",
		writesA, writesB, drops, reannounce, rep.Explored, rep.Terminals, asymmetric, withSibs)
	return rep.Terminals, asymmetric, withSibs
}

// Without a drop every minted sibling is delivered: the sets agree.
func TestConflictCopy_NoDropAlwaysSymmetric(t *testing.T) {
	for _, tc := range []struct{ a, b int }{{1, 1}, {2, 1}, {2, 2}} {
		_, asymmetric, withSibs := checkCC(t, tc.a, tc.b, 0, false, true)
		_ = withSibs
		if asymmetric != 0 {
			t.Fatalf("writes=%d+%d no drop: %d asymmetric terminals", tc.a, tc.b, asymmetric)
		}
	}
}

// One drop: interleavings exist where the sibling ends on exactly one peer.
func TestConflictCopy_DropLosesSiblingOnOnePeer(t *testing.T) {
	_, asymmetric, withSibs := checkCC(t, 1, 1, 1, false, false)
	if withSibs == 0 {
		t.Fatal("scope produced no conflict-copy histories; scope too small")
	}
	if asymmetric == 0 {
		t.Fatal("expected asymmetric sibling terminals under a session drop; found none")
	}
}

// The fix rule: reconnect re-announces local siblings; every interleaving
// ends with equal sets. Re-sends are idempotent.
func TestConflictCopy_ReannounceRestoresSymmetry(t *testing.T) {
	for _, tc := range []struct{ a, b, drops int }{{1, 1, 1}, {2, 1, 1}, {2, 2, 1}} {
		_, asymmetric, _ := checkCC(t, tc.a, tc.b, tc.drops, true, true)
		if asymmetric != 0 {
			t.Fatalf("writes=%d+%d drops=%d with reannounce: %d asymmetric terminals",
				tc.a, tc.b, tc.drops, asymmetric)
		}
	}
}
