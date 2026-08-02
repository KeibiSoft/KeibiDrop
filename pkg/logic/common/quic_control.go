// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
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
	onAccept  func() // Fold trigger on conn-up. Set before Serve.

	mu   sync.Mutex
	last *session.SecureConn // Most recent accepted conn: epoch observability and fold staging.
}

func (l *quicControlListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := session.NewSecureConn(c, l.key, l.suite, session.NoncePrefixInbound)
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
func (kd *KeibiDrop) listenQUICControlRelay(ctx context.Context, s *session.Session, room string) (net.Listener, error) {
	key, err := inboundQUICKey(s)
	if err != nil {
		return nil, err
	}
	udp, _, err := kd.dialUDPRelayDir(ctx, s, room, kd.logger.With("component", "quic-control"))
	if err != nil {
		return nil, err
	}
	ln, err := transport.ListenOnConn(udp)
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("quic control relay listen %s: %w", room, err)
	}
	return newQUICControlListener(s, ln, key), nil
}

// dialQUICControlRelay brings up the outbound channel through the relay's UDP room.
// The relay path yields no migration handle.
func (kd *KeibiDrop) dialQUICControlRelay(ctx context.Context, s *session.Session, room string) (net.Conn, error) {
	key := s.SEKOutboundQUIC
	if len(key) == 0 {
		return nil, fmt.Errorf("quic control: no outbound QUIC key (peer did not negotiate QUIC)")
	}
	udp, relayAddr, err := kd.dialUDPRelayDir(ctx, s, room, kd.logger.With("component", "quic-control"))
	if err != nil {
		return nil, err
	}
	conn, err := transport.DialOnConn(ctx, udp, relayAddr)
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("quic control relay dial %s: %w", room, err)
	}
	sc := session.NewSecureConn(conn, key, s.NegotiatedSuite(), session.NoncePrefixOutbound)
	sc.SetKeyUpdate(s.UseKeyUpdate())
	return sc, nil
}

// startQUICControlRelay brings the lane up through the relay's UDP rooms. It runs in its
// own goroutine: registration blocks until the peer registers and must not delay connect.
func (kd *KeibiDrop) startQUICControlRelay(ctx context.Context, s *session.Session, dialRoom, listenRoom string) {
	logger := kd.logger.With("component", "quic-control")

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
		if current && kd.quicRedialing.CompareAndSwap(false, true) {
			go kd.maintainQUICControl(ctx, peerAddr)
		}
	}

	ln, err := kd.listenQUICControlRelay(ctx, s, listenRoom)
	if err != nil {
		logger.Warn("QUIC relay listen failed; staying TCP-only", "room", listenRoom, "error", err)
		return
	}
	if qln, ok := ln.(*quicControlListener); ok {
		qln.onAccept = kd.maybeStartEagerFoldSoon // Set before Serve starts accepting.
	}
	kd.mu.Lock()
	superseded := kd.ctx != ctx // A reconnect swapped the generation during registration.
	if !superseded {
		kd.quicControlLn = ln
	}
	kd.mu.Unlock()
	if superseded {
		_ = ln.Close()
		return
	}
	go kd.serveQUICControl(ln, relayAddrPrefix+listenRoom)
}

// StartQUICControlChannel brings up the QUIC control pair alongside the TCP session:
// an inbound listener on the local UDP port and a background outbound dial to the peer.
// Any failure logs and leaves the session TCP-only. The dial never delays connect.
func (kd *KeibiDrop) StartQUICControlChannel() {
	// Reconnect re-runs this function, so release any prior instance first.
	kd.StopQUICControlChannel()

	logger := kd.logger.With("component", "quic-control")
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
		go kd.startQUICControlRelay(ctx, s, dialRoom, listenRoom)
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
	if kd.quicRedialing.CompareAndSwap(false, true) {
		go kd.maintainQUICControl(ctx, peerAddr)
	}
}

// quicReprobeInterval sets how often a session with QUIC down re-probes UDP. The probe
// is cheap, so the fast lane returns by itself once the network allows.
const quicReprobeInterval = 30 * time.Second

// maintainQUICControl re-establishes the outbound channel: a dial burst, then a
// periodic re-probe while down. It exits when up, on generation change, or when ctx
// ends. Single-flight via kd.quicRedialing.
func (kd *KeibiDrop) maintainQUICControl(ctx context.Context, peerAddr string) {
	defer kd.quicRedialing.Store(false)
	logger := kd.logger.With("component", "quic-control")
	for first := true; ; first = false {
		kd.mu.Lock()
		cur := kd.quicPeerAddr
		up := kd.quicControlClient != nil
		kd.mu.Unlock()
		if ctx.Err() != nil || cur != peerAddr || up {
			return
		}
		if kd.dialQUICControlWithRetry(ctx, peerAddr) {
			return
		}
		if first {
			logger.Info("QUIC channel unavailable; staying on TCP, re-probing periodically",
				"peer", peerAddr, "every", quicReprobeInterval)
		}
		select {
		case <-time.After(quicReprobeInterval):
		case <-ctx.Done():
			return
		}
	}
}

// dialQUICControlWithRetry dials the peer's QUIC control endpoint with retries,
// because the peer's listener may not be up yet. It reports whether the channel came up.
func (kd *KeibiDrop) dialQUICControlWithRetry(ctx context.Context, peerAddr string) bool {
	logger := kd.logger.With("component", "quic-control")
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		// Read under kd.mu. Teardown can nil kd.session concurrently.
		kd.mu.Lock()
		s := kd.session
		kd.mu.Unlock()
		if s == nil {
			return false
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
			conn, err = kd.dialQUICControlRelay(dctx, s, room)
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
				return false
			}
			kd.mu.Lock()
			if kd.quicPeerAddr != peerAddr { // Channel stopped or superseded during the dial.
				kd.mu.Unlock()
				_ = cc.Close()
				if mc != nil { // Nil on the relay path: not migratable.
					_ = mc.Close()
				}
				return false
			}
			hbCtx, hbCancel := context.WithCancel(ctx)
			old := kd.quicControlClient
			oldMig := kd.quicControlMig
			oldHB := kd.quicHBCancel
			kd.quicControlClient = cc
			kd.quicControlMig = mc
			kd.quicHBCancel = hbCancel
			kd.quicControlSC, _ = conn.(*session.SecureConn) // Epoch observability handle.
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
			logger.Info("QUIC control channel connected", "peer", peerAddr)
			// The control-Ping heartbeat owns QUIC-lane liveness. Metadata rides TCP first.
			go kd.quicControlHeartbeat(hbCtx, cc)
			kd.maybeStartEagerFoldSoon() // New wire: one coalesced fold round covers it.
			return true
		}
		logger.Debug("QUIC control dial attempt failed", "attempt", attempt, "error", err)
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return false
		}
	}
	// The caller, maintainQUICControl, logs the stay-on-TCP state once and re-probes.
	logger.Debug("QUIC control dial burst exhausted", "peer", peerAddr)
	return false
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
	if kd.quicRedialing.CompareAndSwap(false, true) { // Single-flight.
		go kd.maintainQUICControl(ctx, peerAddr)
	}
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

// QUICControlConnected reports whether the outbound QUIC control channel is up. For tests.
func (kd *KeibiDrop) QUICControlConnected() bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	return kd.quicControlClient != nil
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
