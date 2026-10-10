// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: One pull per local file. A second pull, or the reconnect auto-resume, must not
// ABOUTME: take a file that is still downloading for a fresh download and truncate it.

package common

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeStreamCli serves StreamFile from data in BlockSize chunks. The first stream sends
// its first chunk, then waits for release before the rest, so a test can act while that
// pull runs. A later stream sends nothing and ends with its context. Other RPCs come
// from the embedded nil interface and panic if called.
type fakeStreamCli struct {
	bindings.KeibiServiceClient
	data    []byte
	release chan struct{}
	streams atomic.Int32
}

func (f *fakeStreamCli) StreamFile(ctx context.Context, req *bindings.StreamFileRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[bindings.StreamFileResponse], error) {
	first := f.streams.Add(1) == 1
	return &fakeFileStream{ctx: ctx, data: f.data, offset: req.StartOffset, first: first, release: f.release}, nil
}

type fakeFileStream struct {
	grpc.ClientStream
	ctx     context.Context
	data    []byte
	offset  uint64
	first   bool
	release chan struct{}
	sent    int
}

func (s *fakeFileStream) Recv() (*bindings.StreamFileResponse, error) {
	if !s.first || s.sent > 0 {
		var wait chan struct{} // nil for a later stream: it waits for its context only
		if s.first {
			wait = s.release
		}
		select {
		case <-wait:
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		}
	}
	if s.offset >= uint64(len(s.data)) {
		return nil, io.EOF
	}
	end := min(s.offset+uint64(config.BlockSize), uint64(len(s.data)))
	resp := &bindings.StreamFileResponse{Data: s.data[s.offset:end], Offset: s.offset}
	s.offset = end
	s.sent++
	return resp, nil
}

// twoChunks returns a file of two chunks with no zero bytes, so a truncate shows.
func twoChunks() []byte {
	return bytes.Repeat([]byte("bob to alice AFTER rekey"), config.BlockSize/24+2)
}

// newPullTestKD returns a bare KeibiDrop that pulls name from cli into a temp save
// folder. Cancel ends its context, and with it any pull still waiting.
func newPullTestKD(t *testing.T, cli bindings.KeibiServiceClient, name string, size int) (*KeibiDrop, string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	kd := newBareKD()
	kd.ctx = ctx
	kd.ToSave = t.TempDir()
	kd.SyncTracker = synctracker.NewSyncTracker()
	kd.SyncTracker.RemoteFiles[name] = &synctracker.File{RelativePath: name, Size: uint64(size)}
	kd.activeDownloads = make(map[string]context.CancelFunc)
	kd.activeBitmaps = make(map[string]*filesystem.ChunkBitmap)
	kd.dlRegistry = newDownloadRegistry("", nil)
	kd.session = &session.Session{GRPCClient: cli, ExpectedPeerFingerprint: "test-peer"}
	return kd, filepath.Join(kd.ToSave, name), cancel
}

// startHeldPull starts a pull and returns when it has written its first chunk and
// waits for the rest. The pull is then in the download registry, where the reconnect
// auto-resume finds it, and it has not saved a .kdbitmap yet.
func startHeldPull(t *testing.T, kd *KeibiDrop, name, dst string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- kd.PullFile(name, dst) }()
	require.Eventually(t, func() bool { return kd.GetDownloadProgress(name) > 0 }, 5*time.Second, time.Millisecond)
	return done
}

func requireFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, len(want), len(got), "file size")
	if i := bytes.IndexByte(got, 0); i >= 0 {
		t.Fatalf("file has a zero byte at offset %d: its bytes were truncated", i)
	}
	require.True(t, bytes.Equal(want, got), "file bytes differ")
}

// TestPull_AutoResumeDuringAPullKeepsItsBytes: after a reconnect, the auto-resume found a
// pull that was still running in the registry and pulled the same file again. That pull
// found no .kdbitmap, opened the file with O_TRUNC and the first pull's bytes became zeros.
func TestPull_AutoResumeDuringAPullKeepsItsBytes(t *testing.T) {
	data := twoChunks()
	cli := &fakeStreamCli{data: data, release: make(chan struct{})}
	kd, dst, cancel := newPullTestKD(t, cli, "f.bin", len(data))
	done := startHeldPull(t, kd, "f.bin", dst)

	resumed := make(chan struct{})
	go func() { kd.resumePartialDownloads(kd.logger); close(resumed) }()
	select {
	case <-resumed: // it left the running pull alone
	case <-time.After(200 * time.Millisecond): // it is pulling the same file a second time
	}

	close(cli.release)
	require.NoError(t, <-done)
	requireFileEquals(t, dst, data)
	require.EqualValues(t, 1, cli.streams.Load(), "the auto-resume pulled a file that is still downloading")

	cancel() // ends a second pull, if one started
	<-resumed
}

// TestPull_SecondPullOfARunningFileIsRefused: a second pull of the same local file
// returns ErrDownloadInProgress at once and does not touch the file.
func TestPull_SecondPullOfARunningFileIsRefused(t *testing.T) {
	data := twoChunks()
	cli := &fakeStreamCli{data: data, release: make(chan struct{})}
	kd, dst, cancel := newPullTestKD(t, cli, "f.bin", len(data))
	done := startHeldPull(t, kd, "f.bin", dst)

	second := make(chan error, 1)
	go func() { second <- kd.PullFile("f.bin", dst) }()
	select {
	case err := <-second:
		require.ErrorIs(t, err, ErrDownloadInProgress)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the second pull did not return: it started a parallel download of the same file")
	}

	close(cli.release)
	require.NoError(t, <-done)
	requireFileEquals(t, dst, data)
	require.EqualValues(t, 1, cli.streams.Load(), "the second pull opened a stream")
}
