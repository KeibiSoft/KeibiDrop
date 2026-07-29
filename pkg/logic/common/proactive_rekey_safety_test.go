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

// closeRecorder records whether Close was called, so a test can assert a rekey did or did not drop the sockets.
type closeRecorder struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// newRecordedSecureConn returns a *session.SecureConn whose conn records Close calls.
// Zero key is fine: these conns are only closed, never read or written.
func newRecordedSecureConn(t *testing.T, prefix uint32) (*session.SecureConn, *closeRecorder) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	rec := &closeRecorder{Conn: c1}
	key := make([]byte, kbc.KeySize)
	return session.NewSecureConn(rec, key, kbc.CipherChaCha20, prefix), rec
}

// newInitiatorRekeyKD builds a KeibiDrop where every onRekeyNeeded guard passes (ratchet
// off, this peer the initiator, Connected, no cooldown); a test adds one blocker and asserts the rekey defers.
func newInitiatorRekeyKD(t *testing.T) (kd *KeibiDrop, inRec, outRec *closeRecorder) {
	t.Helper()
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

// TestOnRekeyNeeded_SessionNilRace_NoPanic reproduces F2: teardown nils kd.session while
// the health monitor calls onRekeyNeeded; the fix snapshots kd.session once under kd.mu.
func TestOnRekeyNeeded_SessionNilRace_NoPanic(t *testing.T) {
	kd := newTestKD(t)
	// Initiator held past the rekey cooldown, so a call surviving the nil race returns
	// before any OnDisconnect side effects.
	kd.session.OwnFingerprint = "aaa"
	kd.session.ExpectedPeerFingerprint = "zzz"
	kd.session.LastRekeyAt = time.Now()
	kd.ReconnectManager = session.NewReconnectManager(kd.session, kd.logger)
	t.Cleanup(func() { kd.ReconnectManager.Stop() })

	// The writer swaps kd.session between nil and a ready replacement (also an initiator past cooldown).
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
	// Reaching here without a panic (and no -race DATA RACE) is the pass.
}

// TestOnRekeyNeeded_ActiveTransfer_DefersRekey (F3): an active transfer must not have its
// sockets dropped by a proactive rekey; onRekeyNeeded carries the hasActiveTransfers guard.
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

// TestOnRekeyNeeded_RatchetOn_DefersRekey (T5): with the in-band ratchet negotiated, the
// re-handshake rekey must not fire; the UseKeyUpdate guard defers it.
func TestOnRekeyNeeded_RatchetOn_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)
	kd.session.PeerSupportsKeyUpdate = true // both peers advertised: ratchet on

	epochBefore := kd.session.CurrentEpoch
	if got := kd.onRekeyNeeded(); got {
		t.Fatal("onRekeyNeeded with the ratchet on = true, want false (the ratchet rotates)")
	}
	if kd.session.CurrentEpoch != epochBefore {
		t.Fatalf("epoch bumped with the ratchet on: %d -> %d", epochBefore, kd.session.CurrentEpoch)
	}
	if inRec.closed.Load() || outRec.closed.Load() {
		t.Fatalf("sockets closed with the ratchet on (inbound=%v outbound=%v)",
			inRec.closed.Load(), outRec.closed.Load())
	}
}

// TestOnRekeyNeeded_InflightRemoveNotify_DefersRekey (#6): a REMOVE notify in flight must
// defer the rekey, else the drop loses the delete; the pendingNotifies gate blocks it. The
// test drives the real notify worker with a client that blocks inside BatchNotify.
func TestOnRekeyNeeded_InflightRemoveNotify_DefersRekey(t *testing.T) {
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

	// Start the real notify worker; setupFilesystem creates the FS struct and worker goroutine without mounting.
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

// TestOnRekeyNeeded_QueuedRemoveNotify_DefersRekey (#6): a REMOVE queued but not yet
// dequeued (pendingNotifies still 0) must defer the rekey; onRekeyNeeded checks channel depth too.
func TestOnRekeyNeeded_QueuedRemoveNotify_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)

	// A REMOVE sits buffered but not yet dequeued, so pendingNotifies stays 0.
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

// TestOnRekeyNeeded_KeyUpdateNearEpochWrap_Rekeys (MED-2): near the epoch-wrap guard the
// ratchet stops advancing, so onRekeyNeeded must re-handshake for a fresh epoch-0 key.
func TestOnRekeyNeeded_KeyUpdateNearEpochWrap_Rekeys(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)
	kd.session.PeerSupportsKeyUpdate = true // ratchet on: would normally defer

	// Writer epoch at the guard where the ratchet stops: 0xFFFF is above
	// epochRehandshakeThreshold, so NearEpochWrap() is true.
	kd.session.Session.Outbound.SetWriterEpochForTest(0xFFFF)

	epochBefore := kd.session.CurrentEpoch
	if got := kd.onRekeyNeeded(); !got {
		t.Fatal("onRekeyNeeded near the epoch-wrap guard = false, want true (must re-handshake)")
	}
	if kd.session.CurrentEpoch == epochBefore {
		t.Fatalf("epoch not bumped near the wrap guard: stayed at %d", epochBefore)
	}
	if !inRec.closed.Load() || !outRec.closed.Load() {
		t.Fatalf("sockets not dropped by the near-wrap re-handshake (inbound=%v outbound=%v)",
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

// blockingNotifyClient's BatchNotify signals once entered then blocks until release is
// closed, holding a notify in flight; other RPCs are no-ops.
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
