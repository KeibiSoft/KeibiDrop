// ABOUTME: Tests for Read handler fallback to SyncTracker.LocalFiles when FUSE is active.
// ABOUTME: Regression test for drag-and-drop files not found by FUSE-mode Read handler.

package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockReadStream implements bindings.KeibiService_ReadServer for testing.
type mockReadStream struct {
	requests []*bindings.ReadRequest
	idx      int
	sent     []*bindings.ReadResponse
}

func (m *mockReadStream) Send(resp *bindings.ReadResponse) error {
	m.sent = append(m.sent, resp)
	return nil
}

func (m *mockReadStream) Recv() (*bindings.ReadRequest, error) {
	if m.idx >= len(m.requests) {
		return nil, io.EOF
	}
	req := m.requests[m.idx]
	m.idx++
	return req, nil
}

func (m *mockReadStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockReadStream) SendHeader(metadata.MD) error  { return nil }
func (m *mockReadStream) SetTrailer(metadata.MD)        {}
func (m *mockReadStream) Context() context.Context       { return context.Background() }
func (m *mockReadStream) SendMsg(interface{}) error      { return nil }
func (m *mockReadStream) RecvMsg(interface{}) error      { return nil }

// TestRead_FUSEMode_FallbackToLocalFiles verifies that when FUSE is active but a
// file was added via drag-and-drop (AddFile → SyncTracker.LocalFiles), the Read
// handler can still find and serve the file.
func TestRead_FUSEMode_FallbackToLocalFiles(t *testing.T) {
	// Create a temp file to serve as the "dropped" file.
	tmpFile, err := os.CreateTemp(t.TempDir(), "dropped-*.txt")
	require.NoError(t, err)
	content := []byte("hello from drag and drop")
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := &KeibidropServiceImpl{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FS: &filesystem.FS{
			Root: &filesystem.Dir{
				AfmLock:    sync.RWMutex{},
				AllFileMap: make(map[string]*filesystem.File),
			},
		},
		SyncTracker: synctracker.NewSyncTracker(),
	}

	// Simulate AddFile: file goes into SyncTracker.LocalFiles only.
	svc.SyncTracker.LocalFiles["test.txt"] = &synctracker.File{
		Name:           "test.txt",
		RelativePath:   "test.txt",
		RealPathOfFile: tmpFile.Name(),
		Size:           uint64(len(content)),
	}

	stream := &mockReadStream{
		requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "test.txt", Offset: 0, Size: uint32(len(content))},
		},
	}

	err = svc.Read(stream)
	require.NoError(t, err)
	require.Len(t, stream.sent, 1)
	assert.Equal(t, content, stream.sent[0].Data)
}

// TestRead_FUSEMode_AllFileMapStillWorks verifies the normal FUSE path still works
// (files in AllFileMap are found without needing the fallback).
func TestRead_FUSEMode_AllFileMapStillWorks(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "fuse-*.txt")
	require.NoError(t, err)
	content := []byte("file from FUSE mount")
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := &KeibidropServiceImpl{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FS: &filesystem.FS{
			Root: &filesystem.Dir{
				AfmLock: sync.RWMutex{},
				AllFileMap: map[string]*filesystem.File{
					"/test.txt": {
						RealPathOfFile: tmpFile.Name(),
					},
				},
			},
		},
		SyncTracker: synctracker.NewSyncTracker(),
	}

	stream := &mockReadStream{
		requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "test.txt", Offset: 0, Size: uint32(len(content))},
		},
	}

	err = svc.Read(stream)
	require.NoError(t, err)
	require.Len(t, stream.sent, 1)
	assert.Equal(t, content, stream.sent[0].Data)
}

// TestRead_FUSEMode_FileNotFoundAnywhere verifies we still get NotFound when
// the file isn't in AllFileMap OR LocalFiles.
func TestRead_FUSEMode_FileNotFoundAnywhere(t *testing.T) {
	svc := &KeibidropServiceImpl{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FS: &filesystem.FS{
			Root: &filesystem.Dir{
				AfmLock:    sync.RWMutex{},
				AllFileMap: make(map[string]*filesystem.File),
			},
		},
		SyncTracker: synctracker.NewSyncTracker(),
	}

	stream := &mockReadStream{
		requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "nonexistent.txt", Offset: 0, Size: 100},
		},
	}

	err := svc.Read(stream)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}
