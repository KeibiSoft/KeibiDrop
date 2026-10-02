// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// Model v4: swap-save stamps. Every earlier model mints a fresh stamp at the
// moment of the write or commit (nextVer++), so a new version always outranks
// every version its writer has seen: the clock condition of an LWW register.
// The shipped swap-save does not meet it. The app writes the working copy at
// time t, a peer version can land between t and the rename, and the RENAME
// announces the working copy's DISK mtime (2026-10-01: fuse_directory.go
// Rename, renStat), while the retargeted temp ADD announces the Go-clock stamp
// Write took after the pwrite (utils.go retarget plus refreshAttrFromDisk).
// So one version carries two stamps, and the newer save can carry the older
// one. Stamps here: a write at clock c has disk stamp 4c and Go stamp 4c+2,
// so a +1 bump over a disk stamp stays below the same write's Go stamp, as a
// 1 ns bump stays below a Go stamp taken microseconds later.
//
// The checked claims: the shipped rules diverge; a Lamport stamp (new version
// above every stamp the writer has seen) closes the divergence only when the
// version also has ONE stamp on the wire; the metadata-watermark base still
// loses versions under any stamp rule (model v3 again).
package model

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type ssMsg struct {
	add     bool // the debounced retargeted ADD, not the RENAME
	cid     int
	stamp   int
	base    int
	baseOwn bool // the base names the sender's own version
}

type ssPeer struct {
	canon      int // content id at the canonical name
	canonStamp int // identity stamp of canon here (stat mtime)
	held       int // content id whose bytes are on disk; -1 none
	heldStamp  int // HeldMtimeNs
	watermark  int // RemoteMtimeNs
	own        int // LastAnnouncedMtimeNs
	dirty      bool
	tmp        int // working copy content id; 0 none
	tmpClock   int // clock at the working copy's write
	pendAdd    int // stamp of the queued retargeted ADD; -1 none
	pendBase   int
	pendOwn    bool
	swapsLeft  int
}

type ssState struct {
	p         [2]ssPeer
	q         [2][]ssMsg
	clock     int
	nextCid   int
	derived   map[int]int  // content id -> content id its working copy came from
	preserved map[int]bool // content ids saved as conflict copies
	victims   map[int]bool // content ids replaced by content not derived from them
}

type ssCfg struct {
	heldBase    bool // swap base = held bytes (model v3 rule) instead of the watermark
	lamport     bool // commit stamp raised above every stamp the writer has seen
	oneStamp    bool // the retargeted ADD carries the RENAME's stamp
	coarse      bool // a working copy may reuse the current clock tick (Linux ext4)
	bobOutranks bool // fingerprint rank: peer 1 wins exact ties
	fetchInTmp  bool // a reader may fetch the target while a working copy is open
	baseAuthor  bool // announces say whose version the base is (stamp ties)
}

func cloneSS(s ssState) ssState {
	n := s
	for i := 0; i < 2; i++ {
		n.q[i] = append([]ssMsg(nil), s.q[i]...)
	}
	n.derived = map[int]int{}
	for k, v := range s.derived {
		n.derived[k] = v
	}
	n.preserved = map[int]bool{}
	for k, v := range s.preserved {
		n.preserved[k] = v
	}
	n.victims = map[int]bool{}
	for k, v := range s.victims {
		n.victims[k] = v
	}
	return n
}

// fmt prints maps in key order, so the key is canonical.
func ssKey(s ssState) string { return fmt.Sprintf("%+v", s) }

func (c ssCfg) peerWins(receiver int) bool {
	if c.bobOutranks {
		return receiver == 0
	}
	return receiver == 1
}

func ssSuccessors(s ssState, cfg ssCfg) []ssState {
	var out []ssState
	for i := 0; i < 2; i++ {
		o := 1 - i
		pi := s.p[i]

		// WriteTemp: the app copies the canonical file (a read: fetched if
		// the bytes are not here) and edits the copy.
		if pi.swapsLeft > 0 && pi.tmp == 0 && (pi.held == pi.canon || s.p[o].held == pi.canon) {
			ticks := []int{s.clock + 1}
			if cfg.coarse && s.clock > 0 {
				ticks = append(ticks, s.clock)
			}
			for _, c := range ticks {
				n := cloneSS(s)
				p := &n.p[i]
				if p.held != p.canon {
					p.held = p.canon
					p.heldStamp = p.watermark
				}
				n.clock = c
				n.nextCid++
				p.tmp = n.nextCid
				p.tmpClock = c
				n.derived[p.tmp] = p.canon
				out = append(out, n)
			}
		}

		// Commit: rename the working copy over the canonical name.
		if pi.tmp != 0 {
			n := cloneSS(s)
			p := &n.p[i]
			var base int
			baseOwn := false
			if cfg.heldBase {
				base = max(p.heldStamp, p.own)
				baseOwn = p.own > p.heldStamp
				if p.dirty && p.canonStamp > base {
					base = p.canonStamp
					baseOwn = true
				}
				if base == 0 && p.watermark > 0 {
					base = -1
				}
			} else {
				base = p.watermark
				if p.dirty && p.canonStamp > base {
					base = p.canonStamp
				}
			}
			disk := 4 * p.tmpClock
			goStamp := disk + 2
			if cfg.lamport {
				if seen := max(p.watermark, p.own, p.canonStamp); disk <= seen {
					disk = seen + 1
				}
			}
			addStamp := max(goStamp, disk)
			if cfg.oneStamp {
				addStamp = disk
			}
			if old := p.canon; old != n.derived[p.tmp] {
				n.victims[old] = true
			}
			p.canon = p.tmp
			p.canonStamp = disk
			p.held = p.tmp
			p.heldStamp = 0
			p.own = disk
			p.dirty = true
			p.tmp = 0
			p.swapsLeft--
			p.pendAdd = addStamp
			p.pendBase = base
			p.pendOwn = baseOwn
			n.q[o] = append(n.q[o], ssMsg{cid: p.canon, stamp: disk, base: base, baseOwn: baseOwn})
			out = append(out, n)
		}

		// FlushAdd: the debounced retargeted ADD goes out.
		if pi.pendAdd >= 0 {
			n := cloneSS(s)
			p := &n.p[i]
			n.q[o] = append(n.q[o], ssMsg{add: true, cid: p.canon, stamp: p.pendAdd, base: p.pendBase, baseOwn: p.pendOwn})
			p.pendAdd = -1
			out = append(out, n)
		}

		// Deliver: AddRemoteFileWithBase, the shipped acceptance and conflict
		// predicates (the RENAME conflict path ends there too).
		if len(s.q[i]) > 0 {
			n := cloneSS(s)
			p := &n.p[i]
			m := n.q[i][0]
			n.q[i] = append([]ssMsg(nil), n.q[i][1:]...)
			identity := p.watermark
			if p.dirty && p.canonStamp > identity {
				identity = p.canonStamp
			}
			accepted := m.stamp > identity || (m.stamp == identity && p.dirty && cfg.peerWins(i))
			unseen := m.base < identity
			if cfg.baseAuthor && m.base == identity && m.baseOwn && p.canonStamp > p.watermark {
				// Equal stamps, two authors: the base is the sender's own
				// version, not the receiver's, which the sender never saw.
				unseen = true
			}
			conflict := accepted && p.dirty && m.base != 0 && unseen
			switch {
			case accepted:
				if m.cid != p.canon {
					if conflict {
						n.preserved[p.canon] = true
					} else if n.derived[m.cid] != p.canon {
						n.victims[p.canon] = true
					}
				}
				p.pendAdd = -1 // CancelPendingNotify
				p.canon = m.cid
				p.canonStamp = m.stamp
				p.watermark = max(p.watermark, m.stamp)
				p.dirty = false
				if p.held != m.cid {
					p.held = -1
				}
			case p.dirty:
				// Older than local authority: keep it.
			default:
				// A clean receiver takes the announced state.
				if m.cid != p.canon {
					if n.derived[m.cid] != p.canon {
						n.victims[p.canon] = true
					}
					p.canon = m.cid
					p.canonStamp = m.stamp
					if p.held != m.cid {
						p.held = -1
					}
				}
			}
			out = append(out, n)
		}

		// Fetch: bytes by path from the peer that holds the canonical version.
		if pi.held != pi.canon && s.p[o].held == pi.canon && (pi.tmp == 0 || cfg.fetchInTmp) {
			n := cloneSS(s)
			n.p[i].held = n.p[i].canon
			n.p[i].heldStamp = n.p[i].watermark
			out = append(out, n)
		}
	}
	return out
}

type ssReport struct {
	terminals, divergent, lossy, withCopies, stuck int
	example                                        string
}

func checkSwapStamp(t *testing.T, swapsA, swapsB int, cfg ssCfg) ssReport {
	t.Helper()
	var r ssReport
	m := Model[ssState]{
		Key:        ssKey,
		Successors: func(s ssState) []ssState { return ssSuccessors(s, cfg) },
		// A peer whose remaining save cannot start (its canonical bytes are
		// held nowhere: the peers adopted each other's version and preserved
		// their own) is a legal stopping point; the terminal check counts it.
		Quiescent: func(s ssState) bool {
			for i := 0; i < 2; i++ {
				if s.p[i].tmp != 0 || s.p[i].pendAdd >= 0 || len(s.q[i]) > 0 {
					return false
				}
			}
			return true
		},
		CheckTerminal: func(s ssState) error {
			r.terminals++
			if s.p[0].swapsLeft > 0 || s.p[1].swapsLeft > 0 {
				r.stuck++
			}
			if s.p[0].canon != s.p[1].canon {
				r.divergent++
				if r.example == "" {
					r.example = fmt.Sprintf("alice canon=%d bob canon=%d preserved=%v", s.p[0].canon, s.p[1].canon, s.preserved)
				}
			}
			for v := range s.victims {
				superseded := false
				for _, from := range s.derived {
					if from == v {
						superseded = true
					}
				}
				live := s.p[0].canon == v || s.p[1].canon == v
				if !superseded && !live && !s.preserved[v] {
					r.lossy++
					break
				}
			}
			if len(s.preserved) > 0 {
				r.withCopies++
			}
			return nil
		},
	}

	// v1 (content 0) written by Bob (peer 1) at clock 1, announced, fetched
	// and read by Alice (peer 0): the state both subtests start from.
	init := ssState{clock: 1, derived: map[int]int{}, preserved: map[int]bool{}, victims: map[int]bool{}}
	init.p[0] = ssPeer{canon: 0, canonStamp: 4, held: 0, heldStamp: 4, watermark: 4, pendAdd: -1, swapsLeft: swapsA}
	init.p[1] = ssPeer{canon: 0, canonStamp: 4, held: 0, own: 4, dirty: true, pendAdd: -1, swapsLeft: swapsB}
	rep, err := Explore(m, init)
	require.NoError(t, err)
	t.Logf("swaps=%d+%d %+v: explored=%d terminals=%d divergent=%d lossy=%d withCopies=%d stuck=%d %s",
		swapsA, swapsB, cfg, rep.Explored, r.terminals, r.divergent, r.lossy, r.withCopies, r.stuck, r.example)
	return r
}

var ssPairs = []struct{ a, b int }{{1, 1}, {2, 1}, {1, 2}, {2, 2}}

// The rules shipped before 2026-10-01 (watermark base, working-copy stamps)
// both lose versions (PR #81 CI) and diverge.
func TestSwapStamp_PreFixLosesAndDiverges(t *testing.T) {
	for _, rank := range []bool{false, true} {
		r := checkSwapStamp(t, 1, 1, ssCfg{bobOutranks: rank})
		require.Positive(t, r.lossy, "expected silent losses with the watermark base")
		require.Positive(t, r.divergent, "expected divergence with working-copy stamps")
	}
}

// The rules on main after e05ed32 (held base, working-copy stamps): no loss,
// but the ordered swap still diverges (CI run 36870254753).
func TestSwapStamp_HeldBaseStillDiverges(t *testing.T) {
	for _, rank := range []bool{false, true} {
		r := checkSwapStamp(t, 1, 1, ssCfg{heldBase: true, bobOutranks: rank})
		require.Zero(t, r.lossy, "the held base must not lose a version")
		require.Positive(t, r.divergent, "expected divergence with working-copy stamps")
	}
}

// A Lamport bump on the RENAME alone is not enough: the retargeted ADD's
// Go-clock stamp outranks the bumped stamp and the two peers swap canonicals.
func TestSwapStamp_BumpWithTwoStampsSwaps(t *testing.T) {
	diverged := 0
	for _, rank := range []bool{false, true} {
		r := checkSwapStamp(t, 1, 1, ssCfg{heldBase: true, lamport: true, bobOutranks: rank})
		diverged += r.divergent
	}
	require.Positive(t, diverged, "expected the two-stamp swap with the bump alone")
}

// The proposed rule: held base, Lamport commit stamp, one stamp per version.
// No interleaving diverges or loses a version (fine clock; coarse ticks are
// TestSwapStamp_CoarseStampCollision).
func TestSwapStamp_LamportOneStampConverges(t *testing.T) {
	for _, pr := range ssPairs {
		for _, rank := range []bool{false, true} {
			r := checkSwapStamp(t, pr.a, pr.b, ssCfg{heldBase: true, lamport: true, oneStamp: true, bobOutranks: rank})
			require.Zero(t, r.divergent, "divergence under the proposed rule: %s", r.example)
			require.Zero(t, r.lossy, "silent loss under the proposed rule")
		}
	}
}

// Stamps alone are not version identities: with coarse ticks a peer's own
// version and the other peer's can share a stamp, and a later base naming the
// first reads as having seen the second. Shipped rules first, then the
// proposed rule, then the proposed rule with the base's author on the wire.
func TestSwapStamp_CoarseStampCollision(t *testing.T) {
	shipped := checkSwapStamp(t, 2, 1, ssCfg{heldBase: true, coarse: true, bobOutranks: true})
	require.Positive(t, shipped.divergent, "expected divergence with coarse ticks on the shipped rules")
	proposed := checkSwapStamp(t, 2, 1, ssCfg{heldBase: true, lamport: true, oneStamp: true, coarse: true, bobOutranks: true})
	require.Positive(t, proposed.lossy, "expected the stamp-collision loss without the base author")
	for _, pr := range ssPairs {
		for _, rank := range []bool{false, true} {
			r := checkSwapStamp(t, pr.a, pr.b, ssCfg{heldBase: true, lamport: true, oneStamp: true, coarse: true, bobOutranks: rank, baseAuthor: true})
			require.Zero(t, r.divergent, "divergence with base author: %s", r.example)
			require.Zero(t, r.lossy, "silent loss with base author")
		}
	}
}

// The stamp rule does not replace the base rule: with the watermark base the
// proposed stamps still lose versions (model v3's claim, for swaps).
func TestSwapStamp_WatermarkBaseLosesUnderAnyStamp(t *testing.T) {
	r := checkSwapStamp(t, 1, 1, ssCfg{lamport: true, oneStamp: true})
	require.Positive(t, r.lossy, "expected silent losses with the watermark base")
}

// One writer taking turns: no conflict copies under the proposed rule.
func TestSwapStamp_TurnTakingStaysCopyFree(t *testing.T) {
	for _, pr := range []struct{ a, b int }{{2, 0}, {0, 2}, {3, 0}} {
		r := checkSwapStamp(t, pr.a, pr.b, ssCfg{heldBase: true, lamport: true, oneStamp: true, coarse: true})
		require.Zero(t, r.withCopies, "single-writer swaps produced conflict copies")
		require.Zero(t, r.divergent)
	}
}

// Residual of the held rule: a read of the peer version by ANY process while
// the app's working copy is open (an indexer, Windows Defender's pre-read)
// raises the held base and the receiver's version can be lost.
func TestSwapStamp_ResidualFetchDuringWorkingCopy(t *testing.T) {
	r := checkSwapStamp(t, 1, 1, ssCfg{heldBase: true, lamport: true, oneStamp: true, fetchInTmp: true})
	t.Logf("residual: lossy=%d of %d terminals", r.lossy, r.terminals)
}
