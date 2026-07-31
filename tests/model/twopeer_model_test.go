// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// Package model is a bounded explicit-state model checker for the two-peer edit
// protocol: LWW acceptance by announced-version watermark over an async FIFO
// notification channel (the abstraction of RemoteMtimeNs + AddRemoteFile adoption).
// It enumerates EVERY interleaving of writes and deliveries at a small scope and
// checks, at each quiescent terminal state: (1) both peers hold identical content,
// (2) that content is the globally newest write (LWW correctness). It also counts
// conflict histories — a peer overwrote content it had never observed — which is
// the deterministic justification for the conflict-copy policy: convergence holds,
// but one side's data survives nowhere.
package model

import "testing"

type peerState struct {
	content   int // id of the write this peer currently holds (0 = initial)
	watermark int // highest announced version accepted
	own       int // this peer's own newest local write (LocalNewer's version)
}

type msg struct{ content, ver int }

type state struct {
	p          [2]peerState
	q          [2][]msg // FIFO of announcements toward each peer
	nextVer    int
	writesLeft [2]int
	// seen[i] = highest version peer i had observed (own write or accepted
	// delivery) at any point; used to detect blind overwrites (conflicts).
	seen      [2]int
	conflicts int
}

func (s state) key() string {
	b := make([]byte, 0, 32)
	enc := func(v int) { b = append(b, byte(v), byte(v>>8)) }
	for i := 0; i < 2; i++ {
		enc(s.p[i].content)
		enc(s.p[i].watermark)
		enc(s.p[i].own)
		enc(s.writesLeft[i])
		enc(s.seen[i])
		enc(len(s.q[i]))
		for _, m := range s.q[i] {
			enc(m.content)
			enc(m.ver)
		}
	}
	enc(s.nextVer)
	enc(s.conflicts)
	return string(b)
}

func copyState(s state) state {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]msg(nil), s.q[i]...)
	}
	return n
}

// successors enumerates every enabled action: a local write on either peer, or
// delivery of the head announcement to either peer. coalesce models the notify
// debounce: a queued announcement for the same file is replaced by the newer one.
func successors(s state, coalesce bool) []state {
	var out []state
	for i := 0; i < 2; i++ {
		if s.writesLeft[i] > 0 {
			n := copyState(s)
			n.nextVer++
			// A blind overwrite: this peer writes without having observed the
			// current globally-newest version. Convergence survives; bytes do not.
			if s.nextVer > 0 && s.seen[i] < s.nextVer {
				n.conflicts++
			}
			n.p[i].content = n.nextVer
			n.p[i].own = n.nextVer
			n.seen[i] = n.nextVer
			n.writesLeft[i]--
			other := 1 - i
			m := msg{content: n.nextVer, ver: n.nextVer}
			if coalesce && len(n.q[other]) > 0 {
				n.q[other][len(n.q[other])-1] = m
			} else {
				n.q[other] = append(n.q[other], m)
			}
			out = append(out, n)
		}
		if len(s.q[i]) > 0 {
			n := copyState(s)
			m := n.q[i][0]
			n.q[i] = append([]msg(nil), n.q[i][1:]...)
			// Acceptance clears BOTH the announced watermark and the peer's own
			// newest local write: an in-flight older announcement must never
			// overwrite fresher local content (found by this checker, 2026-07-30).
			if m.ver > n.p[i].watermark && m.ver > n.p[i].own {
				n.p[i].watermark = m.ver
				n.p[i].content = m.content
				if m.ver > n.seen[i] {
					n.seen[i] = m.ver
				}
			}
			out = append(out, n)
		}
	}
	return out
}

func quiescent(s state) bool {
	return s.writesLeft[0] == 0 && s.writesLeft[1] == 0 &&
		len(s.q[0]) == 0 && len(s.q[1]) == 0
}

// check runs BFS over the full interleaving space and validates every terminal.
func check(t *testing.T, writesA, writesB int, coalesce bool) (terminals, conflictHistories int) {
	t.Helper()
	init := state{writesLeft: [2]int{writesA, writesB}}
	frontier := []state{init}
	visited := map[string]bool{init.key(): true}
	explored := 0
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		explored++
		succ := successors(s, coalesce)
		if len(succ) == 0 {
			if !quiescent(s) {
				t.Fatalf("deadlock: non-quiescent state with no actions: %+v", s)
			}
			terminals++
			if s.p[0].content != s.p[1].content {
				t.Fatalf("CONVERGENCE VIOLATION: peers diverged at quiescence: %+v", s)
			}
			if s.p[0].content != s.nextVer {
				t.Fatalf("LWW VIOLATION: final content %d is not the newest write %d: %+v",
					s.p[0].content, s.nextVer, s)
			}
			if s.conflicts > 0 {
				conflictHistories++
			}
			continue
		}
		for _, n := range succ {
			k := n.key()
			if !visited[k] {
				visited[k] = true
				frontier = append(frontier, n)
			}
		}
	}
	t.Logf("writes=%d+%d coalesce=%v: states=%d terminals=%d conflictHistories=%d",
		writesA, writesB, coalesce, explored, terminals, conflictHistories)
	return terminals, conflictHistories
}

// Turn-taking (one writer at a time overall) and concurrent scopes, with and
// without debounce coalescing. Convergence and LWW must hold in ALL of them.
func TestTwoPeer_ConvergenceAndLWW(t *testing.T) {
	for _, tc := range []struct{ a, b int }{{1, 0}, {2, 0}, {1, 1}, {2, 1}, {2, 2}, {3, 2}} {
		for _, co := range []bool{false, true} {
			check(t, tc.a, tc.b, co)
		}
	}
}

// A single writer never produces a conflict history: everything the reader
// accepts is causally newest. This is the formal shape of "turn-taking is safe".
func TestTwoPeer_SingleWriterHasNoConflicts(t *testing.T) {
	_, conflicts := check(t, 3, 0, false)
	if conflicts != 0 {
		t.Fatalf("single-writer scope produced %d conflict histories", conflicts)
	}
}

// With concurrent writers, conflict histories exist: convergence holds (LWW),
// but some interleavings overwrite content the writer never saw. This is the
// deterministic case for the conflict-copy policy, produced by enumeration,
// not anecdote.
func TestTwoPeer_ConcurrentWritersProduceConflicts(t *testing.T) {
	_, conflicts := check(t, 1, 1, false)
	if conflicts == 0 {
		t.Fatal("expected conflict histories under concurrent writers; found none")
	}
}
