// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/transport"
	cpb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The QUIC control channel mirrors the TCP pair on UDP: one outbound conn, one inbound
// conn from the peer, on the same port numbers. It carries control, metadata, and the
// network-change signal. Bulk transfer stays on TCP.
//
// There is no per-connection handshake. Keys come from the TCP handshake payload:
// SEKOutboundQUIC and SEKInboundQUIC, one per direction. Each stream wraps in a
// SecureConn with a direction-specific nonce prefix, so the directions never share
// nonce space. With in-band key-update negotiated, the QUIC conns ratchet like the
// TCP pair. The KEM fold also stages on these conns via quicFoldConns/ExtraFoldConns.
// A re-handshake re-derives fresh QUIC keys.

// DialQUICControl brings up the outbound QUIC control channel to peerUDPAddr. It wraps
// a migratable QUIC stream in the outbound SecureConn and returns the migration handle.
// It errors when the peer negotiated no QUIC key, so the caller falls back to TCP-only.
func DialQUICControl(ctx context.Context, s *session.Session, peerUDPAddr string) (net.Conn, *transport.MigratableConn, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("quic control: nil session")
	}
	key := s.SEKOutboundQUIC
	if len(key) == 0 {
		return nil, nil, fmt.Errorf("quic control: no outbound QUIC key (peer did not negotiate QUIC)")
	}
	mc, err := transport.DialQUICMigratable(ctx, peerUDPAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("quic control dial %s: %w", peerUDPAddr, err)
	}
	sc := session.NewSecureConn(mc, key, s.NegotiatedSuite(), session.NoncePrefixOutbound)
	// Opt into the negotiated ratchet before any I/O. The SessionSockets opt-in does
	// not reach these conns.
	sc.SetKeyUpdate(s.UseKeyUpdate())
	return sc, mc, nil
}

// ListenQUICControl starts the inbound QUIC control listener on udpAddr. It wraps each
// accepted stream in the inbound SecureConn and errors when the peer negotiated no QUIC key.
func ListenQUICControl(s *session.Session, udpAddr string) (net.Listener, error) {
	key, err := inboundQUICKey(s)
	if err != nil {
		return nil, err
	}
	ln, err := transport.QUIC().Listen(udpAddr)
	if err != nil {
		return nil, fmt.Errorf("quic control listen %s: %w", udpAddr, err)
	}
	return newQUICControlListener(s, ln, key), nil
}

// inboundQUICKey returns the session's inbound QUIC key. It errors when the peer
// negotiated no QUIC lane, so no listener binds for a channel that cannot key.
func inboundQUICKey(s *session.Session) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("quic control: nil session")
	}
	if len(s.SEKInboundQUIC) == 0 {
		return nil, fmt.Errorf("quic control: no inbound QUIC key (peer did not negotiate QUIC)")
	}
	return s.SEKInboundQUIC, nil
}

func newQUICControlListener(s *session.Session, ln net.Listener, key []byte) net.Listener {
	return &quicControlListener{Listener: ln, key: key, suite: s.NegotiatedSuite(), keyUpdate: s.UseKeyUpdate()}
}

// quicControlListener upgrades every accepted QUIC stream to the inbound control
// SecureConn. keyUpdate is the negotiated ratchet capability, snapshotted at listen time.
type quicControlListener struct {
	net.Listener
	key       []byte
	suite     kbc.CipherSuite
	keyUpdate bool
	onAccept  func()                // Fold trigger on conn-up. Set before Serve.
	onInbound func(remote net.Addr) // Inbound-half visibility. Set before Serve.

	accepts atomic.Uint64 // Conns accepted by THIS listener. Zero means the peer never reached it.
	live    atomic.Int64  // Accepted conns not yet closed. Zero means no peer is on the inbound half.

	mu   sync.Mutex
	last *session.SecureConn // Most recent accepted conn: epoch observability and fold staging.
}

func (l *quicControlListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// The inbound half had no signal of its own before. A lane can be up outbound and
	// dead inbound, which is how the two peers disagreed on 2026-08-16.
	l.accepts.Add(1)
	l.live.Add(1)
	if l.onInbound != nil {
		l.onInbound(c.RemoteAddr())
	}
	// laneConn goes UNDER the SecureConn, so Accept still returns a *session.SecureConn
	// and SecureConn.Close, which closes what it wraps, reports the death for us.
	sc := session.NewSecureConn(&laneConn{Conn: c, closed: &l.live}, l.key, l.suite, session.NoncePrefixInbound)
	sc.SetKeyUpdate(l.keyUpdate)
	l.mu.Lock()
	l.last = sc
	l.mu.Unlock()
	// Signal the fold trigger so this new wire gets fresh entropy too. Arrivals coalesce
	// into one deferred round, so a signal per accepted conn is cheap and safe.
	if l.onAccept != nil {
		l.onAccept()
	}
	return sc, nil
}

// laneConn reports the inbound conn's death back to the listener. Without it the listener
// cannot tell a healthy quiet lane from a room whose peer has gone, and only one of those
// needs re-registering.
type laneConn struct {
	net.Conn
	once   sync.Once
	closed *atomic.Int64
}

func (c *laneConn) Close() error {
	c.once.Do(func() { c.closed.Add(-1) })
	return c.Conn.Close()
}

// LastAccepted returns the most recently accepted server-side SecureConn, or nil.
func (l *quicControlListener) LastAccepted() *session.SecureConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// relayAddrPrefix marks a quicPeerAddr as a relay room, not a peer UDP endpoint.
// The shared dial path then registers with the bridge instead of dialing the peer.
const relayAddrPrefix = "relay:"

// quicDialTimeout bounds one dial attempt of the control channel.
const quicDialTimeout = 5 * time.Second

// relayQUICRooms returns this peer's (dial, listen) rooms, direction-tagged like the
// TCP bridge's pair1/pair2. The fingerprint election hands each peer the opposite
// pair, so one peer's dialer always meets the other's listener.
func relayQUICRooms(s *session.Session) (dial, listen string) {
	if s.IsFoldInitiator() {
		return "quic1", "quic2"
	}
	return "quic2", "quic1"
}

// listenQUICControlRelay registers the inbound room and serves QUIC on the paired socket.
// rs owns the socket across attempts, exactly as on the dial side.
func (kd *KeibiDrop) listenQUICControlRelay(ctx context.Context, s *session.Session, room string, rs *roomSocket, logger *slog.Logger) (net.Listener, error) {
	key, err := inboundQUICKey(s)
	if err != nil {
		return nil, err
	}
	udp, _, err := rs.pair(ctx, s, logger.With("half", "listen"))
	if err != nil {
		return nil, err
	}
	ln, err := transport.ListenOnConn(udp)
	if err != nil {
		rs.poison()
		return nil, fmt.Errorf("quic control relay listen %s: %w", room, err)
	}
	rs.release() // The QUIC listener owns the socket now.
	return newQUICControlListener(s, ln, key), nil
}

// dialQUICControlRelay brings up the outbound channel through rs's UDP room. The relay
// path yields no migration handle. rs owns the socket across attempts, so a failed
// handshake poisons it and the next attempt rebinds; a failed registration keeps it.
func (kd *KeibiDrop) dialQUICControlRelay(ctx context.Context, s *session.Session, room string, rs *roomSocket, logger *slog.Logger) (net.Conn, error) {
	key := s.SEKOutboundQUIC
	if len(key) == 0 {
		return nil, fmt.Errorf("quic control: no outbound QUIC key (peer did not negotiate QUIC)")
	}
	udp, relayAddr, err := rs.pair(ctx, s, logger.With("half", "dial"))
	if err != nil {
		return nil, err
	}
	conn, err := transport.DialOnConn(ctx, udp, relayAddr)
	if err != nil {
		rs.poison() // The room paired but the far end is dead. Never reuse this pairing.
		return nil, fmt.Errorf("quic control relay dial %s: %w", room, err)
	}
	rs.release() // QUIC owns the socket now.
	sc := session.NewSecureConn(conn, key, s.NegotiatedSuite(), session.NoncePrefixOutbound)
	sc.SetKeyUpdate(s.UseKeyUpdate())
	return sc, nil
}

// startQUICControlRelay brings the lane up through the relay's UDP rooms. It runs in its
// own goroutine: registration blocks until the peer registers and must not delay connect.
func (kd *KeibiDrop) startQUICControlRelay(ctx context.Context, gen uint64, s *session.Session, dialRoom, listenRoom string) {
	logger := kd.logger.With("component", "quic-control", "gen", gen)

	// Start the dial half first, concurrent with the listen half. The peer's dial room
	// pairs the listen room, so listen-first deadlocks both peers until the budget expires.
	if len(s.SEKOutboundQUIC) != 0 {
		peerAddr := relayAddrPrefix + dialRoom
		kd.mu.Lock()
		current := kd.ctx == ctx
		if current {
			kd.quicPeerAddr = peerAddr // Generation marker for the maintainer's self-heal redial.
		}
		kd.mu.Unlock()
		if current {
			go kd.ensureQUICMaintainer(ctx, gen, peerAddr)
		}
	}

	kd.maintainQUICListen(ctx, gen, s, listenRoom, logger)
}

// maintainQUICListen keeps the inbound room usable for the whole generation.
//
// It replaces a single listen attempt, which could not survive its own success. A relay
// room is CONSUMED by its first pairing: once our listen socket is paired to the peer's
// dial socket, the peer's next dial attempt finds no waiter and parks forever, because
// nothing on our side ever registers the room again. That is why the lane on the WAN pair
// worked once, carried 1.6 MB, and then never came back for the rest of the session.
//
// So the loop watches for a listener that pairs but never accepts. Zero accepts after a
// re-probe interval means the room is paired to a socket the peer has abandoned, and the
// only way to rebuild it is to rebind and register again.
func (kd *KeibiDrop) maintainQUICListen(ctx context.Context, gen uint64, s *session.Session, room string, logger *slog.Logger) {
	logger = logger.With("half", "listen", "room", room)
	rs := kd.newRoomSocket(room)
	defer rs.close()
	quiet := 0 // Consecutive checks that found no live inbound conn.

	for {
		kd.mu.Lock()
		current := kd.ctx == ctx
		kd.mu.Unlock()
		if !current || ctx.Err() != nil {
			return
		}

		ln, err := kd.listenQUICControlRelay(ctx, s, room, rs, logger)
		if err != nil {
			// No ack only means the peer has not registered yet. Re-register at once, so
			// the waiter stays fresh and the room pairs the moment the peer arrives.
			// Backing off here left the socket registered but unserved, and the peer
			// paired with it and lost a whole cycle to a QUIC handshake against nothing.
			if errors.Is(err, errNoPairingAck) {
				continue
			}
			logger.Warn("QUIC relay listen failed; retrying, staying TCP-only for now",
				"error", err, "every", quicReprobeInterval)
			select {
			case <-time.After(quicReprobeInterval):
				continue
			case <-ctx.Done():
				return
			}
		}

		qln, _ := ln.(*quicControlListener)
		if qln != nil {
			qln.onAccept = kd.maybeStartEagerFoldSoon // Set before Serve starts accepting.
			qln.onInbound = kd.inboundQUICObserver(gen, room)
		}
		kd.mu.Lock()
		superseded := kd.ctx != ctx // A reconnect swapped the generation during registration.
		if !superseded {
			old := kd.quicControlLn
			oldSrv := kd.quicControlServer
			kd.quicControlLn = ln
			kd.quicControlServer = nil
			kd.mu.Unlock()
			if oldSrv != nil {
				oldSrv.Stop() // Retires the previous serve goroutine.
			}
			if old != nil {
				_ = old.Close()
			}
		} else {
			kd.mu.Unlock()
			_ = ln.Close()
			return
		}
		go kd.serveQUICControl(ln, relayAddrPrefix+room)

		select {
		case <-time.After(quicReprobeInterval):
		case <-ctx.Done():
			return
		}
		if qln == nil {
			return
		}
		kd.mu.Lock()
		stillOurs := kd.quicControlLn == ln && kd.ctx == ctx
		kd.mu.Unlock()
		if !stillOurs {
			return
		}
		if qln.live.Load() > 0 {
			quiet = 0
			continue // A peer is on the inbound half. Keep this room and check again later.
		}
		// No live inbound conn. Either the peer never arrived, or it arrived and left.
		// Both mean the room may be paired to a socket nobody is using, and the peer's
		// dial attempts would park against it forever.
		//
		// Wait for a SECOND quiet observation before rebuilding. A healthy lane can go
		// briefly connectionless when the inbound conn hits its 20s QUIC idle timeout
		// between bursts, and rebinding on that single sample churned the room every
		// re-probe on an idle session.
		quiet++
		if quiet < quietRoundsBeforeRebind && qln.accepts.Load() > 0 {
			continue
		}
		quiet = 0
		logger.Warn("inbound QUIC room has no live peer; rebinding so the peer can pair again",
			"accepts", qln.accepts.Load(), "after", quicReprobeInterval)
		rs.close() // Registering from a paired socket only re-acks. A fresh one can pair.
	}
}

// StartQUICControlChannel brings up the QUIC control pair alongside the TCP session:
// an inbound listener on the local UDP port and a background outbound dial to the peer.
// Any failure logs and leaves the session TCP-only. The dial never delays connect.
func (kd *KeibiDrop) StartQUICControlChannel() {
	// Reconnect re-runs this function, so release any prior instance first.
	kd.StopQUICControlChannel()

	// One generation per bring-up. Every lane log line carries it, so a stale cycle's
	// lines can never be read as the current one's.
	gen := kd.quicGen.Add(1)
	logger := kd.logger.With("component", "quic-control", "gen", gen)
	// Snapshot session and ctx under kd.mu. Teardown nils kd.session under the lock.
	kd.mu.Lock()
	s := kd.session
	ctx := kd.ctx // This generation's context. A reconnect swaps kd.ctx for a fresh one.
	kd.mu.Unlock()
	if s == nil || len(s.SEKInboundQUIC) == 0 {
		logger.Info("peer did not negotiate a QUIC channel; staying TCP-only")
		return
	}

	// Bridge mode has no direct path, so a local bind and a peer dial can never meet.
	// KEIBIDROP_NO_QUIC_RELAY=1 keeps bridge sessions TCP-only, for A/B against the lane.
	if kd.ConnectionMode == "bridge" && kd.BridgeAddr != "" && os.Getenv("KEIBIDROP_NO_QUIC_RELAY") != "1" {
		dialRoom, listenRoom := relayQUICRooms(s)
		logger.Info("Bridge mode: bringing the QUIC lane up over the relay",
			"relay", kd.BridgeAddr, "dial", dialRoom, "listen", listenRoom)
		go kd.startQUICControlRelay(ctx, gen, s, dialRoom, listenRoom)
		return
	}

	// Inbound listener on the local UDP port. The gRPC server attaches in a goroutine
	// with a bounded wait for kd.KDSvc: gRPC forbids service registration after Serve.
	inAddr := net.JoinHostPort("::", strconv.Itoa(kd.inboundPort))
	ln, err := ListenQUICControl(s, inAddr)
	if err != nil {
		logger.Warn("QUIC control listen failed; staying TCP-only", "addr", inAddr, "error", err)
	} else {
		if qln, ok := ln.(*quicControlListener); ok {
			qln.onAccept = kd.maybeStartEagerFoldSoon // Set before Serve starts accepting.
			qln.onInbound = kd.inboundQUICObserver(gen, "direct")
		}
		kd.mu.Lock()
		kd.quicControlLn = ln
		kd.mu.Unlock()
		go kd.serveQUICControl(ln, inAddr)
	}

	// Dial the peer's UDP control endpoint in the background, so connect never waits.
	if kd.PeerIPv6IP == "" || len(s.SEKOutboundQUIC) == 0 {
		return
	}
	peerAddr := net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(s.PeerPort))
	kd.mu.Lock()
	kd.quicPeerAddr = peerAddr // Generation marker for the maintainer's self-heal redial.
	kd.mu.Unlock()
	go kd.ensureQUICMaintainer(ctx, gen, peerAddr)
}

// quicReprobeInterval sets how often a session with QUIC down re-probes UDP. The probe
// is cheap, so the fast lane returns by itself once the network allows.
const quicReprobeInterval = 30 * time.Second

// maintainQUICControl re-establishes the outbound channel: a dial burst, then a
// periodic re-probe while down. It exits when up, on generation change, or when ctx
// ends. Single-flight via kd.quicRedialing.
func (kd *KeibiDrop) maintainQUICControl(ctx context.Context, gen uint64, peerAddr string) {
	defer kd.quicRedialing.Store(false)
	logger := kd.logger.With("component", "quic-control", "gen", gen, "half", "dial")
	for first := true; ; first = false {
		kd.mu.Lock()
		cur := kd.quicPeerAddr
		up := kd.quicControlClient != nil
		kd.mu.Unlock()
		if ctx.Err() != nil || cur != peerAddr || up {
			return
		}
		ok, lastErr := kd.dialQUICControlWithRetry(ctx, gen, peerAddr)
		if ok {
			return
		}
		kd.setQUICDialErr(lastErr)
		if first {
			// In bridge mode the lane is the product's latency feature and there is no
			// direct path to fall back to, so its loss is an operator signal, not a note.
			if kd.ConnectionMode == "bridge" {
				logger.Warn("QUIC lane down in bridge mode; on-demand reads fall back to TCP",
					"peer", peerAddr, "every", quicReprobeInterval, "error", lastErr)
			} else {
				logger.Info("QUIC channel unavailable; staying on TCP, re-probing periodically",
					"peer", peerAddr, "every", quicReprobeInterval, "error", lastErr)
			}
		}
		select {
		case <-time.After(quicReprobeInterval):
		case <-ctx.Done():
			return
		}
	}
}

// quietRoundsBeforeRebind is how many consecutive checks must find no live inbound
// conn before the listen room is rebuilt. One is too eager: a healthy lane goes
// connectionless whenever the inbound conn hits its 20s QUIC idle timeout.
const quietRoundsBeforeRebind = 2

// quicArmRetryInterval sets how often arming retries while a stale maintainer holds the
// single-flight flag. It is short because the window it covers is a lost dial half.
const quicArmRetryInterval = 1 * time.Second

// ensureQUICMaintainer arms the dial maintainer for this generation and keeps trying.
//
// A maintainer from a PREVIOUS generation can still hold the single-flight flag while it
// sleeps out its 30s re-probe. One CAS attempt therefore left the new generation with no
// dial half at all, for the whole session: the old maintainer wakes, sees the address
// changed, exits and clears the flag, and nobody re-arms.
func (kd *KeibiDrop) ensureQUICMaintainer(ctx context.Context, gen uint64, peerAddr string) {
	if !kd.quicArming.CompareAndSwap(false, true) {
		return // Another arming loop is already waiting for the flag.
	}
	defer kd.quicArming.Store(false)
	for {
		kd.mu.Lock()
		current := kd.ctx == ctx && kd.quicPeerAddr == peerAddr
		up := kd.quicControlClient != nil
		kd.mu.Unlock()
		if !current || up || ctx.Err() != nil {
			return
		}
		if kd.quicRedialing.CompareAndSwap(false, true) {
			kd.maintainQUICControl(ctx, gen, peerAddr) // Clears the flag on exit.
			return
		}
		select {
		case <-time.After(quicArmRetryInterval):
		case <-ctx.Done():
			return
		}
	}
}

// quicVerifyTimeout bounds the control Ping that proves the peer is really there. It must
// outlast the peer's own server start: serveQUICControl waits up to 5s for the service
// impl before it serves anything. A shorter bound rejects healthy peers.
const quicVerifyTimeout = 12 * time.Second

// verifyQUICPeer sends one control Ping over cc before the lane is published. A relay
// room can pair with a dead socket, so a completed QUIC handshake alone proves nothing
// about WHO answered. Only an answered Ping does.
//
// WaitForReady keeps the call queued while the peer's server attaches, instead of failing
// the instant the connection is not ready yet.
func verifyQUICPeer(ctx context.Context, cc *grpc.ClientConn) error {
	vctx, cancel := context.WithTimeout(ctx, quicVerifyTimeout)
	defer cancel()
	if _, err := cpb.NewControlClient(cc).Ping(vctx, &cpb.PingRequest{}, grpc.WaitForReady(true)); err != nil {
		return fmt.Errorf("quic control verification ping: %w", err)
	}
	return nil
}

// dialQUICControlWithRetry dials the peer's QUIC control endpoint with retries,
// because the peer's listener may not be up yet. It reports whether the channel came up
// and, when it did not, the last failure so the caller can name a reason.
func (kd *KeibiDrop) dialQUICControlWithRetry(ctx context.Context, gen uint64, peerAddr string) (bool, error) {
	logger := kd.logger.With("component", "quic-control", "gen", gen, "half", "dial")
	var lastErr error
	// One socket for every attempt of this burst. Binding per attempt is what let the
	// client pair with its own corpse at the bridge.
	var rs *roomSocket
	if room, relayed := strings.CutPrefix(peerAddr, relayAddrPrefix); relayed {
		rs = kd.newRoomSocket(room)
		defer rs.close()
	}
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Read under kd.mu. Teardown can nil kd.session concurrently.
		kd.mu.Lock()
		s := kd.session
		kd.mu.Unlock()
		if s == nil {
			return false, ErrInvalidSession
		}
		// A relay room registers with the bridge and yields no migration handle. A direct
		// endpoint dials the peer. Everything downstream is the same either way.
		room, relayed := strings.CutPrefix(peerAddr, relayAddrPrefix)
		timeout := quicDialTimeout
		if relayed {
			timeout += udpRelayBudget // The room also waits for the peer to register.
		}
		dctx, cancel := context.WithTimeout(ctx, timeout)
		var (
			conn net.Conn
			mc   *transport.MigratableConn
			err  error
		)
		if relayed {
			conn, err = kd.dialQUICControlRelay(dctx, s, room, rs, logger.With("attempt", attempt))
		} else {
			conn, mc, err = DialQUICControl(dctx, s, peerAddr)
		}
		cancel()
		if err == nil {
			cc, cErr := grpc.NewClient("passthrough:///quic-control",
				append([]grpc.DialOption{
					grpc.WithTransportCredentials(insecure.NewCredentials()),
					grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return conn, nil }),
				}, kdDialOptions()...)...,
			)
			if cErr != nil {
				logger.Warn("QUIC control client build failed; staying TCP-only", "error", cErr)
				_ = conn.Close()
				if mc != nil { // Nil on the relay path: not migratable.
					_ = mc.Close()
				}
				return false, cErr
			}
			logger.Debug("outbound QUIC transport handshake complete (unverified)",
				"attempt", attempt, "peer", peerAddr)
			// A relay room pairs on address, not identity, so the handshake can complete
			// against a stale socket. Verify the PEER answers before publishing the lane.
			t0 := time.Now()
			if vErr := verifyQUICPeer(ctx, cc); vErr != nil {
				logger.Debug("QUIC control verification failed; treating as a failed attempt",
					"attempt", attempt, "error", vErr)
				lastErr = vErr
				_ = cc.Close()
				_ = conn.Close()
				if mc != nil { // Nil on the relay path: not migratable.
					_ = mc.Close()
				}
				select {
				case <-time.After(1 * time.Second):
				case <-ctx.Done():
					return false, ctx.Err()
				}
				continue
			}
			rtt := time.Since(t0)
			kd.mu.Lock()
			if kd.quicPeerAddr != peerAddr { // Channel stopped or superseded during the dial.
				kd.mu.Unlock()
				_ = cc.Close()
				if mc != nil { // Nil on the relay path: not migratable.
					_ = mc.Close()
				}
				return false, nil
			}
			hbCtx, hbCancel := context.WithCancel(ctx)
			old := kd.quicControlClient
			oldMig := kd.quicControlMig
			oldHB := kd.quicHBCancel
			kd.quicControlClient = cc
			kd.quicControlMig = mc
			kd.quicHBCancel = hbCancel
			kd.quicControlSC, _ = conn.(*session.SecureConn) // Epoch observability handle.
			kd.quicLastDialErr = ""
			kd.mu.Unlock()
			if oldHB != nil {
				oldHB() // The superseded heartbeat exits now.
			}
			if old != nil {
				_ = old.Close() // Exactly one live client.
			}
			if oldMig != nil {
				_ = oldMig.Close()
			}
			// verify_ms is the round trip PLUS whatever the peer's server took to attach,
			// so it is a bring-up cost, not a link RTT. Do not read it as latency.
			logger.Info("QUIC control lane verified", "peer", peerAddr,
				"attempt", attempt, "verify_ms", rtt.Milliseconds())
			// The control-Ping heartbeat owns QUIC-lane liveness. Metadata rides TCP first.
			go kd.quicControlHeartbeat(hbCtx, cc)
			kd.maybeStartEagerFoldSoon() // New wire: one coalesced fold round covers it.
			return true, nil
		}
		lastErr = err
		logger.Debug("QUIC control dial attempt failed", "attempt", attempt, "error", err)
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	// The caller, maintainQUICControl, logs the stay-on-TCP state once and re-probes.
	logger.Debug("QUIC control dial burst exhausted", "peer", peerAddr, "error", lastErr)
	return false, lastErr
}

// serveQUICControl attaches the control Ping plus the full KeibiService to ln.
// Another goroutine creates kd.KDSvc, so wait for it with a bound. On timeout,
// serve Control-only and let the peer fall back to TCP.
func (kd *KeibiDrop) serveQUICControl(ln net.Listener, inAddr string) {
	logger := kd.logger.With("component", "quic-control")

	var svc *service.KeibidropServiceImpl
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kd.mu.Lock()
		svc = kd.KDSvc
		current := kd.quicControlLn == ln
		kd.mu.Unlock()
		if !current {
			return // A newer generation owns the port now.
		}
		if svc != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	srv := grpc.NewServer(kdServerOptions()...)
	cpb.RegisterControlServer(srv, quicControlService{kd: kd})
	if svc != nil {
		bindings.RegisterKeibiServiceServer(srv, svc)
	} else {
		logger.Warn("KeibiService impl not ready in time; QUIC serves control Ping only (peer falls back to TCP)")
	}

	kd.mu.Lock()
	if kd.quicControlLn != ln { // stopped or restarted while we waited
		kd.mu.Unlock()
		srv.Stop()
		return
	}
	kd.quicControlServer = srv
	kd.mu.Unlock()

	logger.Info("QUIC control channel listening", "addr", inAddr, "keibiservice", svc != nil)
	if serveErr := srv.Serve(ln); serveErr != nil {
		logger.Debug("QUIC control server exited", "error", serveErr)
	}
}

// StopQUICControlChannel tears down the QUIC control pair. It is safe when the pair
// never came up. The listener close also retires a waiting serveQUICControl goroutine.
func (kd *KeibiDrop) StopQUICControlChannel() {
	kd.mu.Lock()
	cc := kd.quicControlClient
	srv := kd.quicControlServer
	ln := kd.quicControlLn
	mc := kd.quicControlMig
	hb := kd.quicHBCancel
	kd.quicHBCancel = nil
	kd.quicControlClient = nil
	kd.quicControlServer = nil
	kd.quicControlLn = nil
	kd.quicControlMig = nil
	kd.quicControlSC = nil
	kd.quicPeerAddr = "" // A later demote redial has nowhere to dial.
	kd.mu.Unlock()
	if hb != nil {
		hb()
	}
	if cc != nil {
		_ = cc.Close()
	}
	if srv != nil {
		srv.Stop()
	}
	if ln != nil {
		_ = ln.Close() // Releases the UDP port.
	}
	if mc != nil {
		_ = mc.Close() // Releases the outbound sockets.
	}
}

// MigrateQUICControl moves the live QUIC control connection to a fresh local UDP socket
// without a drop: QUIC keys the connection on connection ID, not the 5-tuple. It pings
// after, so the peer migrates its send path. On error the channel demotes and the
// maintainer re-establishes.
func (kd *KeibiDrop) MigrateQUICControl(ctx context.Context) error {
	kd.mu.Lock()
	mc := kd.quicControlMig
	kd.mu.Unlock()
	if mc == nil {
		return fmt.Errorf("quic control: no migratable channel")
	}
	oldAddr, newAddr, err := mc.Migrate(ctx)
	if err != nil {
		kd.demoteQUICControl(err)
		return fmt.Errorf("quic control migrate: %w", err)
	}
	// Send from the new path so the peer switches its send direction over.
	if err := kd.PingQUICControl(ctx); err != nil {
		kd.demoteQUICControl(err)
		return fmt.Errorf("quic control post-migration ping: %w", err)
	}

	// Announce the new coordinates over the surviving channel. The peer then re-dials the
	// fresh address at once, with no heartbeat timeout or relay lookup. Same-address
	// announces are a no-op. Best-effort: the peer still recovers via the heartbeat.
	kd.mu.Lock()
	cc := kd.quicControlClient
	kd.mu.Unlock()
	if cc != nil && kd.LocalIPv6IP != "" {
		actx, cancel := context.WithTimeout(ctx, quicMetaTimeout)
		_, aErr := cpb.NewControlClient(cc).Announce(actx, &cpb.AnnounceRequest{
			Ip:      kd.LocalIPv6IP,
			TcpPort: uint32(kd.inboundPort), //nolint:gosec // G115: port validated in 26000-27000
		})
		cancel()
		if aErr != nil {
			kd.logger.Warn("post-migration announce failed; peer will recover via heartbeat", "error", aErr)
		}
	}

	kd.logger.Info("QUIC control channel migrated", "old", oldAddr, "new", newAddr)
	return nil
}

// QUICKeibiClient returns a KeibiService client over the QUIC control channel, or nil
// when it is down. The channel is transport-isolated from TCP bulk, so metadata never
// queues behind a prefetch.
func (kd *KeibiDrop) QUICKeibiClient() bindings.KeibiServiceClient {
	kd.mu.Lock()
	cc := kd.quicControlClient
	kd.mu.Unlock()
	if cc == nil {
		return nil
	}
	return bindings.NewKeibiServiceClient(cc)
}

// QUICMetadataSent reports how many metadata RPCs rode the QUIC channel.
func (kd *KeibiDrop) QUICMetadataSent() uint64 { return kd.quicMetaSent.Load() }

// quicFoldConns returns the live QUIC control SecureConns, dial-side plus last accepted,
// so a fold round can stage its secret on them. Wired as session.ExtraFoldConns.
// An empty result means the fold covers TCP alone.
func (kd *KeibiDrop) quicFoldConns() []*session.SecureConn {
	kd.mu.Lock()
	sc := kd.quicControlSC
	ln, _ := kd.quicControlLn.(*quicControlListener)
	kd.mu.Unlock()
	conns := make([]*session.SecureConn, 0, 2)
	if sc != nil {
		conns = append(conns, sc)
	}
	if ln != nil {
		if a := ln.LastAccepted(); a != nil {
			conns = append(conns, a)
		}
	}
	return conns
}

// QUICWriterEpoch returns the highest writer key-epoch across the live QUIC control
// conns, or 0 when the channel is down. The QUIC lane ratchets independently of the
// TCP pair, so rekey observability must read both lanes.
func (kd *KeibiDrop) QUICWriterEpoch() uint16 {
	kd.mu.Lock()
	sc := kd.quicControlSC
	ln, _ := kd.quicControlLn.(*quicControlListener)
	kd.mu.Unlock()
	var e uint16
	if sc != nil {
		e = sc.WriterEpoch()
	}
	if ln != nil {
		if a := ln.LastAccepted(); a != nil && a.WriterEpoch() > e {
			e = a.WriterEpoch()
		}
	}
	return e
}

// quicMetaTimeout bounds a metadata RPC's QUIC attempt. A half-dead channel would
// otherwise stall until the QUIC idle timeout of ~20s. The worst case is one short
// stall, then demoteQUICControl routes everything to TCP.
const quicMetaTimeout = 2 * time.Second

// demoteQUICControl drops the outbound QUIC client, so later RPCs skip the
// quicMetaTimeout cost and go straight to TCP. A single-flight background redial
// heals transient failures. The inbound server stays up throughout.
func (kd *KeibiDrop) demoteQUICControl(err error) {
	kd.mu.Lock()
	cc := kd.quicControlClient
	mc := kd.quicControlMig
	hb := kd.quicHBCancel
	kd.quicHBCancel = nil
	kd.quicControlClient = nil
	kd.quicControlMig = nil
	kd.quicControlSC = nil
	peerAddr := kd.quicPeerAddr
	ctx := kd.ctx
	kd.mu.Unlock()
	if hb != nil {
		hb()
	}
	if cc == nil {
		return // Already demoted by a concurrent failure.
	}
	kd.logger.Warn("QUIC control lane failed; demoting, redialing in background", "error", err)
	_ = cc.Close()
	if mc != nil {
		_ = mc.Close()
	}

	if peerAddr == "" || ctx == nil || ctx.Err() != nil {
		return
	}
	go kd.ensureQUICMaintainer(ctx, kd.quicGen.Load(), peerAddr)
}

// metadataSend routes metadata TCP-first with QUIC as a bounded fallback. Notify
// bursts must ride TCP, so they never congest the scarce QUIC lane that cache-miss
// seeks and control depend on. The fallback lets a notify survive a TCP outage that
// QUIC outlives, such as an IP change.
func metadataSend[Req, Resp any](kd *KeibiDrop, ctx context.Context, req Req,
	call func(bindings.KeibiServiceClient, context.Context, Req) (Resp, error),
) (Resp, error) {
	// Snapshot under kd.mu. Teardown rewrites kd.session under the lock.
	kd.mu.Lock()
	s := kd.session
	kd.mu.Unlock()

	tcpErr := error(ErrInvalidSession)
	if s != nil && s.GRPCClient != nil {
		resp, err := call(s.GRPCClient, ctx, req)
		if err == nil {
			return resp, nil
		}
		tcpErr = err
	}
	// TCP failed. The QUIC fallback is bounded, so a dead lane cannot hang the notify.
	if qc := kd.QUICKeibiClient(); qc != nil {
		qctx, cancel := context.WithTimeout(ctx, quicMetaTimeout)
		resp, err := call(qc, qctx, req)
		cancel()
		if err == nil {
			kd.quicMetaSent.Add(1)
			return resp, nil
		}
	}
	var zero Resp
	return zero, tcpErr
}

// sendNotify and sendBatchNotify are the metadata RPCs. Each names its call.
func (kd *KeibiDrop) sendNotify(ctx context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	return metadataSend(kd, ctx, req, func(c bindings.KeibiServiceClient, ctx context.Context, r *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
		return c.Notify(ctx, r)
	})
}

func (kd *KeibiDrop) sendBatchNotify(ctx context.Context, req *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
	return metadataSend(kd, ctx, req, func(c bindings.KeibiServiceClient, ctx context.Context, r *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
		return c.BatchNotify(ctx, r)
	})
}

// QUICControlConnected reports whether the outbound QUIC control channel is up and
// VERIFIED: the client is published only after a control Ping the peer answered.
func (kd *KeibiDrop) QUICControlConnected() bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	return kd.quicControlClient != nil
}

// QUICInboundAccepted reports how many inbound QUIC control conns this peer accepted.
// The outbound half can be up while this stays 0, which is a one-directional lane.
func (kd *KeibiDrop) QUICInboundAccepted() uint64 { return kd.quicAccepted.Load() }

// inboundQUICObserver returns the accept-side hook: count and log. The inbound half had
// no signal of its own, so a lane that was up one way looked fully up.
func (kd *KeibiDrop) inboundQUICObserver(gen uint64, room string) func(net.Addr) {
	return func(remote net.Addr) {
		kd.logger.Info("QUIC control inbound accepted", "component", "quic-control",
			"gen", gen, "half", "listen", "room", room, "remote", remote,
			"total", kd.quicAccepted.Add(1))
	}
}

// QUICLaneStatus reports the outbound lane as "up", or "down (<reason>)". kd status and
// peer-info show it, so a silently dead lane is visible without reading DEBUG logs.
func (kd *KeibiDrop) QUICLaneStatus() string {
	kd.mu.Lock()
	up := kd.quicControlClient != nil
	reason := kd.quicLastDialErr
	kd.mu.Unlock()
	if up {
		return "up"
	}
	if reason == "" {
		return "down"
	}
	return "down (" + reason + ")"
}

// setQUICDialErr records why the outbound lane is down, for QUICLaneStatus.
func (kd *KeibiDrop) setQUICDialErr(err error) {
	kd.mu.Lock()
	if err == nil {
		kd.quicLastDialErr = ""
	} else {
		kd.quicLastDialErr = err.Error()
	}
	kd.mu.Unlock()
}

// quicHeartbeatInterval sets how often the QUIC lane pings to detect a dead channel.
// Metadata is TCP-first, so this heartbeat is the lane's own liveness check.
const quicHeartbeatInterval = 5 * time.Second

// quicControlHeartbeat pings the QUIC control lane and demotes it on failure, which
// kicks the self-heal maintainer. One runs per channel generation. It exits when a
// newer generation supersedes cc or when ctx ends.
func (kd *KeibiDrop) quicControlHeartbeat(ctx context.Context, cc *grpc.ClientConn) {
	t := time.NewTicker(quicHeartbeatInterval)
	defer t.Stop()
	client := cpb.NewControlClient(cc)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			kd.mu.Lock()
			current := kd.quicControlClient == cc
			kd.mu.Unlock()
			if !current {
				return // A newer generation has its own heartbeat.
			}
			pctx, cancel := context.WithTimeout(ctx, quicMetaTimeout)
			_, err := client.Ping(pctx, &cpb.PingRequest{})
			cancel()
			if err != nil {
				kd.demoteQUICControl(err) // Closes cc and kicks the self-heal maintainer.
				return
			}
		}
	}
}

// PingQUICControl sends one control Ping over the QUIC channel, or errors if it is down.
func (kd *KeibiDrop) PingQUICControl(ctx context.Context) error {
	kd.mu.Lock()
	cc := kd.quicControlClient
	kd.mu.Unlock()
	if cc == nil {
		return fmt.Errorf("quic control channel not connected")
	}
	_, err := cpb.NewControlClient(cc).Ping(ctx, &cpb.PingRequest{})
	return err
}

// quicControlService answers the control channel's own RPCs: Ping and Announce.
type quicControlService struct {
	cpb.UnimplementedControlServer
	kd *KeibiDrop
}

func (quicControlService) Ping(context.Context, *cpb.PingRequest) (*cpb.PingReply, error) {
	return &cpb.PingReply{}, nil
}

// Announce receives the network-change signal: the peer moved. It refreshes the cached
// coordinates and, on an address change, kicks reconnection now instead of after ~25s
// of heartbeat failures plus a relay lookup. Same-address is a no-op.
func (s quicControlService) Announce(_ context.Context, req *cpb.AnnounceRequest) (*cpb.AnnounceReply, error) {
	kd := s.kd
	if kd == nil || req.Ip == "" || req.TcpPort == 0 {
		return &cpb.AnnounceReply{}, nil
	}
	port := int(req.TcpPort)
	// Read under kd.mu. Teardown nils kd.session under the lock.
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	changed := kd.PeerIPv6IP != req.Ip || (sess != nil && sess.PeerPort != port)
	if !changed {
		return &cpb.AnnounceReply{}, nil
	}

	kd.logger.Info("peer announced new address over QUIC control", "ip", req.Ip, "tcp-port", port)
	kd.PeerIPv6IP = req.Ip
	if kd.ReconnectManager != nil {
		kd.ReconnectManager.CachedPeerIP = req.Ip
		kd.ReconnectManager.CachedPeerPort = port
	}
	kd.mu.Lock()
	kd.quicPeerAddr = net.JoinHostPort(req.Ip, strconv.Itoa(port)) // The maintainer redials the new address.
	kd.mu.Unlock()

	// The old TCP pair is dead. Reconnect with the fresh coordinates. onDisconnect
	// guards re-entrancy.
	if kd.ReconnectManager != nil {
		go kd.onDisconnect()
	}
	return &cpb.AnnounceReply{}, nil
}
