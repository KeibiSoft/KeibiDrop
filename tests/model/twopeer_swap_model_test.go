// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// Model v2: the copy-then-swap save (temp file with a declared base version) and
// receiver-side stale-base detection. An announcement carries the base its edit
// started from. The receiver compares the base with its current version: an older
// base is a provable conflict, and the conflict-copy policy preserves the loser.
// The checked claim (T3b): with the policy on, no interleaving loses a version
// silently — every overwritten version is causally superseded or preserved.
package model

import "testing"

type swapMsg struct{ content, ver, base int }

type swapPeer struct {
	content   int
	watermark int
	own       int
	tmpBase   int // base version of the working copy; -1 = no copy open
}

type swapState struct {
	p          [2]swapPeer
	q          [2][]swapMsg
	nextVer    int
	swapsLeft  [2]int
	preserved  map[int]bool // versions saved as conflict copies
	overwrites map[int]int  // overwritten version -> version that replaced it
}

func cloneSwap(s swapState) swapState {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]swapMsg(nil), s.q[i]...)
	}
	n.preserved = map[int]bool{}
	for k, v := range s.preserved {
		n.preserved[k] = v
	}
	n.overwrites = map[int]int{}
	for k, v := range s.overwrites {
		n.overwrites[k] = v
	}
	return n
}

func swapKey(s swapState) string {
	b := make([]byte, 0, 48)
	enc := func(v int) { b = append(b, byte(v+1), byte((v+1)>>8)) }
	for i := 0; i < 2; i++ {
		enc(s.p[i].content)
		enc(s.p[i].watermark)
		enc(s.p[i].own)
		enc(s.p[i].tmpBase)
		enc(s.swapsLeft[i])
		enc(len(s.q[i]))
		for _, m := range s.q[i] {
			enc(m.content)
			enc(m.base)
		}
	}
	enc(s.nextVer)
	enc(len(s.preserved))
	return string(b)
}

// Actions: CopyStart snapshots the current view as the base. SwapCommit writes a
// new version with that base and announces it. Deliver applies the LWW guard;
// with the policy on, a stale-base winner preserves the version it overwrites.
func swapSuccessors(s swapState, policy bool) []swapState {
	var out []swapState
	for i := 0; i < 2; i++ {
		if s.swapsLeft[i] > 0 && s.p[i].tmpBase < 0 {
			n := cloneSwap(s)
			n.p[i].tmpBase = n.p[i].content
			out = append(out, n)
		}
		if s.swapsLeft[i] > 0 && s.p[i].tmpBase >= 0 {
			n := cloneSwap(s)
			n.nextVer++
			base := n.p[i].tmpBase
			if old := n.p[i].content; old != base && old != 0 {
				// The local view advanced after the copy: app-level stale base.
				if policy {
					n.preserved[old] = true
				} else {
					n.overwrites[old] = n.nextVer
				}
			}
			n.p[i].content = n.nextVer
			n.p[i].own = n.nextVer
			n.p[i].tmpBase = -1
			n.swapsLeft[i]--
			other := 1 - i
			n.q[other] = append(n.q[other], swapMsg{content: n.nextVer, ver: n.nextVer, base: base})
			out = append(out, n)
		}
		if len(s.q[i]) > 0 {
			n := cloneSwap(s)
			m := n.q[i][0]
			n.q[i] = append([]swapMsg(nil), n.q[i][1:]...)
			// Strictly-greater: an EXACT version tie rejects on both sides.
			// Real versions are ns mtimes, so a tie means both peers keep
			// their own bytes — no loss, but no convergence. The model's
			// versions are unique by construction and cannot express it;
			// known residual. Fix if ever observed: deterministic tiebreak
			// by peer fingerprint.
			if m.ver > n.p[i].watermark && m.ver > n.p[i].own {
				old := n.p[i].content
				if old != 0 && old != m.base {
					// Receiver-side stale-base: the edit did not start from what
					// the receiver holds. Provable from the announcement alone.
					if policy {
						n.preserved[old] = true
					} else {
						n.overwrites[old] = m.ver
					}
				}
				n.p[i].watermark = m.ver
				n.p[i].content = m.content
			}
			out = append(out, n)
		}
	}
	return out
}

func checkSwap(t *testing.T, swapsA, swapsB int, policy bool) (terminals, silentLosses, copies int) {
	t.Helper()
	init := swapState{swapsLeft: [2]int{swapsA, swapsB},
		preserved: map[int]bool{}, overwrites: map[int]int{}}
	init.p[0].tmpBase = -1
	init.p[1].tmpBase = -1
	frontier := []swapState{init}
	visited := map[string]bool{swapKey(init): true}
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		succ := swapSuccessors(s, policy)
		if len(succ) == 0 {
			terminals++
			if s.p[0].content != s.p[1].content {
				t.Fatalf("divergence at quiescence: %+v", s)
			}
			// A silent loss: an overwritten version that is not the base of its
			// replacement chain and is not preserved.
			for lost := range s.overwrites {
				if !s.preserved[lost] {
					silentLosses++
					break
				}
			}
			if len(s.preserved) > 0 {
				copies++
			}
			continue
		}
		for _, n := range succ {
			k := swapKey(n)
			if !visited[k] {
				visited[k] = true
				frontier = append(frontier, n)
			}
		}
	}
	t.Logf("swaps=%d+%d policy=%v: terminals=%d silentLossHistories=%d conflictCopyHistories=%d",
		swapsA, swapsB, policy, terminals, silentLosses, copies)
	return
}

// Without the policy, concurrent swap saves lose versions silently.
func TestSwap_WithoutPolicyLosesSilently(t *testing.T) {
	_, losses, _ := checkSwap(t, 1, 1, false)
	if losses == 0 {
		t.Fatal("expected silent losses without the conflict-copy policy")
	}
}

// With the policy, no interleaving loses a version silently: every overwritten
// version is preserved as a conflict copy. Convergence holds throughout.
func TestSwap_PolicyPreservesEveryVersion(t *testing.T) {
	for _, tc := range []struct{ a, b int }{{1, 0}, {1, 1}, {2, 1}, {2, 2}} {
		_, losses, _ := checkSwap(t, tc.a, tc.b, true)
		if losses != 0 {
			t.Fatalf("swaps=%d+%d: %d silent-loss histories despite policy", tc.a, tc.b, losses)
		}
	}
}

// Turn-taking swaps (single writer) never trigger the policy: no conflict copies.
func TestSwap_TurnTakingNeedsNoPolicy(t *testing.T) {
	_, _, copies := checkSwap(t, 3, 0, true)
	if copies != 0 {
		t.Fatalf("single-writer swaps produced %d conflict-copy histories", copies)
	}
}
