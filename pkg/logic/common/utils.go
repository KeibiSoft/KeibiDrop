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
	var encResp EncryptedRegistration
	if err := json.NewDecoder(resp.Body).Decode(&encResp); err != nil {
		logger.Error("Failed to decode encrypted response", "error", err)
		return err
	}
	_ = resp.Body.Close()

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
	if !isValidIPv6(peerReg.Listen.IP) {
		logger.Warn("Invalid peer IP", "got", peerReg.Listen.IP, "error", ErrInvalidIP)
		return ErrInvalidIP
	}

	kd.PeerIPv6IP = peerReg.Listen.IP

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

	fs.OnLocalChange = func(event types.FileEvent) {
		if kd.session == nil || kd.session.GRPCClient == nil {
			return
		}

		req := &bindings.NotifyRequest{
			Type:    bindings.NotifyType(event.Action),
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
			logger.Info(">>> SENDING NOTIFICATION TO PEER",
				"path", event.Path,
				"action", event.Action,
				"size", event.Attr.Size)
		}

		_, err := kd.session.GRPCClient.Notify(context.Background(), req)
		if err != nil {
			logger.Error("Failed to notify peer", "error", err)
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

		// only try if the inbound connection (single-conn listener) is non-nil
		if kd.session != nil && kd.session.Session != nil && kd.session.Session.Inbound != nil {
			// create gRPC client using the hijacked outbound conn
			dialer := func(ctx context.Context, _ string) (net.Conn, error) {
				return kd.session.Session.Outbound, nil
			}

			conn, err := grpc.Dial(
				"keibipipe", // fake target name
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
	conn   net.Conn
	done   chan struct{}
	inUse  bool
	closed chan struct{}
}

func NewSingleConnListener(conn net.Conn) net.Listener {
	return &singleConnListener{
		conn:   conn,
		done:   make(chan struct{}),
		closed: make(chan struct{}),
		inUse:  false,
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, io.EOF
	default:
		if l.inUse {
			<-l.closed
			return nil, nil
		}
		l.inUse = true
		conn := l.conn
		l.conn = nil
		return conn, nil
	}
}

func (l *singleConnListener) Close() error {
	if l.conn != nil {
		close(l.closed)
		close(l.done)
		return l.conn.Close()
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return &net.TCPAddr{IP: net.IPv6loopback, Port: 0}
}
