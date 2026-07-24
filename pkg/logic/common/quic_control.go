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

// The QUIC control channel mirrors the TCP pair on UDP: an outbound conn we dial and an
// inbound conn the peer dials, reusing the TCP port numbers. It carries control,
// metadata, and the network-change signal; bulk transfer stays on TCP.
//
// No per-connection handshake: keys come from the TCP handshake payload
// (SEKOutboundQUIC/SEKInboundQUIC, one per direction). Each stream is wrapped in a
// SecureConn with a direction-specific nonce prefix, so the two directions never share
// nonce space. When both peers negotiated in-band key-update, the QUIC conns ratchet
// like the TCP pair (SetKeyUpdate below). The KEM fold stages on these QUIC conns too
// (quicFoldConns/ExtraFoldConns); a re-handshake also re-derives fresh QUIC keys.

// DialQUICControl brings up the outbound QUIC control channel to peerUDPAddr. It wraps a
// migratable QUIC stream in the session's outbound SecureConn (SEKOutboundQUIC); the
// returned MigratableConn is the migration handle. Errors if the peer negotiated no QUIC
// key, so the caller can fall back to TCP-only.
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
	// Opt into the negotiated ratchet before any I/O; the SessionSockets opt-in doesn't
	// reach these conns.
	sc.SetKeyUpdate(s.UseKeyUpdate())
	return sc, mc, nil
}

// ListenQUICControl starts the inbound QUIC control listener on udpAddr. Each accepted
// stream is wrapped in the session's inbound SecureConn (SEKInboundQUIC). Errors if the
// peer negotiated no QUIC key.
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
// SecureConn. keyUpdate is the negotiated ratchet capability, snapshotted at listen time.
type quicControlListener struct {
	net.Listener
	key       []byte
	suite     kbc.CipherSuite
	keyUpdate bool
	onAccept  func() // fold trigger on conn-up; set before Serve

	mu   sync.Mutex
	last *session.SecureConn // most recent accepted conn (epoch observability + fold staging)
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
	// New wire: re-run the eager fold so this conn gets fresh entropy too. Single-flight
	// and supersede-idempotent staging make repeats safe.
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

// StartQUICControlChannel brings up the QUIC control pair alongside the TCP session: an
// inbound listener on our UDP port and a background outbound dial to the peer. Fail-soft:
// any failure logs and leaves the session TCP-only. The outbound dial is backgrounded so
// it never delays connect.
func (kd *KeibiDrop) StartQUICControlChannel() {
	// Idempotent: release any prior instance before rebinding (reconnect re-runs this).
	kd.StopQUICControlChannel()

	logger := kd.logger.With("component", "quic-control")
	// Snapshot session + ctx under kd.mu: teardown nils kd.session under the lock.
	kd.mu.Lock()
	s := kd.session
	ctx := kd.ctx // this generation's context; a reconnect swaps kd.ctx for a fresh one
	kd.mu.Unlock()
	if s == nil || len(s.SEKInboundQUIC) == 0 {
		logger.Info("peer did not negotiate a QUIC channel; staying TCP-only")
		return
	}

	// Inbound listener on our UDP port. The gRPC server attaches in a goroutine that waits
	// (bounded) for kd.KDSvc, since gRPC forbids registering a service after Serve.
	inAddr := net.JoinHostPort("::", strconv.Itoa(kd.inboundPort))
	ln, err := ListenQUICControl(s, inAddr)
	if err != nil {
		logger.Warn("QUIC control listen failed; staying TCP-only", "addr", inAddr, "error", err)
	} else {
		if qln, ok := ln.(*quicControlListener); ok {
			qln.onAccept = kd.maybeStartEagerFold // set before Serve starts accepting
		}
		kd.mu.Lock()
		kd.quicControlLn = ln
		kd.mu.Unlock()
		go kd.serveQUICControl(ln, inAddr)
	}

	// Outbound dial to the peer's UDP control endpoint, backgrounded so it never delays connect.
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

// quicReprobeInterval is how often a session with QUIC down re-probes UDP. Cheap, so the
// fast lane returns by itself once the network allows: TCP-only is a fallback, not a verdict.
const quicReprobeInterval = 30 * time.Second

// maintainQUICControl keeps (re)establishing the outbound channel: a dial burst, then a
// periodic re-probe while down. Exits when up, the session generation changes, or ctx ends.
// Single-flight via kd.quicRedialing.
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
// because the peer's listener may not be up yet. Reports whether the channel came up.
func (kd *KeibiDrop) dialQUICControlWithRetry(ctx context.Context, peerAddr string) bool {
	logger := kd.logger.With("component", "quic-control")
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		// Under kd.mu: teardown can nil kd.session concurrently.
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
			hbCtx, hbCancel := context.WithCancel(ctx)
			old := kd.quicControlClient
			oldMig := kd.quicControlMig
			oldHB := kd.quicHBCancel
			kd.quicControlClient = cc
			kd.quicControlMig = mc
			kd.quicHBCancel = hbCancel
			kd.quicControlSC, _ = conn.(*session.SecureConn) // epoch observability handle
			kd.mu.Unlock()
			if oldHB != nil {
				oldHB() // superseded heartbeat exits now
			}
			if old != nil {
				_ = old.Close() // exactly one live client
			}
			if oldMig != nil {
				_ = oldMig.Close()
			}
			logger.Info("QUIC control channel connected", "peer", peerAddr)
			// The control-Ping heartbeat owns QUIC-lane liveness now (metadata rides TCP first).
			go kd.quicControlHeartbeat(hbCtx, cc)
			// New wire: re-run the eager fold for the dial-side conn (safe to repeat).
			kd.maybeStartEagerFold()
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

// serveQUICControl attaches the QUIC control gRPC server to ln: the control Ping plus the
// full KeibiService (the same impl the TCP server uses). kd.KDSvc is created concurrently,
// so wait for it (bounded); on timeout serve Control-only and let the peer fall back to TCP.
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

// StopQUICControlChannel tears down the QUIC control pair. Safe when it was never brought
// up (fields nil). Closing the listener also retires a waiting serveQUICControl goroutine.
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
	kd.quicPeerAddr = "" // a later demote redial has nowhere to dial
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
		_ = ln.Close() // releases the UDP port
	}
	if mc != nil {
		_ = mc.Close() // releases the outbound sockets
	}
}

// MigrateQUICControl moves the live QUIC control connection to a fresh local UDP socket
// (the Wi-Fi to LTE case) without dropping the channel: QUIC identifies the connection by
// connection ID, not the 5-tuple, so the stream, SecureConn, and gRPC channel all survive.
// It pings right after so the peer migrates its send path too. Fail-soft: on error the
// channel demotes and the maintainer re-establishes.
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

	// Announce our coordinates over the surviving channel so the peer re-dials the fresh
	// address immediately, skipping the heartbeat timeout and relay lookup. Same-address
	// announces are a no-op; best-effort, since the peer still recovers via the heartbeat.
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

// QUICKeibiClient returns a KeibiService client over the QUIC control channel, or nil when
// it is down. Metadata prefers it: transport-isolated from TCP bulk, so it never queues behind a prefetch.
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

// quicFoldConns returns the live QUIC control SecureConns (dial-side + last accepted) so a
// fold round can stage its secret on them too. Wired as session.ExtraFoldConns; returns
// only what exists now (empty when the channel is down, so the fold covers TCP alone).
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

// QUICWriterEpoch returns the highest writer key-epoch across the live QUIC control conns,
// or 0 when the channel is down. The QUIC lane ratchets independently of the TCP pair, so
// transport-wide rekey observability must read both lanes, not just the TCP SessionSockets.
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

// quicMetaTimeout bounds a metadata RPC's QUIC attempt. A half-dead channel (UDP
// blackholed) would otherwise stall until the QUIC idle timeout (~20s); with this bound
// the worst case is one short stall, after which demoteQUICControl routes everything to TCP.
const quicMetaTimeout = 2 * time.Second

// demoteQUICControl drops the outbound QUIC client after a failure, so subsequent RPCs go
// straight to TCP instead of paying quicMetaTimeout each time, and starts a single-flight
// background redial so a transient failure heals itself. A genuinely dead channel stays
// demoted (the redial fails). The inbound server stays up throughout.
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
		return // already demoted by a concurrent failure
	}
	kd.logger.Warn("QUIC control lane failed; demoting, redialing in background", "error", err)
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

// metadataSend is the metadata routing policy: TCP-first, with the QUIC channel as a
// bounded fallback. Bulk notify bursts must ride TCP so they never congest the scarce QUIC
// lane that cache-miss seeks and control depend on. QUIC is used only when TCP is
// unavailable, so a notify still gets through a TCP outage that QUIC survives (an IP change).
func metadataSend[Req, Resp any](kd *KeibiDrop, ctx context.Context, req Req,
	call func(bindings.KeibiServiceClient, context.Context, Req) (Resp, error),
) (Resp, error) {
	// Snapshot under kd.mu: teardown rewrites kd.session under the lock.
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
	// TCP unavailable: fall back to the bounded QUIC lane, so a dead lane can't hang the notify.
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

// sendNotify / sendBatchNotify: the metadata RPCs, each just naming its call.
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

// quicHeartbeatInterval is how often the QUIC control lane is pinged to detect a dead
// channel. Metadata is TCP-first, so this heartbeat is the lane's own liveness check.
const quicHeartbeatInterval = 5 * time.Second

// quicControlHeartbeat pings the QUIC control lane and, on failure, demotes it (kicking the
// self-heal maintainer). One per channel generation: exits when cc is superseded or ctx ends.
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
				return // superseded by a newer generation; that one has its own heartbeat
			}
			pctx, cancel := context.WithTimeout(ctx, quicMetaTimeout)
			_, err := client.Ping(pctx, &cpb.PingRequest{})
			cancel()
			if err != nil {
				kd.demoteQUICControl(err) // closes cc + kicks the self-heal maintainer
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

// Announce is the receiver side of the network-change signal: the peer moved, so refresh
// the cached coordinates and, if the address changed, kick reconnection now instead of
// waiting out ~25s of heartbeat failures plus a relay lookup. Same-address is a no-op.
func (s quicControlService) Announce(_ context.Context, req *cpb.AnnounceRequest) (*cpb.AnnounceReply, error) {
	kd := s.kd
	if kd == nil || req.Ip == "" || req.TcpPort == 0 {
		return &cpb.AnnounceReply{}, nil
	}
	port := int(req.TcpPort)
	// Under kd.mu: teardown nils kd.session under the lock.
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

	// The old TCP pair is dead; reconnect with the fresh coordinates (onDisconnect guards
	// re-entrancy).
	if kd.ReconnectManager != nil {
		go kd.onDisconnect()
	}
	return &cpb.AnnounceReply{}, nil
}
