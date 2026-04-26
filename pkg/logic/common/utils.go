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

func (kd *KeibiDrop) registerRoomToRelay() error {
	logger := kd.logger.With("method", "register-room-to-relay")
	if kd.relayClient == nil || kd.session == nil || kd.session.OwnKeys == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	ownFp := kd.session.OwnFingerprint

	// Extract room password and derive relay keys for privacy.
	roomPassword, err := crypto.ExtractRoomPassword(ownFp)
	if err != nil {
		logger.Error("Failed to extract room password", "error", err)
		return err
	}

	lookupKey, encryptionKey, err := crypto.DeriveRelayKeys(roomPassword)
	if err != nil {
		logger.Error("Failed to derive relay keys", "error", err)
		return err
	}
	lookupToken := base64.RawURLEncoding.EncodeToString(lookupKey)

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

	path, err := url.JoinPath(kd.RelayEndoint.String(), "register")
	if err != nil {
		logger.Error("Failed to add register path", "error", err)
		return err
	}

	registerUrl, err := url.Parse(path)
	if err != nil {
		logger.Error("Failed to parse url", "error", err)
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

	// Extract room password and derive relay keys for privacy.
	roomPassword, err := crypto.ExtractRoomPassword(outOfBandFingerPrint)
	if err != nil {
		logger.Error("Failed to extract room password", "error", err)
		return err
	}

	lookupKey, encryptionKey, err := crypto.DeriveRelayKeys(roomPassword)
	if err != nil {
		logger.Error("Failed to derive relay keys", "error", err)
		return err
	}
	lookupToken := base64.RawURLEncoding.EncodeToString(lookupKey)

	path, err := url.JoinPath(kd.RelayEndoint.String(), "fetch")
	if err != nil {
		logger.Error("Failed to add fetch path", "error", err)
		return err
	}

	fetchUrl, err := url.Parse(path)
	if err != nil {
		logger.Error("Failed to parse url", "error", err)
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
	if peerReg.Listen.Port < 26000 || peerReg.Listen.Port > 27000 {
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

func isValidIPv6(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	// TODO: Comment
	return ip != nil && ip.To4() == nil
	// return ip != nil && ip.To4() == nil && !ip.IsLoopback() && !ip.IsPrivate()
}

//

func (kd *KeibiDrop) setupFilesystem(logger *slog.Logger, ready chan struct{}) error {
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Warn("Session or gRPC client not initialized")
		return ErrNilPointer
	}

	fs := filesystem.NewFS(logger)
	kd.FS = fs

	// Set collab sync options.
	fs.PrefetchOnOpen = kd.PrefetchOnOpen
	fs.PushOnWrite = kd.PushOnWrite

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
	kd.notifyCh = make(chan *bindings.NotifyRequest, 2048)
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
			if kd.session == nil || kd.session.GRPCClient == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := kd.session.GRPCClient.BatchNotify(ctx, &bindings.BatchNotifyRequest{
				Notifications: batch,
				Seq:           batchSeq.Add(1),
				Timestamp:     uint64(time.Now().UnixNano()),
			})
			cancel()
			if err != nil {
				logger.Error("BatchNotify failed, falling back to individual", "count", len(batch), "error", err)
				for _, req := range batch {
					ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
					_, _ = kd.session.GRPCClient.Notify(ctx2, req)
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
						if p.req.Attr != nil && kd.FS != nil {
							diskPath := filepath.Join(kd.FS.Root.LocalDownloadFolder, p.req.Path)
							if info, err := os.Lstat(diskPath); err == nil {
								p.req.Attr.Size = info.Size()
								p.req.Attr.ModificationTime = uint64(info.ModTime().UnixNano())
							} else if os.IsNotExist(err) {
								delete(pending, path)
								continue
							}
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
						if p.req.Attr != nil && kd.FS != nil {
							diskPath := filepath.Join(kd.FS.Root.LocalDownloadFolder, p.req.Path)
							if info, err := os.Lstat(diskPath); err == nil {
								p.req.Attr.Size = info.Size()
								p.req.Attr.ModificationTime = uint64(info.ModTime().UnixNano())
							} else if os.IsNotExist(err) {
								delete(pending, path)
								continue
							}
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
		default:
			logger.Warn("Notification queue full, dropping", "path", event.Path)
		}
	}

	fs.OpenStreamProvider = func() types.FileStreamProvider {
		return NewImplStreamProvider(kd.session.GRPCClient)
	}

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

//

func (kd *KeibiDrop) startGRPCServer() error {
	kd.session.GRPCListener = kd.session.Session.Inbound

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(config.GRPCMaxMsgSize),
		grpc.MaxSendMsgSize(config.GRPCMaxMsgSize),
	)
	kd.grpcServer = grpcServer

	ln := NewSingleConnListener(kd.session.Session.Inbound)

	svc := &service.KeibidropServiceImpl{
		Session:     kd.session,
		Logger:      kd.logger.With("component", "keibidrop-server"),
		SyncTracker: kd.SyncTracker,
		OnEvent:     kd.OnEvent,
		OnDisconnect: func() {
			// Unmount first to unblock Mount() in Run()'s Start handler.
			// Mount() blocks the select loop, so ctx.Done() can't fire
			// until Mount() returns (which only happens on Unmount).
			if kd.FS != nil {
				kd.FS.Unmount()
			}
			kd.cancelContext()
		},
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
