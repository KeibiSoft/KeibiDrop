// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// Model v3: on-demand fetch splits metadata from content. Accepting an
// announcement moves the watermark only; a separate fetch action moves the
// bytes. A writer's announced base is either the metadata watermark (the
// shipped rule) or the version of the bytes it actually held (the honest
// rule). The checked claim: with the metadata rule, a blind overwrite of a
// version the writer never fetched clobbers the only copy of those bytes and
// no interleaving preserves them; with the held-bytes rule, the receiver-side
// conflict predicate fires and every such version survives as a conflict
// copy. Turn-taking through a real fetch stays copy-free under both rules.
package model

import "testing"

type fetchMsg struct{ ver, base int }

type fetchPeer struct {
	canon     int // version the canonical name resolves to
	held      int // version of the bytes on local disk (0 = initial content)
	watermark int // highest accepted announced version
	own       int // own newest local write
}

type fetchState struct {
	p          [2]fetchPeer
	q          [2][]fetchMsg
	nextVer    int
	writesLeft [2]int
	// blind[v] = the canonical version v replaced without its author ever
	// holding those bytes. honest[u] = some write replaced u while its
	// author held u (normal supersession). A blind victim needs
	// preservation only when nothing superseded it honestly.
	blind     map[int]int
	honest    map[int]bool
	preserved map[int]bool
}

func cloneFetch(s fetchState) fetchState {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]fetchMsg(nil), s.q[i]...)
	}
	n.blind = map[int]int{}
	for k, v := range s.blind {
		n.blind[k] = v
	}
	n.honest = map[int]bool{}
	for k, v := range s.honest {
		n.honest[k] = v
	}
	n.preserved = map[int]bool{}
	for k, v := range s.preserved {
		n.preserved[k] = v
	}
	return n
}

func fetchKey(s fetchState) string {
	b := make([]byte, 0, 64)
	enc := func(v int) { b = append(b, byte(v+1), byte((v+1)>>8)) }
	for i := 0; i < 2; i++ {
		enc(s.p[i].canon)
		enc(s.p[i].held)
		enc(s.p[i].watermark)
		enc(s.p[i].own)
		enc(s.writesLeft[i])
		enc(len(s.q[i]))
		for _, m := range s.q[i] {
			enc(m.ver)
			enc(m.base)
		}
	}
	enc(s.nextVer)
	enc(len(s.blind))
	enc(len(s.honest))
	enc(len(s.preserved))
	return string(b)
}

// fetchSuccessors enumerates writes, deliveries, and fetches. heldBase selects
// the announced-base rule: false = metadata watermark (shipped), true = the
// version of the bytes the writer held (the fix).
func fetchSuccessors(s fetchState, heldBase bool) []fetchState {
	var out []fetchState
	for i := 0; i < 2; i++ {
		other := 1 - i
		// Write: a full overwrite through the mount. The base declares what
		// this session started from. With the metadata rule that is the
		// watermark even when those bytes were never fetched.
		if s.writesLeft[i] > 0 {
			n := cloneFetch(s)
			n.nextVer++
			v := n.nextVer
			base := max(n.p[i].watermark, n.p[i].own)
			if heldBase {
				base = max(n.p[i].held, n.p[i].own)
			}
			if n.p[i].canon != n.p[i].held {
				// The canonical resolved to a version whose bytes never
				// arrived here: this write is blind about it.
				n.blind[v] = n.p[i].canon
			} else if n.p[i].canon != 0 {
				n.honest[n.p[i].canon] = true
			}
			n.p[i].canon = v
			n.p[i].held = v
			n.p[i].own = v
			n.writesLeft[i]--
			n.q[other] = append(n.q[other], fetchMsg{ver: v, base: base})
			out = append(out, n)
		}
		// Deliver: metadata acceptance. The receiver-side predicate preserves
		// its held bytes when the announce provably never saw them.
		if len(s.q[i]) > 0 {
			n := cloneFetch(s)
			m := n.q[i][0]
			n.q[i] = append([]fetchMsg(nil), n.q[i][1:]...)
			if m.ver > n.p[i].watermark && m.ver > n.p[i].own {
				identity := max(n.p[i].watermark, n.p[i].own)
				if held := n.p[i].held; held != 0 && held == n.p[i].canon &&
					m.base < identity {
					n.preserved[held] = true
				}
				n.p[i].watermark = m.ver
				n.p[i].canon = m.ver
				// Bytes do not move: held now lags canon until a fetch.
				if n.p[i].held != m.ver {
					n.p[i].held = 0 // the reset clobbers the local bytes
				}
			}
			out = append(out, n)
		}
		// Fetch: materialize the canonical version's bytes from the peer that
		// holds them. Enabled only while the bytes exist somewhere.
		if s.p[i].held != s.p[i].canon &&
			(s.p[other].held == s.p[i].canon || s.preserved[s.p[i].canon]) {
			n := cloneFetch(s)
			n.p[i].held = n.p[i].canon
			out = append(out, n)
		}
	}
	return out
}

func checkFetch(t *testing.T, writesA, writesB int, heldBase bool) (terminals, losses, copies int) {
	t.Helper()
	init := fetchState{writesLeft: [2]int{writesA, writesB},
		blind: map[int]int{}, preserved: map[int]bool{}}
	frontier := []fetchState{init}
	visited := map[string]bool{fetchKey(init): true}
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		succ := fetchSuccessors(s, heldBase)
		if len(succ) == 0 {
			terminals++
			if s.p[0].canon != s.p[1].canon {
				t.Fatalf("divergence at quiescence: %+v", s)
			}
			// A silent loss: a blindly-replaced version whose bytes survive
			// nowhere and that no write honestly superseded.
			for _, victim := range s.blind {
				if victim == 0 || s.honest[victim] {
					continue
				}
				survives := s.p[0].held == victim || s.p[1].held == victim ||
					s.preserved[victim]
				if !survives {
					losses++
					break
				}
			}
			if len(s.preserved) > 0 {
				copies++
			}
			continue
		}
		for _, n := range succ {
			k := fetchKey(n)
			if !visited[k] {
				visited[k] = true
				frontier = append(frontier, n)
			}
		}
	}
	t.Logf("writes=%d+%d heldBase=%v: terminals=%d lossHistories=%d copyHistories=%d",
		writesA, writesB, heldBase, terminals, losses, copies)
	return
}

// The shipped metadata-base rule loses a never-fetched version silently: the
// writer's watermark equals the victim's identity, the predicate reads
// turn-taking, and the only copy of the bytes is clobbered.
func TestFetch_MetadataBaseLosesUnfetchedVersion(t *testing.T) {
	_, losses, _ := checkFetch(t, 1, 1, false)
	if losses == 0 {
		t.Fatal("expected silent losses with the metadata-base rule")
	}
}

// The held-bytes base rule preserves every blindly-overwritten version: the
// base can never claim content the writer did not hold, so the receiver-side
// predicate fires and the conflict copy survives.
func TestFetch_HeldBasePreservesUnfetchedVersion(t *testing.T) {
	for _, tc := range []struct{ a, b int }{{1, 1}, {2, 1}, {2, 2}} {
		_, losses, _ := checkFetch(t, tc.a, tc.b, true)
		if losses != 0 {
			t.Fatalf("writes=%d+%d: %d loss histories despite held-base rule",
				tc.a, tc.b, losses)
		}
	}
}

// A writer that fetched before writing is honest turn-taking under both
// rules: no conflict copies from single-writer chains.
func TestFetch_FetchedTurnTakingStaysCopyFree(t *testing.T) {
	for _, heldBase := range []bool{false, true} {
		_, _, copies := checkFetch(t, 3, 0, heldBase)
		if copies != 0 {
			t.Fatalf("heldBase=%v: single-writer chains produced %d copy histories",
				heldBase, copies)
		}
	}
}
