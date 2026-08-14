// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
package common

// Prepaid relay tokens (PayWord hash chains). The wallet holds pasted codes;
// a funded session sends the anchor in the bridge preamble and a background
// loop reveals chain values to the signaling relay to cover HALF the bytes it
// observes (the peer covers the other half when it also pays; the bridge
// expects exactly this split). Nothing here ever touches the data path: the
// preamble is the same single write a legacy token was, the one ack byte is
// stripped transparently on the first read, and reveals ride their own HTTPS
// calls. A dry chain or dead relay degrades the session to the free class,
// never stalls or disconnects it.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
)

// TokenUnitBytes is the bandwidth one chain step buys. PROTOCOL CONSTANT
// shared with the relay ledger, the bridge and the token service.
const TokenUnitBytes int64 = 10 << 20

// TokensBuyURL is where money changes hands. Prices live on that page and in
// the token service's pack table, never in this binary.
const TokensBuyURL = "https://tokens.keibidrop.com/buy" //nolint:gosec // G101: URL, not a credential

const (
	walletFile     = ".kd_tokens"
	maxChainUnits  = 1_000_000
	revealTick     = 3 * time.Second
	revealLead     = 2  // units revealed ahead of consumption
	revealMaxBatch = 64 // relay caps steps per reveal call
)

// payMagic opens the funded bridge preamble: magic, version, anchor, tier,
// then the usual room token. Wire format shared with the bridge.
var payMagic = []byte("\x01KDPAY1")

const (
	ackPaidBit       = 1 << 0
	ackContentionBit = 1 << 1
)

// ---- chain math (PayWord; mirrors the token service's chain package) ----

func chainHashAt(seed [32]byte, position int) [32]byte {
	cur := seed
	for i := 0; i < position; i++ {
		cur = sha256.Sum256(cur[:])
	}
	return cur
}

// decodeTokenCode validates a pasted "KDT1." code (seed, units, checksum).
func decodeTokenCode(code string) (seed [32]byte, units int, err error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(code), "KDT1.")
	if !ok {
		return seed, 0, fmt.Errorf("not a KeibiDrop token code (expected \"KDT1.\" prefix)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) != 40 {
		return seed, 0, fmt.Errorf("malformed token code")
	}
	sum := sha256.Sum256(raw[:36])
	if !bytes.Equal(sum[:4], raw[36:]) {
		return seed, 0, fmt.Errorf("token code checksum mismatch (typo?)")
	}
	copy(seed[:], raw[:32])
	units = int(binary.BigEndian.Uint32(raw[32:36]))
	if units < 1 || units > maxChainUnits {
		return seed, 0, fmt.Errorf("token code units out of range")
	}
	return seed, units, nil
}

func encodeTokenCode(seed [32]byte, units int) string {
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], uint32(units)) // #nosec G115 -- units <= maxChainUnits
	sum := sha256.Sum256(append(seed[:], be[:]...))
	raw := make([]byte, 0, 40)
	raw = append(raw, seed[:]...)
	raw = append(raw, be[:]...)
	raw = append(raw, sum[:4]...)
	return "KDT1." + base64.RawURLEncoding.EncodeToString(raw)
}

// ---- wallet ----

type walletChain struct {
	Seed     string `json:"seed"` // base64url
	Units    int    `json:"units"`
	Revealed int    `json:"revealed"` // local view; the ledger is authoritative
	AddedAt  int64  `json:"added_at"`
	Dead     bool   `json:"dead"` // ledger rejected it permanently

	seed   [32]byte
	anchor [32]byte
}

type walletFileFormat struct {
	Version int            `json:"version"`
	Chains  []*walletChain `json:"chains"`
}

// TokenWallet is the on-disk stash of prepaid chains. Plaintext JSON at 0600:
// the seeds ARE cash by design, and the user was warned to back up the codes.
type TokenWallet struct {
	mu     sync.Mutex
	path   string
	chains []*walletChain
}

func openTokenWallet(dir string) (*TokenWallet, error) {
	if dir == "" {
		dir = config.ConfigDir()
	}
	w := &TokenWallet{path: filepath.Join(dir, walletFile)}
	data, err := os.ReadFile(w.path) // #nosec G304 -- own config dir
	if os.IsNotExist(err) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	var ff walletFileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		return nil, fmt.Errorf("wallet %s unreadable: %w", w.path, err)
	}
	for _, c := range ff.Chains {
		raw, err := base64.RawURLEncoding.DecodeString(c.Seed)
		if err != nil || len(raw) != 32 || c.Units < 1 || c.Units > maxChainUnits {
			continue // skip corrupt entries rather than losing the file
		}
		copy(c.seed[:], raw)
		c.anchor = chainHashAt(c.seed, c.Units)
		w.chains = append(w.chains, c)
	}
	return w, nil
}

func (w *TokenWallet) saveLocked() error {
	ff := walletFileFormat{Version: 1, Chains: w.chains}
	data, err := json.MarshalIndent(ff, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0750); err != nil {
		return err
	}
	return identity.WriteFileAtomic(w.path, data, 0600)
}

// Add validates a pasted code and stores its chain. Duplicate anchors merge.
func (w *TokenWallet) Add(code string) (*walletChain, error) {
	seed, units, err := decodeTokenCode(code)
	if err != nil {
		return nil, err
	}
	c := &walletChain{
		Seed:    base64.RawURLEncoding.EncodeToString(seed[:]),
		Units:   units,
		AddedAt: time.Now().Unix(),
		seed:    seed,
		anchor:  chainHashAt(seed, units),
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, have := range w.chains {
		if have.anchor == c.anchor {
			return have, nil // already pasted
		}
	}
	w.chains = append(w.chains, c)
	if err := w.saveLocked(); err != nil {
		w.chains = w.chains[:len(w.chains)-1]
		return nil, fmt.Errorf("save wallet: %w", err)
	}
	return c, nil
}

// pickFunded returns the oldest chain with value left, or nil.
func (w *TokenWallet) pickFunded() *walletChain {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.chains {
		if !c.Dead && c.Revealed < c.Units {
			return c
		}
	}
	return nil
}

func (w *TokenWallet) markRevealed(c *walletChain, revealed int, dead bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if revealed > c.Revealed {
		c.Revealed = revealed
	}
	if dead {
		c.Dead = true
	}
	_ = w.saveLocked() // best-effort; the ledger stays authoritative
}

// remove drops a chain the ledger refused at paste time.
func (w *TokenWallet) remove(c *walletChain) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, have := range w.chains {
		if have == c {
			w.chains = append(w.chains[:i], w.chains[i+1:]...)
			_ = w.saveLocked()
			return
		}
	}
}

// TokenChainSummary is what CLIs and UIs display.
type TokenChainSummary struct {
	Code      string  `json:"code"`
	GBTotal   float64 `json:"gb_total"`
	GBLeft    float64 `json:"gb_left"`
	UnitsLeft int     `json:"units_left"`
	Dead      bool    `json:"dead"`
	AddedAt   int64   `json:"added_at"`
}

func (w *TokenWallet) Summaries() []TokenChainSummary {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]TokenChainSummary, 0, len(w.chains))
	for _, c := range w.chains {
		left := c.Units - c.Revealed
		if left < 0 {
			left = 0
		}
		out = append(out, TokenChainSummary{
			Code:      encodeTokenCode(c.seed, c.Units),
			GBTotal:   float64(c.Units) * float64(TokenUnitBytes) / float64(1<<30),
			GBLeft:    float64(left) * float64(TokenUnitBytes) / float64(1<<30),
			UnitsLeft: left,
			Dead:      c.Dead,
			AddedAt:   c.AddedAt,
		})
	}
	return out
}

// Low-credit warning thresholds, in chain units (10 MiB each). One event per
// downward crossing; a top-up above the low mark re-arms both.
const (
	lowCreditUnits      = 5120 // 50 GiB
	criticalCreditUnits = 205  // ~2 GiB
)

// unitsLeft sums the spendable units across live chains.
func (w *TokenWallet) unitsLeft() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.chains {
		if !c.Dead && c.Revealed < c.Units {
			n += c.Units - c.Revealed
		}
	}
	return n
}

// ---- kd accessors ----

// Wallet lazily opens the machine-wide token wallet. Identity-independent by
// design: prepaid chains are bearer instruments, usable in incognito too.
func (kd *KeibiDrop) Wallet() *TokenWallet {
	kd.walletOnce.Do(func() {
		if kd.wallet != nil {
			return // injected (tests)
		}
		w, err := openTokenWallet("")
		if err != nil {
			kd.logger.Warn("Token wallet unavailable", "error", err)
			w = &TokenWallet{path: filepath.Join(config.ConfigDir(), walletFile)}
		}
		kd.wallet = w
	})
	return kd.wallet
}

// TokensAdd pastes a code into the wallet and returns its spendable size in
// GB. The ledger is asked once so a spent or never-minted code is refused at
// paste time instead of at first spend. No ledger answer keeps the add: the
// wallet is a cache and pasting offline must keep working.
func (kd *KeibiDrop) TokensAdd(code string) (float64, error) {
	c, err := kd.Wallet().Add(code)
	if err != nil {
		return 0, err
	}
	gb := float64(c.Units) * float64(TokenUnitBytes) / float64(1<<30)
	var resp struct {
		UnitsRemaining int    `json:"units_remaining"`
		State          string `json:"state"`
	}
	lerr := kd.postRelayJSON("anchor/balance", map[string]string{
		"anchor": base64.RawURLEncoding.EncodeToString(c.anchor[:]),
	}, &resp)
	var he *relayHTTPError
	switch {
	case lerr == nil && resp.UnitsRemaining <= 0:
		kd.Wallet().markRevealed(c, c.Units, resp.State == "spent")
		return 0, fmt.Errorf("this code is already spent")
	case lerr == nil:
		kd.Wallet().markRevealed(c, c.Units-resp.UnitsRemaining, false)
		gb = float64(resp.UnitsRemaining) * float64(TokenUnitBytes) / float64(1<<30)
	case errors.As(lerr, &he) && he.status == http.StatusNotFound && strings.Contains(he.body, "unknown anchor"):
		// The ledger answered: it has never seen this chain. A route miss
		// on an old or foreign relay says "Not Found" instead and lands in
		// the default case.
		kd.Wallet().remove(c)
		return 0, fmt.Errorf("this code is not on the relay ledger")
	}
	kd.noteCreditLevel()
	return gb, nil
}

// exhaustEvent picks the honest message for a dry chain: with another funded
// chain in the wallet the next connection pays again, so "ran out" would be
// wrong. Codes never overwrite each other; each is its own chain, oldest
// spends first.
func (kd *KeibiDrop) exhaustEvent() string {
	if kd.Wallet().pickFunded() != nil {
		return "tokens_chain_done:next"
	}
	return "tokens_exhausted:chain"
}

// noteCreditLevel surfaces the wallet's remaining credit: one event per
// downward crossing of each threshold, re-armed by any top-up that clears
// the low mark. The critical copy is honest: dry credit means the free
// tier, which is only slower under contention, never a cutoff.
func (kd *KeibiDrop) noteCreditLevel() {
	left := kd.Wallet().unitsLeft()
	gb := float64(left) * float64(TokenUnitBytes) / float64(1<<30)
	switch {
	case left >= lowCreditUnits:
		kd.creditLowNoted.Store(false)
		kd.creditCritNoted.Store(false)
	case left < criticalCreditUnits:
		kd.creditLowNoted.Store(true)
		if kd.creditCritNoted.CompareAndSwap(false, true) {
			kd.emitEvent(fmt.Sprintf("tokens_critical:%.1f", gb))
		}
	default:
		if kd.creditLowNoted.CompareAndSwap(false, true) {
			kd.emitEvent(fmt.Sprintf("tokens_low:%.0f", gb))
		}
	}
}

// CreditStatus is the level-readable relay credit state. Events are edges;
// agents and frontends poll this for the current level.
type CreditStatus struct {
	WalletGB float64 `json:"wallet_gb"`
	Level    string  `json:"level"` // "ok", "low", "critical", "empty"
	Notice   string  `json:"notice,omitempty"`
	BuyURL   string  `json:"buy_url"`
}

// TokensCreditStatus reports remaining relay credit against the same
// thresholds noteCreditLevel uses. The copy stays honest: dry credit means
// the free tier on bridged transfers, slower only under contention, never a
// cutoff, and direct connections are never affected.
func (kd *KeibiDrop) TokensCreditStatus() CreditStatus {
	left := kd.Wallet().unitsLeft()
	st := CreditStatus{
		WalletGB: float64(left) * float64(TokenUnitBytes) / float64(1<<30),
		BuyURL:   TokensBuyURL,
	}
	switch {
	case left == 0:
		st.Level = "empty"
		st.Notice = "No relay credit: bridged transfers ride the free tier, slower when the bridge is busy. Direct connections are unaffected. Top up to restore paid speed."
	case left < criticalCreditUnits:
		st.Level = "critical"
		st.Notice = fmt.Sprintf("Relay credit nearly gone (%.1f GB). At zero, bridged transfers drop to the free tier, slower when the bridge is busy. Top up now to avoid the slowdown.", st.WalletGB)
	case left < lowCreditUnits:
		st.Level = "low"
		st.Notice = fmt.Sprintf("Relay credit is getting low (%.0f GB left). Top up before it runs out to keep paid bridge speed.", st.WalletGB)
	default:
		st.Level = "ok"
	}
	return st
}

// TokensSummaries lists wallet chains for display.
func (kd *KeibiDrop) TokensSummaries() []TokenChainSummary {
	return kd.Wallet().Summaries()
}

// TokensRefreshBalances asks the relay ledger for each chain's remaining
// units and adopts its view (it is authoritative; the local count only lags).
func (kd *KeibiDrop) TokensRefreshBalances() []TokenChainSummary {
	w := kd.Wallet()
	w.mu.Lock()
	chains := append([]*walletChain(nil), w.chains...)
	w.mu.Unlock()
	for _, c := range chains {
		var resp struct {
			UnitsRemaining int    `json:"units_remaining"`
			State          string `json:"state"`
		}
		err := kd.postRelayJSON("anchor/balance", map[string]string{
			"anchor": base64.RawURLEncoding.EncodeToString(c.anchor[:]),
		}, &resp)
		if err != nil {
			continue // offline view is fine; the wallet is a cache
		}
		w.markRevealed(c, c.Units-resp.UnitsRemaining, resp.State == "spent" && resp.UnitsRemaining == 0)
	}
	kd.noteCreditLevel()
	return w.Summaries()
}

// ---- self-registering buy flow ----

// tokensServiceBase is the token service origin. A var so tests can point it
// at a local server; claimPollTick/Window shrink in tests too.
var (
	tokensServiceBase = "https://tokens.keibidrop.com" //nolint:gosec // G101: URL, not a credential
	claimPollTick     = 3 * time.Second
	claimPollWindow   = 30 * time.Minute
)

// TokensBuyStart returns the buy page URL to open in a browser, carrying a
// fresh claim ref, and starts a background poll that adds the purchased code
// to the wallet the moment the payment lands. No copy-paste, no account:
// the claim is a random one-shot ID, generated here, tied to nothing.
// Starting a new purchase replaces the previous poll.
func (kd *KeibiDrop) TokensBuyStart() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return TokensBuyURL // plain page; the paste path still works
	}
	claim := base64.RawURLEncoding.EncodeToString(b[:])
	stop := make(chan struct{})
	kd.mu.Lock()
	if kd.buyClaimStop != nil {
		close(kd.buyClaimStop)
	}
	kd.buyClaimStop = stop
	kd.mu.Unlock()
	go kd.claimPollLoop(claim, stop)
	return tokensServiceBase + "/buy?claim=" + claim
}

func (kd *KeibiDrop) claimPollLoop(claim string, stop chan struct{}) {
	client := kd.relayClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	deadline := time.Now().Add(claimPollWindow)
	t := time.NewTicker(claimPollTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if time.Now().After(deadline) {
				return
			}
			req, err := http.NewRequest(http.MethodGet, tokensServiceBase+"/collect?claim="+claim, nil)
			if err != nil {
				return
			}
			req.Header.Set("Accept", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				continue // offline is fine, the purchase waits server-side
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				continue // 404 until the webhook lands
			}
			var out struct {
				Code string `json:"code"`
			}
			err = json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&out)
			resp.Body.Close()
			if err != nil || out.Code == "" {
				continue
			}
			gb, err := kd.TokensAdd(out.Code)
			if err == nil {
				kd.emitEvent(fmt.Sprintf("tokens_added:%.0f", gb))
			}
			return // duplicate add means the user pasted it already; done either way
		}
	}
}

// ---- per-session payment state ----

// tokenSession covers one room session's bridge lanes: both TCP pairs and
// the UDP lane share the anchor, the byte counters and one reveal loop.
type tokenSession struct {
	kd     *KeibiDrop
	chain  *walletChain
	logger *slog.Logger

	sent, recv atomic.Int64
	conns      atomic.Int32
	everConn   atomic.Bool

	paid       atomic.Bool
	contention atomic.Bool

	startOnce sync.Once
	stop      chan struct{}
	stopOnce  sync.Once
}

// tokenSessionFor returns the session's payment state, creating it on the
// first bridge dial when the wallet holds value and the bridge is ours. Only
// KeibiSoft-operated bridges understand the preamble; a custom or self-hosted
// bridge always gets the legacy token.
func (kd *KeibiDrop) tokenSessionFor(addr string, logger *slog.Logger) *tokenSession {
	if !validBridgeHintAddr(addr) {
		return nil
	}
	kd.mu.Lock()
	defer kd.mu.Unlock()
	if kd.tokenSess != nil {
		return kd.tokenSess
	}
	c := kd.Wallet().pickFunded()
	if c == nil {
		return nil
	}
	ts := &tokenSession{kd: kd, chain: c, logger: logger, stop: make(chan struct{})}
	kd.tokenSess = ts
	logger.Info("Session funded by prepaid chain",
		"gb_left", float64(c.Units-c.Revealed)*float64(TokenUnitBytes)/float64(1<<30))
	return ts
}

// currentTokenSession returns the live payment state without creating one.
func (kd *KeibiDrop) currentTokenSession() *tokenSession {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	return kd.tokenSess
}

// resetTokenSession ends the previous room's payment state. Runs at room
// entry, next to the rest of the bridge policy reset.
func (kd *KeibiDrop) resetTokenSession() {
	kd.mu.Lock()
	ts := kd.tokenSess
	kd.tokenSess = nil
	kd.mu.Unlock()
	if ts != nil {
		ts.stopLoop()
	}
}

func (ts *tokenSession) stopLoop() {
	ts.stopOnce.Do(func() { close(ts.stop) })
}

func (ts *tokenSession) anchorBytes() [32]byte { return ts.chain.anchor }

// applyAck records the bridge's one ack byte. The paid bit starts the reveal
// loop; the contention bit is informational (the session already has
// priority, so no upsell fires here).
func (ts *tokenSession) applyAck(b byte) {
	ts.contention.Store(b&ackContentionBit != 0)
	if b&ackPaidBit != 0 {
		ts.paid.Store(true)
		ts.startOnce.Do(func() {
			gb := float64(ts.kd.Wallet().unitsLeft()) * float64(TokenUnitBytes) / float64(1<<30)
			ts.kd.emitEvent(fmt.Sprintf("tokens_in_use:%.0f", gb))
			go ts.revealLoop()
		})
	}
}

// revealLoop keeps the ledger's revealed frontier ahead of half the observed
// bytes. Runs beside the transfer, never in it: any failure just means the
// bridge's grace shrinks until the next successful post.
func (ts *tokenSession) revealLoop() {
	t := time.NewTicker(revealTick)
	defer t.Stop()
	for {
		select {
		case <-ts.stop:
			ts.postReveals() // cover the tail before letting go
			return
		case <-t.C:
			exhausted := ts.postReveals()
			ts.kd.noteCreditLevel()
			if exhausted {
				ts.kd.emitEvent(ts.kd.exhaustEvent())
				ts.logger.Info("Prepaid chain exhausted; session continues on the free tier")
				return
			}
			if ts.everConn.Load() && ts.conns.Load() == 0 {
				ts.postReveals()
				return
			}
		}
	}
}

// postReveals advances the ledger to the current target. Returns true when
// the chain has no value left.
func (ts *tokenSession) postReveals() (exhausted bool) {
	c := ts.chain
	total := ts.sent.Load() + ts.recv.Load()
	target := int((total/2)/TokenUnitBytes) + revealLead
	if target > c.Units {
		target = c.Units
	}
	for {
		ts.kd.Wallet().mu.Lock()
		revealed := c.Revealed
		dead := c.Dead
		ts.kd.Wallet().mu.Unlock()
		if dead {
			return true
		}
		if revealed >= c.Units {
			return true
		}
		if revealed >= target {
			return false
		}
		steps := target - revealed
		if steps > revealMaxBatch {
			steps = revealMaxBatch
		}
		reveal := chainHashAt(c.seed, c.Units-revealed-steps)
		var resp struct {
			UnitsRemaining int `json:"units_remaining"`
		}
		err := ts.kd.postRelayJSON("anchor/reveal", map[string]any{
			"anchor": base64.RawURLEncoding.EncodeToString(c.anchor[:]),
			"hash":   base64.RawURLEncoding.EncodeToString(reveal[:]),
			"steps":  steps,
		}, &resp)
		switch {
		case err == nil:
			ts.kd.Wallet().markRevealed(c, c.Units-resp.UnitsRemaining, resp.UnitsRemaining == 0)
		case isRevealDesync(err):
			if !ts.resyncFromLedger() {
				return true
			}
		default:
			return false // relay unreachable or chain inactive: retry next tick
		}
	}
}

// resyncFromLedger walks the chain against the ledger's last accepted hash,
// healing a stale local count (crash, or the same code pasted on two
// machines). Returns false when the chain cannot be used at all.
func (ts *tokenSession) resyncFromLedger() bool {
	c := ts.chain
	var resp struct {
		UnitsRemaining int    `json:"units_remaining"`
		LastHash       string `json:"last_hash"`
		State          string `json:"state"`
	}
	err := ts.kd.postRelayJSON("anchor/balance", map[string]string{
		"anchor": base64.RawURLEncoding.EncodeToString(c.anchor[:]),
	}, &resp)
	if err != nil {
		return false
	}
	last, err := base64.RawURLEncoding.DecodeString(resp.LastHash)
	if err != nil || len(last) != 32 {
		return false
	}
	for pos := c.Units; pos >= 0; pos-- {
		v := chainHashAt(c.seed, pos)
		if bytes.Equal(v[:], last) {
			ts.kd.Wallet().markRevealed(c, c.Units-pos, resp.State == "spent")
			ts.logger.Info("Chain position resynced from ledger", "revealed", c.Units-pos)
			return resp.State != "spent"
		}
	}
	// The ledger's hash is not on our chain: wrong wallet entry. Retire it.
	ts.kd.Wallet().markRevealed(c, c.Revealed, true)
	ts.logger.Warn("Chain disowned by ledger; marking dead")
	return false
}

// isRevealDesync detects the ledger refusing a reveal that no longer chains
// onto its last accepted value (HTTP 422 from the relay).
func isRevealDesync(err error) bool {
	return err != nil && strings.Contains(err.Error(), "422")
}

// ---- wire helpers ----

// buildPayPreamble assembles magic || ver || anchor || tier || token.
func buildPayPreamble(anchor [32]byte, token [32]byte) []byte {
	pre := make([]byte, 0, len(payMagic)+1+32+1+32)
	pre = append(pre, payMagic...)
	pre = append(pre, 1)
	pre = append(pre, anchor[:]...)
	pre = append(pre, 0) // tier: reserved
	pre = append(pre, token[:]...)
	return pre
}

// payConn wraps a funded bridge connection: it counts bytes for the reveal
// loop and strips the bridge's single ack byte on the first read, so no
// caller ever waits an extra round trip for it.
type payConn struct {
	net.Conn
	ts         *tokenSession
	ackPending bool
	mu         sync.Mutex
}

func newPayConn(conn net.Conn, ts *tokenSession) *payConn {
	ts.conns.Add(1)
	ts.everConn.Store(true)
	return &payConn{Conn: conn, ts: ts, ackPending: true}
}

func (p *payConn) Read(b []byte) (int, error) {
	p.mu.Lock()
	if p.ackPending {
		var ack [1]byte
		if _, err := io.ReadFull(p.Conn, ack[:]); err != nil {
			p.mu.Unlock()
			return 0, err
		}
		p.ackPending = false
		p.mu.Unlock()
		p.ts.applyAck(ack[0])
	} else {
		p.mu.Unlock()
	}
	n, err := p.Conn.Read(b)
	if n > 0 {
		p.ts.recv.Add(int64(n))
	}
	return n, err
}

func (p *payConn) Write(b []byte) (int, error) {
	n, err := p.Conn.Write(b)
	if n > 0 {
		p.ts.sent.Add(int64(n))
	}
	return n, err
}

func (p *payConn) Close() error {
	p.mu.Lock()
	closedBefore := p.ts == nil
	p.mu.Unlock()
	if !closedBefore {
		p.ts.conns.Add(-1)
	}
	return p.Conn.Close()
}

// relayHTTPError is a non-2xx relay answer. The body head rides along so a
// caller can tell an authoritative refusal from a plain route miss.
type relayHTTPError struct {
	sub    string
	status int
	body   string
}

func (e *relayHTTPError) Error() string {
	return fmt.Sprintf("relay %s: status %d", e.sub, e.status)
}

// postRelayJSON posts a JSON body to a public relay endpoint and decodes the
// answer. Non-2xx answers surface as errors carrying the status code.
func (kd *KeibiDrop) postRelayJSON(sub string, payload any, out any) error {
	if kd.RelayEndoint == nil || kd.relayClient == nil {
		return fmt.Errorf("relay not configured")
	}
	u, err := kd.relayURL(sub)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := kd.relayClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &relayHTTPError{sub: sub, status: resp.StatusCode, body: string(head)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out)
}

// emitEvent forwards a user-visible event when a frontend is listening.
func (kd *KeibiDrop) emitEvent(evt string) {
	if kd.OnEvent != nil {
		kd.OnEvent(evt)
	}
}

// noteBusySignal records relay-reported bridge load and surfaces it to
// unfunded users at most once per 10 minutes. Funded sessions already hold
// priority; nagging them converts nothing.
func (kd *KeibiDrop) noteBusySignal(busy bool, notice string) {
	kd.mu.Lock()
	kd.busyFlag = busy
	if busy && notice != "" {
		kd.busyNotice = notice
	}
	kd.mu.Unlock()
	if !busy || kd.Wallet().pickFunded() != nil {
		return
	}
	if notice == "" {
		notice = "Relay is busy right now - priority relay is available at " + TokensBuyURL
	}
	now := time.Now().Unix()
	last := kd.busyEventAt.Load()
	if now-last < 600 || !kd.busyEventAt.CompareAndSwap(last, now) {
		return
	}
	kd.emitEvent("relay_busy:" + notice)
}

// BridgeStatus is the relay-visibility surface: which bridge a session rides,
// whether it holds paid priority, and what the relay says about load. Shown
// by the CLI status, the kd daemon and the desktop UI.
type BridgeStatus struct {
	Addr       string  `json:"addr,omitempty"` // bridge host:port this session dials
	Via        string  `json:"via,omitempty"`  // display form: bridge hostname
	Paid       bool    `json:"paid"`           // bridge acked paid priority for this session
	Contention bool    `json:"contention"`     // bridge signaled contention in its ack
	Busy       bool    `json:"busy"`           // relay flagged the assigned bridge busy
	Notice     string  `json:"notice,omitempty"`
	WalletGB   float64 `json:"wallet_gb"`  // prepaid value left across chains
	SessionGB  float64 `json:"session_gb"` // bytes this session moved via the bridge, both directions
}

func (kd *KeibiDrop) BridgeInfo() BridgeStatus {
	var st BridgeStatus
	kd.mu.Lock()
	ts := kd.tokenSess
	st.Busy = kd.busyFlag
	st.Notice = kd.busyNotice
	kd.mu.Unlock()
	if kd.ConnectionMode == "bridge" {
		st.Addr = kd.effectiveBridgeAddr()
		if host, _, err := net.SplitHostPort(st.Addr); err == nil {
			st.Via = host
		} else {
			st.Via = st.Addr
		}
	}
	if ts != nil {
		st.Paid = ts.paid.Load()
		st.Contention = ts.contention.Load()
		st.SessionGB = float64(ts.sent.Load()+ts.recv.Load()) / float64(1<<30)
	}
	for _, s := range kd.Wallet().Summaries() {
		if !s.Dead {
			st.WalletGB += s.GBLeft
		}
	}
	return st
}
