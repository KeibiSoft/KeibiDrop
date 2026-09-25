// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// The bridge-leg presence gate: a creator whose saved contact has gone quiet
// stops parking a paid leg every round, and every other case keeps the
// behaviour it had. Grown from the 2026-09-19 tm-1 alarm, where the dogfood
// NAS parked one every 15 s for nine days waiting for a laptop that was off.

package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
)

const gatePeerFP = "FP-PEER-0001"

// presenceRelay is a relay whose /presence answers whatever present says.
type presenceRelay struct {
	present atomic.Bool
	gets    atomic.Int32
}

func newPresenceKD(t *testing.T) (*KeibiDrop, *presenceRelay) {
	t.Helper()
	rel := &presenceRelay{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/presence" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			rel.gets.Add(1)
			if !rel.present.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	ab, err := identity.LoadAddressBook(t.TempDir(), nil)
	require.NoError(t, err)
	require.NoError(t, ab.Add("laptop", gatePeerFP))

	kd := newBareKD()
	kd.logger = testkit.DiscardLogger()
	kd.AddressBook = ab
	kd.Identity = &identity.DeviceIdentity{Fingerprint: "FP-SELF-0001"}
	kd.RelayEndoint = u
	kd.relayClient = &http.Client{Timeout: 2 * time.Second}
	// The contact's last handshake declared a cipher (a 0.4.9 or newer build),
	// so its absence may be believed. The older build has its own test.
	kd.presenceReliable.Store(gatePeerFP, true)
	return kd, rel
}

// A contact whose build declared no cipher (0.4.8 or older) keeps a leg every
// round and every dial, however long the relay has not seen it: those desktop
// builds stopped posting presence at their first disconnect of a run while
// they were still online, so their absence proves nothing.
func TestPresenceGate_OlderBuildIsNeverAbsent(t *testing.T) {
	kd, rel := autoConnectPresenceKD(t)
	kd.presenceReliable.Delete(gatePeerFP)

	rel.present.Store(true)
	park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
	require.True(t, park)

	rel.present.Store(false)
	kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-24*time.Hour).Unix())
	park, _ = kd.shouldParkBridgeLeg(gatePeerFP)
	require.True(t, park, "an older build may be online with a dead heartbeat; it gets its leg")
	require.False(t, kd.peerKnownAbsent(), "and the watchdog keeps dialing it")
}

// A contact that has been seen online and then goes quiet past the relay's own
// TTL is absent: no leg. That is the tm-1 NAS.
func TestPresenceGate_AbsentSavedContactSkipsTheLeg(t *testing.T) {
	kd, rel := newPresenceKD(t)

	rel.present.Store(true)
	park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
	require.True(t, park, "a present contact is dialled exactly as before")

	rel.present.Store(false)
	park, _ = kd.shouldParkBridgeLeg(gatePeerFP)
	require.True(t, park, "one missed beat inside the relay TTL is not an absence")

	// Age the observation past the relay's TTL.
	kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-2*relayPresenceTTL).Unix())
	park, reason := kd.shouldParkBridgeLeg(gatePeerFP)
	require.False(t, park, "a contact absent for longer than the relay TTL gets no leg")
	require.Contains(t, reason, "not parking a bridge leg")

	// And it comes straight back when the contact returns.
	rel.present.Store(true)
	park, _ = kd.shouldParkBridgeLeg(gatePeerFP)
	require.True(t, park, "the contact is back; the leg is back")
}

// Everything the gate must never touch. A NAS that stops trying is worse than
// a noisy one, so each of these keeps the old behaviour.
func TestPresenceGate_UnchangedCases(t *testing.T) {
	t.Run("never seen present", func(t *testing.T) {
		kd, rel := newPresenceKD(t)
		rel.present.Store(false)
		// An older peer that posts no presence at all: a leg every round, forever.
		for i := 0; i < 5; i++ {
			park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
			require.True(t, park, "a contact never observed online decides nothing")
		}
	})

	t.Run("not a saved contact", func(t *testing.T) {
		kd, rel := newPresenceKD(t)
		rel.present.Store(false)
		park, _ := kd.shouldParkBridgeLeg("FP-STRANGER")
		require.True(t, park, "a first-time peer is unchanged")
		require.Zero(t, rel.gets.Load(), "and costs the relay nothing")
	})

	t.Run("local mode", func(t *testing.T) {
		kd, rel := newPresenceKD(t)
		kd.IsLocalMode = true
		rel.present.Store(false)
		kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-time.Hour).Unix())
		park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
		require.True(t, park, "on a LAN the relay knows nothing worth asking")
		require.Zero(t, rel.gets.Load())
	})

	t.Run("TOFU and empty fingerprints", func(t *testing.T) {
		kd, _ := newPresenceKD(t)
		for _, fp := range []string{"", "TOFU"} {
			park, _ := kd.shouldParkBridgeLeg(fp)
			require.True(t, park, "no fingerprint to ask about: unchanged")
		}
	})

	t.Run("no relay configured", func(t *testing.T) {
		kd, _ := newPresenceKD(t)
		kd.RelayEndoint = nil
		kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-time.Hour).Unix())
		park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
		require.True(t, park)
	})

	t.Run("relay 5xx", func(t *testing.T) {
		kd, _ := newPresenceKD(t)
		kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-time.Hour).Unix())
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(down.Close)
		u, err := url.Parse(down.URL)
		require.NoError(t, err)
		kd.RelayEndoint = u
		park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
		require.True(t, park, "a relay that is failing is not a peer that is absent")
	})

	t.Run("relay unreachable", func(t *testing.T) {
		kd, _ := newPresenceKD(t)
		kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-time.Hour).Unix())
		bad, err := url.Parse("http://127.0.0.1:1")
		require.NoError(t, err)
		kd.RelayEndoint = bad
		park, _ := kd.shouldParkBridgeLeg(gatePeerFP)
		require.True(t, park, "an unanswered question is not an absence")
	})
}

// autoConnectPresenceKD is a presence-gated kd whose auto-connect loop targets
// the saved contact, driving the real peerKnownAbsent path.
func autoConnectPresenceKD(t *testing.T) (*KeibiDrop, *presenceRelay) {
	t.Helper()
	kd, rel := newPresenceKD(t)
	fp := gatePeerFP
	kd.autoConnectTarget.Store(&fp)
	return kd, rel
}

// TestAutoConnect_HoldsTheDialWhileThePeerIsAbsent is the tm-1 loop from the
// client side: with the owner offline the NAS used to create a room every two
// minutes and park a paid bridge leg every 15 s inside it.
func TestAutoConnect_HoldsTheDialWhileThePeerIsAbsent(t *testing.T) {
	kd, rel := autoConnectPresenceKD(t)
	rel.present.Store(true)
	require.True(t, kd.CheckContactPresence(gatePeerFP))
	// Age the sighting, then take the peer offline: known absent.
	kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-2*relayPresenceTTL).Unix())
	rel.present.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dials atomic.Int32
	var running atomic.Bool
	dial := func() error { dials.Add(1); return errors.New("peer offline") }

	tun := fastTuning
	tun.absentPoll = 2 * time.Millisecond
	tun.absentDialEvery = time.Hour // no safety net inside this test

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, tun, dial, running.Load, func() bool { return false })
		close(done)
	}()

	time.Sleep(60 * tun.poll)
	require.Zero(t, dials.Load(), "no dial while the relay says the peer is away")
	require.NotZero(t, rel.gets.Load(), "and the hold costs one presence GET per poll, nothing more")

	// The owner comes back: the loop dials on the next poll.
	rel.present.Store(true)
	waitFor(t, 3*time.Second, func() bool { return dials.Load() > 0 }, "dialled once the peer returned")
	cancel()
	<-done
}

// The safety net: presence can be wrong, so a long absence still gets a dial.
func TestAutoConnect_SafetyNetDialsWhileStillAbsent(t *testing.T) {
	kd, rel := autoConnectPresenceKD(t)
	rel.present.Store(true)
	require.True(t, kd.CheckContactPresence(gatePeerFP))
	kd.presenceSeen.Store(gatePeerFP, time.Now().Add(-2*relayPresenceTTL).Unix())
	rel.present.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dials atomic.Int32
	var running atomic.Bool
	dial := func() error { dials.Add(1); return errors.New("peer offline") }

	tun := fastTuning
	tun.absentPoll = 2 * time.Millisecond
	tun.absentDialEvery = 40 * time.Millisecond

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, tun, dial, running.Load, func() bool { return false })
		close(done)
	}()

	waitFor(t, 3*time.Second, func() bool { return dials.Load() >= 2 },
		"the safety net keeps dialling an absent peer, just rarely")
	cancel()
	<-done
}

// A peer that never posts presence must behave exactly as it did before the
// gate existed: dial, back off, dial again.
func TestAutoConnect_PeerThatNeverPostsPresenceIsUnchanged(t *testing.T) {
	kd, rel := autoConnectPresenceKD(t)
	rel.present.Store(false) // never seen online, so presence decides nothing

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dials atomic.Int32
	var running atomic.Bool
	dial := func() error { dials.Add(1); return errors.New("peer offline") }

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, fastTuning, dial, running.Load, func() bool { return false })
		close(done)
	}()

	waitFor(t, 3*time.Second, func() bool { return dials.Load() >= 3 }, "dials as it always did")
	cancel()
	<-done
}
