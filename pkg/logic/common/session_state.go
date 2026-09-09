// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"fmt"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// SessionState is the one line every surface shows about the link. kd status,
// the kdmcp kd_status tool and the desktop app read the same struct, so a
// person at a terminal, an agent and a person at the window get the same
// words. State is stable for scripts; Text is for people.
type SessionState struct {
	// State is one of idle, waiting_for_peer, connected, reconnecting,
	// gave_up, mount_gone, mount_failed.
	State string `json:"state"`
	Text  string `json:"state_text"`
	// Mode is lan, direct, bridge, direct-out (our reads direct, the return leg on
	// the relay) or direct-in (the peer's reads direct); empty before a session.
	Mode string `json:"mode"`
	// Paid reports a funded relay lane. False on a free lane and off the bridge.
	Paid bool `json:"paid"`
	// Throttled reports a free relay lane that held a reader for seconds this
	// session. A data pack lifts it. Never true on a paid or direct session.
	Throttled bool `json:"throttled"`
	// DiskLow reports the save folder's disk under the free-space floor at the
	// last fetch. Reads that need bytes from the peer fail until space is freed.
	DiskLow bool `json:"disk_low"`
	// MountReady reports the FUSE folder mounted and served. False with FUSE off.
	MountReady  bool `json:"mount_ready"`
	Attempt     int  `json:"attempt"`
	MaxAttempts int  `json:"max_attempts"`
	// Bytes per second over the last sample interval; see StartThroughputSampler.
	RecvBps uint64 `json:"recv_bps"`
	SentBps uint64 `json:"sent_bps"`
}

// Session states, as scripts see them.
const (
	StateIdle           = "idle"
	StateWaitingForPeer = "waiting_for_peer"
	StateConnected      = "connected"
	StateReconnecting   = "reconnecting"
	StateGaveUp         = "gave_up"
	StateMountGone      = "mount_gone"
	StateMountFailed    = "mount_failed"
)

// SessionState derives the state from what already exists: the run flag, the
// health monitor, the reconnect manager, the connect wait and the mount. It
// never blocks on the network.
func (kd *KeibiDrop) SessionState() SessionState {
	kd.mu.Lock()
	rm := kd.ReconnectManager
	sess := kd.session
	kd.mu.Unlock()

	st := SessionState{Mode: kd.ConnectionMode, DiskLow: kd.diskLow.Load()}
	st.SentBps, st.RecvBps = kd.throughput()
	if rm != nil {
		st.Attempt = rm.Attempts()
		st.MaxAttempts = rm.MaxAttempts
	}
	running := kd.IsRunning()
	if running {
		st.Paid = kd.BridgeInfo().Paid
		st.Throttled = st.Mode == "bridge" && !st.Paid && kd.relayThrottled.Load()
		// IsMounted lags an eject by the time the host takes to return (10 s
		// measured), so ask the OS as well.
		st.MountReady = kd.IsFUSE && kd.FS != nil && kd.FS.IsMounted() && MountIsLive(kd.ToMount)
	}
	health := kd.ConnectionStatus()
	reconnect := kd.ReconnectionState()
	peer := kd.peerLabel(sess)

	switch {
	case reconnect == session.ReconnectStateReconnecting.String(),
		reconnect == session.ReconnectStateWaitingPeer.String():
		st.State = StateReconnecting
		st.Text = fmt.Sprintf("%s, reconnecting (attempt %d of %d)", awayLabel(peer), max(st.Attempt, 1), st.MaxAttempts)
	case reconnect == session.ReconnectStateGaveUp.String():
		if kd.autoConnectArmed.Load() {
			st.State = StateWaitingForPeer
			st.Text = awayLabel(peer) + ", will keep trying"
		} else {
			st.State = StateGaveUp
			if peer != "" {
				st.Text = "Gave up reaching " + peer + ". Connect again from the menu"
			} else {
				st.Text = "Gave up reconnecting. Connect again from the menu"
			}
		}
	case running && health == session.HealthDisconnected.String():
		st.State = StateReconnecting
		st.Text = "Connection lost, reconnecting"
	case running:
		switch {
		case kd.IsFUSE && !st.MountReady && kd.mountFailedReason() != "":
			st.State = StateMountFailed
			st.Text = "Connected, but the folder could not be remounted"
		case kd.IsFUSE && !st.MountReady:
			st.State = StateMountGone
			st.Text = "Connected, but the folder is not mounted. Remounting"
		default:
			st.State = StateConnected
			st.Text = connectedText(st.Mode, st.Paid, health == session.HealthDegraded.String(), st.Throttled)
		}
	case kd.connectStatusText() != "":
		st.State = StateWaitingForPeer
		st.Text = waitingText(kd.connectStatusText(), peer)
	default:
		st.State = StateIdle
		st.Text = "Not connected"
	}
	if st.DiskLow {
		st.Text += ", save disk almost full"
	}
	return st
}

// awayLabel names the absent peer for a person: "Nas is away" or "Peer away".
func awayLabel(peer string) string {
	if peer != "" {
		return peer + " is away"
	}
	return "Peer away"
}

// connectedText names the lane. A shared free lane says so before a slow
// link does: the person can act on the first, only wait out the second.
func connectedText(mode string, paid, degraded, throttled bool) string {
	var text string
	switch mode {
	case "lan":
		text = "Connected on the local network"
	case "direct":
		text = "Connected directly"
	case "bridge":
		if paid {
			text = "Connected via relay, paid lane"
		} else {
			text = "Connected via relay, free lane"
		}
	case ModeDirectOut:
		text = "Connected directly for reads, relay for the rest"
		if paid {
			text += ", paid lane"
		}
	case ModeDirectIn:
		text = "Connected, peer reads directly, relay for the rest"
		if paid {
			text += ", paid lane"
		}
	default:
		text = "Connected"
	}
	switch {
	case throttled:
		text += ", shared right now"
	case degraded:
		text += ", slow link"
	}
	return text
}

func waitingText(status, peer string) string {
	other := "the other side"
	if peer != "" {
		other = peer
	}
	if status == "peer_not_ready" {
		return other + " has not pressed Connect yet"
	}
	return "Waiting for " + other
}

// peerLabel returns the saved name of the expected peer, the auto-connect
// target, or "" when nothing names it.
func (kd *KeibiDrop) peerLabel(sess *session.Session) string {
	fp := ""
	if sess != nil {
		fp = sess.ExpectedPeerFingerprint
	}
	if fp != "" && fp != "TOFU" && kd.AddressBook != nil {
		if c := kd.AddressBook.Lookup(fp); c != nil && c.Name != "" {
			return c.Name
		}
	}
	return kd.AutoConnectPeer
}

// emitConnectStatus records the connect wait text and pushes it as an event.
func (kd *KeibiDrop) emitConnectStatus(s string) {
	kd.connectStatus.Store(s)
	if kd.OnEvent != nil {
		kd.OnEvent("connect_status:" + s)
	}
}

func (kd *KeibiDrop) clearConnectStatus() { kd.connectStatus.Store("") }

func (kd *KeibiDrop) connectStatusText() string {
	v, _ := kd.connectStatus.Load().(string)
	return v
}

// AutoConnectArmed reports whether StartAutoConnect runs its watchdog.
func (kd *KeibiDrop) AutoConnectArmed() bool { return kd.autoConnectArmed.Load() }

// throughput returns bytes per second since the previous call, when that call
// was between half a second and fifteen seconds ago; otherwise the last rate,
// or zero after a long gap. StartThroughputSampler keeps the window at one
// second for the daemons; a bare caller gets the rate since its own last call.
func (kd *KeibiDrop) throughput() (sentBps, recvBps uint64) {
	sent, recv := kd.WireStatsTotal()
	now := time.Now()
	kd.tpMu.Lock()
	defer kd.tpMu.Unlock()
	if kd.tpAt.IsZero() {
		kd.tpAt, kd.tpSent, kd.tpRecv = now, sent, recv
		return 0, 0
	}
	elapsed := now.Sub(kd.tpAt)
	if elapsed < 500*time.Millisecond {
		return kd.tpRateSent, kd.tpRateRecv
	}
	if elapsed > 15*time.Second {
		kd.tpRateSent, kd.tpRateRecv = 0, 0
	} else {
		kd.tpRateSent = bytesPerSecond(sent, kd.tpSent, elapsed)
		kd.tpRateRecv = bytesPerSecond(recv, kd.tpRecv, elapsed)
	}
	kd.tpAt, kd.tpSent, kd.tpRecv = now, sent, recv
	return kd.tpRateSent, kd.tpRateRecv
}

func bytesPerSecond(now, prev uint64, elapsed time.Duration) uint64 {
	if now < prev || elapsed <= 0 {
		return 0
	}
	return uint64(float64(now-prev) / elapsed.Seconds())
}

// StartThroughputSampler samples the wire counters once a second until ctx
// ends, so SessionState reports the rate over the last second. The daemons
// call it once at start; tests and short-lived callers need not.
func (kd *KeibiDrop) StartThroughputSampler(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				kd.throughput()
			}
		}
	}()
}
