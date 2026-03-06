// ABOUTME: Fuzz tests for the gRPC Notify handler.
// ABOUTME: Tests that malformed protobuf payloads never cause panics.

package service

import (
	"context"
	"io"
	"log/slog"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"google.golang.org/protobuf/proto"
)

// seeds returns one valid NotifyRequest per NotifyType for the fuzz corpus.
func seeds() []*bindings.NotifyRequest {
	attr := &bindings.Attr{
		Dev:              1,
		Ino:              1000,
		Mode:             0644,
		Size:             512,
		AccessTime:       1700000000000000000,
		ModificationTime: 1700000000000000000,
		ChangeTime:       1700000000000000000,
		BirthTime:        1700000000000000000,
		Flags:            0,
	}

	return []*bindings.NotifyRequest{
		{Type: bindings.NotifyType_UNKNOWN, Path: "/test"},
		{Type: bindings.NotifyType_ADD_DIR, Path: "/testdir", Name: "testdir"},
		{Type: bindings.NotifyType_ADD_FILE, Path: "/testfile.txt", Name: "testfile.txt", Attr: attr},
		{Type: bindings.NotifyType_EDIT_DIR, Path: "/testdir"},
		{Type: bindings.NotifyType_EDIT_FILE, Path: "/testfile.txt", Name: "testfile.txt", Attr: attr},
		{Type: bindings.NotifyType_REMOVE_DIR, Path: "/testdir"},
		{Type: bindings.NotifyType_REMOVE_FILE, Path: "/testfile.txt"},
		{Type: bindings.NotifyType_RENAME_FILE, Path: "/renamed.txt", OldPath: "/testfile.txt", Name: "renamed.txt"},
		{Type: bindings.NotifyType_RENAME_DIR, Path: "/renameddir", OldPath: "/testdir", Name: "renameddir"},
		// Nil attr cases (triggers error paths for ADD_FILE/EDIT_FILE).
		{Type: bindings.NotifyType_ADD_FILE, Path: "/noattr.txt", Name: "noattr.txt"},
		{Type: bindings.NotifyType_EDIT_FILE, Path: "/noattr.txt", Name: "noattr.txt"},
	}
}

// maxFuzzInputSize caps fuzzed input to avoid excessive allocation in proto.Unmarshal.
const maxFuzzInputSize = 4096

func FuzzNotify(f *testing.F) {
	// Serialize each seed and add to the corpus.
	for _, seed := range seeds() {
		data, err := proto.Marshal(seed)
		if err != nil {
			f.Fatalf("failed to marshal seed: %v", err)
		}
		f.Add(data)
	}

	// Hoist logger outside the closure: level set high so Enabled() returns
	// false and no formatting occurs. Eliminates per-iteration slog overhead
	// that causes GC-induced stalls during long fuzz runs.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.Level(9999),
	}))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFuzzInputSize {
			return
		}

		// Fresh SyncTracker per iteration for isolation.
		tracker := synctracker.NewSyncTracker()
		svc := &KeibidropServiceImpl{
			Logger:      logger,
			SyncTracker: tracker,
		}

		// Pre-populate a file so EDIT_FILE/REMOVE_FILE/RENAME_FILE
		// can exercise their happy paths (not just "not found").
		tracker.RemoteFiles["/testfile.txt"] = &synctracker.File{
			Name:         "testfile.txt",
			RelativePath: "/testfile.txt",
			Size:         512,
		}

		// Attempt to unmarshal the fuzzed bytes into a NotifyRequest.
		var req bindings.NotifyRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			// Invalid protobuf — skip (we're testing the handler, not proto).
			return
		}

		// Call the handler. Must not panic. Error returns are fine.
		_, _ = svc.Notify(context.Background(), &req)
	})
}
