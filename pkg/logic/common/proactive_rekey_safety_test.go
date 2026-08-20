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
	"github.com/stretchr/testify/require"
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

// TestOnRekeyNeeded_SessionNilRace_NoPanic reproduces a nil-deref TOCTOU: teardown nils
// kd.session while the health monitor calls onRekeyNeeded; the fix snapshots kd.session once under kd.mu.
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

// TestOnRekeyNeeded_ActiveTransfer_DefersRekey: an active transfer must not have its
// sockets dropped by a proactive rekey; onRekeyNeeded carries the hasActiveTransfers guard.
func TestOnRekeyNeeded_ActiveTransfer_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)

	// An in-progress download makes hasActiveTransfers() true.
	kd.activeDownloadsMu.Lock()
	kd.activeDownloads["serving.bin"] = func() {}
	kd.activeDownloadsMu.Unlock()

	epochBefore := kd.session.CurrentEpoch
	got := kd.onRekeyNeeded()
	require.False(t, got, "onRekeyNeeded during active transfer must defer, not rekey")
	require.Equal(t, epochBefore, kd.session.CurrentEpoch, "epoch bumped during active transfer")
	require.False(t, inRec.closed.Load(), "inbound socket closed during active transfer")
	require.False(t, outRec.closed.Load(), "outbound socket closed during active transfer")
}

// TestOnRekeyNeeded_RatchetOn_DefersRekey: with the in-band ratchet negotiated, the
// re-handshake rekey must not fire; the UseKeyUpdate guard defers it.
func TestOnRekeyNeeded_RatchetOn_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)
	kd.session.PeerSupportsKeyUpdate = true // both peers advertised: ratchet on

	epochBefore := kd.session.CurrentEpoch
	got := kd.onRekeyNeeded()
	require.False(t, got, "onRekeyNeeded with the ratchet on must defer (the ratchet rotates)")
	require.Equal(t, epochBefore, kd.session.CurrentEpoch, "epoch bumped with the ratchet on")
	require.False(t, inRec.closed.Load(), "inbound socket closed with the ratchet on")
	require.False(t, outRec.closed.Load(), "outbound socket closed with the ratchet on")
}

// TestOnRekeyNeeded_InflightRemoveNotify_DefersRekey: a REMOVE notify in flight must
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
	require.NoError(t, kd.setupFilesystem(kd.logger, nil), "setupFilesystem")
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

	require.NotZero(t, kd.pendingNotifies.Load(), "pendingNotifies = 0 while a REMOVE is in flight")
	epochBefore := kd.session.CurrentEpoch
	got := kd.onRekeyNeeded()
	require.False(t, got, "onRekeyNeeded with a REMOVE in flight must defer (would drop the delete)")
	require.Equal(t, epochBefore, kd.session.CurrentEpoch, "epoch bumped with a REMOVE in flight")

	// Let the REMOVE finish; the tracker must clear so a later idle rekey can proceed.
	releaseFn()
	waitForNotifiesDrained(t, kd, 2*time.Second)
	closeChan()
}

// TestOnRekeyNeeded_QueuedRemoveNotify_DefersRekey: a REMOVE queued but not yet
// dequeued (pendingNotifies still 0) must defer the rekey; onRekeyNeeded checks channel depth too.
func TestOnRekeyNeeded_QueuedRemoveNotify_DefersRekey(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)

	// A REMOVE sits buffered but not yet dequeued, so pendingNotifies stays 0.
	kd.notifyCh = make(chan *bindings.NotifyRequest, 16)
	kd.notifyCh <- &bindings.NotifyRequest{}

	require.Zero(t, kd.pendingNotifies.Load(), "the queued notify must be uncounted")
	epochBefore := kd.session.CurrentEpoch
	got := kd.onRekeyNeeded()
	require.False(t, got, "onRekeyNeeded with a REMOVE queued (undrained) must defer (would drop the delete)")
	require.Equal(t, epochBefore, kd.session.CurrentEpoch, "epoch bumped with a REMOVE queued")
	require.False(t, inRec.closed.Load(), "inbound socket closed with a REMOVE queued")
	require.False(t, outRec.closed.Load(), "outbound socket closed with a REMOVE queued")
}

// TestOnRekeyNeeded_KeyUpdateNearEpochWrap_Rekeys: near the epoch-wrap guard the
// ratchet stops advancing, so onRekeyNeeded must re-handshake for a fresh epoch-0 key.
func TestOnRekeyNeeded_KeyUpdateNearEpochWrap_Rekeys(t *testing.T) {
	kd, inRec, outRec := newInitiatorRekeyKD(t)
	kd.session.PeerSupportsKeyUpdate = true // ratchet on: would normally defer

	// Writer epoch at the guard where the ratchet stops: 0xFFFF is above
	// epochRehandshakeThreshold, so NearEpochWrap() is true.
	kd.session.Session.Outbound.SetWriterEpochForTest(0xFFFF)

	epochBefore := kd.session.CurrentEpoch
	got := kd.onRekeyNeeded()
	require.True(t, got, "onRekeyNeeded near the epoch-wrap guard must re-handshake")
	require.NotEqual(t, epochBefore, kd.session.CurrentEpoch, "epoch not bumped near the wrap guard")
	require.True(t, inRec.closed.Load(), "inbound socket not dropped by the near-wrap re-handshake")
	require.True(t, outRec.closed.Load(), "outbound socket not dropped by the near-wrap re-handshake")
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

func (c *blockingNotifyClient) ReadBatch(_ context.Context, _ *bindings.ReadBatchRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[bindings.ReadBatchResponse], error) {
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
