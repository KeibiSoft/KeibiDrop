// ABOUTME: Tests for the prepaid token wallet, the paid bridge conn wrapper
// ABOUTME: (ack strip + byte counting), the reveal loop math, and busy events.
package common

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSeed(t *testing.T) [32]byte {
	t.Helper()
	var s [32]byte
	if _, err := rand.Read(s[:]); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTokenTestKD(t *testing.T, relayURL string) *KeibiDrop {
	t.Helper()
	kd := &KeibiDrop{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		relayClient: &http.Client{Timeout: 2 * time.Second},
	}
	if relayURL != "" {
		u, err := url.Parse(relayURL)
		if err != nil {
			t.Fatal(err)
		}
		kd.RelayEndoint = u
	}
	w, err := openTokenWallet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kd.wallet = w
	return kd
}

func TestTokenCodeRoundtripClient(t *testing.T) {
	seed := testSeed(t)
	code := encodeTokenCode(seed, 25600)
	got, units, err := decodeTokenCode(" " + code + "\n")
	if err != nil || got != seed || units != 25600 {
		t.Fatalf("roundtrip: units=%d err=%v", units, err)
	}
	broken := []byte(code)
	broken[len(broken)-1] ^= 1
	if _, _, err := decodeTokenCode(string(broken)); err == nil {
		t.Fatal("corrupted code accepted")
	}
	if _, _, err := decodeTokenCode("KDT2.whatever"); err == nil {
		t.Fatal("wrong prefix accepted")
	}
}

func TestWalletAddPersistAndPerms(t *testing.T) {
	dir := t.TempDir()
	w, err := openTokenWallet(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed := testSeed(t)
	code := encodeTokenCode(seed, 100)

	c, err := w.Add(code)
	if err != nil {
		t.Fatal(err)
	}
	if c.Units != 100 || c.anchor != chainHashAt(seed, 100) {
		t.Fatalf("chain wrong: %+v", c)
	}
	if dup, err := w.Add(code); err != nil || dup != c {
		t.Fatal("duplicate paste must merge, not double-add")
	}

	info, err := os.Stat(filepath.Join(dir, walletFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("wallet mode %o, want 0600", info.Mode().Perm())
	}

	w.markRevealed(c, 40, false)
	w2, err := openTokenWallet(dir)
	if err != nil {
		t.Fatal(err)
	}
	sums := w2.Summaries()
	if len(sums) != 1 || sums[0].UnitsLeft != 60 || sums[0].Code != code {
		t.Fatalf("persisted summary wrong: %+v", sums)
	}
	if w2.pickFunded() == nil {
		t.Fatal("60 units left but no funded pick")
	}
	w2.markRevealed(w2.pickFunded(), 100, false)
	if w2.pickFunded() != nil {
		t.Fatal("exhausted chain still picked")
	}
}

func TestPayConnAckStripAndCounting(t *testing.T) {
	kd := newTokenTestKD(t, "")
	seed := testSeed(t)
	if _, err := kd.Wallet().Add(encodeTokenCode(seed, 10)); err != nil {
		t.Fatal(err)
	}
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	if ts == nil {
		t.Fatal("funded wallet on a keibi bridge must create a token session")
	}
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
	if err != nil || string(buf[:n]) != "peer-bytes" {
		t.Fatalf("ack byte leaked into the stream: %q err=%v", buf[:n], err)
	}
	if !ts.paid.Load() || !ts.contention.Load() {
		t.Fatal("ack bits not applied")
	}
	if ts.recv.Load() != int64(len("peer-bytes")) {
		t.Fatalf("recv counted %d, want %d (ack must not count)", ts.recv.Load(), len("peer-bytes"))
	}
	go func() { _, _ = io.Copy(io.Discard, server) }()
	if _, err := pc.Write([]byte("up")); err != nil {
		t.Fatal(err)
	}
	if ts.sent.Load() != 2 {
		t.Fatalf("sent counted %d, want 2", ts.sent.Load())
	}
	_ = pc.Close()
	if ts.conns.Load() != 0 {
		t.Fatalf("conn count %d after close", ts.conns.Load())
	}
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"units_remaining": m.remaining,
			"last_hash":       base64.RawURLEncoding.EncodeToString(m.last[:]),
			"state":           state,
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
	if _, err := kd.Wallet().Add(encodeTokenCode(seed, units)); err != nil {
		t.Fatal(err)
	}
	ts := kd.tokenSessionFor("fra1.bridge.keibidrop.com:26600", kd.logger)
	if ts == nil {
		t.Fatal("no token session")
	}

	// 6 units of total traffic -> half is 3, plus the lead of 2 -> 5 revealed.
	ts.sent.Store(3 * 2 * TokenUnitBytes)
	if exhausted := ts.postReveals(); exhausted {
		t.Fatal("chain reported exhausted early")
	}
	if led.remaining != units-5 {
		t.Fatalf("ledger remaining %d, want %d", led.remaining, units-5)
	}

	// Local wallet forgets its position (crash / second device): the loop
	// must resync from the ledger instead of dying on 422.
	ts.chain.Revealed = 1
	if exhausted := ts.postReveals(); exhausted {
		t.Fatal("desync treated as exhaustion")
	}
	if ts.chain.Revealed != 5 {
		t.Fatalf("resync landed on %d, want 5", ts.chain.Revealed)
	}

	// Consumption beyond the chain: reveal everything, report exhausted.
	ts.sent.Store(int64(units) * 2 * 2 * TokenUnitBytes)
	if exhausted := ts.postReveals(); !exhausted {
		t.Fatal("spent chain not reported exhausted")
	}
	if led.remaining != 0 {
		t.Fatalf("ledger remaining %d after exhaustion", led.remaining)
	}
}

func TestTokenSessionGating(t *testing.T) {
	kd := newTokenTestKD(t, "")
	if ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger); ts != nil {
		t.Fatal("empty wallet created a token session")
	}
	seed := testSeed(t)
	if _, err := kd.Wallet().Add(encodeTokenCode(seed, 10)); err != nil {
		t.Fatal(err)
	}
	if ts := kd.tokenSessionFor("my.selfhosted.example:26600", kd.logger); ts != nil {
		t.Fatal("non-KeibiSoft bridge must never see the paid preamble")
	}
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	if ts == nil {
		t.Fatal("funded + keibi bridge must fund the session")
	}
	if again := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger); again != ts {
		t.Fatal("token session not sticky within the room session")
	}
	kd.resetTokenSession()
	if kd.currentTokenSession() != nil {
		t.Fatal("reset left a stale token session")
	}
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
	if n != 1 || events[0] != "relay_busy:custom copy" {
		t.Fatalf("busy events: %v", events)
	}

	// A funded wallet mutes the upsell entirely.
	kd2 := newTokenTestKD(t, "")
	kd2.OnEvent = func(e string) { t.Errorf("funded user nagged: %s", e) }
	if _, err := kd2.Wallet().Add(encodeTokenCode(testSeed(t), 10)); err != nil {
		t.Fatal(err)
	}
	kd2.noteBusySignal(true, "x")

	st := kd2.BridgeInfo()
	if st.WalletGB == 0 {
		t.Fatal("BridgeInfo lost the wallet balance")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	kd.noteCreditLevel()
	if got := take(); len(got) != 0 {
		t.Fatalf("above thresholds emitted %v", got)
	}

	kd.Wallet().markRevealed(c, 1500, false) // 4500 units left, under the 50 GB mark
	kd.noteCreditLevel()
	kd.noteCreditLevel()
	if got := take(); len(got) != 1 || !strings.HasPrefix(got[0], "tokens_low:") {
		t.Fatalf("want one tokens_low, got %v", got)
	}

	kd.Wallet().markRevealed(c, 5900, false) // 100 units left, under the 2 GB mark
	kd.noteCreditLevel()
	kd.noteCreditLevel()
	if got := take(); len(got) != 1 || !strings.HasPrefix(got[0], "tokens_critical:") {
		t.Fatalf("want one tokens_critical, got %v", got)
	}

	big, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 25600))
	if err != nil {
		t.Fatal(err)
	}
	kd.noteCreditLevel() // top-up above the low mark re-arms silently
	if got := take(); len(got) != 0 {
		t.Fatalf("top-up emitted %v", got)
	}

	kd.Wallet().markRevealed(c, 6000, true)
	kd.Wallet().markRevealed(big, 21000, false) // 4600 units left again
	kd.noteCreditLevel()
	if got := take(); len(got) != 1 || !strings.HasPrefix(got[0], "tokens_low:") {
		t.Fatalf("re-armed low warning missing, got %v", got)
	}
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
	if !strings.Contains(url, "/buy?claim=") {
		t.Fatalf("buy url missing claim: %s", url)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && kd.Wallet().unitsLeft() != 1024 {
		time.Sleep(10 * time.Millisecond)
	}
	if kd.Wallet().unitsLeft() != 1024 {
		t.Fatal("code never added by the claim poll")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if strings.HasPrefix(e, "tokens_added:") {
			return
		}
	}
	t.Fatalf("no tokens_added event, got %v", events)
}

func TestExhaustEventHonesty(t *testing.T) {
	kd := newTokenTestKD(t, "")
	if e := kd.exhaustEvent(); e != "tokens_exhausted:chain" {
		t.Fatalf("empty wallet: %s", e)
	}
	if _, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 100)); err != nil {
		t.Fatal(err)
	}
	if e := kd.exhaustEvent(); e != "tokens_chain_done:next" {
		t.Fatalf("funded wallet must promise the next pack: %s", e)
	}
}
