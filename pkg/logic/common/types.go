// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"fmt"
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
	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/transport"
	"google.golang.org/grpc"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

type KeibiDrop struct {
	logger       *slog.Logger
	relayClient  *http.Client
	RelayEndoint *url.URL

	Identity       *identity.DeviceIdentity
	AddressBook    *identity.AddressBook
	Incognito      bool
	identityOpts   EnableOpts
	identityConfig string

	IsFUSE         bool
	IsLocalMode    bool
	BridgeAddr     string // TCP bridge relay address for firewall traversal
	StrictMode     bool   // Disable data relay fallback (direct connections only)
	ConnectionMode string // "lan", "direct", "bridge" - set after successful connection
	OpInProgress   atomic.Int32

	session *session.Session

	PeerIPv6IP     string
	PeerLocalAddrs []string // LAN IPs from relay registration (for same-network direct connect)

	LocalIPv6IP string
	inboundPort int
	listener    net.Listener

	// Filesystem.
	FS             *filesystem.FS
	KDSvc          *service.KeibidropServiceImpl
	KDClient       bindings.KeibiServiceClient
	grpcClientConn *grpc.ClientConn

	// QUIC control channel (0.4.0 Track B) — a UDP gRPC pair mirroring the TCP pair,
	// for control + metadata + surviving IP changes, while file transfer stays on TCP.
	// Brought up whenever the peer negotiated QUIC keys (SEK*QUIC from the handshake
	// payload); fail-soft, so a failure just leaves the session TCP-only. See
	// docs/transport-architecture.html.
	quicControlClient *grpc.ClientConn
	quicControlServer *grpc.Server
	quicControlLn     net.Listener              // generation marker: serveQUICControl only attaches to the current one
	quicControlMig    *transport.MigratableConn // migration handle: MigrateQUICControl moves the path, gRPC never notices
	quicControlSC     *session.SecureConn       // dial-side conn: epoch observability across the transport manager (QUICWriterEpoch)
	quicPeerAddr      string                    // peer's UDP control endpoint, kept for the self-heal redial
	quicRedialing     atomic.Bool               // single-flight guard: at most one background redial
	quicMetaSent      atomic.Uint64             // metadata RPCs that actually rode the QUIC channel (diagnostics/tests)

	// Non-FUSE fallback.
	SyncTracker *synctracker.SyncTracker

	// Paths for virtual mount point and for save folder.
	ToMount string
	ToSave  string

	// Collab sync options.
	PrefetchOnOpen bool
	PushOnWrite    bool
	// AutoCache (live_collab) enables the macFUSE auto_cache mount option so a
	// peer's same-size in-place edit is seen live. Set by the caller from
	// config.LiveCollab (not a constructor arg). Default false = git-safe.
	AutoCache         bool
	PrefetchAutoMB    int // files >= this many MB auto-prefetch on open; set from config.PrefetchAutoMB (0=off)
	ReadAheadWindowMB int // cap (MB) for predictive sequential read-ahead on on-demand reads; set from config.ReadAheadWindowMB (0=off)

	// Signals for loop management.
	signals      chan TaskSignal
	running      atomic.Bool
	stopDone     chan struct{} // closed when Stop handler completes
	ctx          context.Context
	Cancel       context.CancelFunc // exported so FFI layer can call it for app exit
	shutdown     chan struct{}      // closed by Shutdown() to permanently exit Run()
	shutdownOnce sync.Once
	mu           sync.Mutex

	// For session refresh.
	refreshSession func() *session.Session

	// For stopping the grpc server.
	grpcServer          *grpc.Server
	filesystemReady     chan struct{}
	filesystemReadyOnce sync.Once
	serverReadyMu       sync.Mutex

	// Event callback (wired by FFI layer to push events to the UI).
	OnEvent func(string)

	// OnPeerVerified fires with the peer's verified fingerprint the instant the
	// handshake confirms identity, before any files sync. The handshake only EMITS
	// this; it knows nothing of what reacts. Wired in setupFilesystem to scope the
	// FUSE cache to the peer (drop a different peer's view, keep the same peer's).
	OnPeerVerified func(fp string)

	// Connection resilience.
	HealthMonitor    *session.HealthMonitor
	ReconnectManager *session.ReconnectManager
	RelayKeepalive   *RelayKeepalive

	// Notification queue — bounded channel to avoid spawning 600+ goroutines
	// during large clones. A single worker drains and sends sequentially.
	notifyCh chan *bindings.NotifyRequest

	// pendingNotifies counts REMOVE/RENAME/ADD_DIR notifies that are queued or in
	// flight but not yet delivered. The notify worker has no retry queue and
	// notifyRestoredFiles replays only ADD_FILE, so a proactive rekey that dropped
	// the socket while this is non-zero would silently lose the delete/rename and
	// diverge the peers. onRekeyNeeded defers while it is > 0. See #6.
	pendingNotifies atomic.Int64

	// tearingDown is set at the top of Run's ctx.Done teardown so the reconnect goroutine
	// (onReconnected) bails instead of rebuilding the session/gRPC stack that teardown is
	// concurrently niling. Serializes the reconnect-vs-teardown lifecycle. See the
	// SESSION-TEARDOWN-RACE report.
	tearingDown atomic.Bool

	// eagerFoldInFlight is a single-flight guard for the eager entropy fold: the initiator
	// runs at most one fold round at a time (a duplicate would be superseded by the
	// SecureConn's single-use staging anyway). Cleared when the round finishes; a reconnect
	// re-folds on the fresh session key.
	eagerFoldInFlight atomic.Bool

	// Active downloads registry for pause/cancel support.
	activeDownloads   map[string]context.CancelFunc
	activeBitmaps     map[string]*filesystem.ChunkBitmap
	activeDownloadsMu sync.Mutex

	// Download registry: tracks which bitmaps belong to which peer (privacy-preserving).
	dlRegistry  *downloadRegistry
	registryKey []byte

	// Preserved shared files from previous session (only served to same peer).
	lastSharedPeerFP string
	lastSharedFiles  map[string]*synctracker.File
	sharedStore      *sharedFilesStore
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
		// Non-fatal: Android restricts net.Interfaces(). Mobile peers use
		// the bridge relay (outbound-only), so a local IPv6 isn't required.
		logger.Warn("Failed to get local IPv6 (non-fatal on mobile)", "error", err)
		ipv6 = ""
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

	addr := net.JoinHostPort("", strconv.Itoa(inboundPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	// Wrap incoming context so Cancel is always available for disconnect handling.
	ctx, cancel := context.WithCancel(ctx)

	kd := &KeibiDrop{
		logger:       logger,
		IsFUSE:       isFuse,
		relayClient:  client,
		RelayEndoint: relayURL,
		session:      session,
		LocalIPv6IP:  ipv6Address,
		inboundPort:  inboundPort,
		listener:     listener,
		signals:      make(chan TaskSignal, 2),
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
		activeDownloads: make(map[string]context.CancelFunc),
		activeBitmaps:   make(map[string]*filesystem.ChunkBitmap),
		dlRegistry:      newDownloadRegistry("", nil),
	}

	return kd, nil
}

// EnableOpts controls how persistent identity is loaded:
//   - PassphraseProtect: opt into Tier 2 (passphrase-derived key).
//     Requires PassphraseProvider to be non-nil.
//   - PassphraseProvider: called once when PassphraseProtect=true to obtain
//     the user's passphrase. Typical sources: TTY prompt (CLI),
//     stdin (rustbridge / FFI), test stub.
type EnableOpts struct {
	PassphraseProtect  bool
	PassphraseProvider func() (string, error)
	ExternalMaster     []byte // 32-byte key from mobile bridge (iOS Keychain / Android Keystore)
}

// EnablePersistentIdentity replaces ephemeral keys with a stable device identity.
// Must be called before CreateRoom/JoinRoom. Loads or creates identity from configDir,
// rebuilds the session with stable keys, and loads the address book.
func (kd *KeibiDrop) EnablePersistentIdentity(configDir string, opts EnableOpts) error {
	logger := kd.logger.With("method", "enable-persistent-identity", "configDir", configDir, "passphrase", opts.PassphraseProtect)
	logger.Info("Starting")

	src, err := identity.NewMasterKeySource(identity.KeySourceOpts{
		ConfigDir:          configDir,
		PassphraseProtect:  opts.PassphraseProtect,
		PassphraseProvider: opts.PassphraseProvider,
		ExternalMaster:     opts.ExternalMaster,
	})
	if err != nil {
		return fmt.Errorf("init key source: %w", err)
	}

	logger.Info("Key source tier selected", "tier", src.Tier())

	id, err := identity.LoadOrCreate(configDir, src)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}

	ab, err := identity.LoadAddressBook(configDir, src)
	if err != nil {
		return fmt.Errorf("load address book: %w", err)
	}

	outPort := kd.session.DefaultOutboundPort
	inPort := kd.session.DefaultInboundPort
	sess, err := session.InitSessionWithKeys(kd.logger, id.Keys, outPort, inPort)
	if err != nil {
		return fmt.Errorf("init session with keys: %w", err)
	}

	kd.Identity = id
	kd.AddressBook = ab
	sess.ExtraFoldConns = kd.quicFoldConns // fold rounds cover the QUIC lane too
	kd.mu.Lock()
	kd.session = sess
	kd.mu.Unlock()
	kd.identityOpts = opts
	kd.identityConfig = configDir
	regKey := opts.ExternalMaster
	if len(regKey) == 0 {
		regKey = []byte(id.Fingerprint)
	}
	kd.registryKey = regKey
	kd.dlRegistry = newDownloadRegistry(configDir, regKey)
	kd.sharedStore = newSharedFilesStore(configDir, regKey)

	kd.refreshSession = func() *session.Session {
		s, err := session.InitSessionWithKeys(kd.logger, id.Keys, outPort, inPort)
		if err != nil {
			logger.Error("Failed to refresh persistent session", "error", err)
			return nil
		}
		s.ExtraFoldConns = kd.quicFoldConns // fold rounds cover the QUIC lane too
		return s
	}

	logger.Info("Persistent identity enabled", "fingerprint", id.Fingerprint)
	return nil
}

// EnablePersistentIdentityDefault is a convenience wrapper that uses zero-value
// EnableOpts (keychain or file tier, no passphrase).
func (kd *KeibiDrop) EnablePersistentIdentityDefault(configDir string) error {
	return kd.EnablePersistentIdentity(configDir, EnableOpts{})
}

// ToggleIncognito switches between persistent and ephemeral identity.
// When enabling incognito: generates fresh ephemeral keys.
// When disabling: restores persistent identity keys.
// Returns the new fingerprint.
func (kd *KeibiDrop) ToggleIncognito(incognito bool, configDir string) (string, error) {
	if incognito {
		kd.Incognito = true
		outPort := kd.session.DefaultOutboundPort
		inPort := kd.session.DefaultInboundPort
		sess, err := session.InitSession(kd.logger, outPort, inPort)
		if err != nil {
			return "", fmt.Errorf("init ephemeral session: %w", err)
		}
		sess.ExtraFoldConns = kd.quicFoldConns // fold rounds cover the QUIC lane too
		kd.mu.Lock()
		kd.session = sess
		kd.mu.Unlock()
		kd.refreshSession = func() *session.Session {
			s, err := session.InitSession(kd.logger, outPort, inPort)
			if err != nil {
				kd.logger.Error("Failed to refresh ephemeral session", "error", err)
				return nil
			}
			s.ExtraFoldConns = kd.quicFoldConns // fold rounds cover the QUIC lane too
			return s
		}
		return sess.OwnFingerprint, nil
	}

	kd.Incognito = false
	if err := kd.EnablePersistentIdentity(configDir, kd.identityOpts); err != nil {
		kd.logger.Warn("Failed to restore persistent identity", "error", err)
		fp := ""
		if kd.session != nil {
			fp = kd.session.OwnFingerprint
		}
		return fp, err
	}
	return kd.Identity.Fingerprint, nil
}

type PeerRegistration struct {
	Fingerprint string            `json:"fingerprint"`
	PublicKeys  map[string]string `json:"public_keys"` // base64 encoded
	Listen      *ConnectionHint   `json:"listen"`
	Reverse     *ConnectionHint   `json:"reverse,omitempty"`
	LocalAddrs  []string          `json:"local_addrs,omitempty"` // LAN IPs (192.168.x.x, fe80::x) for same-network detection
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
	Blob   string `json:"blob"`             // base64-encoded ChaCha20-Poly1305 ciphertext
	Bridge string `json:"bridge,omitempty"` // relay-suggested bridge address (e.g., "fra1.bridge.keibisoft.com:26600")
	Tier   string `json:"tier,omitempty"`   // bandwidth tier: "free", "priority" (relay metadata, not encrypted)
}

// Map server status errors to semantic errors.
type ErrorMapperFunc func(statusCode int, err error) error

// InboundPort returns the port this instance listens on for incoming connections.
func (kd *KeibiDrop) InboundPort() int { return kd.inboundPort }

// UpgradeListenerDualStack replaces the IPv6-only listener with a dual-stack
// listener that accepts both IPv4 and IPv6. Used when entering local mode
// (LAN discovery needs IPv4 connectivity).
func (kd *KeibiDrop) UpgradeListenerDualStack() error {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	if kd.listener != nil {
		kd.listener.Close()
	}
	addr := net.JoinHostPort("", strconv.Itoa(kd.inboundPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	kd.listener = ln
	return nil
}

// DowngradeListenerIPv6Only replaces the dual-stack listener with an IPv6-only
// listener. Used when leaving local mode.
func (kd *KeibiDrop) DowngradeListenerIPv6Only() error {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	if kd.listener != nil {
		kd.listener.Close()
	}
	addr := net.JoinHostPort("::", strconv.Itoa(kd.inboundPort))
	ln, err := net.Listen("tcp6", addr)
	if err != nil {
		return err
	}
	kd.listener = ln
	return nil
}

// IsRunning returns whether the KeibiDrop instance is in a connected session.
func (kd *KeibiDrop) IsRunning() bool {
	return kd.running.Load()
}

// Running process for KeibiDrop.
func (kd *KeibiDrop) Start() {
	kd.signals <- Start
}

// Stop cleanly disconnects the current session. Run() continues after cleanup,
// ready for the next CreateRoom/JoinRoom. Thread-safe.
func (kd *KeibiDrop) Stop() {
	logger := kd.logger.With("method", "stop")
	kd.mu.Lock()
	if !kd.running.Load() {
		kd.mu.Unlock()
		logger.Info("Stop called but not running, returning")
		return
	}
	done := make(chan struct{})
	kd.stopDone = done
	cancel := kd.Cancel
	kd.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		logger.Warn("Stop timed out, forcing cleanup")
		kd.running.Store(false)
		kd.mu.Lock()
		kd.stopDone = nil
		kd.mu.Unlock()
	}
}

// cancelContext cancels the current context under mutex protection.
func (kd *KeibiDrop) cancelContext() {
	kd.mu.Lock()
	cancel := kd.Cancel
	kd.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Shutdown permanently stops the Run() goroutine. Use this for app exit.
// For temporary disconnects (peer left), use Stop() instead.
// Safe to call multiple times from any goroutine.
func (kd *KeibiDrop) Shutdown() {
	select {
	case <-kd.shutdown:
		// Already closed — nothing to do.
	default:
		kd.shutdownOnce.Do(func() { close(kd.shutdown) })
	}
	kd.cancelContext()
}

// Run as a go-routine.
func (kd *KeibiDrop) Run() {
	logger := kd.logger.With("method", "run-state")
	for {
		select {
		case <-kd.ctx.Done():
			logger.Info("Stopping KeibiDrop run instance (ctx cancelled)")
			// Signal teardown before joining the resilience goroutines so a still-running
			// onReconnected bails instead of rebuilding the stack we are about to nil.
			kd.tearingDown.Store(true)
			kd.StopConnectionResilience()
			// Nil the resilience handles under kd.mu: onDisconnect and onRekeyNeeded snapshot
			// kd.ReconnectManager / kd.RelayKeepalive, so these writes must pair with them.
			// Bare assignments only, so kd.mu is never held across a blocking call.
			kd.mu.Lock()
			kd.HealthMonitor = nil
			kd.ReconnectManager = nil
			kd.RelayKeepalive = nil
			kd.mu.Unlock()
			// Swap out the gRPC stack under kd.mu so the reconnect-rebuild path
			// (onReconnected / startGRPCServer / connectGRPCClientWithRetry), which
			// publishes these same fields, gets a happens-before instead of racing this
			// teardown. Stop()/Close() run on the locals OUTSIDE the lock so kd.mu is
			// never held across a blocking call.
			kd.mu.Lock()
			oldServer := kd.grpcServer
			oldConn := kd.grpcClientConn
			kd.grpcServer = nil
			kd.grpcClientConn = nil
			kd.KDClient = nil
			kd.KDSvc = nil
			kd.mu.Unlock()
			// Safe to Stop()/Close() here: handleNotifyDisconnect waits
			// grpcDisconnectGraceDelay before cancelling the context, so the
			// in-flight DISCONNECT RPC response has flushed. See #122.
			if oldServer != nil {
				oldServer.Stop()
			}
			if oldConn != nil {
				oldConn.Close()
			}
			kd.StopQUICControlChannel()
			// Nil the session under kd.mu so the detached reconnect-resume goroutine
			// (onReconnected spawns resumePartialDownloads) and onRekeyNeeded, which
			// snapshot kd.session under kd.mu, get a real happens-before and never race
			// this teardown write.
			prevPeerFP := ""
			kd.mu.Lock()
			if kd.session != nil {
				prevPeerFP = kd.session.ExpectedPeerFingerprint
			}
			kd.session = nil
			kd.mu.Unlock()
			kd.PeerIPv6IP = ""

			// Permanent shutdown — close listener and exit.
			select {
			case <-kd.shutdown:
				if kd.FS != nil {
					kd.FS.Unmount()
				}
				if kd.listener != nil {
					kd.listener.Close()
					kd.listener = nil
				}
				kd.running.Store(false)
				kd.mu.Lock()
				done := kd.stopDone
				kd.stopDone = nil
				kd.mu.Unlock()
				if done != nil {
					close(done)
				}
				logger.Info("Run loop: permanent shutdown")
				return
			default:
			}

			// Temporary disconnect — refresh session and context.
			// Preserve LocalFiles tagged with peer identity so they're only
			// served back to the SAME peer on reconnect, not leaked to others.
			prevLocal := kd.SyncTracker.LocalFiles
			// Compute the fresh session outside kd.mu (keygen), then swap under the lock so
			// background readers (notify flush, relay keepalive, pull workers) snapshot a
			// consistent pointer instead of racing this write.
			newSess := kd.refreshSession()
			kd.mu.Lock()
			kd.session = newSess
			kd.mu.Unlock()
			kd.SyncTracker = synctracker.NewSyncTracker()
			kd.lastSharedPeerFP = prevPeerFP
			kd.lastSharedFiles = prevLocal
			kd.activeDownloadsMu.Lock()
			for _, cancel := range kd.activeDownloads {
				cancel()
			}
			kd.activeDownloads = make(map[string]context.CancelFunc)
			kd.activeBitmaps = make(map[string]*filesystem.ChunkBitmap)
			kd.activeDownloadsMu.Unlock()
			ctx, c := context.WithCancel(context.Background())
			kd.running.Store(false)
			kd.mu.Lock()
			kd.ctx = ctx
			kd.Cancel = c
			done := kd.stopDone
			kd.stopDone = nil
			kd.mu.Unlock()
			if done != nil {
				close(done)
			}
			continue
		case s := <-kd.signals:
			if s == Start {
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

				// Mark running BEFORE the blocking Mount() call so that
				// external code (tests, FFI) can observe the transition
				// from running→not-running when a disconnect happens.
				kd.running.Store(true)

				if kd.FS != nil && kd.KDSvc != nil {
					kd.KDSvc.FS = kd.FS
					if kd.FS.IsMounted() {
						logger.Info("FUSE already mounted, waiting for disconnect")
						<-kd.ctx.Done()
					} else {
						logger.Info("Mounting filesystem", "mount", kd.ToMount, "save", kd.ToSave)
						mountDone := make(chan struct{})
						go func() {
							if err := kd.FS.Mount(filepath.Clean(kd.ToMount), false, filepath.Clean(kd.ToSave)); err != nil {
								logger.Error("Filesystem mount failed", "error", err)
							} else {
								logger.Info("Filesystem mount session ended")
							}
							close(mountDone)
						}()
						select {
						case <-mountDone:
						case <-kd.ctx.Done():
							logger.Info("Context cancelled while FUSE mounted")
						}
					}
				} else {
					logger.Warn("No FS to mount")
					<-kd.ctx.Done()
				}
				continue
			}
		}
	}
}
