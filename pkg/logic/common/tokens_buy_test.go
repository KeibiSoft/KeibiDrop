// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Tests for the buy flow's claims: each names the client that opened it, every open
// ABOUTME: purchase is polled by one loop on a backing-off schedule, and the loop ends on its own.
package common

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/tokens"
	"github.com/stretchr/testify/require"
)

// tokenServiceStub answers /collect: a claim in paid gets its code, any other
// claim "not ready". It records every claim it was asked about.
type tokenServiceStub struct {
	mu    sync.Mutex
	paid  map[string]string
	asked []string
}

func (s *tokenServiceStub) pay(claim, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paid[claim] = code
}

func (s *tokenServiceStub) askedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asked)
}

func startTokenServiceStub(t *testing.T, tick, window time.Duration) *tokenServiceStub {
	t.Helper()
	stub := &tokenServiceStub{paid: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claim := r.URL.Query().Get("claim")
		if r.URL.Path != "/collect" || claim == "" || r.Header.Get("Accept") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.asked = append(stub.asked, claim)
		code, ok := stub.paid[claim]
		stub.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":%q,"units":1024}`, code)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldTick, oldWindow := tokensServiceBase, claimPollTick, claimPollWindow
	tokensServiceBase, claimPollTick, claimPollWindow = srv.URL, tick, window
	t.Cleanup(func() { tokensServiceBase, claimPollTick, claimPollWindow = oldBase, oldTick, oldWindow })
	return stub
}

func claimOf(t *testing.T, buyURL string) string {
	t.Helper()
	_, claim, ok := strings.Cut(buyURL, "/buy?claim=")
	require.True(t, ok, "buy URL %q carries no claim", buyURL)
	return claim
}

// stopBuyPolls ends kd's poll at its next round, so no loop outlives the test.
func stopBuyPolls(t *testing.T, kd *KeibiDrop) {
	t.Helper()
	kd.mu.Lock()
	kd.buyClaims = nil
	kd.mu.Unlock()
	require.Eventually(t, func() bool { return pollStopped(kd) }, 3*time.Second, 5*time.Millisecond)
}

func pollStopped(kd *KeibiDrop) bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	return !kd.buyPolling && len(kd.buyClaims) == 0
}

// The claim names the client that opened the purchase, so the token service's
// record and Stripe's client_reference_id say where the sale was made.
func TestTokensBuyStart_ClaimNamesTheSurface(t *testing.T) {
	startTokenServiceStub(t, 10*time.Millisecond, 50*time.Millisecond)
	for _, surface := range []string{"desktop", "kd", "cli", "mcp"} {
		kd := newTokenTestKD(t, "")
		kd.Surface = surface
		require.Equal(t, surface, tokens.ClaimSurface(claimOf(t, kd.TokensBuyStart())))
		stopBuyPolls(t, kd)
	}
	kd := newTokenTestKD(t, "")
	require.Equal(t, "", tokens.ClaimSurface(claimOf(t, kd.TokensBuyStart())), "an unnamed client keeps the bare claim")
	stopBuyPolls(t, kd)
}

// A person opens the buy page twice and pays in the first tab: the code still
// lands. The poll used to follow only the newest claim.
func TestTokensBuyStart_SecondClickKeepsTheFirstPurchase(t *testing.T) {
	stub := startTokenServiceStub(t, 10*time.Millisecond, 5*time.Second)
	kd := newTokenTestKD(t, "")
	kd.Surface = "desktop"
	t.Cleanup(func() { stopBuyPolls(t, kd) })
	var mu sync.Mutex
	var events []string
	kd.OnEvent = func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }

	first := claimOf(t, kd.TokensBuyStart())
	second := claimOf(t, kd.TokensBuyStart())
	require.NotEqual(t, first, second)
	stub.pay(first, encodeTokenCode(testSeed(t), 1024))

	require.Eventually(t, func() bool { return kd.Wallet().unitsLeft() == 1024 }, 3*time.Second, 10*time.Millisecond,
		"the first purchase never landed")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if strings.HasPrefix(e, "tokens_added:") {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)

	// The landed claim is no longer asked about; the open one still is.
	kd.mu.Lock()
	open := append([]buyClaim(nil), kd.buyClaims...)
	kd.mu.Unlock()
	require.Len(t, open, 1)
	require.Equal(t, second, open[0].ref)
}

// Many clicks share one loop, each round asks once per open claim, and the
// number of open claims is bounded.
func TestTokensBuyStart_OneLoopForManyClicks(t *testing.T) {
	stub := startTokenServiceStub(t, 20*time.Millisecond, 400*time.Millisecond)
	kd := newTokenTestKD(t, "")
	t.Cleanup(func() { stopBuyPolls(t, kd) })
	for i := 0; i < maxBuyClaims+3; i++ {
		kd.TokensBuyStart()
	}
	kd.mu.Lock()
	open := len(kd.buyClaims)
	kd.mu.Unlock()
	require.Equal(t, maxBuyClaims, open)

	time.Sleep(110 * time.Millisecond) // about five rounds
	asked := stub.askedCount()
	require.LessOrEqual(t, asked, 6*maxBuyClaims, "more than one loop is polling: %d asks", asked)
	require.GreaterOrEqual(t, asked, maxBuyClaims, "the loop never polled")
}

// Unpaid claims expire with the window, the loop ends by itself, and nothing
// asks the token service after that.
func TestClaimPollLoop_EndsWhenNoClaimIsLeft(t *testing.T) {
	stub := startTokenServiceStub(t, 10*time.Millisecond, 100*time.Millisecond)
	kd := newTokenTestKD(t, "")
	kd.TokensBuyStart()
	require.Eventually(t, func() bool { return pollStopped(kd) }, 2*time.Second, 10*time.Millisecond,
		"the poll outlived its window")
	n := stub.askedCount()
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, n, stub.askedCount(), "the token service was asked after the poll ended")

	// A new purchase starts a new loop.
	stub.pay(claimOf(t, kd.TokensBuyStart()), encodeTokenCode(testSeed(t), 2048))
	require.Eventually(t, func() bool { return kd.Wallet().unitsLeft() == 2048 }, 2*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return pollStopped(kd) }, 2*time.Second, 10*time.Millisecond)
}

// While the ledger fails, reveals space out up to revealMaxDelay; the first
// success restores the tick.
func TestRevealSkips(t *testing.T) {
	require.Equal(t, 0, revealSkips(0))
	require.Equal(t, 0, revealSkips(1), "one failure retries at the next tick")
	require.Equal(t, 1, revealSkips(2))
	require.Equal(t, 3, revealSkips(3))
	require.Equal(t, 7, revealSkips(4))
	require.Equal(t, int(revealMaxDelay/revealTick)-1, revealSkips(5))
	require.Equal(t, int(revealMaxDelay/revealTick)-1, revealSkips(1000))
}
