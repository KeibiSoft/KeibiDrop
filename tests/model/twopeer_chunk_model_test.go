// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// Model v5: the per-chunk version ledger (swap-save-stamps-2026-10-01, Part
// 3). A file is two chunks. Each chunk carries one record: a content id (the
// hash stands in for the bytes), a stamp and an author. A save changes the
// chunks whose content differs and keeps the record of every chunk whose
// content is the same, whatever the app rewrote: the swap-save false
// positive never mints a version. A changed chunk takes a stamp above every
// stamp its writer has seen (the clock rule, per chunk) and names as its
// base the record the writer held. The receiver judges chunk by chunk: a
// newer stamp wins (the author rank on an exact tie); when the receiver's
// own record is a local change the incoming base never saw, that record is
// preserved first. Chunks a save did not change are left alone.
//
// Checked over every interleaving of saves, deliveries and fetches with up
// to two saves per peer: the ledgers converge; no chunk version is lost; a
// conflict copy appears only for a chunk both sides changed from the same
// base; disjoint edits merge with no copy; a single writer never produces a
// copy; a rewrite with identical content changes nothing.
package model

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

const chunks = 2

type chRec struct {
	id     int // content id: the chunk hash
	stamp  int
	author int
}

type chMsg struct {
	changed [chunks]bool
	recs    [chunks]chRec // the sender's records for the changed chunks
	base    [chunks]int   // the content id the sender held before each change
}

type chPeer struct {
	led       [chunks]chRec
	held      [chunks]bool // the bytes of led[c] are on this peer
	dirty     [chunks]bool // led[c] is a local change the peer has not judged yet
	savesLeft int
}

type chState struct {
	p         [2]chPeer
	q         [2][]chMsg
	nextID    int
	preserved map[int]bool // content ids kept as conflict copies
	victims   map[int]int  // content id replaced -> by content id
	derived   map[int]int  // content id -> the content id it was changed from
	both      map[int]bool // content ids both peers changed from (the true conflicts)
}

func cloneCh(s chState) chState {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]chMsg(nil), s.q[i]...)
	}
	n.preserved = map[int]bool{}
	for k, v := range s.preserved {
		n.preserved[k] = v
	}
	n.victims = map[int]int{}
	for k, v := range s.victims {
		n.victims[k] = v
	}
	n.derived = map[int]int{}
	for k, v := range s.derived {
		n.derived[k] = v
	}
	n.both = map[int]bool{}
	for k, v := range s.both {
		n.both[k] = v
	}
	return n
}

func chKey(s chState) string { return fmt.Sprintf("%+v", s) }

type chCfg struct {
	aWinsTie bool              // the author rank for an exact stamp tie
	sets     [2][][chunks]bool // per peer, the chunk sets a save may change
	reorder  bool              // deliver queued messages in any order, not FIFO (VERIFY.md A4)
}

// chJudge is the receiver's rule for one chunk. The model's own rule is the
// default; the driver test puts the code's JudgeChunk in its place to prove
// the two agree over every interleaving.
var chJudge = func(own chRec, ownDirty bool, in chRec, base int, peerWins bool) (accept, conflict bool) {
	if in.id == own.id {
		return false, false // redelivery
	}
	accept = in.stamp > own.stamp || (in.stamp == own.stamp && peerWins)
	if !accept {
		return false, false // the sender learns when this peer's record reaches it
	}
	return true, ownDirty && base != own.id
}

func (c chCfg) wins(author int) bool {
	if c.aWinsTie {
		return author == 0
	}
	return author == 1
}

func (p *chPeer) seen() int {
	m := 0
	for c := 0; c < chunks; c++ {
		if p.led[c].stamp > m {
			m = p.led[c].stamp
		}
	}
	return m
}

func chSuccessors(s chState, cfg chCfg) []chState {
	var out []chState
	for i := 0; i < 2; i++ {
		o := 1 - i
		pi := s.p[i]

		// Fetch: the bytes of a chunk this peer accepted but does not hold,
		// from the peer that holds that content, or from a conflict copy.
		for c := 0; c < chunks; c++ {
			id := pi.led[c].id
			if pi.held[c] {
				continue
			}
			if (s.p[o].led[c].id == id && s.p[o].held[c]) || s.preserved[id] {
				n := cloneCh(s)
				n.p[i].held[c] = true
				out = append(out, n)
			}
		}

		// Save: the app rewrites its working copy; only the chunks in the
		// change set carry new content. A save needs every chunk's bytes.
		if pi.savesLeft > 0 && pi.held[0] && pi.held[1] {
			for _, set := range cfg.sets[i] {
				n := cloneCh(s)
				p := &n.p[i]
				var m chMsg
				any := false
				for c := 0; c < chunks; c++ {
					if !set[c] {
						continue
					}
					any = true
					n.nextID++
					old := p.led[c]
					rec := chRec{id: n.nextID, stamp: p.seen() + 1, author: i}
					n.derived[rec.id] = old.id
					if p.dirty[c] {
						// A second local change supersedes the first; the
						// earlier record is replaced by content derived from it.
						n.victims[old.id] = rec.id
					}
					p.led[c] = rec
					p.held[c] = true
					p.dirty[c] = true
					m.changed[c] = true
					m.recs[c] = rec
					m.base[c] = old.id
				}
				p.savesLeft--
				if any {
					n.q[o] = append(n.q[o], m)
				}
				out = append(out, n)
			}
		}

		// Deliver: judge chunk by chunk. FIFO by default; with reorder, any
		// queued message may land first (the notify worker's cross-class
		// reorder, VERIFY.md A4).
		last := 0
		if cfg.reorder {
			last = len(s.q[i]) - 1
		}
		for k := 0; k <= last && k < len(s.q[i]); k++ {
			n := cloneCh(s)
			p := &n.p[i]
			m := n.q[i][k]
			n.q[i] = append(append([]chMsg(nil), n.q[i][:k]...), n.q[i][k+1:]...)
			for c := 0; c < chunks; c++ {
				if !m.changed[c] {
					continue
				}
				in := m.recs[c]
				own := p.led[c]
				accepted, conflict := chJudge(own, p.dirty[c], in, m.base[c], cfg.wins(in.author))
				if !accepted {
					continue
				}
				if conflict {
					// Both sides changed this chunk from the same base: a
					// true conflict. The loser's content survives as a copy.
					n.preserved[own.id] = true
					n.both[own.id] = true
					n.both[in.id] = true
				} else if m.base[c] != own.id {
					n.victims[own.id] = in.id
				}
				p.led[c] = in
				p.held[c] = false
				p.dirty[c] = false
			}
			out = append(out, n)
		}
	}
	return out
}

type chReport struct {
	terminals, divergent, lossy, withCopies, copiesOutsideBoth int
	example                                                    string
}

func checkChunk(t *testing.T, savesA, savesB int, cfg chCfg) chReport {
	t.Helper()
	var r chReport
	m := Model[chState]{
		Key:        chKey,
		Successors: func(s chState) []chState { return chSuccessors(s, cfg) },
		Quiescent:  func(s chState) bool { return len(s.q[0]) == 0 && len(s.q[1]) == 0 },
		CheckTerminal: func(s chState) error {
			r.terminals++
			for c := 0; c < chunks; c++ {
				if s.p[0].led[c].id != s.p[1].led[c].id {
					r.divergent++
					if r.example == "" {
						r.example = fmt.Sprintf("chunk %d: a=%d b=%d", c, s.p[0].led[c].id, s.p[1].led[c].id)
					}
					break
				}
			}
			for v := range s.victims {
				live := false
				for i := 0; i < 2; i++ {
					for c := 0; c < chunks; c++ {
						if s.p[i].led[c].id == v {
							live = true
						}
					}
				}
				superseded := false
				for _, from := range s.derived {
					if from == v {
						superseded = true
					}
				}
				if !live && !superseded && !s.preserved[v] {
					r.lossy++
					break
				}
			}
			if len(s.preserved) > 0 {
				r.withCopies++
			}
			for id := range s.preserved {
				if !s.both[id] {
					r.copiesOutsideBoth++
					break
				}
			}
			return nil
		},
	}
	// One file, two chunks, content 0 on both peers, held by both.
	init := chState{nextID: 0, preserved: map[int]bool{}, victims: map[int]int{}, derived: map[int]int{}, both: map[int]bool{}}
	for i := 0; i < 2; i++ {
		init.p[i].held = [chunks]bool{true, true}
	}
	init.p[0].savesLeft = savesA
	init.p[1].savesLeft = savesB
	rep, err := Explore(m, init)
	require.NoError(t, err)
	t.Logf("saves=%d+%d aWinsTie=%v sets=%d/%d: explored=%d terminals=%d divergent=%d lossy=%d withCopies=%d copiesOutsideBoth=%d %s",
		savesA, savesB, cfg.aWinsTie, len(cfg.sets[0]), len(cfg.sets[1]), rep.Explored, r.terminals, r.divergent, r.lossy, r.withCopies, r.copiesOutsideBoth, r.example)
	return r
}

var chAllSets = [][chunks]bool{{true, false}, {false, true}, {true, true}}

func chBoth(sets [][chunks]bool) [2][][chunks]bool { return [2][][chunks]bool{sets, sets} }

// Every interleaving converges, loses no chunk version, and preserves a copy
// only for a chunk both sides changed from the same base.
func TestChunk_ConvergesAndPreservesOnlyTrueConflicts(t *testing.T) {
	for _, pr := range []struct{ a, b int }{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
		for _, rank := range []bool{false, true} {
			r := checkChunk(t, pr.a, pr.b, chCfg{aWinsTie: rank, sets: chBoth(chAllSets)})
			require.Zero(t, r.divergent, "divergence: %s", r.example)
			require.Zero(t, r.lossy, "a chunk version was lost")
			require.Zero(t, r.copiesOutsideBoth, "a copy for a chunk only one side changed")
		}
	}
}

// Disjoint edits: one side changes chunk 0, the other chunk 1. The ledgers
// merge both changes and no copy is made.
func TestChunk_DisjointEditsMergeWithoutCopies(t *testing.T) {
	cfg := chCfg{sets: [2][][chunks]bool{{{true, false}}, {{false, true}}}}
	for _, pr := range []struct{ a, b int }{{1, 1}, {2, 2}} {
		r := checkChunk(t, pr.a, pr.b, cfg)
		require.Zero(t, r.divergent)
		require.Zero(t, r.lossy)
		require.Zero(t, r.withCopies, "a copy from disjoint chunk edits")
	}
}

// Turn-taking: a single writer never produces a copy.
func TestChunk_SingleWriterNeverCopies(t *testing.T) {
	for _, pr := range []struct{ a, b int }{{2, 0}, {0, 2}, {3, 0}} {
		r := checkChunk(t, pr.a, pr.b, chCfg{sets: chBoth(chAllSets)})
		require.Zero(t, r.withCopies)
		require.Zero(t, r.divergent)
	}
}

// The swap-save false positive: a rewrite whose every chunk has the same
// content mints no version and sends nothing.
func TestChunk_IdenticalRewriteChangesNothing(t *testing.T) {
	r := checkChunk(t, 2, 2, chCfg{sets: chBoth([][chunks]bool{{false, false}})})
	require.Equal(t, 1, r.terminals, "an identical rewrite must leave one end state")
	require.Zero(t, r.withCopies)
	require.Zero(t, r.divergent)
}

// Same-chunk edits on both sides from the same base: exactly the conflict
// case, and the loser's content is preserved every time.
func TestChunk_SameChunkEditsAlwaysPreserve(t *testing.T) {
	r := checkChunk(t, 1, 1, chCfg{sets: chBoth([][chunks]bool{{true, false}})})
	require.Zero(t, r.lossy)
	require.Positive(t, r.withCopies, "both sides changed chunk 0: a copy must exist")
}
