// ABOUTME: Repro tests for the three proactive-rekey drop defects (F2 nil-deref TOCTOU,
// ABOUTME: F3 mid-transfer drop, #6 in-flight REMOVE/RENAME notify loss) in onRekeyNeeded.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"google.golang.org/grpc"
)

// closeRecorder wraps a net.Conn and records whether Close was called, so a test
// can assert a rekey did (or did not) drop the underlying sockets.
type closeRecorder struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// newRecordedSecureConn returns a real *session.SecureConn whose underlying conn
// records Close calls. The zero key is fine: these conns are only ever closed,
// never read from or written to.
func newRecordedSecureConn(t *testing.T, prefix uint32) (*session.SecureConn, *closeRecorder) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	rec := &closeRecorder{Conn: c1}
	key := make([]byte, kbc.KeySize)
	return session.NewSecureConn(rec, key, kbc.CipherChaCha20, prefix), rec
}

// newInitiatorRekeyKD builds a KeibiDrop for which every onRekeyNeeded guard
// passes: proactive rekey enabled, this peer is the deterministic initiator
// (lower fingerprint), reconnect state Connected, and no cooldown (LastRekeyAt
// zero). A test then adds exactly one blocker and asserts the rekey defers.
func newInitiatorRekeyKD(t *testing.T) (kd *KeibiDrop, inRec, outRec *closeRecorder) {
	t.Helper()
	t.Setenv("KD_PROACTIVE_REKEY", "1")
	kd = newTestKD(t)
	kd.session.OwnFingerprint = "aaa"
	kd.session.ExpectedPeerFingerprint = "zzz" // "aaa" < "zzz" => initiator
	inbound, inRec := newRecordedSecureConn(t, session.NoncePrefixInbound)
	outbound, outRec := newRecordedSecureConn(t, session.NoncePrefixOutbound)
	kd.session.Session = &session.SessionSockets{Inbound: inbound, Outbound: outbound}
	kd.ReconnectManager = session.NewReconnectManager(kd.session, kd.logger)
	t.Cleanup(func() { kd.ReconnectManager.Stop() })
	return kd, inRec, outRec
}

// TestOnRekeyNeeded_SessionNilRace_NoPanic reproduces F2: Run's ctx.Done teardown
// nils kd.session concurrently with the health monitor calling onRekeyNeeded. The
// pre-fix code re-read kd.session after its nil-check and dereferenced it (RekeyMu,
// Session), panicking when the pointer flipped to nil mid-call. The fix snapshots
// kd.session once under kd.mu. Both sides go through kd.mu so the pointer access is
// race-clean (same discipline as reconnect_race_test.go); reverting the snapshot
// brings back both the DATA RACE and the nil-deref panic.
func TestOnRekeyNeeded_SessionNilRace_NoPanic(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")
	kd := newTestKD(t)
	// Keep this session an initiator and hold it past the rekey cooldown so a call
	// that survives the nil race still returns before any OnDisconnect side effects.
	kd.session.OwnFingerprint = "aaa"
	kd.session.ExpectedPeerFingerprint = "zzz"
	kd.session.LastRekeyAt = time.Now()
	kd.ReconnectManager = session.NewReconnectManager(kd.session, kd.logger)
	t.Cleanup(func() { kd.ReconnectManager.Stop() })

	// The teardown writer swaps kd.session between nil and a ready-made replacement
	// (no per-iteration keygen). The replacement is also an initiator past cooldown.
	replacement := &session.Session{
		OwnFingerprint:          "aaa",
		ExpectedPeerFingerprint: "zzz",
		LastRekeyAt:             time.Now(),
	}

	var start, done sync.WaitGroup
	start.Add(1)

	const readers = 4
	const iters = 20000
	done.Add(readers)
	for range readers {
		go func() {
			defer done.Done()
			start.Wait()
			for range iters {
				kd.onRekeyNeeded()
			}
		}()
	}

	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for range iters {
			kd.mu.Lock()
			kd.session = nil
			kd.mu.Unlock()
			kd.mu.Lock()
			kd.session = replacement
			kd.mu.Unlock()
		}
	}()

	start.Done()
	done.Wait()
	// Reaching here without a panic (and, under -race, without a DATA RACE) is the pass.
}

// TestOnRekeyNeeded_ActiveTransfer_DefersRekey reproduces F3: a serving/on-demand
// transfer trickling under the health monitor's idle gate must not have its sockets
// dropped by a proactive rekey. onRekeyNeeded now carries onDisconnect's
// hasActiveTransfers guard. Removing that guard makes this test fail (epoch bumped,
// sockets closed).
func TestOnRekeyNeeded_ActiveTransfer_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)

	// An in-progress download makes hasActiveTransfers() true.
	kd.activeDownloadsMu.Lock()
	kd.activeDownloads["serving.bin"] = func() {}
	kd.activeDownloadsMu.Unlock()

	epochBefore := kd.session.CurrentEpoch
	if got := kd.onRekeyNeeded(); got {
		t.Fatalf("onRekeyNeeded during active transfer = true, want false (rekey must defer)")
	}
	if kd.session.CurrentEpoch != epochBefore {
		t.Fatalf("epoch bumped during active transfer: %d -> %d", epochBefore, kd.session.CurrentEpoch)
	}
	if inRec.closed.Load() || outRec.closed.Load() {
		t.Fatalf("sockets closed during active transfer (inbound=%v outbound=%v)",
			inRec.closed.Load(), outRec.closed.Load())
	}
}

// TestOnRekeyNeeded_InflightRemoveNotify_DefersRekey reproduces #6: a REMOVE notify
// is in flight over the outbound gRPC client when a proactive rekey wants to drop
// that socket. The notify worker has no retry queue and notifyRestoredFiles replays
// only ADD_FILE, so dropping here loses the delete and diverges the peers. The fix
// gates onRekeyNeeded on pendingNotifies. This test drives the REAL notify worker
// with a client that blocks inside BatchNotify, so the REMOVE is genuinely mid-flight
// when onRekeyNeeded is called. Removing the gate makes it fire (epoch bumped).
func TestOnRekeyNeeded_InflightRemoveNotify_DefersRekey(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")
	kd := newTestKD(t)
	kd.session.OwnFingerprint = "aaa"
	kd.session.ExpectedPeerFingerprint = "zzz"

	fake := &blockingNotifyClient{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	var releaseOnce, closeOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(fake.release) }) }
	kd.session.GRPCClient = fake
	kd.ReconnectManager = session.NewReconnectManager(kd.session, kd.logger)
	t.Cleanup(func() { kd.ReconnectManager.Stop() })

	// Start the real notify worker (wires fs.OnLocalChange). setupFilesystem does not
	// mount; it only creates the FS struct and the worker goroutine.
	if err := kd.setupFilesystem(kd.logger, nil); err != nil {
		t.Fatalf("setupFilesystem: %v", err)
	}
	closeChan := func() { closeOnce.Do(func() { close(kd.notifyCh) }) }
	t.Cleanup(closeChan) // stop the worker (runs after releaseFn below)
	t.Cleanup(releaseFn) // unblock BatchNotify even if an assertion fails first

	// Enqueue a REMOVE through the real FUSE-change path.
	kd.FS.OnLocalChange(types.FileEvent{Action: types.RemoveFile, Path: "gone.txt"})

	// Wait until the worker has the REMOVE in flight (blocked inside BatchNotify).
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("notify worker never sent the REMOVE")
	}

	if n := kd.pendingNotifies.Load(); n == 0 {
		t.Fatal("pendingNotifies = 0 while a REMOVE is in flight, want > 0")
	}
	epochBefore := kd.session.CurrentEpoch
	if got := kd.onRekeyNeeded(); got {
		t.Fatal("onRekeyNeeded with a REMOVE in flight = true, want false (would drop the delete)")
	}
	if kd.session.CurrentEpoch != epochBefore {
		t.Fatalf("epoch bumped with a REMOVE in flight: %d -> %d", epochBefore, kd.session.CurrentEpoch)
	}

	// Let the REMOVE finish; the tracker must clear so a later idle rekey can proceed.
	releaseFn()
	waitForNotifiesDrained(t, kd, 2*time.Second)
	closeChan()
}

// TestOnRekeyNeeded_QueuedRemoveNotify_DefersRekey covers the buffered-but-undrained
// window of #6: a REMOVE is enqueued on notifyCh but the worker has not dequeued it
// yet, so pendingNotifies is still 0. A rotation here would drop the socket and, if the
// worker then flushes during the reconnect gap, lose the delete. onRekeyNeeded also
// checks the channel depth; dropping that check makes this fire (epoch bumped) despite
// the queued REMOVE.
func TestOnRekeyNeeded_QueuedRemoveNotify_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)

	// A REMOVE sits in the buffered channel, not yet dequeued: pendingNotifies stays 0,
	// mirroring the enqueue-to-dequeue window in the real notify worker.
	kd.notifyCh = make(chan *bindings.NotifyRequest, 16)
	kd.notifyCh <- &bindings.NotifyRequest{}

	if n := kd.pendingNotifies.Load(); n != 0 {
		t.Fatalf("pendingNotifies = %d, want 0 (the point is the queued notify is uncounted)", n)
	}
	epochBefore := kd.session.CurrentEpoch
	if got := kd.onRekeyNeeded(); got {
		t.Fatal("onRekeyNeeded with a REMOVE queued (undrained) = true, want false (would drop the delete)")
	}
	if kd.session.CurrentEpoch != epochBefore {
		t.Fatalf("epoch bumped with a REMOVE queued: %d -> %d", epochBefore, kd.session.CurrentEpoch)
	}
	if inRec.closed.Load() || outRec.closed.Load() {
		t.Fatalf("sockets closed with a REMOVE queued (inbound=%v outbound=%v)",
			inRec.closed.Load(), outRec.closed.Load())
	}
}

func waitForNotifiesDrained(t *testing.T, kd *KeibiDrop, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if kd.pendingNotifies.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pendingNotifies did not drain to 0 within %s (got %d)", timeout, kd.pendingNotifies.Load())
}

// blockingNotifyClient is a bindings.KeibiServiceClient whose BatchNotify signals
// once it has been entered and then blocks until release is closed, holding a notify
// "in flight" for as long as the test needs. All other RPCs are unused no-ops.
type blockingNotifyClient struct {
	entered chan struct{}
	release chan struct{}
}

func (c *blockingNotifyClient) BatchNotify(_ context.Context, _ *bindings.BatchNotifyRequest, _ ...grpc.CallOption) (*bindings.BatchNotifyResponse, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return &bindings.BatchNotifyResponse{}, nil
}

func (c *blockingNotifyClient) Notify(_ context.Context, _ *bindings.NotifyRequest, _ ...grpc.CallOption) (*bindings.NotifyResponse, error) {
	return &bindings.NotifyResponse{}, nil
}

func (c *blockingNotifyClient) Open(_ context.Context, _ *bindings.OpenRequest, _ ...grpc.CallOption) (*bindings.OpenResponse, error) {
	return nil, nil
}

func (c *blockingNotifyClient) Write(_ context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[bindings.WriteRequest, bindings.WriteResponse], error) {
	return nil, nil
}

func (c *blockingNotifyClient) Read(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse], error) {
	return nil, nil
}

func (c *blockingNotifyClient) Fsync(_ context.Context, _ *bindings.FsyncRequest, _ ...grpc.CallOption) (*bindings.FsyncResponse, error) {
	return nil, nil
}

func (c *blockingNotifyClient) Close(_ context.Context, _ *bindings.CloseRequest, _ ...grpc.CallOption) (*bindings.CloseResponse, error) {
	return nil, nil
}

func (c *blockingNotifyClient) StreamFile(_ context.Context, _ *bindings.StreamFileRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[bindings.StreamFileResponse], error) {
	return nil, nil
}

func (c *blockingNotifyClient) GetChunkHashes(_ context.Context, _ *bindings.GetChunkHashesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[bindings.GetChunkHashesResponse], error) {
	return nil, nil
}

func (c *blockingNotifyClient) Debug(_ context.Context, _ *bindings.DebugRequest, _ ...grpc.CallOption) (*bindings.DebugResponse, error) {
	return nil, nil
}

func (c *blockingNotifyClient) Rekey(_ context.Context, _ *bindings.RekeyRequest, _ ...grpc.CallOption) (*bindings.RekeyResponse, error) {
	return nil, nil
}

func (c *blockingNotifyClient) Heartbeat(_ context.Context, _ *bindings.HeartbeatRequest, _ ...grpc.CallOption) (*bindings.HeartbeatResponse, error) {
	return nil, nil
}
