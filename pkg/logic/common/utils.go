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
	if kd.relayClient == nil || kd.session == nil || kd.session.OwnKeys == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	ownFp := kd.session.OwnFingerprint

	lookupToken, encryptionKey, err := deriveRelayAuth(ownFp)
	if err != nil {
		logger.Error("Failed to derive relay auth", "error", err)
		return err
	}

	pkMap, err := kd.session.OwnKeys.ExportPubKeysAsMap()
	if err != nil {
		logger.Error("Failed to export own keys", "error", err)
		return err
	}

	peerReg := PeerRegistration{
		Fingerprint: ownFp,
		Listen: &ConnectionHint{
			IPv6:  true,
			IP:    kd.LocalIPv6IP,
			Proto: "tcp",
			Port:  kd.inboundPort,
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
	if kd.relayClient == nil || kd.session == nil || kd.session.OwnKeys == nil {
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

	kd.session.PeerPubKeys = peerKeys
	if !config.ValidPeerPort(peerReg.Listen.Port) {
		logger.Warn("Provided outbound port is out of known range, defaulting to config", "provided-port", peerReg.Listen.Port, "default-to", config.OutboundPort)
		peerReg.Listen.Port = config.OutboundPort
	}

	kd.session.PeerPort = peerReg.Listen.Port
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

// refreshAttrFromDisk updates req.Attr's Size/ModTime from the on-disk file
// before a debounced notification is sent (the size captured when the event was
// queued may be stale). It returns false only when the file no longer exists,
// signalling the caller to drop the pending notification.
func refreshAttrFromDisk(req *bindings.NotifyRequest, downloadFolder string) bool {
	if req.Attr == nil {
		return true
	}
	info, err := os.Lstat(filepath.Join(downloadFolder, req.Path))
	if err != nil {
		// Option A: file is gone at flush — it either flickered (rename/replace)
		// during the 200ms debounce, or is a true transient (.keep/.lock). Do NOT
		// signal a drop: keep the ADD with the attr captured at enqueue. Silently
		// dropping here loses REAL files that merely flickered (e.g. files generated
		// then atomically replaced); a true phantom is harmless because the peer's
		// on-demand Read returns EOF for a gone remote file (see fuse Read handler).
		return true
	}
	req.Attr.Size = info.Size()
	req.Attr.ModificationTime = uint64(info.ModTime().UnixNano())
	return true
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

	// Notification worker with per-path debounce and batching.
	//
	// Problem: LFS downloads a 420MB file over several seconds. Each intermediate
	// write triggers ADD_FILE. Without debounce, the peer restarts prefetch 10+
	// times and never gets the correct content.
	//
	// Solution: Per-path debounce. For ADD_FILE/EDIT_FILE, we track the last
	// notification per path. Each update resets a 200ms timer for that path.
	// Only when the path is stable for 200ms do we include it in the batch.
	// RENAME/REMOVE/ADD_DIR are sent immediately (no debounce).
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
			// Capture the client under the nil check: Run()'s ctx.Done teardown
			// nils kd.session concurrently, so re-dereferencing kd.session below
			// would race into a nil-deref panic. Holding a local reference is the
			// same pattern connectGRPCClientWithRetry uses for the outbound conn.
			if kd.session == nil || kd.session.GRPCClient == nil {
				return
			}
			client := kd.session.GRPCClient
			seq := batchSeq.Add(1)
			// Full lifecycle trace: log every path on the wire (path/type/size/mtime).
			for _, n := range batch {
				var sz int64 = -1
				var mt uint64
				if n.Attr != nil {
					sz = n.Attr.Size
					mt = n.Attr.ModificationTime
				}
				logger.Info("notify-send", "seq", seq, "path", n.Path, "type", n.Type, "oldPath", n.OldPath, "size", sz, "mtime", mt)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := client.BatchNotify(ctx, &bindings.BatchNotifyRequest{
				Notifications: batch,
				Seq:           seq,
				Timestamp:     uint64(time.Now().UnixNano()),
			})
			cancel()
			if err != nil {
				logger.Error("BatchNotify failed, falling back to individual", "count", len(batch), "seq", seq, "error", err)
				for _, req := range batch {
					ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
					if _, ferr := client.Notify(ctx2, req); ferr != nil {
						logger.Warn("notify-drop: individual fallback Notify failed", "path", req.Path, "type", req.Type, "error", ferr)
					}
					cancel2()
				}
			}
		}

		for {
			select {
			case req, ok := <-kd.notifyCh:
				if !ok {
					// Channel closed — flush all pending with fresh sizes.
					remaining := make([]*bindings.NotifyRequest, 0, len(pending))
					for path, p := range pending {
						if kd.FS != nil && kd.FS.Root != nil && !refreshAttrFromDisk(p.req, kd.FS.Root.LocalDownloadFolder) {
							cp := filepath.Join(kd.FS.Root.LocalDownloadFolder, path)
							_, se := os.Lstat(cp)
							logger.Warn("notify-drop: refreshAttrFromDisk false → DROPPING notification", "path", path, "type", p.req.Type, "action", p.req.Type, "checkedPath", cp, "lstatErr", se, "wantSize", func() int64 {
								if p.req.Attr != nil {
									return p.req.Attr.Size
								}
								return -1
							}())
							delete(pending, path)
							continue
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

				switch req.Type {
				case bindings.NotifyType_ADD_FILE, bindings.NotifyType_EDIT_FILE:
					// Per-path debounce: update pending and reset deadline.
					// Only send when the path is stable for 200ms.
					pending[req.Path] = &pendingNotify{
						req:      req,
						deadline: time.Now().Add(200 * time.Millisecond),
					}
				case bindings.NotifyType_RENAME_FILE, bindings.NotifyType_RENAME_DIR:
					// RENAME: send immediately. If there's a pending ADD_FILE for
					// the old path, re-target it to the new path so the peer still
					// downloads the content (at the correct path after rename).
					if old, exists := pending[req.OldPath]; exists {
						delete(pending, req.OldPath)
						old.req.Path = req.Path // retarget to new path
						pending[req.Path] = old
					}
					immediate = append(immediate, req)
				default:
					// REMOVE, ADD_DIR, REMOVE_DIR, DISCONNECT — send immediately.
					// Skip REMOVE if there's a debounced ADD for the same path:
					// the file was deleted and recreated quickly (e.g., initdb).
					if req.Type == bindings.NotifyType_REMOVE_FILE || req.Type == bindings.NotifyType_REMOVE_DIR {
						if _, hasPending := pending[req.Path]; hasPending {
							continue
						}
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
						if kd.FS != nil && kd.FS.Root != nil && !refreshAttrFromDisk(p.req, kd.FS.Root.LocalDownloadFolder) {
							cp := filepath.Join(kd.FS.Root.LocalDownloadFolder, path)
							_, se := os.Lstat(cp)
							logger.Warn("notify-drop: refreshAttrFromDisk false → DROPPING notification", "path", path, "type", p.req.Type, "action", p.req.Type, "checkedPath", cp, "lstatErr", se, "wantSize", func() int64 {
								if p.req.Attr != nil {
									return p.req.Attr.Size
								}
								return -1
							}())
							delete(pending, path)
							continue
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
		if kd.session == nil || kd.session.GRPCClient == nil {
			logger.Warn("notify-drop: session/grpc client nil at OnLocalChange", "path", event.Path, "action", event.Action)
			return
		}

		req := &bindings.NotifyRequest{
			Type:    bindings.NotifyType(event.Action), // #nosec G115 -- action values are small enums
			Path:    event.Path,
			OldPath: event.OldPath, // For RENAME operations.
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
		// If the channel is full (1024 pending), drop the notification
		// rather than blocking the FUSE handler.
		select {
		case kd.notifyCh <- req:
			var esz int64 = -1
			var emt uint64
			if event.Attr != nil {
				esz = event.Attr.Size
				emt = event.Attr.ModificationTime
			}
			logger.Info("notify-enqueue", "path", event.Path, "action", event.Action, "oldPath", event.OldPath, "size", esz, "mtime", emt)
		default:
			logger.Warn("notify-drop: queue full (channel 16384 saturated)", "path", event.Path, "action", event.Action)
		}
	}

	fs.OpenStreamProvider = func() types.FileStreamProvider {
		return NewImplStreamProvider(kd.session.GRPCClient)
	}

	fs.RefreshCallbacks()

	if ready != nil {
		kd.filesystemReadyOnce.Do(func() {
			close(ready)
		})
	}

	return nil
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

		// only try if the outbound connection is available
		if kd.session != nil && kd.session.Session != nil && kd.session.Session.Outbound != nil {
			// Capture the outbound conn so the dialer closure is safe even
			// if kd.session is nil'd later by the Stop handler.
			outboundConn := kd.session.Session.Outbound

			dialer := func(ctx context.Context, _ string) (net.Conn, error) {
				return outboundConn, nil
			}

			conn, err := grpc.Dial( //nolint:staticcheck // SA1019: custom dialer over hijacked conn
				"keibipipe",
				grpc.WithContextDialer(dialer),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(config.GRPCMaxMsgSize),
					grpc.MaxCallSendMsgSize(config.GRPCMaxMsgSize),
				),
				grpc.WithInitialWindowSize(config.GRPCWindowSize),
				grpc.WithInitialConnWindowSize(config.GRPCWindowSize),
				grpc.WithWriteBufferSize(config.GRPCIOBufferSize),
				grpc.WithReadBufferSize(config.GRPCIOBufferSize),
			)
			if err != nil {
				logger.Debug("grpc dial attempt failed, retrying", "err", err)
			} else {
				kd.grpcClientConn = conn
				kd.session.GRPCClient = bindings.NewKeibiServiceClient(conn)
				kd.KDClient = kd.session.GRPCClient
				logger.Info("connected to grpc server")
				return nil
			}
		}

		time.Sleep(100 * time.Millisecond) // short retry delay
	}
}

// grpcDisconnectGraceDelay is the wait between a peer's Notify(DISCONNECT)
// arriving and our own context cancellation. The DISCONNECT handler returns
// a response on the same gRPC server we then tear down; without this wait
// the teardown races the response write and either crashes the server (the
// original bug) or, post-#121, leaks the Serve() and ClientConn maintenance
// goroutines once Stop()/Close() are restored on the cleanup path. See #122.
//
// Declared as var (not const) so tests can shrink it.
var grpcDisconnectGraceDelay = 250 * time.Millisecond

// handleNotifyDisconnect runs the post-DISCONNECT teardown: clear the FUSE
// view (keeping the mount alive for reuse on reconnect), then sleep the grace
// window to let the in-flight DISCONNECT RPC response flush before we cancel
// the context (which the Run() ctx.Done branch uses to call grpcServer.Stop()
// and grpcClientConn.Close()).
//
// MUST be invoked in a goroutine separate from the gRPC handler — the
// handler is still writing the response when this runs.
func (kd *KeibiDrop) handleNotifyDisconnect() {
	if kd.FS != nil {
		// Keep the FUSE mount alive across the disconnect — cgofuse allows only
		// one mount per process, so a fresh remount on reconnect is fragile and
		// can fail (leaving the mount point dead). ClearFiles drops the peer's
		// file view and cancels in-flight reads but leaves the mount up; Run()'s
		// Start handler then reuses it on reconnect (it returns via the ctx.Done
		// select, not by Mount() unblocking). Full Unmount stays on app exit.
		kd.FS.ClearFiles()
	}
	time.Sleep(grpcDisconnectGraceDelay)
	kd.cancelContext()
}

func (kd *KeibiDrop) startGRPCServer() error {
	kd.session.GRPCListener = kd.session.Session.Inbound

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(config.GRPCMaxMsgSize),
		grpc.MaxSendMsgSize(config.GRPCMaxMsgSize),
		grpc.InitialWindowSize(config.GRPCWindowSize),
		grpc.InitialConnWindowSize(config.GRPCWindowSize),
		grpc.WriteBufferSize(config.GRPCIOBufferSize),
		grpc.ReadBufferSize(config.GRPCIOBufferSize),
	)
	kd.grpcServer = grpcServer

	ln := NewSingleConnListener(kd.session.Session.Inbound)

	svc := &service.KeibidropServiceImpl{
		Session:      kd.session,
		Logger:       kd.logger.With("component", "keibidrop-server"),
		SyncTracker:  kd.SyncTracker,
		OnEvent:      kd.OnEvent,
		OnDisconnect: kd.handleNotifyDisconnect,
	}

	kd.KDSvc = svc
	bindings.RegisterKeibiServiceServer(grpcServer, svc)

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
