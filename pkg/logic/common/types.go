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
	StrictMode     bool   // Disables the data relay fallback: direct connections only.
	ConnectionMode string // "lan", "direct", or "bridge". Set after a successful connection.
	OpInProgress   atomic.Int32

	bridgeBase     string      // Bridge address as configured at room entry, the dial failover target.
	bridgeFellBack atomic.Bool // A bridge dial failed over to bridgeBase; sticky so every lane and reconnect agrees.

	// Prepaid relay tokens (tokens.go).
	wallet      *TokenWallet
	walletOnce  sync.Once
	tokenSess   *tokenSession // one per room session, under mu; nil when unfunded
	busyFlag    bool          // relay flagged the assigned bridge busy; under mu
	busyNotice  string        // server-supplied user-facing text; under mu
	busyEventAt atomic.Int64  // unix time of the last relay_busy event (throttle)

	creditLowNoted  atomic.Bool   // latched after the 50 GB warning fires
	creditCritNoted atomic.Bool   // latched after the 2 GB warning fires
	buyClaimStop    chan struct{} // cancels the active buy-claim poll, under mu

	session *session.Session

	PeerIPv6IP     string
	PeerLocalAddrs []string // LAN IPs from the relay registration, for same-network direct connect.

	LocalIPv6IP string
	inboundPort int
	listener    net.Listener // Guarded by kd.mu.

	// Filesystem.
	FS             *filesystem.FS
	KDSvc          *service.KeibidropServiceImpl
	KDClient       bindings.KeibiServiceClient
	grpcClientConn *grpc.ClientConn

	// QUIC control channel: a UDP gRPC pair that mirrors the TCP pair for control,
	// metadata, and IP-change survival. File transfer stays on TCP. It comes up when the
	// peer negotiated QUIC keys, and any failure leaves the session TCP-only.
	quicControlClient   *grpc.ClientConn
	quicControlServer   *grpc.Server
	quicControlLn       net.Listener              // Generation marker: serveQUICControl only attaches to the current one.
	quicControlMig      *transport.MigratableConn // Migration handle: MigrateQUICControl moves the path, and gRPC never notices.
	quicControlSC       *session.SecureConn       // Dial-side conn: epoch observability for QUICWriterEpoch.
	quicPeerAddr        string                    // Peer's UDP control endpoint, kept for the self-heal redial.
	quicRedialing       atomic.Bool               // Single-flight guard: at most one background redial.
	quicArming          atomic.Bool               // Single-flight guard for the arming loop that waits out a stale maintainer.
	connectAborted      atomic.Bool               // Set by NotifyDisconnect so a create still waiting for its peer stops.
	quicHBCancel        context.CancelFunc        // Cancels the current generation's control heartbeat. Guarded by kd.mu.
	disconnectDeferrals atomic.Int32              // Bounded count of disconnects deferred for active transfers.
	quicMetaSent        atomic.Uint64             // Metadata RPCs that rode the QUIC channel. For diagnostics and tests.
	quicGen             atomic.Uint64             // Channel generation. Every lane log line carries it, so two cycles never read as one.
	quicAccepted        atomic.Uint64             // Inbound QUIC control conns accepted. The half-lane that was invisible before.
	quicLastDialErr     string                    // Reason the outbound lane is down, for kd status. Guarded by kd.mu.

	// Non-FUSE fallback.
	SyncTracker *synctracker.SyncTracker

	// Paths for the virtual mount point and the save folder.
	ToMount string
	ToSave  string

	// Collab sync options.
	PrefetchOnOpen bool
	PushOnWrite    bool
	// AutoCache enables the macFUSE auto_cache mount option, so a peer's same-size
	// in-place edit shows live. The caller sets it from config.LiveCollab. False = git-safe.
	AutoCache         bool
	PrefetchAutoMB    int    // Files >= this many MB auto-prefetch on open. From config.PrefetchAutoMB. 0 = off.
	ReadAheadWindowMB int    // Cap in MB for predictive sequential read-ahead. From config.ReadAheadWindowMB. 0 = off.
	ScanSharedOnStart bool   // Announce files already in ToSave when a session starts. From config.ScanSharedOnStart.
	AutoConnectPeer   string // Saved contact to connect to on startup, with retry. From config.AutoConnectPeer.
	ShareReadOnly     bool   // Refuse every inbound peer mutation at the service layer. From config.ShareReadOnly.
	MountReadOnly     bool   // Local FUSE mount returns EROFS on write ops. From config.MountReadOnly.
	PreserveMetadata  bool   // Apply the origin's mode and times to files saved on disk. From config.PreserveMetadata.

	// Signals for loop management.
	signals      chan TaskSignal
	running      atomic.Bool
	stopDone     chan struct{} // Closed when the Stop handler completes.
	ctx          context.Context
	Cancel       context.CancelFunc // Exported so the FFI layer can call it for app exit.
	shutdown     chan struct{}      // Shutdown closes it to exit Run permanently.
	shutdownOnce sync.Once
	mu           sync.Mutex

	refreshSession func() *session.Session

	// gRPC server and readiness signals.
	grpcServer          *grpc.Server
	filesystemReady     chan struct{}
	filesystemReadyOnce sync.Once
	serverReadyMu       sync.Mutex

	// Event callback. The FFI layer wires it to push events to the UI.
	OnEvent func(string)

	// OnPeerVerified fires with the peer's verified fingerprint when the handshake
	// confirms identity, before any files sync. setupFilesystem wires it to scope the
	// FUSE cache to the peer: drop another peer's view, keep the same peer's.
	OnPeerVerified func(fp string)

	// Connection resilience.
	HealthMonitor    *session.HealthMonitor
	ReconnectManager *session.ReconnectManager
	RelayKeepalive   *RelayKeepalive

	// Notification queue: a bounded channel, so large clones do not spawn 600+
	// goroutines. A single worker drains and sends sequentially.
	notifyCh chan *bindings.NotifyRequest

	// pendingNotifies counts REMOVE/RENAME/ADD_DIR notifies queued or in flight. The
	// worker has no retry queue, and only ADD_FILE replays, so a socket drop here would
	// lose the delete or rename. onRekeyNeeded defers while it is > 0.
	pendingNotifies atomic.Int64

	// wireBase folds the totals of retired sessions and demoted QUIC lanes,
	// so WireStatsTotal survives session rebuilds and lane changes.
	wireBaseSent atomic.Uint64
	wireBaseRecv atomic.Uint64

	// peerRefusedWrites counts local changes the peer refused because its share
	// is read-only. Those bytes stay local and shadow the origin in the mount.
	peerRefusedWrites atomic.Uint64

	// tearingDown gates the reconnect goroutine: Run's teardown sets it first, so
	// onReconnected stops instead of rebuilding the stack that teardown nils.
	tearingDown atomic.Bool

	// eagerFoldInFlight is the single-flight guard for the eager entropy fold: at most
	// one round at a time. The round clears it on finish. A reconnect re-folds on the
	// fresh session key.
	eagerFoldInFlight atomic.Bool

	// eagerFoldRearm: a trigger during an in-flight round runs one follow-up instead of dropping.
	eagerFoldRearm atomic.Bool

	// eagerFoldScheduled is the coalescing latch for lane-wire arrival triggers.
	eagerFoldScheduled atomic.Bool

	// Reachability hints. inboundBlocked means nothing reaches the local listener. It
	// rides the encrypted registration, so the peer skips a doomed dial.
	// peerInboundBlocked is the same flag from the peer. *On/*At scope the verdict, so
	// it expires instead of pinning the node to the relay.
	inboundBlocked     atomic.Bool
	peerInboundBlocked atomic.Bool
	inboundBlockedOn   atomic.Value // string
	probedOn           atomic.Value // string: the local address the relay probe last ran on
	probedAt           atomic.Int64 // Unix nano of that probe, so the verdict expires.
	probedReachable    atomic.Bool  // The relay reached the listener on that probe.
	parkedBridgeIn     net.Conn     // Creator's bridge inbound leg kept open between rounds; see bridgeInbound.

	// Active downloads registry for pause/cancel support.
	activeDownloads   map[string]context.CancelFunc
	activeBitmaps     map[string]*filesystem.ChunkBitmap
	activeDownloadsMu sync.Mutex

	// Download registry: maps bitmaps to their peer with HMAC tags, not fingerprints.
	dlRegistry  *downloadRegistry
	registryKey []byte

	// Preserved shared files from the previous session. Served only to the same peer.
	lastSharedPeerFP string
	lastSharedFiles  map[string]*synctracker.File
	sharedStore      *sharedFilesStore
}

type TaskSignal int

const (
	Start TaskSignal = iota
	Stop
)

// NewKeibiDrop builds a KeibiDrop and probes the local global IPv6 address.
func NewKeibiDrop(ctx context.Context, logger *slog.Logger, isFuse bool, relayURL *url.URL, inboundPort int, defaultOutboundPort int, toMount string, toSave string, prefetchOnOpen bool, pushOnWrite bool) (*KeibiDrop, error) {
	ipv6, err := GetGlobalIPv6()
	if err != nil {
		// Non-fatal: Android restricts net.Interfaces(). Mobile peers use the
		// outbound-only bridge relay, so a local IPv6 is not required.
		logger.Warn("Failed to get local IPv6 (non-fatal on mobile)", "error", err)
		ipv6 = ""
	}

	return NewKeibiDropWithIP(ctx, logger, isFuse, relayURL, inboundPort, defaultOutboundPort, toMount, toSave, prefetchOnOpen, pushOnWrite, ipv6)
}

// NewKeibiDropWithIP is NewKeibiDrop with an explicit IPv6 address instead of a
// network probe. It enables tests on machines without a global IPv6 address.
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

	// Wrap the incoming context, so Cancel is always available for disconnect handling.
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
		// running stays false until Start.
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

// EnableOpts controls how persistent identity loads:
//   - PassphraseProtect: opt into Tier 2, a passphrase-derived key.
//     Requires a non-nil PassphraseProvider.
//   - PassphraseProvider: runs once when PassphraseProtect is true to obtain the
//     passphrase. Typical sources: TTY prompt, stdin bridge, test stub.
type EnableOpts struct {
	PassphraseProtect  bool
	PassphraseProvider func() (string, error)
	ExternalMaster     []byte // 32-byte key from mobile bridge (iOS Keychain / Android Keystore)
}

// EnablePersistentIdentity replaces ephemeral keys with a stable device identity.
// Call it before CreateRoom/JoinRoom. It loads or creates the identity in configDir,
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
	sess.ExtraFoldConns = kd.quicFoldConns // Fold rounds cover the QUIC lane too.
	kd.mu.Lock()
	oldSess := kd.session
	kd.session = sess
	kd.mu.Unlock()
	kd.rollSessionWire(oldSess)
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
		s.ExtraFoldConns = kd.quicFoldConns // Fold rounds cover the QUIC lane too.
		return s
	}

	logger.Info("Persistent identity enabled", "fingerprint", id.Fingerprint)
	return nil
}

// EnablePersistentIdentityDefault calls EnablePersistentIdentity with zero-value
// EnableOpts: keychain or file tier, no passphrase.
func (kd *KeibiDrop) EnablePersistentIdentityDefault(configDir string) error {
	return kd.EnablePersistentIdentity(configDir, EnableOpts{})
}

// ToggleIncognito switches between persistent and ephemeral identity. Enabling
// generates fresh ephemeral keys. Disabling restores the persistent keys.
// It returns the new fingerprint.
func (kd *KeibiDrop) ToggleIncognito(incognito bool, configDir string) (string, error) {
	if incognito {
		kd.Incognito = true
		outPort := kd.session.DefaultOutboundPort
		inPort := kd.session.DefaultInboundPort
		sess, err := session.InitSession(kd.logger, outPort, inPort)
		if err != nil {
			return "", fmt.Errorf("init ephemeral session: %w", err)
		}
		sess.ExtraFoldConns = kd.quicFoldConns // Fold rounds cover the QUIC lane too.
		kd.mu.Lock()
		oldSess := kd.session
		kd.session = sess
		kd.mu.Unlock()
		kd.rollSessionWire(oldSess)
		kd.refreshSession = func() *session.Session {
			s, err := session.InitSession(kd.logger, outPort, inPort)
			if err != nil {
				kd.logger.Error("Failed to refresh ephemeral session", "error", err)
				return nil
			}
			s.ExtraFoldConns = kd.quicFoldConns // Fold rounds cover the QUIC lane too.
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
	IP    string `json:"ip"`             // Public IP address, v4 or v6.
	Port  int    `json:"port"`           // Where the peer listens.
	IPv6  bool   `json:"ipv6"`           // True when this hint prefers IPv6.
	Proto string `json:"proto"`          // For example "tcp".
	Note  string `json:"note,omitempty"` // Optional: NAT behavior notes.
	// InboundBlocked means nothing reaches the address above, so a dial only burns the
	// timeout. It rides inside the encrypted blob, so the relay never sees it. Older
	// peers omit it, which decodes as false.
	InboundBlocked bool `json:"inbound_blocked,omitempty"`
}

// EncryptedRegistration is the relay-visible payload, an opaque blob. Only peers
// with the shared room password can decrypt it.
type EncryptedRegistration struct {
	Blob   string `json:"blob"`             // base64-encoded ChaCha20-Poly1305 ciphertext
	Bridge string `json:"bridge,omitempty"` // relay-suggested bridge address (e.g., "fra1.bridge.keibisoft.com:26600")
	Tier   string `json:"tier,omitempty"`   // bandwidth tier: "free", "priority" (relay metadata, not encrypted)
	Busy   bool   `json:"busy,omitempty"`   // assigned bridge is in contention right now
	Notice string `json:"notice,omitempty"` // server-controlled text shown to the user with busy
}

// ErrorMapperFunc maps server status errors to semantic errors.
type ErrorMapperFunc func(statusCode int, err error) error

// InboundPort returns the port this instance listens on for incoming connections.
func (kd *KeibiDrop) InboundPort() int { return kd.inboundPort }

// PeerRefusedWrites returns how many local changes the peer refused because
// its share is read-only. Non-zero means the mount shows bytes the peer does
// not hold.
func (kd *KeibiDrop) PeerRefusedWrites() uint64 { return kd.peerRefusedWrites.Load() }

// WireStats sums bytes over the CURRENT conns: the session's two TCP conns
// plus the live QUIC control lanes. A reconnect, re-handshake, or lane change
// replaces conns and their counters vanish with them, so this is bytes since
// the current connections were installed, not a session total. WireStatsTotal
// is the figure that survives replacement.
func (kd *KeibiDrop) WireStats() (bytesSent, bytesRecv uint64) {
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	if sess == nil {
		return 0, 0
	}
	conns := []*session.SecureConn{sess.InboundConn(), sess.OutboundConn()}
	conns = append(conns, kd.quicFoldConns()...)
	for _, c := range conns {
		if c == nil {
			continue
		}
		bs, br, _, _ := c.GetStats()
		bytesSent += bs
		bytesRecv += br
	}
	return bytesSent, bytesRecv
}

// WireStatsTotal returns bytes moved this daemon run: retired sessions and
// demoted lanes plus everything the live conns carry. Unlike WireStats it
// survives reconnects, re-handshakes, and QUIC lane changes. Known gap: an
// accepted-side QUIC conn replaced by a newer accept is not rolled up.
func (kd *KeibiDrop) WireStatsTotal() (bytesSent, bytesRecv uint64) {
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	bytesSent, bytesRecv = kd.wireBaseSent.Load(), kd.wireBaseRecv.Load()
	if sess != nil {
		bs, br := sess.WireTotals()
		bytesSent += bs
		bytesRecv += br
	}
	for _, c := range kd.quicFoldConns() {
		if c == nil {
			continue
		}
		bs, br, _, _ := c.GetStats()
		bytesSent += bs
		bytesRecv += br
	}
	return bytesSent, bytesRecv
}

// rollSessionWire folds a retiring session's wire totals into the daemon
// base before the session pointer is dropped.
func (kd *KeibiDrop) rollSessionWire(sess *session.Session) {
	if sess == nil {
		return
	}
	bs, br := sess.WireTotals()
	kd.wireBaseSent.Add(bs)
	kd.wireBaseRecv.Add(br)
}

// UpgradeListenerDualStack replaces the IPv6-only listener with a dual-stack one.
// Local mode uses it: LAN discovery needs IPv4 connectivity.
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
// listener when local mode ends.
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

// Start signals the Run loop to begin a session.
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

// Shutdown permanently stops the Run goroutine. Use it for app exit. For a
// temporary disconnect, use Stop. Safe to call many times from any goroutine.
func (kd *KeibiDrop) Shutdown() {
	select {
	case <-kd.shutdown:
		// Already closed. Nothing to do.
	default:
		kd.shutdownOnce.Do(func() { close(kd.shutdown) })
	}
	kd.cancelContext()
}

// Run is the main loop. Call it as a goroutine.
func (kd *KeibiDrop) Run() {
	logger := kd.logger.With("method", "run-state")
	for {
		select {
		case <-kd.ctx.Done():
			logger.Info("Stopping KeibiDrop run instance (ctx cancelled)")
			// Signal teardown before joining the resilience goroutines, so a still-running
			// onReconnected stops instead of rebuilding the stack this teardown nils.
			kd.tearingDown.Store(true)
			kd.StopConnectionResilience()
			// Nil the resilience handles under kd.mu: onDisconnect and onRekeyNeeded snapshot
			// them under the same lock, so these writes pair with those reads. Bare
			// assignments only, so kd.mu never spans a blocking call.
			kd.mu.Lock()
			kd.HealthMonitor = nil
			kd.ReconnectManager = nil
			kd.RelayKeepalive = nil
			kd.mu.Unlock()
			// Swap out the gRPC stack under kd.mu. The reconnect-rebuild path publishes the
			// same fields, so it gets a happens-before instead of a race. Stop and Close
			// run on the locals outside the lock.
			kd.mu.Lock()
			oldServer := kd.grpcServer
			oldConn := kd.grpcClientConn
			kd.grpcServer = nil
			kd.grpcClientConn = nil
			kd.KDClient = nil
			kd.KDSvc = nil
			kd.mu.Unlock()
			// Stop and Close are safe here: handleNotifyDisconnect waits grpcDisconnectGraceDelay
			// before it cancels the context, so the DISCONNECT response has flushed.
			if oldServer != nil {
				oldServer.Stop()
			}
			if oldConn != nil {
				oldConn.Close()
			}
			kd.StopQUICControlChannel()
			// Nil the session under kd.mu. The detached resume goroutine and onRekeyNeeded
			// snapshot kd.session under the same lock, so they never race this write.
			prevPeerFP := ""
			kd.mu.Lock()
			if kd.session != nil {
				prevPeerFP = kd.session.ExpectedPeerFingerprint
			}
			kd.session = nil
			kd.mu.Unlock()
			kd.PeerIPv6IP = ""

			// Permanent shutdown: close the listener and exit.
			select {
			case <-kd.shutdown:
				if kd.FS != nil {
					kd.FS.Unmount()
				}
				// Close+nil under kd.mu: CreateRoom/JoinRoom's local-mode accept snapshots
				// the listener under the same lock, so this write pairs with those reads.
				// Bare assignment inside the lock, Close() outside, so kd.mu never spans
				// a blocking call.
				kd.mu.Lock()
				ln := kd.listener
				kd.listener = nil
				kd.mu.Unlock()
				if ln != nil {
					ln.Close()
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

			// Temporary disconnect: refresh session and context. Preserve LocalFiles tagged
			// with the peer identity, so only the same peer gets them back on reconnect.
			prevLocal := kd.SyncTracker.LocalFiles
			// Compute the fresh session outside kd.mu, because keygen is slow. Swap under
			// the lock, so background readers snapshot a consistent pointer.
			newSess := kd.refreshSession()
			kd.mu.Lock()
			oldSess := kd.session
			kd.session = newSess
			kd.mu.Unlock()
			kd.rollSessionWire(oldSess)
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
				if kd.session == nil || kd.session.InboundConn() == nil {
					logger.Warn("Nil session")
					continue
				}

				logger.Info("Signal start success")

				go func() {
					err := kd.startGRPCServer()
					if err != nil {
						logger.Error("Failed to start gRPC server", "error", err)
					}
				}()

				// Wait for filesystem setup or context cancellation.
				select {
				case <-kd.filesystemReady:
				case <-kd.ctx.Done():
					logger.Info("Context cancelled while waiting for filesystem")
					continue
				}

				// Mark running before the blocking Mount call, so external code observes the
				// running to not-running transition on disconnect.
				kd.running.Store(true)

				// Snapshot KDSvc under kd.mu: startGRPCServer publishes it under
				// the same lock on another goroutine. filesystemReady does not
				// order that publish (the client dial it gates on goes to the
				// PEER's server), so a one-shot nil read here skipped the mount
				// for the whole session. Wait for the publish, bounded; on
				// timeout fall through to the no-FS park as before.
				kd.mu.Lock()
				svc := kd.KDSvc
				kd.mu.Unlock()
				svcDeadline := time.Now().Add(10 * time.Second)
				for svc == nil && kd.ctx.Err() == nil && time.Now().Before(svcDeadline) {
					time.Sleep(5 * time.Millisecond)
					kd.mu.Lock()
					svc = kd.KDSvc
					kd.mu.Unlock()
				}
				if kd.FS != nil && svc != nil {
					svc.SetFS(kd.FS)
					// ADD_FILE notifies that raced this publish landed only in
					// SyncTracker. Fold them into the tree before serving.
					kd.BackfillRemoteFilesIntoFS()
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
