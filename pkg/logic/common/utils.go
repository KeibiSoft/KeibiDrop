// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// deriveRelayAuth derives the relay lookup token (sent as the Bearer header)
// and the registration encryption key from a fingerprint's room password.
// Both register and fetch use this; the relay only ever sees the opaque token.
func deriveRelayAuth(fp string) (lookupToken string, encryptionKey []byte, err error) {
	roomPassword, err := crypto.ExtractRoomPassword(fp)
	if err != nil {
		return "", nil, fmt.Errorf("extract room password: %w", err)
	}
	lookupKey, encryptionKey, err := crypto.DeriveRelayKeys(roomPassword)
	if err != nil {
		return "", nil, fmt.Errorf("derive relay keys: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(lookupKey), encryptionKey, nil
}

// relayURL joins sub onto the configured relay endpoint and parses the result.
func (kd *KeibiDrop) relayURL(sub string) (*url.URL, error) {
	joined, err := url.JoinPath(kd.RelayEndoint.String(), sub)
	if err != nil {
		return nil, fmt.Errorf("join relay path %q: %w", sub, err)
	}
	u, err := url.Parse(joined)
	if err != nil {
		return nil, fmt.Errorf("parse relay url: %w", err)
	}
	return u, nil
}

func (kd *KeibiDrop) registerRoomToRelay() error {
	logger := kd.logger.With("method", "register-room-to-relay")
	// Snapshot under kd.mu: the keepalive goroutine calls this while teardown may nil
	// kd.session. Lock only for the read to keep the rk.mu -> kd.mu order intact.
	kd.mu.Lock()
	s := kd.session
	kd.mu.Unlock()
	if kd.relayClient == nil || s == nil || s.OwnKeys == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	// Learn our reachability before publishing, so the hint is right on the first connect.
	// Cached, so the keepalive's re-registrations do not re-probe.
	kd.ProbeInboundReachability(kd.ctx)

	ownFp := s.OwnFingerprint

	lookupToken, encryptionKey, err := deriveRelayAuth(ownFp)
	if err != nil {
		logger.Error("Failed to derive relay auth", "error", err)
		return err
	}

	pkMap, err := s.OwnKeys.ExportPubKeysAsMap()
	if err != nil {
		logger.Error("Failed to export own keys", "error", err)
		return err
	}

	peerReg := PeerRegistration{
		Fingerprint: ownFp,
		Listen: &ConnectionHint{
			IPv6:           true,
			IP:             kd.LocalIPv6IP,
			Proto:          "tcp",
			Port:           kd.inboundPort,
			InboundBlocked: kd.InboundBlocked(),
		},
		LocalAddrs: GetLocalAddrs(),
		PublicKeys: pkMap,
		Timestamp:  time.Now().UnixNano(),
	}

	// Serialize and encrypt the registration.
	plaintext, err := json.Marshal(peerReg)
	if err != nil {
		logger.Error("Failed to marshal registration", "error", err)
		return err
	}

	encryptedBlob, err := crypto.Encrypt(encryptionKey, plaintext)
	if err != nil {
		logger.Error("Failed to encrypt registration", "error", err)
		return err
	}

	// Send encrypted blob to relay (relay cannot read contents).
	payload := EncryptedRegistration{
		Blob: base64.RawURLEncoding.EncodeToString(encryptedBlob),
	}

	registerUrl, err := kd.relayURL("register")
	if err != nil {
		logger.Error("Failed to build register url", "error", err)
		return err
	}

	resp, err := PostJSONWithURL(kd.relayClient, registerUrl, map[string]string{"Authorization": "Bearer " + lookupToken}, payload, RegisterErrorMapper)
	if err != nil {
		logger.Error("Failed to register", "error", err)
		// TODO: On the caller of this method; handle the retry logic, and appropriate display of message.
		return err
	}

	// Server reached its limit. Retry in 10 minutes or later.
	if resp.StatusCode == http.StatusServiceUnavailable {
		logger.Warn("Relay server at full capacity retry in 10 minutes")
		return ErrRelayAtFullCapacityRetryLater
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		logger.Warn("We got a weird status code", "code", resp.StatusCode)
	}

	// Check if relay returned metadata (bridge hint, tier) in register response.
	var regResp EncryptedRegistration
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err == nil {
		if regResp.Bridge != "" {
			logger.Info("Relay assigned bridge", "bridge", regResp.Bridge)
			kd.BridgeAddr = regResp.Bridge
		}
	}
	_ = resp.Body.Close()

	logger.Info("Success")

	return nil
}

func (kd *KeibiDrop) getRoomFromRelay(outOfBandFingerPrint string) error {
	logger := kd.logger.With("method", "get-room-from-relay")
	// Snapshot under kd.mu; reconnect's RelayLookup calls this while teardown may nil
	// kd.session.
	kd.mu.Lock()
	s := kd.session
	kd.mu.Unlock()
	if kd.relayClient == nil || s == nil || s.OwnKeys == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	lookupToken, encryptionKey, err := deriveRelayAuth(outOfBandFingerPrint)
	if err != nil {
		logger.Error("Failed to derive relay auth", "error", err)
		return err
	}

	fetchUrl, err := kd.relayURL("fetch")
	if err != nil {
		logger.Error("Failed to build fetch url", "error", err)
		return err
	}

	resp, err := GetJSONWithURL(kd.relayClient, fetchUrl, map[string]string{"Authorization": "Bearer " + lookupToken}, RegisterErrorMapper)
	if err != nil {
		logger.Error("Failed to fetch", "error", err)
		// TODO: On the caller of this method; handle the retry logic, and appropriate display of message.
		return err
	}

	if resp.StatusCode == http.StatusNotFound {
		logger.Warn("Not found")
		return ErrNotFound
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		logger.Warn("We got a weird status code", "code", resp.StatusCode)
	}

	// Decode encrypted response from relay.
	// The response may include relay metadata (bridge hint, tier) alongside the encrypted blob.
	var encResp EncryptedRegistration
	if err := json.NewDecoder(resp.Body).Decode(&encResp); err != nil {
		logger.Error("Failed to decode encrypted response", "error", err)
		return err
	}
	_ = resp.Body.Close()

	// If the relay suggests a bridge, use it (both peers get the same suggestion).
	if encResp.Bridge != "" {
		logger.Info("Relay suggested bridge", "bridge", encResp.Bridge)
		kd.BridgeAddr = encResp.Bridge
	}

	encryptedBlob, err := base64.RawURLEncoding.DecodeString(encResp.Blob)
	if err != nil {
		logger.Error("Failed to decode blob", "error", err)
		return err
	}

	plaintext, err := crypto.Decrypt(encryptionKey, encryptedBlob)
	if err != nil {
		logger.Error("Failed to decrypt registration", "error", err)
		return err
	}

	peerReg := &PeerRegistration{}
	if err := json.Unmarshal(plaintext, peerReg); err != nil {
		logger.Error("Failed to unmarshal decrypted registration", "error", err)
		return err
	}

	if peerReg.Listen == nil {
		logger.Error("Invalid listen details")
		return ErrInvalidResponse
	}

	if subtle.ConstantTimeCompare([]byte(peerReg.Fingerprint), []byte(outOfBandFingerPrint)) != 1 {
		logger.Warn("Fingerprint mismatch")
		return ErrFingerprintMismatch
	}

	peerKeysMap := make(map[string][]byte)
	for k, v := range peerReg.PublicKeys {
		asByte, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			logger.Error("Failed to decode peer public key", "alg", k, "error", err)
			return err
		}
		peerKeysMap[k] = asByte
	}

	peerKeys, err := crypto.ParsePeerKeys(peerKeysMap)
	if err != nil {
		logger.Error("Failed to parse peer keys", "error", err)
		return err
	}

	err = peerKeys.Validate()
	if err != nil {
		logger.Error("Failed to validate peer keys", "error", err)
		return err
	}

	computedFp, err := peerKeys.Fingerprint()
	if err != nil {
		logger.Error("Failed to compute peer fingerprint", "error", err)
		return err
	}

	if subtle.ConstantTimeCompare([]byte(computedFp), []byte(outOfBandFingerPrint)) != 1 {
		logger.Warn("Fingerprint mismatch")
		return ErrFingerprintMismatch
	}

	s.PeerPubKeys = peerKeys

	// Identity confirmed: emit it. The reactor (the FUSE cache scoper, wired in
	// setupFilesystem) is decoupled from this verifier.
	if kd.OnPeerVerified != nil {
		kd.OnPeerVerified(computedFp)
	}

	if !config.ValidPeerPort(peerReg.Listen.Port) {
		logger.Warn("Provided outbound port is out of known range, defaulting to config", "provided-port", peerReg.Listen.Port, "default-to", config.OutboundPort)
		peerReg.Listen.Port = config.OutboundPort
	}

	s.PeerPort = peerReg.Listen.Port
	kd.peerInboundBlocked.Store(peerReg.Listen.InboundBlocked)
	if peerReg.Listen.InboundBlocked {
		logger.Info("Peer advertises a blocked inbound; its direct dial will be skipped")
	}
	if isValidIPv6(peerReg.Listen.IP) {
		kd.PeerIPv6IP = peerReg.Listen.IP
	} else if peerReg.Listen.IP != "" {
		logger.Warn("Peer has no valid IPv6 (mobile peer?)", "got", peerReg.Listen.IP)
	}

	// Store peer's local addresses for LAN discovery.
	kd.PeerLocalAddrs = peerReg.LocalAddrs
	if len(kd.PeerLocalAddrs) > 0 {
		logger.Info("Peer has local addresses", "addrs", kd.PeerLocalAddrs)
	}

	logger.Info("Success")

	return nil
}

// isValidIPv6 reports whether ipStr parses as an IPv6 address (i.e. a valid IP
// that is not IPv4). Loopback/ULA/link-local are intentionally accepted.
func isValidIPv6(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	return ip != nil && ip.To4() == nil
}

//

// refreshAttrFromDisk refreshes req.Attr's Size/ModTime from disk before a debounced notify is
// sent (the queued size may be stale). A file gone at flush time is kept, not dropped.
func refreshAttrFromDisk(req *bindings.NotifyRequest, downloadFolder string) {
	if req.Attr == nil {
		return
	}
	info, err := os.Lstat(filepath.Join(downloadFolder, req.Path))
	if err != nil {
		// File gone at flush: it either flickered (rename/replace) during the debounce or is a
		// true transient. Keep the ADD with the attr captured at enqueue rather than drop, which
		// would lose real files that merely flickered; a true phantom is harmless (the peer's
		// on-demand Read returns EOF for a gone remote file).
		return
	}
	req.Attr.Size = info.Size()
	req.Attr.ModificationTime = uint64(info.ModTime().UnixNano())
}

// isDebouncedNotify reports whether a notify type is per-path debounced (ADD_FILE/EDIT_FILE).
// Everything else is sent immediately and is what pendingNotifies tracks.
func isDebouncedNotify(t bindings.NotifyType) bool {
	return t == bindings.NotifyType_ADD_FILE || t == bindings.NotifyType_EDIT_FILE
}

// countImmediateNotifies returns how many entries in a flush batch are the immediate
// REMOVE/RENAME/ADD_DIR kind that pendingNotifies counts, to balance the per-event increment.
func countImmediateNotifies(batch []*bindings.NotifyRequest) int64 {
	var n int64
	for _, r := range batch {
		if !isDebouncedNotify(r.Type) {
			n++
		}
	}
	return n
}

func (kd *KeibiDrop) setupFilesystem(logger *slog.Logger, ready chan struct{}) error {
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Warn("Session or gRPC client not initialized")
		return ErrNilPointer
	}

	fs := kd.FS
	if fs == nil {
		fs = filesystem.NewFS(logger)
		kd.FS = fs
	}

	// Set collab sync options.
	fs.PrefetchOnOpen = kd.PrefetchOnOpen
	fs.PushOnWrite = kd.PushOnWrite
	fs.AutoCache = kd.AutoCache // live_collab → macFUSE auto_cache (set before Mount)
	fs.PrefetchAutoMB = kd.PrefetchAutoMB
	fs.ReadAheadWindowMB = kd.ReadAheadWindowMB

	// The handshake emits OnPeerVerified; the FUSE cache reacts by scoping to that peer. A
	// different peer drops the prior cache (no cross-peer leak); the same peer reconnecting or
	// resuming keeps it (no re-fetch).
	kd.OnPeerVerified = func(fp string) {
		if kd.FS != nil {
			kd.FS.EnsurePeerScope(fp)
		}
	}

	// Notification worker with per-path debounce and batching. Without debounce, a multi-write
	// download (e.g. LFS writing a 420MB file) triggers ADD_FILE repeatedly and the peer
	// restarts prefetch each time. ADD_FILE/EDIT_FILE are debounced per path on a 200ms timer;
	// RENAME/REMOVE/ADD_DIR are sent immediately.
	kd.notifyCh = make(chan *bindings.NotifyRequest, 16384) // 8x headroom for git-clone bursts (~1-1.5 events/file)
	var batchSeq atomic.Uint64
	go func() {
		// Per-path debounce state.
		type pendingNotify struct {
			req      *bindings.NotifyRequest
			deadline time.Time // send after this time
		}
		pending := make(map[string]*pendingNotify)          // path → latest ADD_FILE
		immediate := make([]*bindings.NotifyRequest, 0, 64) // RENAME/REMOVE/ADD_DIR
		ticker := time.NewTicker(100 * time.Millisecond)    // check deadlines
		defer ticker.Stop()

		flush := func(batch []*bindings.NotifyRequest) {
			if len(batch) == 0 {
				return
			}
			// This batch is leaving the worker's hands: clear its immediate REMOVE/RENAME
			// events from pendingNotifies on every return path so the counter can't leak.
			defer kd.pendingNotifies.Add(-countImmediateNotifies(batch))
			// Snapshot the session under kd.mu: teardown rewrites kd.session concurrently and
			// this worker never took kd.mu, so a bare read would race. The RPCs below route
			// over the QUIC metadata channel when up and fall back to TCP.
			kd.mu.Lock()
			s := kd.session
			kd.mu.Unlock()
			if s == nil || s.GRPCClient == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := kd.sendBatchNotify(ctx, &bindings.BatchNotifyRequest{
				Notifications: batch,
				Seq:           batchSeq.Add(1),
				Timestamp:     uint64(time.Now().UnixNano()),
			})
			cancel()
			if err != nil {
				logger.Error("BatchNotify failed, falling back to individual", "count", len(batch), "error", err)
				for _, req := range batch {
					ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
					_, _ = kd.sendNotify(ctx2, req)
					cancel2()
				}
			}
		}

		for {
			select {
			case req, ok := <-kd.notifyCh:
				if !ok {
					// Channel closed: flush everything buffered. Immediate REMOVE/RENAME/ADD_DIR
					// first (so buffered deletes aren't dropped and their counts are balanced),
					// then the debounced ADD_FILE map. A REMOVE already cleared any pending ADD.
					remaining := make([]*bindings.NotifyRequest, 0, len(immediate)+len(pending))
					remaining = append(remaining, immediate...)
					for _, p := range pending {
						if kd.FS != nil && kd.FS.Root != nil {
							refreshAttrFromDisk(p.req, kd.FS.Root.LocalDownloadFolder)
						}
						remaining = append(remaining, p.req)
					}
					flush(remaining)
					return
				}

				// Filter macOS metadata files from peer sync.
				baseName := filepath.Base(req.Path)
				if baseName == ".DS_Store" || baseName == ".fseventsd" || strings.HasPrefix(baseName, "._") {
					continue
				}

				// Count REMOVE/RENAME/ADD_DIR (immediate, non-replayed kinds) on accept so a
				// proactive rekey defers until they're delivered. Balanced by flush.
				if !isDebouncedNotify(req.Type) {
					kd.pendingNotifies.Add(1)
				}

				switch req.Type {
				case bindings.NotifyType_ADD_FILE, bindings.NotifyType_EDIT_FILE:
					// Per-path debounce: update pending and reset deadline.
					// Only send when the path is stable for 200ms.
					pending[req.Path] = &pendingNotify{
						req:      req,
						deadline: time.Now().Add(200 * time.Millisecond),
					}
				case bindings.NotifyType_RENAME_FILE, bindings.NotifyType_RENAME_DIR:
					// RENAME: send immediately. Re-target any pending ADD_FILE for the old path
					// to the new path so the peer still downloads the content.
					if old, exists := pending[req.OldPath]; exists {
						delete(pending, req.OldPath)
						old.req.Path = req.Path // retarget to new path
						pending[req.Path] = old
					}
					immediate = append(immediate, req)
				default:
					// REMOVE, ADD_DIR, REMOVE_DIR, DISCONNECT: send immediately. On
					// create-then-delete in the debounce window, drop the pending ADD.
					if req.Type == bindings.NotifyType_REMOVE_FILE || req.Type == bindings.NotifyType_REMOVE_DIR {
						delete(pending, req.Path)
					}
					immediate = append(immediate, req)
				}

				// Flush immediate notifications if enough accumulated.
				if len(immediate) >= 32 {
					flush(immediate)
					immediate = immediate[:0]
				}

			case <-ticker.C:
				// Check for expired debounce deadlines.
				now := time.Now()
				ready := make([]*bindings.NotifyRequest, 0, 16)
				for path, p := range pending {
					if now.After(p.deadline) {
						if kd.FS != nil && kd.FS.Root != nil {
							refreshAttrFromDisk(p.req, kd.FS.Root.LocalDownloadFolder)
						}
						ready = append(ready, p.req)
						delete(pending, path)
					}
				}
				// Also flush any accumulated immediate notifications.
				if len(immediate) > 0 {
					ready = append(ready, immediate...)
					immediate = immediate[:0]
				}
				flush(ready)
			}
		}
	}()

	fs.OnLocalChange = func(event types.FileEvent) {
		// Snapshot under kd.mu; teardown rewrites kd.session on another goroutine.
		kd.mu.Lock()
		s := kd.session
		kd.mu.Unlock()
		if s == nil || s.GRPCClient == nil {
			return
		}

		req := &bindings.NotifyRequest{
			Type:        bindings.NotifyType(event.Action), // #nosec G115 -- action values are small enums
			Path:        event.Path,
			OldPath:     event.OldPath, // For RENAME operations.
			BaseMtimeNs: event.BaseMtimeNs,
		}

		// Attr may be nil for removal events.
		if event.Attr != nil {
			req.Attr = &bindings.Attr{
				Dev:              event.Attr.Dev,
				Ino:              event.Attr.Ino,
				Mode:             event.Attr.Mode,
				Size:             event.Attr.Size,
				AccessTime:       event.Attr.AccessTime,
				ModificationTime: event.Attr.ModificationTime,
				ChangeTime:       event.Attr.ChangeTime,
				BirthTime:        event.Attr.BirthTime,
				Flags:            event.Attr.Flags,
			}
			// logger.Info(">>> SENDING NOTIFICATION TO PEER",
			// 	"path", event.Path,
			// 	"action", event.Action,
			// 	"size", event.Attr.Size)
		}

		// Non-blocking send to notification channel.
		// If the channel is full (16384 pending), drop the notification
		// rather than blocking the FUSE handler.
		select {
		case kd.notifyCh <- req:
			logger.Debug("Local change queued", "action", event.Action, "path", event.Path)
		default:
			logger.Warn("Notification queue full, dropping", "path", event.Path)
		}
	}

	fs.OpenStreamProvider = kd.openStreamProvider

	fs.RefreshCallbacks()

	if ready != nil {
		kd.filesystemReadyOnce.Do(func() {
			close(ready)
		})
	}

	return nil
}

// openStreamProvider is the FUSE OpenStreamProvider callback. It snapshots the session under
// kd.mu and returns nil if the session (or its gRPC client) is gone, so a goroutine waking from
// PrefetchSem during teardown can't deref a nil session. Callers handle the nil return.
func (kd *KeibiDrop) openStreamProvider() types.FileStreamProvider {
	kd.mu.Lock()
	s := kd.session
	qcc := kd.quicControlClient
	kd.mu.Unlock()
	if s == nil || s.GRPCClient == nil {
		return nil
	}
	// With the QUIC control channel up, split prefetch (StreamFile) on TCP from on-demand reads
	// and chunk hashes on QUIC. Providers are per open, so a later channel is picked up.
	if qcc != nil {
		return NewImplStreamProviderDual(s.GRPCClient, bindings.NewKeibiServiceClient(qcc))
	}
	return NewImplStreamProvider(s.GRPCClient)
}

// connectGRPCClientWithRetry waits until the gRPC server is ready and then creates the client.
// timeout is the maximum total wait duration.
func (kd *KeibiDrop) connectGRPCClientWithRetry(timeout time.Duration) error {
	logger := kd.logger.With("method", "connect-grpc-retry")
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout connecting to grpc server")
		}

		// Snapshot kd.session under kd.mu (teardown nils it concurrently); this runs on
		// the reconnect goroutine.
		kd.mu.Lock()
		s := kd.session
		kd.mu.Unlock()
		// only try if the outbound connection is available
		if outboundConn := s.OutboundConn(); s != nil && outboundConn != nil {
			// outboundConn is snapshotted under socketsMu, so the dialer closure is
			// safe even if kd.session is nil'd later by the Stop handler.

			dialer := func(ctx context.Context, _ string) (net.Conn, error) {
				return outboundConn, nil
			}

			conn, err := grpc.Dial( //nolint:staticcheck // SA1019: custom dialer over hijacked conn
				"keibipipe",
				append([]grpc.DialOption{
					grpc.WithContextDialer(dialer),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				}, kdDialOptions()...)...,
			)
			if err != nil {
				logger.Debug("grpc dial attempt failed, retrying", "err", err)
			} else {
				newClient := bindings.NewKeibiServiceClient(conn)
				s.GRPCClient = newClient
				// Publish the client stack under kd.mu, gated on tearingDown: a reconnect
				// finishing after teardown must not resurrect it.
				kd.mu.Lock()
				if kd.tearingDown.Load() {
					kd.mu.Unlock()
					_ = conn.Close()
					return fmt.Errorf("connect grpc client: shutting down")
				}
				kd.grpcClientConn = conn
				kd.KDClient = newClient
				kd.mu.Unlock()
				logger.Info("connected to grpc server")
				return nil
			}
		}

		time.Sleep(100 * time.Millisecond) // short retry delay
	}
}

// grpcDisconnectGraceDelay is the wait between a peer's Notify(DISCONNECT) arriving and our own
// context cancellation. The DISCONNECT handler returns a response on the same gRPC server we
// then tear down; without this wait the teardown races the response write and crashes the
// server or leaks the Serve()/ClientConn goroutines. Declared as var so tests can shrink it.
var grpcDisconnectGraceDelay = 250 * time.Millisecond

// handleNotifyDisconnect runs the post-DISCONNECT teardown: clear the FUSE view (keeping the
// mount alive for reconnect), then sleep the grace window so the in-flight DISCONNECT response
// flushes before we cancel the context. Must run in a goroutine separate from the gRPC handler,
// which is still writing the response when this runs.
func (kd *KeibiDrop) handleNotifyDisconnect() {
	if kd.FS != nil {
		// Keep the FUSE mount alive across the disconnect: cgofuse allows only one mount per
		// process, so a remount on reconnect is fragile. CancelInFlight cancels in-flight reads
		// but preserves the peer's file view, so a same-peer reconnect finds its files with no
		// re-fetch; a different peer drops them at its next handshake. Full Unmount is on app exit.
		kd.FS.CancelInFlight()
	}
	time.Sleep(grpcDisconnectGraceDelay)
	kd.cancelContext()
}

// kdServerOptions is the single source of the tuned gRPC server options. The TCP server and
// the QUIC control server both use it, so the two transports can't drift apart.
func kdServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(config.GRPCMaxMsgSize),
		grpc.MaxSendMsgSize(config.GRPCMaxMsgSize),
		grpc.InitialWindowSize(config.GRPCWindowSize),
		grpc.InitialConnWindowSize(config.GRPCWindowSize),
		grpc.WriteBufferSize(config.GRPCIOBufferSize),
		grpc.ReadBufferSize(config.GRPCIOBufferSize),
	}
}

// kdDialOptions is the client-side counterpart of kdServerOptions. Per-channel options
// (dialer, credentials) are the caller's; everything tuned lives here once.
func kdDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(config.GRPCMaxMsgSize),
			grpc.MaxCallSendMsgSize(config.GRPCMaxMsgSize),
		),
		grpc.WithInitialWindowSize(config.GRPCWindowSize),
		grpc.WithInitialConnWindowSize(config.GRPCWindowSize),
		grpc.WithWriteBufferSize(config.GRPCIOBufferSize),
		grpc.WithReadBufferSize(config.GRPCIOBufferSize),
	}
}

func (kd *KeibiDrop) startGRPCServer() error {
	// Snapshot kd.session under kd.mu: onReconnected spawns this as a goroutine, so
	// teardown can nil kd.session concurrently. Use the local for every deref below.
	kd.mu.Lock()
	s := kd.session
	kd.mu.Unlock()
	if s == nil || s.Session == nil {
		return ErrInvalidSession
	}
	inboundConn := s.InboundConn()
	s.GRPCListener = inboundConn

	grpcServer := grpc.NewServer(kdServerOptions()...)

	ln := NewSingleConnListener(inboundConn)

	svc := &service.KeibidropServiceImpl{
		Session:     s,
		Logger:      kd.logger.With("component", "keibidrop-server"),
		SyncTracker: kd.SyncTracker,
		// Re-wire the FUSE tree here: a reconnect rebuilds KDSvc but the post-mount wiring in
		// Run() is not re-run, so without this a reconnected FUSE peer would never show
		// notify-received files. kd.FS is nil for non-FUSE and the first connect (Run backfills).
		FS:           kd.FS,
		OnEvent:      kd.OnEvent,
		OnDisconnect: kd.handleNotifyDisconnect,
	}

	bindings.RegisterKeibiServiceServer(grpcServer, svc)

	// Publish the server stack under kd.mu, gated on tearingDown so a reconnect finishing after
	// teardown doesn't resurrect a server on a torn-down session. serveQUICControl polls for
	// this publication to register the same impl on the QUIC server.
	kd.mu.Lock()
	if kd.tearingDown.Load() {
		kd.mu.Unlock()
		grpcServer.Stop()
		return ErrInvalidSession
	}
	kd.grpcServer = grpcServer
	kd.KDSvc = svc
	kd.mu.Unlock()

	kd.logger.Info("Starting gRPC server...")
	if err := grpcServer.Serve(ln); err != nil {
		kd.logger.Error("gRPC server exited with error", "err", err)
	}

	return nil
}

type singleConnListener struct {
	conn      net.Conn
	addr      net.Addr
	done      chan struct{}
	inUse     bool
	closeOnce sync.Once
}

func NewSingleConnListener(conn net.Conn) net.Listener {
	return &singleConnListener{
		conn: conn,
		addr: conn.LocalAddr(),
		done: make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.inUse {
		// Block until Close is called, then return EOF.
		<-l.done
		return nil, io.EOF
	}
	l.inUse = true
	conn := l.conn
	l.conn = nil
	return conn, nil
}

func (l *singleConnListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
	})
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	return l.addr
}
