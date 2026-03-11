// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"google.golang.org/grpc"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

type KeibiDrop struct {
	logger       *slog.Logger
	relayClient  *http.Client
	RelayEndoint *url.URL

	IsFUSE       bool
	OpInProgress atomic.Int32

	session *session.Session

	PeerIPv6IP string

	LocalIPv6IP string
	inboundPort int
	listener    net.Listener

	// Filesystem.
	FS             *filesystem.FS
	KDSvc          *service.KeibidropServiceImpl
	KDClient       bindings.KeibiServiceClient
	grpcClientConn *grpc.ClientConn

	// Non-FUSE fallback.
	SyncTracker *synctracker.SyncTracker

	// Paths for virtual mount point and for save folder.
	ToMount string
	ToSave  string

	// Collab sync options.
	PrefetchOnOpen bool
	PushOnWrite    bool

	// Signals for loop management.
	signals  chan TaskSignal
	running  atomic.Bool
	stopDone chan struct{} // closed when Stop handler completes
	ctx      context.Context
	Cancel   context.CancelFunc // exported so FFI layer can call it for app exit
	shutdown chan struct{}       // closed by Shutdown() to permanently exit Run()
	mu       sync.Mutex

	// For session refresh.
	refreshSession func() *session.Session

	// For stopping the grpc server.
	grpcServer      *grpc.Server
	filesystemReady     chan struct{}
	filesystemReadyOnce sync.Once
	serverReadyMu       sync.Mutex

	// Event callback (wired by FFI layer to push events to the UI).
	OnEvent func(string)

	// Connection resilience.
	HealthMonitor    *session.HealthMonitor
	ReconnectManager *session.ReconnectManager
	RelayKeepalive   *RelayKeepalive
}

type TaskSignal int

const (
	Start TaskSignal = iota
	Stop
)

// Factory-style constructor
func NewKeibiDrop(ctx context.Context, logger *slog.Logger, isFuse bool, relayURL *url.URL, inboundPort int, defaultOutboundPort int, toMount string, toSave string, prefetchOnOpen bool, pushOnWrite bool) (*KeibiDrop, error) {
	ipv6, err := GetGlobalIPv6()
	if err != nil {
		logger.Error("Failed to get local IPv6", "error", err)
		return nil, err
	}

	return NewKeibiDropWithIP(ctx, logger, isFuse, relayURL, inboundPort, defaultOutboundPort, toMount, toSave, prefetchOnOpen, pushOnWrite, ipv6)
}

// NewKeibiDropWithIP is identical to NewKeibiDrop but accepts an explicit IPv6
// address instead of probing the network. This enables testing on machines
// without a global IPv6 address.
func NewKeibiDropWithIP(ctx context.Context, logger *slog.Logger, isFuse bool, relayURL *url.URL, inboundPort int, defaultOutboundPort int, toMount string, toSave string, prefetchOnOpen bool, pushOnWrite bool, ipv6Address string) (*KeibiDrop, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	refreshSession := func() *session.Session {
		session, err := session.InitSession(logger, defaultOutboundPort, inboundPort)
		if err != nil {
			logger.Error("Failed to init session", "error", err)
			return nil
		}

		return session
	}

	session, err := session.InitSession(logger, defaultOutboundPort, inboundPort)
	if err != nil {
		logger.Error("Failed to init session", "error", err)
		return nil, err
	}

	addr := net.JoinHostPort("::", strconv.Itoa(inboundPort))
	listener, err := net.Listen("tcp6", addr)
	if err != nil {
		return nil, err
	}

	// Wrap incoming context so Cancel is always available for disconnect handling.
	ctx, cancel := context.WithCancel(ctx)

	kd := &KeibiDrop{
		logger:          logger,
		IsFUSE:          isFuse,
		relayClient:     client,
		RelayEndoint:    relayURL,
		session:         session,
		LocalIPv6IP:     ipv6Address,
		inboundPort:     inboundPort,
		listener:        listener,
		signals:         make(chan TaskSignal, 2),
		// running is zero-value (false) by default.
		ctx:             ctx,
		Cancel:          cancel,
		shutdown:        make(chan struct{}),
		mu:              sync.Mutex{},
		refreshSession:  refreshSession,
		ToMount:         toMount,
		ToSave:          toSave,
		PrefetchOnOpen:  prefetchOnOpen,
		PushOnWrite:     pushOnWrite,
		filesystemReady: make(chan struct{}),
		serverReadyMu:   sync.Mutex{},
		SyncTracker:     synctracker.NewSyncTracker(),
	}

	return kd, nil
}

type PeerRegistration struct {
	Fingerprint string            `json:"fingerprint"`
	PublicKeys  map[string]string `json:"public_keys"` // base64 encoded
	Listen      *ConnectionHint   `json:"listen"`
	Reverse     *ConnectionHint   `json:"reverse,omitempty"`
	Timestamp   int64             `json:"timestamp"`
}

type ConnectionHint struct {
	IP    string `json:"ip"`             // public IP address (either v4 or v6)
	Port  int    `json:"port"`           // where peer is listening
	IPv6  bool   `json:"ipv6"`           // does this prefer IPv6?
	Proto string `json:"proto"`          // e.g., "tcp"
	Note  string `json:"note,omitempty"` // optional: NAT behavior, etc.
}

// EncryptedRegistration is the relay-visible payload (opaque blob).
// The relay cannot read the contents - only the peers with the shared
// room password can decrypt it.
type EncryptedRegistration struct {
	Blob string `json:"blob"` // base64-encoded ChaCha20-Poly1305 ciphertext
}

// Map server status errors to semantic errors.
type ErrorMapperFunc func(statusCode int, err error) error

// IsRunning returns whether the KeibiDrop instance is in a connected session.
func (kd *KeibiDrop) IsRunning() bool {
	return kd.running.Load()
}

// Running process for KeibiDrop.
func (kd *KeibiDrop) Start() {
	kd.signals <- Start
}

func (kd *KeibiDrop) Stop() {
	kd.running.Store(false)
	done := make(chan struct{})
	kd.stopDone = done
	select {
	case kd.signals <- Stop:
		<-done // wait for Stop handler to complete
	case <-kd.ctx.Done():
		// Context already cancelled — Run() loop will handle its own cleanup.
		// Just refresh context here so Run() can continue.
		ctx, c := context.WithCancel(context.Background())
		kd.ctx = ctx
		kd.Cancel = c
	}
}

// Shutdown permanently stops the Run() goroutine. Use this for app exit.
// For temporary disconnects (peer left), use Stop() instead.
func (kd *KeibiDrop) Shutdown() {
	close(kd.shutdown)
	if kd.Cancel != nil {
		kd.Cancel()
	}
}

// Run as a go-routine.
func (kd *KeibiDrop) Run() {
	logger := kd.logger.With("method", "run-state")
	for {
		select {
		case <-kd.ctx.Done():
			logger.Info("Stopping KeibiDrop run instance (ctx cancelled)")
			kd.StopConnectionResilience()
			// Nil out managers so late health-monitor callbacks are no-ops.
			kd.HealthMonitor = nil
			kd.ReconnectManager = nil
			kd.RelayKeepalive = nil
			if kd.FS != nil {
				kd.FS.Unmount()
				kd.FS = nil
			}
			if kd.grpcServer != nil {
				kd.grpcServer.Stop() // Force stop — GracefulStop blocks on open streams.
				kd.grpcServer = nil
			}
			if kd.grpcClientConn != nil {
				kd.grpcClientConn.Close()
				kd.grpcClientConn = nil
			}
			kd.KDClient = nil
			kd.KDSvc = nil
			kd.session = nil
			kd.PeerIPv6IP = ""

			// Permanent shutdown — exit the goroutine.
			select {
			case <-kd.shutdown:
				kd.running.Store(false)
				logger.Info("Run loop: permanent shutdown")
				return
			default:
			}

			// Temporary disconnect — refresh session and context BEFORE
			// setting running=false, so callers can use the session immediately.
			kd.session = kd.refreshSession()
			ctx, c := context.WithCancel(context.Background())
			kd.ctx = ctx
			kd.Cancel = c
			kd.running.Store(false)
			logger.Info("Run loop: context refreshed, ready for next session")
			continue
		case s := <-kd.signals:
			switch s {
			case Start:
				logger.Info("Signal start")
				if kd.session == nil || kd.session.Session == nil || kd.session.Session.Inbound == nil {
					logger.Warn("Nil session")
					continue
				}

				logger.Info("Signal start success")

				// prepare serverReady channel
				go func() {
					err := kd.startGRPCServer()
					if err != nil {
						logger.Error("Failed to start gRPC server", "error", err)
					}
				}()

				// Wait for filesystem setup (or context cancellation).
				select {
				case <-kd.filesystemReady:
				case <-kd.ctx.Done():
					logger.Info("Context cancelled while waiting for filesystem")
					continue
				}

				if kd.FS != nil {
					logger.Info("Mounting filesystem", "mount", kd.ToMount, "save", kd.ToSave)
					kd.KDSvc.FS = kd.FS
					kd.FS.Mount(filepath.Clean(kd.ToMount), false, filepath.Clean(kd.ToSave))
					logger.Info("Filesystem mounted successfully")
				} else {
					logger.Warn("No FS to mount")
				}

				// Only mark running if we haven't been asked to stop while
				// Mount() was blocking. Check both context cancellation AND
				// whether FS was externally unmounted (nilled by UnmountFilesystem).
				if kd.FS == nil && kd.IsFUSE {
					logger.Info("FS externally unmounted during mount, skipping running=true")
					continue
				}
				select {
				case <-kd.ctx.Done():
					logger.Info("Context cancelled during mount, skipping running=true")
					continue
				default:
					kd.running.Store(true)
				}

			case Stop:
				logger.Info("Stop signal")
				kd.StopConnectionResilience()
				kd.HealthMonitor = nil
				kd.ReconnectManager = nil
				kd.RelayKeepalive = nil
				if kd.FS != nil {
					kd.FS.Unmount()
					kd.FS = nil
				}
				if kd.grpcServer != nil {
					kd.grpcServer.Stop()
					kd.grpcServer = nil
				}

				// Close and nil gRPC client connection.
				if kd.grpcClientConn != nil {
					kd.grpcClientConn.Close()
					kd.grpcClientConn = nil
				}
				kd.KDClient = nil

				if kd.KDSvc != nil {
					kd.KDSvc = nil
				}

				if kd.session != nil {
					kd.session = nil
				}

				kd.PeerIPv6IP = ""
				kd.session = kd.refreshSession()

				kd.running.Store(false)
				logger.Info("Stop signal completed, ready for next session")
				if kd.stopDone != nil {
					close(kd.stopDone)
				}
			}
		}
	}
}
