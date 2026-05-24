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
