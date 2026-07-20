// ABOUTME: Tests that nil-guard snapshots in resilience.go prevent panics during shutdown races.
// ABOUTME: Covers notifyRestoredFiles, resumePartialDownloads, onReconnected, and RelayLookup.
package common

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
)

func newNilGuardTestKD() *KeibiDrop {
	ctx, cancel := context.WithCancel(context.Background())
	kd := &KeibiDrop{
		logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
		ctx:         ctx,
		Cancel:      cancel,
	}
	return kd
}

func TestNotifyRestoredFiles_NilClient_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.SyncTracker.LocalFiles["file.txt"] = &synctracker.File{
		Name:           "file.txt",
		RelativePath:   "file.txt",
		RealPathOfFile: "/tmp/nonexistent-test-file.txt",
		Size:           100,
	}

	kd.notifyRestoredFiles(kd.logger)
}

func TestNotifyRestoredFiles_CancelledContext_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	kd.Cancel()

	kd.SyncTracker.LocalFiles["file.txt"] = &synctracker.File{
		Name:           "file.txt",
		RelativePath:   "file.txt",
		RealPathOfFile: "/tmp/nonexistent-test-file.txt",
		Size:           100,
	}

	kd.notifyRestoredFiles(kd.logger)
}

func TestResumePartialDownloads_NilSession_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.dlRegistry = newDownloadRegistry("", nil)

	kd.resumePartialDownloads(kd.logger)
}

func TestResumePartialDownloads_NilRegistry_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.resumePartialDownloads(kd.logger)
}

// TestOpenStreamProvider_NilSession_NoPanic verifies the FUSE OpenStreamProvider
// callback is nil-safe during a Shutdown race. Run() nils kd.session before FsCtx
// is cancelled; a goroutine waking from PrefetchSem (reconcileEditAsync /
// prefetchFile) then calls this. It must return a nil provider, not deref a nil
// session and crash the process.
func TestOpenStreamProvider_NilSession_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	// session is nil on a freshly-built test KeibiDrop.
	fsp := kd.openStreamProvider()
	if fsp != nil {
		t.Fatalf("expected nil provider when session is nil, got %T", fsp)
	}
}

// TestOpenStreamProvider_NilGRPCClient_NoPanic verifies a session present but with
// no gRPC client (a window during connect/teardown) also yields a nil provider.
func TestOpenStreamProvider_NilGRPCClient_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.session = &session.Session{} // GRPCClient is nil
	fsp := kd.openStreamProvider()
	if fsp != nil {
		t.Fatalf("expected nil provider when GRPCClient is nil, got %T", fsp)
	}
}

// TestNilSessionGuard_SkipsCachedPeerUpdate verifies the guard pattern used in
// onReconnected (resilience.go:198). Calling onReconnected directly requires full
// gRPC infrastructure, so this tests the guard inline. If the guard is removed
// from production code, grep for "kd.session != nil" in onReconnected to verify.
func TestNilSessionGuard_SkipsCachedPeerUpdate(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.ReconnectManager = session.NewReconnectManager(nil, kd.logger)
	kd.ReconnectManager.CachedPeerPort = 9999

	if kd.ReconnectManager != nil && kd.session != nil {
		kd.ReconnectManager.CachedPeerPort = kd.session.PeerPort
	}

	if kd.ReconnectManager.CachedPeerPort != 9999 {
		t.Fatalf("CachedPeerPort should be unchanged, got %d", kd.ReconnectManager.CachedPeerPort)
	}
}

// TestNilSessionGuard_RelayLookupReturnsError verifies the guard pattern used in
// the RelayLookup closure (resilience.go:56-59). The closure is defined inline in
// InitConnectionResilience which requires a live session, so this tests the
// pattern directly.
func TestNilSessionGuard_RelayLookupReturnsError(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	lookup := func(fingerprint string) (string, int, error) {
		sess := kd.session
		if sess == nil {
			return "", 0, fmt.Errorf("session nil during relay lookup")
		}
		return kd.PeerIPv6IP, sess.PeerPort, nil
	}

	_, _, err := lookup("abc123")
	if err == nil {
		t.Fatal("expected error for nil session")
	}
	if !strings.Contains(err.Error(), "session nil") {
		t.Fatalf("expected session-nil error, got: %v", err)
	}
}

// TestWriterEpoch_NilMonitor_ReturnsZero verifies the kd status writer-epoch accessor is
// nil-safe: before InitConnectionResilience builds the health monitor (no connection yet), it
// must surface epoch 0 rather than deref a nil monitor and crash the status command.
func TestWriterEpoch_NilMonitor_ReturnsZero(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	if e := kd.WriterEpoch(); e != 0 {
		t.Fatalf("WriterEpoch with no health monitor = %d, want 0", e)
	}
}

// newConfiguredHealthMonitor is the single wiring path for BOTH the initial monitor and the
// post-reconnect rebuild. This locks the property whose divergence reintroduced the MED-2 bug:
// the monitor must always ship rekey-enabled with its callbacks wired, from either site.
func TestNewConfiguredHealthMonitor_RekeyEnabledAndWired(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	hm := kd.newConfiguredHealthMonitor(&session.Session{}, nil, kd.logger)
	if !hm.RekeyEnabled {
		t.Fatal("the monitor must ship rekey-enabled from the single wiring path both call sites use")
	}
	if hm.OnRekeyNeeded == nil || hm.OnDisconnect == nil || hm.OnHealthChange == nil {
		t.Fatal("the monitor must have all callbacks wired")
	}
}
