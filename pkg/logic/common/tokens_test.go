// ABOUTME: Tests for the prepaid token wallet, the paid bridge conn wrapper
// ABOUTME: (ack strip + byte counting), the reveal loop math, and busy events.
package common

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testSeed(t *testing.T) [32]byte {
	t.Helper()
	var s [32]byte
	_, err := rand.Read(s[:])
	require.NoError(t, err)
	return s
}

func newTokenTestKD(t *testing.T, relayURL string) *KeibiDrop {
	t.Helper()
	kd := newBareKD()
	kd.relayClient = &http.Client{Timeout: 2 * time.Second}
	if relayURL != "" {
		u, err := url.Parse(relayURL)
		require.NoError(t, err)
		kd.RelayEndoint = u
	}
	w, err := openTokenWallet(t.TempDir())
	require.NoError(t, err)
	kd.wallet = w
	return kd
}

func TestTokenCodeRoundtripClient(t *testing.T) {
	seed := testSeed(t)
	code := encodeTokenCode(seed, 25600)
	got, units, err := decodeTokenCode(" " + code + "\n")
	require.NoError(t, err)
	require.Equal(t, seed, got)
	require.Equal(t, 25600, units)

	broken := []byte(code)
	broken[len(broken)-1] ^= 1
	_, _, err = decodeTokenCode(string(broken))
	require.Error(t, err, "corrupted code accepted")

	_, _, err = decodeTokenCode("KDT2.whatever")
	require.Error(t, err, "wrong prefix accepted")
}

func TestWalletAddPersistAndPerms(t *testing.T) {
	dir := t.TempDir()
	w, err := openTokenWallet(dir)
	require.NoError(t, err)
	seed := testSeed(t)
	code := encodeTokenCode(seed, 100)

	c, err := w.Add(code)
	require.NoError(t, err)
	require.Equal(t, 100, c.Units)
	require.Equal(t, chainHashAt(seed, 100), c.anchor)

	dup, err := w.Add(code)
	require.NoError(t, err)
	require.Same(t, c, dup, "duplicate paste must merge, not double-add")

	info, err := os.Stat(filepath.Join(dir, walletFile))
	require.NoError(t, err)
	// Windows has no Unix permission bits; Stat reports 0666 there.
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}

	w.markRevealed(c, 40, false)
	w2, err := openTokenWallet(dir)
	require.NoError(t, err)
	sums := w2.Summaries()
	require.Len(t, sums, 1)
	require.Equal(t, 60, sums[0].UnitsLeft)
	require.Equal(t, code, sums[0].Code)
	require.NotNil(t, w2.pickFunded(), "60 units left but no funded pick")

	w2.markRevealed(w2.pickFunded(), 100, false)
	require.Nil(t, w2.pickFunded(), "exhausted chain still picked")
}

func TestPayConnAckStripAndCounting(t *testing.T) {
	kd := newTokenTestKD(t, "")
	seed := testSeed(t)
	_, err := kd.Wallet().Add(encodeTokenCode(seed, 10))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts, "funded wallet on a keibi bridge must create a token session")
	t.Cleanup(kd.resetTokenSession) // stop the reveal loop applyAck starts

	client, server := net.Pipe()
	pc := newPayConn(client, ts)
	go func() {
		// Bridge side: one ack byte, then peer data.
		_, _ = server.Write([]byte{ackPaidBit | ackContentionBit})
		_, _ = server.Write([]byte("peer-bytes"))
	}()
	buf := make([]byte, 16)
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := pc.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "peer-bytes", string(buf[:n]), "ack byte leaked into the stream")
	require.True(t, ts.paid.Load(), "ack bits not applied")
	require.True(t, ts.contention.Load(), "ack bits not applied")
	require.Equal(t, int64(len("peer-bytes")), ts.recv.Load(), "ack must not count")

	go func() { _, _ = io.Copy(io.Discard, server) }()
	_, err = pc.Write([]byte("up"))
	require.NoError(t, err)
	require.Equal(t, int64(2), ts.sent.Load())

	_ = pc.Close()
	require.Equal(t, int32(0), ts.conns.Load())
}

// anchorBalanceBody is the /anchor/balance response shape the wallet decodes
// (TokensAdd and tokenSession.resyncFromLedger).
type anchorBalanceBody struct {
	UnitsRemaining int    `json:"units_remaining"`
	LastHash       string `json:"last_hash"`
	State          string `json:"state"`
	Message        string `json:"message"`
}

// miniLedger implements the relay's public reveal/balance surface for tests.
type miniLedger struct {
	mu        sync.Mutex
	last      [32]byte
	remaining int
}

func (m *miniLedger) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/anchor/reveal", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hash  string `json:"hash"`
			Steps int    `json:"steps"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, err := base64.RawURLEncoding.DecodeString(req.Hash)
		if err != nil || len(raw) != 32 || req.Steps < 1 || req.Steps > 64 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		h := append([]byte(nil), raw...)
		for i := 0; i < req.Steps; i++ {
			sum := sha256.Sum256(h)
			h = sum[:]
		}
		if string(h) != string(m.last[:]) || req.Steps > m.remaining {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		copy(m.last[:], raw)
		m.remaining -= req.Steps
		_ = json.NewEncoder(w).Encode(map[string]int{"units_remaining": m.remaining})
	})
	mux.HandleFunc("/anchor/balance", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		state := "active"
		if m.remaining == 0 {
			state = "spent"
		}
		_ = json.NewEncoder(w).Encode(anchorBalanceBody{
			UnitsRemaining: m.remaining,
			LastHash:       base64.RawURLEncoding.EncodeToString(m.last[:]),
			State:          state,
		})
	})
	return mux
}

func TestRevealLoopCoversHalfAndResyncs(t *testing.T) {
	seed := testSeed(t)
	const units = 10
	led := &miniLedger{last: chainHashAt(seed, units), remaining: units}
	srv := httptest.NewServer(led.handler())
	defer srv.Close()

	kd := newTokenTestKD(t, srv.URL)
	_, err := kd.Wallet().Add(encodeTokenCode(seed, units))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("fra1.bridge.keibidrop.com:26600", kd.logger)
	require.NotNil(t, ts, "no token session")

	// 6 units of total traffic -> half is 3, plus the lead of 2 -> 5 revealed.
	ts.sent.Store(3 * 2 * TokenUnitBytes)
	require.False(t, ts.postReveals(), "chain reported exhausted early")
	require.Equal(t, units-5, led.remaining)

	// Local wallet forgets its position (crash / second device): the loop
	// must resync from the ledger instead of dying on 422.
	ts.chain.Revealed = 1
	require.False(t, ts.postReveals(), "desync treated as exhaustion")
	require.Equal(t, 5, ts.chain.Revealed, "resync landed on the wrong offset")

	// Consumption beyond the chain: reveal everything, report exhausted.
	ts.sent.Store(int64(units) * 2 * 2 * TokenUnitBytes)
	require.True(t, ts.postReveals(), "spent chain not reported exhausted")
	require.Zero(t, led.remaining, "ledger remaining after exhaustion")
}

func TestTokenSessionGating(t *testing.T) {
	kd := newTokenTestKD(t, "")
	require.Nil(t, kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger), "empty wallet created a token session")

	seed := testSeed(t)
	_, err := kd.Wallet().Add(encodeTokenCode(seed, 10))
	require.NoError(t, err)
	require.Nil(t, kd.tokenSessionFor("my.selfhosted.example:26600", kd.logger),
		"non-KeibiSoft bridge must never see the paid preamble")

	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts, "funded + keibi bridge must fund the session")
	require.Same(t, ts, kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger),
		"token session not sticky within the room session")

	kd.resetTokenSession()
	require.Nil(t, kd.currentTokenSession(), "reset left a stale token session")
}

func TestNoteBusySignalThrottleAndFundedMute(t *testing.T) {
	kd := newTokenTestKD(t, "")
	var mu sync.Mutex
	var events []string
	kd.OnEvent = func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }

	kd.noteBusySignal(true, "custom copy")
	kd.noteBusySignal(true, "custom copy") // throttled
	mu.Lock()
	n := len(events)
	mu.Unlock()
	require.Equal(t, 1, n)
	require.Equal(t, "relay_busy:custom copy", events[0])

	// A funded wallet mutes the upsell entirely.
	kd2 := newTokenTestKD(t, "")
	kd2.OnEvent = func(e string) { t.Errorf("funded user nagged: %s", e) }
	_, err := kd2.Wallet().Add(encodeTokenCode(testSeed(t), 10))
	require.NoError(t, err)
	kd2.noteBusySignal(true, "x")

	st := kd2.BridgeInfo()
	require.NotZero(t, st.WalletGB, "BridgeInfo lost the wallet balance")
}

func TestCreditLevelEvents(t *testing.T) {
	kd := newTokenTestKD(t, "")
	var mu sync.Mutex
	var events []string
	kd.OnEvent = func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), events...)
		events = nil
		return out
	}

	c, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 6000)) // ~58 GB
	require.NoError(t, err)
	kd.noteCreditLevel()
	require.Empty(t, take(), "above thresholds must emit nothing")

	kd.Wallet().markRevealed(c, 1500, false) // 4500 units left, under the 50 GB mark
	kd.noteCreditLevel()
	kd.noteCreditLevel()
	got := take()
	require.Len(t, got, 1)
	require.True(t, strings.HasPrefix(got[0], "tokens_low:"), "want one tokens_low, got %v", got)

	kd.Wallet().markRevealed(c, 5900, false) // 100 units left, under the 2 GB mark
	kd.noteCreditLevel()
	kd.noteCreditLevel()
	got = take()
	require.Len(t, got, 1)
	require.True(t, strings.HasPrefix(got[0], "tokens_critical:"), "want one tokens_critical, got %v", got)

	big, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 25600))
	require.NoError(t, err)
	kd.noteCreditLevel() // top-up above the low mark re-arms silently
	require.Empty(t, take(), "top-up must emit nothing")

	kd.Wallet().markRevealed(c, 6000, true)
	kd.Wallet().markRevealed(big, 21000, false) // 4600 units left again
	kd.noteCreditLevel()
	got = take()
	require.Len(t, got, 1)
	require.True(t, strings.HasPrefix(got[0], "tokens_low:"), "re-armed low warning missing, got %v", got)
}

func TestClaimBuyFlowAddsCode(t *testing.T) {
	kd := newTokenTestKD(t, "")
	var mu sync.Mutex
	var events []string
	kd.OnEvent = func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }

	code := encodeTokenCode(testSeed(t), 1024)
	var hmu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collect" || r.URL.Query().Get("claim") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hmu.Lock()
		hits++
		n := hits
		hmu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusNotFound) // webhook not landed yet
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":%q,"units":1024}`, code)
	}))
	defer srv.Close()

	oldBase, oldTick, oldWindow := tokensServiceBase, claimPollTick, claimPollWindow
	tokensServiceBase, claimPollTick, claimPollWindow = srv.URL, 10*time.Millisecond, time.Second
	t.Cleanup(func() { tokensServiceBase, claimPollTick, claimPollWindow = oldBase, oldTick, oldWindow })

	url := kd.TokensBuyStart()
	require.Contains(t, url, "/buy?claim=")

	// Hand-rolled poll, not testkit.Poll/Eventually: the interval sleep is one
	// of the package's timing-sensitive waits, left as-is (see task report).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && kd.Wallet().unitsLeft() != 1024 {
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, 1024, kd.Wallet().unitsLeft(), "code never added by the claim poll")

	// TokensAdd lands before the emit; wait for the event separately.
	// Snapshot under the lock: the failure message must not read the live slice.
	found := false
	var got []string
	evDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(evDeadline) && !found {
		mu.Lock()
		got = append(got[:0], events...)
		mu.Unlock()
		for _, e := range got {
			if strings.HasPrefix(e, "tokens_added:") {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	require.True(t, found, "no tokens_added event, got %v", got)
}

func TestExhaustEventHonesty(t *testing.T) {
	kd := newTokenTestKD(t, "")
	require.Equal(t, "tokens_exhausted:chain", kd.exhaustEvent(), "empty wallet")

	_, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 100))
	require.NoError(t, err)
	require.Equal(t, "tokens_chain_done:next", kd.exhaustEvent(), "funded wallet must promise the next pack")
}

func balanceServer(t *testing.T, status int, body anchorBalanceBody) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anchor/balance" {
			t.Errorf("unexpected relay call %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokensAddRefusesSpentCode(t *testing.T) {
	srv := balanceServer(t, http.StatusOK, anchorBalanceBody{State: "spent"})
	kd := newTokenTestKD(t, srv.URL)
	_, err := kd.TokensAdd(encodeTokenCode(testSeed(t), 25600))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already spent")

	// The chain stays as a dead record, same as a balance refresh would
	// leave it: the code was real once and the wallet history says so.
	s := kd.TokensSummaries()
	require.Len(t, s, 1)
	require.True(t, s[0].Dead)
	require.Zero(t, s[0].UnitsLeft)
}

func TestTokensAddAdoptsLedgerRemaining(t *testing.T) {
	srv := balanceServer(t, http.StatusOK, anchorBalanceBody{UnitsRemaining: 12800, State: "active"})
	kd := newTokenTestKD(t, srv.URL)
	gb, err := kd.TokensAdd(encodeTokenCode(testSeed(t), 25600))
	require.NoError(t, err)
	require.Equal(t, float64(125), gb, "want the ledger's 125 GB, not the nominal size")

	s := kd.TokensSummaries()
	require.Len(t, s, 1)
	require.Equal(t, 12800, s[0].UnitsLeft)
	require.False(t, s[0].Dead)
}

func TestTokensAddRefusesUnknownAnchor(t *testing.T) {
	srv := balanceServer(t, http.StatusNotFound, anchorBalanceBody{Message: "unknown anchor"})
	kd := newTokenTestKD(t, srv.URL)
	_, err := kd.TokensAdd(encodeTokenCode(testSeed(t), 25600))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not on the relay ledger")
	require.Empty(t, kd.TokensSummaries(), "a never-minted chain must not stay in the wallet")
}

func TestTokensAddOfflineStaysOptimistic(t *testing.T) {
	// No relay configured at all.
	kd := newTokenTestKD(t, "")
	gb, err := kd.TokensAdd(encodeTokenCode(testSeed(t), 25600))
	require.NoError(t, err, "offline add must keep working")
	require.Equal(t, float64(250), gb)

	// An old relay without the ledger route answers a plain 404.
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	kd2 := newTokenTestKD(t, srv.URL)
	gb, err = kd2.TokensAdd(encodeTokenCode(testSeed(t), 25600))
	require.NoError(t, err, "route-miss add must keep working")
	require.Equal(t, float64(250), gb)
}

// A payConn must decrement the live-conn count exactly once, however many times it is
// closed. The reveal loop stops when the count reaches zero, so a double close used to
// end payment for the rest of the session while the transfer kept consuming bytes. The
// bridge then saw a chain that consumed and never paid, and demoted it to the free tier.
func TestPayConn_DoubleCloseDecrementsOnce(t *testing.T) {
	kd := newTokenTestKD(t, "")
	seed := testSeed(t)
	_, err := kd.Wallet().Add(encodeTokenCode(seed, 10))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts)
	t.Cleanup(kd.resetTokenSession)

	a, _ := net.Pipe()
	b, _ := net.Pipe()
	pcA, pcB := newPayConn(a, ts), newPayConn(b, ts)
	require.Equal(t, int32(2), ts.conns.Load(), "two live bridge conns")

	require.NoError(t, pcA.Close())
	_ = pcA.Close() // A second close must be a no-op for the counter.
	_ = pcA.Close()
	require.Equal(t, int32(1), ts.conns.Load(),
		"closing one conn three times must leave the other counted, or reveals stop early")

	require.NoError(t, pcB.Close())
	require.Equal(t, int32(0), ts.conns.Load())
}

// A failed reveal must be visible. It used to return silently, so a chain that paid
// nothing looked exactly like one that was merely behind.
func TestRevealHealth_ReportsFailures(t *testing.T) {
	kd := newTokenTestKD(t, "")
	seed := testSeed(t)
	_, err := kd.Wallet().Add(encodeTokenCode(seed, 10))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts)
	t.Cleanup(kd.resetTokenSession)

	require.Equal(t, int64(0), ts.revealFails.Load())
	ts.noteRevealResult(errors.New("relay unreachable"))
	ts.noteRevealResult(errors.New("relay unreachable"))

	h := kd.RevealHealth()
	require.NotNil(t, h, "a funded session must report reveal health")
	require.Equal(t, int64(2), h["failing"])
	require.Equal(t, "relay unreachable", h["last_error"])

	ts.noteRevealResult(nil)
	h = kd.RevealHealth()
	require.Equal(t, int64(0), h["failing"], "a success clears the failure streak")
	require.Equal(t, int64(1), h["accepted"])
	require.NotContains(t, h, "last_error")
}

// A chain that has already paid for earlier sessions must keep paying for new ones.
//
// The reveal target used to be computed from THIS session's bytes and compared against
// the chain's LIFETIME revealed count. On a chain with 166 units already revealed, a new
// session had to move about 3.4 GB on its own before it revealed a single unit, so every
// later session consumed bandwidth and posted nothing. The bridge saw a chain that
// consumed and never paid, and demoted it to the free tier: measured on the WAN pair as
// "delinquent (consumed 102 units, paid 0)".
func TestReveals_TargetIsRelativeToTheChainPosition(t *testing.T) {
	kd := newTokenTestKD(t, "")
	seed := testSeed(t)
	c, err := kd.Wallet().Add(encodeTokenCode(seed, 1024))
	require.NoError(t, err)

	// The chain has history, exactly like a wallet that has been used before.
	kd.Wallet().markRevealed(c, 166, false)

	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts)
	t.Cleanup(kd.resetTokenSession)
	require.Equal(t, 166, ts.baseRevealed, "the session must start from the chain's position")

	// A modest session: 200 MB observed, so it owes 100 MB = 10 units.
	ts.recv.Store(200 << 20)
	require.Equal(t, 10, ts.sessionUnits())

	target := ts.baseRevealed + ts.sessionUnits() + revealLead
	require.Equal(t, 178, target)
	require.Greater(t, target, c.Revealed,
		"a used chain must still owe reveals for a new session, or the bridge demotes it")

	// The old formula, kept here so the regression is unmistakable.
	oldTarget := ts.sessionUnits() + revealLead
	require.Less(t, oldTarget, c.Revealed,
		"the old target sat below the chain position, which is why nothing was ever revealed")
}
