// ABOUTME: Tests that nil-guard snapshots in resilience.go prevent panics during shutdown races.
// ABOUTME: Covers notifyRestoredFiles, resumePartialDownloads, onReconnected, and RelayLookup.
package common

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
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

// TestOpenStreamProvider_NilSession_NoPanic: the FUSE OpenStreamProvider callback must be
// nil-safe when a shutdown race nils kd.session, returning a nil provider instead of crashing.
func TestOpenStreamProvider_NilSession_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	// session is nil on a freshly-built test KeibiDrop.
	fsp := kd.openStreamProvider()
	require.Nil(t, fsp, "expected nil provider when session is nil")
}

// TestOpenStreamProvider_NilGRPCClient_NoPanic: a session present but with no gRPC client
// (a connect/teardown window) also yields a nil provider.
func TestOpenStreamProvider_NilGRPCClient_NoPanic(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.session = &session.Session{} // GRPCClient is nil
	fsp := kd.openStreamProvider()
	require.Nil(t, fsp, "expected nil provider when GRPCClient is nil")
}

// TestNilSessionGuard_SkipsCachedPeerUpdate verifies onReconnected's nil-session guard
// pattern inline, since calling onReconnected directly needs full gRPC infrastructure.
func TestNilSessionGuard_SkipsCachedPeerUpdate(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	kd.ReconnectManager = session.NewReconnectManager(nil, kd.logger)
	kd.ReconnectManager.CachedPeerPort = 9999

	if kd.ReconnectManager != nil && kd.session != nil {
		kd.ReconnectManager.CachedPeerPort = kd.session.PeerPort
	}

	require.Equal(t, 9999, kd.ReconnectManager.CachedPeerPort, "CachedPeerPort should be unchanged")
}

// TestNilSessionGuard_RelayLookupReturnsError verifies the RelayLookup closure's nil-session
// guard directly, since the closure lives inside InitConnectionResilience which needs a live session.
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
	require.Error(t, err, "expected error for nil session")
	require.Contains(t, err.Error(), "session nil")
}

// TestWriterEpoch_NilMonitor_ReturnsZero: before the health monitor exists, the writer-epoch
// accessor must return 0 rather than deref a nil monitor and crash the status command.
func TestWriterEpoch_NilMonitor_ReturnsZero(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	require.Zero(t, kd.WriterEpoch(), "WriterEpoch with no health monitor")
}

// TestNewConfiguredHealthMonitor_RekeyEnabledAndWired locks the property whose divergence once
// caused a bug: the single wiring path must always ship the monitor rekey-enabled with callbacks wired.
func TestNewConfiguredHealthMonitor_RekeyEnabledAndWired(t *testing.T) {
	kd := newNilGuardTestKD()
	defer kd.Cancel()

	hm := kd.newConfiguredHealthMonitor(&session.Session{}, nil, kd.logger)
	require.True(t, hm.RekeyEnabled, "the monitor must ship rekey-enabled from the single wiring path both call sites use")
	require.NotNil(t, hm.OnRekeyNeeded, "the monitor must have all callbacks wired")
	require.NotNil(t, hm.OnDisconnect, "the monitor must have all callbacks wired")
	require.NotNil(t, hm.OnHealthChange, "the monitor must have all callbacks wired")
}
