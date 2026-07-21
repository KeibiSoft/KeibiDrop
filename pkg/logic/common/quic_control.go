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
	"strconv"
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

// The QUIC control channel mirrors the TCP pair on UDP: an outbound conn (we dial the
// peer's UDP control endpoint) and an inbound conn (the peer dials ours), reusing the
// TCP port numbers on UDP. It carries control + metadata + the network-change signal,
// while bulk file transfer stays on TCP.
//
// The channel does NO per-connection handshake. Its keys were already derived in the
// TCP handshake payload (SEKOutboundQUIC / SEKInboundQUIC, one per direction), from the
// peer keys that were already exchanged and validated. Each QUIC stream is simply
// wrapped in a SecureConn with the matching key and a direction-specific nonce prefix
// (dialer = outbound, acceptor = inbound), so the two directions never share nonce
// space even though a QUIC stream is written by both ends.
//
// Forward secrecy: when both peers negotiated the in-band key-update capability, the
// QUIC conns ratchet exactly like the TCP pair (SetKeyUpdate below; same byte/msg/idle
// cadence, make-before-break on the write path, so a rotation never pauses traffic).
// The KEM entropy fold is deliberately TCP-pair-only: fold staging and the session
// rekey driver operate on SessionSockets, and the QUIC channel does not need it —
// every re-handshake (reconnect or proactive rekey) derives brand-new QUIC keys and
// rebuilds this channel from epoch 0, which is a strictly stronger reset than a fold.

// DialQUICControl brings up the OUTBOUND QUIC control channel to peerUDPAddr
// ("host:port"). It opens a QUIC stream on an explicit transport (so the connection
// can MIGRATE across an IP change) and wraps it in the session's outbound SecureConn
// (SEKOutboundQUIC, dialer role). The returned net.Conn is ready to carry gRPC; the
// returned MigratableConn is the migration handle (Migrate moves the underlying path,
// the SecureConn and gRPC above never notice). It errors if the peer negotiated no
// QUIC key (older build), so the caller can fall back to TCP-only.
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
	// Same capability the TCP pair negotiated: ApplyKeyUpdateNegotiation flips only the
	// conns in SessionSockets, so the QUIC conns must opt in here or they would stay at
	// epoch 0 forever (no forward secrecy on this channel). Must be set before any I/O;
	// this conn is handed to gRPC only after return.
	sc.SetKeyUpdate(s.UseKeyUpdate())
	return sc, mc, nil
}

// ListenQUICControl starts the INBOUND QUIC control channel listener on udpAddr
// ("host:port", "host:0" for an ephemeral port). Each accepted stream is wrapped in the
// session's inbound SecureConn (SEKInboundQUIC, acceptor role), ready for a gRPC server
// over a singleConnListener. It errors if the peer negotiated no QUIC key.
func ListenQUICControl(s *session.Session, udpAddr string) (net.Listener, error) {
	if s == nil {
		return nil, fmt.Errorf("quic control: nil session")
	}
	key := s.SEKInboundQUIC
	if len(key) == 0 {
		return nil, fmt.Errorf("quic control: no inbound QUIC key (peer did not negotiate QUIC)")
	}
	ln, err := transport.QUIC().Listen(udpAddr)
	if err != nil {
		return nil, fmt.Errorf("quic control listen %s: %w", udpAddr, err)
	}
	return &quicControlListener{Listener: ln, key: key, suite: s.NegotiatedSuite(), keyUpdate: s.UseKeyUpdate()}, nil
}

// quicControlListener upgrades every accepted QUIC stream to the inbound control
// SecureConn, so a plain grpc.Server can Serve it. keyUpdate is the negotiated in-band
// ratchet capability, snapshotted at listen time: it is per-session-generation exactly
// like the key (a reconnect rebuilds the listener), and every accepted conn must get it
// before gRPC starts I/O on it.
type quicControlListener struct {
	net.Listener
	key       []byte
	suite     kbc.CipherSuite
	keyUpdate bool

	mu   sync.Mutex
	last *session.SecureConn // most recently accepted conn, for epoch observability
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
	return sc, nil
}

// LastAccepted returns the most recently accepted server-side SecureConn, or nil.
func (l *quicControlListener) LastAccepted() *session.SecureConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// StartQUICControlChannel brings up the QUIC control pair alongside the established TCP
// session: an inbound listener on our UDP port (the same number as the TCP inbound
// port) serving the control service, and a background outbound dial to the peer's UDP
// control endpoint. It is FAIL-SOFT: any failure (UDP blocked, a peer without QUIC
// keys, a timing race) logs and leaves the session TCP-only, so it can never break
// connectivity, and the outbound dial runs in the background so it never delays connect.
func (kd *KeibiDrop) StartQUICControlChannel() {
	// Idempotent: a reconnect re-runs finishConnect, so release any prior instance's
	// UDP socket + client before rebinding (otherwise the listener hits address-in-use).
	kd.StopQUICControlChannel()

	logger := kd.logger.With("component", "quic-control")
	// Snapshot session + ctx under kd.mu: teardown nils kd.session under the lock, and
	// a bare read here has no happens-before (his teardown-race probes catch exactly this).
	kd.mu.Lock()
	s := kd.session
	ctx := kd.ctx // this generation's context; a reconnect swaps kd.ctx for a fresh one
	kd.mu.Unlock()
	if s == nil || len(s.SEKInboundQUIC) == 0 {
		logger.Info("peer did not negotiate a QUIC channel; staying TCP-only")
		return
	}

	// Inbound listener on our UDP port (same number as the TCP inbound port). The gRPC
	// server is attached in a background goroutine: it must register the KeibiService
	// impl (kd.KDSvc), which startGRPCServer creates concurrently, and gRPC forbids
	// registering a service after Serve — so the goroutine waits (bounded) for the svc.
	inAddr := net.JoinHostPort("::", strconv.Itoa(kd.inboundPort))
	ln, err := ListenQUICControl(s, inAddr)
	if err != nil {
		logger.Warn("QUIC control listen failed; staying TCP-only", "addr", inAddr, "error", err)
	} else {
		kd.mu.Lock()
		kd.quicControlLn = ln
		kd.mu.Unlock()
		go kd.serveQUICControl(ln, inAddr)
	}

	// Outbound dial to the peer's UDP control endpoint (same number as the TCP peer
	// port), in the background so it never delays connect.
	if kd.PeerIPv6IP == "" || len(s.SEKOutboundQUIC) == 0 {
		return
	}
	peerAddr := net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(s.PeerPort))
	kd.mu.Lock()
	kd.quicPeerAddr = peerAddr // generation marker for the maintainer (self-heal redial)
	kd.mu.Unlock()
	if kd.quicRedialing.CompareAndSwap(false, true) {
		go kd.maintainQUICControl(ctx, peerAddr)
	}
}

// quicReprobeInterval is how often a session with QUIC down re-probes UDP. A probe is
// a few packets, so this is cheap — and it means the fast lane comes back by itself
// when the network allows it again: TCP-only is a fallback state, never a verdict.
const quicReprobeInterval = 30 * time.Second

// maintainQUICControl keeps (re)establishing the outbound QUIC channel: a burst of
// dial attempts, then a periodic re-probe while it stays down. Exits when the channel
// is up, the session generation changes (peer addr cleared/replaced), or ctx ends.
// Single-flight: spawn only after winning kd.quicRedialing.
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

// dialQUICControlWithRetry dials the peer's QUIC control endpoint, retrying a few times
// because the peer's listener may not be up the instant we are (both peers reach this
// just after the TCP handshake). Reports whether the channel came up.
func (kd *KeibiDrop) dialQUICControlWithRetry(ctx context.Context, peerAddr string) bool {
	logger := kd.logger.With("component", "quic-control")
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		// Under kd.mu: this loop runs in a background goroutine, so a teardown can nil
		// kd.session concurrently.
		kd.mu.Lock()
		s := kd.session
		kd.mu.Unlock()
		if s == nil {
			return false
		}
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, mc, err := DialQUICControl(dctx, s, peerAddr)
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
				_ = mc.Close()
				return false
			}
			kd.mu.Lock()
			if kd.quicPeerAddr != peerAddr { // channel stopped/superseded while we dialed
				kd.mu.Unlock()
				_ = cc.Close()
				_ = mc.Close()
				return false
			}
			old := kd.quicControlClient
			oldMig := kd.quicControlMig
			kd.quicControlClient = cc
			kd.quicControlMig = mc
			kd.quicControlSC, _ = conn.(*session.SecureConn) // epoch observability handle
			kd.mu.Unlock()
			if old != nil {
				_ = old.Close() // exactly one live client; a racing redial loses cleanly
			}
			if oldMig != nil {
				_ = oldMig.Close()
			}
			logger.Info("QUIC control channel connected", "peer", peerAddr)
			return true
		}
		logger.Debug("QUIC control dial attempt failed", "attempt", attempt, "error", err)
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return false
		}
	}
	// The caller (maintainQUICControl) logs the stay-on-TCP state once and re-probes.
	logger.Debug("QUIC control dial burst exhausted", "peer", peerAddr)
	return false
}

// serveQUICControl attaches the QUIC control gRPC server to ln. It serves the control
// Ping plus the full KeibiService — the SAME impl instance the TCP server uses, so
// metadata and cache-miss reads landing over QUIC act on shared state. kd.KDSvc is
// created concurrently by startGRPCServer, so wait for it (bounded); on timeout serve
// Control-only — the peer's client falls back to TCP on Unimplemented (fail-soft).
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
			return // superseded by a newer generation (restart); it owns the port now
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

// StopQUICControlChannel tears down the QUIC control pair. Safe to call when it was
// never brought up (fields nil). Closing the listener also retires a serveQUICControl
// goroutine still waiting for the service impl (generation check).
func (kd *KeibiDrop) StopQUICControlChannel() {
	kd.mu.Lock()
	cc := kd.quicControlClient
	srv := kd.quicControlServer
	ln := kd.quicControlLn
	mc := kd.quicControlMig
	kd.quicControlClient = nil
	kd.quicControlServer = nil
	kd.quicControlLn = nil
	kd.quicControlMig = nil
	kd.quicControlSC = nil
	kd.quicPeerAddr = "" // a demote redial after this point has nowhere to dial (stale peer)
	kd.mu.Unlock()
	if cc != nil {
		_ = cc.Close()
	}
	if srv != nil {
		srv.Stop()
	}
	if ln != nil {
		_ = ln.Close() // releases the UDP port even if no server ever attached
	}
	if mc != nil {
		_ = mc.Close() // releases the outbound conn's transports/sockets
	}
}

// MigrateQUICControl moves the live QUIC control connection to a fresh local UDP
// socket — the Wi-Fi -> LTE case — WITHOUT dropping the channel: the stream, the
// SecureConn on it, and the gRPC channel all survive the switch (QUIC identifies the
// connection by connection ID, not the 5-tuple). It pings right after so the peer
// migrates its send path too. This is the network-change entry point (OS signal,
// reconnect logic, or manual). Fail-soft: on error the channel demotes and the
// background maintainer re-establishes from scratch.
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

	// Announce our coordinates over the surviving channel, so on a real IP change the
	// peer re-dials TCP/UDP at the fresh address immediately — no heartbeat timeout,
	// no relay lookup. Same-address announces are a no-op on the receiver. Best-effort:
	// on failure the peer still recovers via the heartbeat path. (On a real change the
	// caller — the OS network-change signal — updates kd.LocalIPv6IP first and kicks
	// its own reconnect after; see docs/transport-architecture.html.)
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
// when the channel is not connected. Metadata RPCs prefer this channel: it is isolated
// by transport from TCP bulk, so they never queue behind a prefetch.
func (kd *KeibiDrop) QUICKeibiClient() bindings.KeibiServiceClient {
	kd.mu.Lock()
	cc := kd.quicControlClient
	kd.mu.Unlock()
	if cc == nil {
		return nil
	}
	return bindings.NewKeibiServiceClient(cc)
}

// QUICMetadataSent reports how many metadata RPCs actually rode the QUIC channel.
func (kd *KeibiDrop) QUICMetadataSent() uint64 { return kd.quicMetaSent.Load() }

// QUICWriterEpoch returns the highest writer key-epoch across the live QUIC control
// conns (dial-side and the last accepted server-side), or 0 when the channel is down.
// The QUIC lane ratchets independently of the TCP pair — on-demand reads ride it when
// it is up — so transport-wide rekey observability (tests, diagnostics) must read both
// lanes, not just the TCP SessionSockets the HealthMonitor captures.
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

// quicMetaTimeout bounds the QUIC-first attempt of a metadata RPC. A half-dead QUIC
// channel (e.g. UDP blackholed mid-session) otherwise stalls the RPC until the QUIC
// idle timeout (~20s, measured); with this bound the worst case is one short stall,
// after which demoteQUICControl sends everything straight to TCP.
const quicMetaTimeout = 2 * time.Second

// demoteQUICControl drops the outbound QUIC client after a failed metadata attempt, so
// subsequent RPCs go straight to TCP instead of paying quicMetaTimeout each time — and
// immediately starts a single-flight background redial, so a transient failure heals
// itself in seconds and the fast lane comes back. Only a genuinely dead channel (UDP
// blocked, peer gone) stays demoted: there the redial fails and TCP-only is correct.
// The inbound server stays up throughout.
func (kd *KeibiDrop) demoteQUICControl(err error) {
	kd.mu.Lock()
	cc := kd.quicControlClient
	mc := kd.quicControlMig
	kd.quicControlClient = nil
	kd.quicControlMig = nil
	kd.quicControlSC = nil
	peerAddr := kd.quicPeerAddr
	ctx := kd.ctx
	kd.mu.Unlock()
	if cc == nil {
		return // already demoted by a concurrent failure
	}
	kd.logger.Warn("QUIC metadata attempt failed; demoting to TCP, redialing in background", "error", err)
	_ = cc.Close()
	if mc != nil {
		_ = mc.Close()
	}

	if peerAddr == "" || ctx == nil || ctx.Err() != nil {
		return
	}
	if kd.quicRedialing.CompareAndSwap(false, true) { // single-flight
		go kd.maintainQUICControl(ctx, peerAddr)
	}
}

// quicFirst is the ONE metadata routing policy, written once: try the QUIC channel
// bounded by quicMetaTimeout (counting successes), demote the channel on failure, fall
// back to the TCP client. The concrete RPC is a parameter, so per-RPC wrappers are
// one-liners; generics are monomorphized at compile time (no reflection, no interface
// boxing on the hot path).
func quicFirst[Req, Resp any](kd *KeibiDrop, ctx context.Context, req Req,
	call func(bindings.KeibiServiceClient, context.Context, Req) (Resp, error),
) (Resp, error) {
	if qc := kd.QUICKeibiClient(); qc != nil {
		qctx, cancel := context.WithTimeout(ctx, quicMetaTimeout)
		resp, err := call(qc, qctx, req)
		cancel()
		if err == nil {
			kd.quicMetaSent.Add(1)
			return resp, nil
		}
		kd.demoteQUICControl(err)
	}
	// Snapshot under kd.mu: the notify worker calls this from its own goroutine while
	// Run's teardown rewrites kd.session under the lock (bare read = data race).
	kd.mu.Lock()
	s := kd.session
	kd.mu.Unlock()
	if s == nil || s.GRPCClient == nil {
		var zero Resp
		return zero, ErrInvalidSession
	}
	return call(s.GRPCClient, ctx, req)
}

// sendNotify / sendBatchNotify: the metadata RPCs, each just naming its call.
func (kd *KeibiDrop) sendNotify(ctx context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	return quicFirst(kd, ctx, req, func(c bindings.KeibiServiceClient, ctx context.Context, r *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
		return c.Notify(ctx, r)
	})
}

func (kd *KeibiDrop) sendBatchNotify(ctx context.Context, req *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
	return quicFirst(kd, ctx, req, func(c bindings.KeibiServiceClient, ctx context.Context, r *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
		return c.BatchNotify(ctx, r)
	})
}

// QUICControlConnected reports whether the outbound QUIC control channel is up (the
// client dialed the peer successfully). For tests and diagnostics.
func (kd *KeibiDrop) QUICControlConnected() bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	return kd.quicControlClient != nil
}

// PingQUICControl sends one control-plane Ping over the QUIC channel, or errors if the
// channel is not up. For tests and as the seed of the heartbeat/RTT loop.
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

// quicControlService answers the QUIC control channel's own RPCs: the heartbeat Ping
// and the network-change Announce.
type quicControlService struct {
	cpb.UnimplementedControlServer
	kd *KeibiDrop
}

func (quicControlService) Ping(context.Context, *cpb.PingRequest) (*cpb.PingReply, error) {
	return &cpb.PingReply{}, nil
}

// Announce is the receiver side of the network-change signal: the peer moved (its QUIC
// channel survived by migration and carried this message), so refresh every cached
// coordinate — TCP reconnect target, UDP control target — and, if the address actually
// changed, kick reconnection NOW instead of waiting ~25s of heartbeat failures plus a
// relay lookup that may still hold the stale address. Same-address announces (e.g. a
// migration between sockets on one host) are a no-op.
func (s quicControlService) Announce(_ context.Context, req *cpb.AnnounceRequest) (*cpb.AnnounceReply, error) {
	kd := s.kd
	if kd == nil || req.Ip == "" || req.TcpPort == 0 {
		return &cpb.AnnounceReply{}, nil
	}
	port := int(req.TcpPort)
	// Under kd.mu: Announce arrives on a gRPC server goroutine; teardown nils kd.session
	// under the lock.
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
	kd.quicPeerAddr = net.JoinHostPort(req.Ip, strconv.Itoa(port)) // maintainer redials the NEW addr
	kd.mu.Unlock()

	// The TCP pair to the old address is dead; reconnect immediately with the fresh
	// coordinates (onDisconnect guards re-entrancy and active transfers itself).
	if kd.ReconnectManager != nil {
		go kd.onDisconnect()
	}
	return &cpb.AnnounceReply{}, nil
}
