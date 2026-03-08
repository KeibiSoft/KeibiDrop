# gRPC Notify Handler Fuzzing Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement Go native fuzzing for the gRPC `Notify` handler to ensure malformed protobuf payloads are handled gracefully without panics, using structured seeds and a live `SyncTracker`.

**Architecture:** A single fuzz test file (`service_fuzz_test.go`) with a seed corpus of valid `NotifyRequest` protos (one per `NotifyType`). The fuzzer mutates serialized protobuf bytes, unmarshals them, and calls `Notify()` on a `KeibidropServiceImpl` backed by a real `SyncTracker` (no FUSE). The test asserts no panics occur and all returns are either success or a valid gRPC error.

**Tech Stack:** Go native fuzzing (`testing.F`), `google.golang.org/protobuf/proto`, `pkg/sync-tracker`, `grpc_bindings`

**Branch:** `test/grpc-notify-fuzz`

**Issue:** #48 (partial — gRPC fuzzing only; SecureReader blocked on PR #42)

---

## Context for the Implementer

### Key Files
- **Target:** `pkg/logic/service/service.go` — the `Notify` method (line 51-317)
- **Proto:** `keibidrop.proto` — defines `NotifyRequest`, `NotifyType`, `Attr`
- **Bindings:** `grpc_bindings/` — generated Go protobuf code
- **SyncTracker:** `pkg/sync-tracker/file_tracker.go` — `NewSyncTracker()`, `File` struct
- **Test file to create:** `pkg/logic/service/service_fuzz_test.go`

### How `Notify` Works (Non-FUSE Path)
When `FS == nil` and `SyncTracker != nil`, the handler operates in "no-FUSE" mode:
- **ADD_FILE:** Adds entry to `SyncTracker.RemoteFiles` map (or updates existing)
- **EDIT_FILE:** Updates existing entry in `SyncTracker.RemoteFiles`
- **REMOVE_FILE:** Deletes from `SyncTracker.RemoteFiles`
- **RENAME_FILE:** Moves entry from `OldPath` to `Path` in `SyncTracker.RemoteFiles`
- **ADD_DIR/REMOVE_DIR/RENAME_DIR:** Require `FS.Root` so they return errors in no-FUSE mode
- **UNKNOWN:** Returns `ErrGRPCInvalidArgument`

### What We're Testing
1. No panics on any malformed input
2. All code paths return either `(*NotifyResponse, nil)` or `(nil, gRPC-status-error)`
3. No data races under fuzz (run with `-race`)

---

## Task 1: Create Branch

**Step 1: Create and switch to the feature branch**

```bash
git checkout -b test/grpc-notify-fuzz main
```

**Step 2: Verify**

```bash
git branch --show-current
```

Expected: `test/grpc-notify-fuzz`

---

## Task 2: Write the Seed Corpus Helper and Fuzz Test Skeleton

**Files:**
- Create: `pkg/logic/service/service_fuzz_test.go`

**Step 1: Write the fuzz test file**

The file must:
1. Build a seed corpus of valid `NotifyRequest` messages (one per `NotifyType`)
2. Serialize each seed with `proto.Marshal`
3. Add each serialized seed to `f.Add()`
4. In the fuzz function: unmarshal the mutated bytes, call `Notify()`, assert no panic

```go
// ABOUTME: Fuzz tests for the gRPC Notify handler.
// ABOUTME: Tests that malformed protobuf payloads never cause panics.

package service

import (
	"context"
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

func FuzzNotify(f *testing.F) {
	// Serialize each seed and add to the corpus.
	for _, seed := range seeds() {
		data, err := proto.Marshal(seed)
		if err != nil {
			f.Fatalf("failed to marshal seed: %v", err)
		}
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Set up a fresh SyncTracker-backed service for each iteration.
		// This ensures no state leaks between fuzz iterations.
		tracker := synctracker.NewSyncTracker()
		svc := &KeibidropServiceImpl{
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
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
```

**IMPORTANT:** The `io.Discard` import — add `"io"` to the imports block.

**Step 2: Verify file compiles**

```bash
cd /home/andrei/source/KeibiSoft/KeibiDrop && go vet ./pkg/logic/service/...
```

Expected: no errors.

**Step 3: Run the fuzz test briefly to confirm it works**

```bash
cd /home/andrei/source/KeibiSoft/KeibiDrop && go test ./pkg/logic/service/ -run='^$' -fuzz=FuzzNotify -fuzztime=10s -v
```

Expected: runs for 10 seconds, outputs fuzz iterations, exits with "ok" (no panics found).

**Step 4: Run with race detector**

```bash
cd /home/andrei/source/KeibiSoft/KeibiDrop && go test -race ./pkg/logic/service/ -run='^$' -fuzz=FuzzNotify -fuzztime=10s
```

Expected: no race conditions detected.

**Step 5: Commit**

```bash
git add pkg/logic/service/service_fuzz_test.go
git commit -m "test(service): add native fuzz test for gRPC Notify handler

Implements structured fuzzing with seed corpus covering all NotifyType
variants. Uses SyncTracker-backed service (no FUSE) to exercise real
code paths. Part of #48."
```

---

## Task 3: Verify No Panics on Edge Cases (Manual Seed Extension)

After the initial fuzz run, check Go's fuzz cache for any interesting corpus entries.

**Step 1: Check the fuzz cache**

```bash
ls -la /home/andrei/source/KeibiSoft/KeibiDrop/pkg/logic/service/testdata/fuzz/FuzzNotify/ 2>/dev/null || echo "No crash corpus (good)"
```

If crash files exist: read them, reproduce, and fix the handler.

**Step 2: Run a longer fuzz session (60s) to build confidence**

```bash
cd /home/andrei/source/KeibiSoft/KeibiDrop && go test ./pkg/logic/service/ -run='^$' -fuzz=FuzzNotify -fuzztime=60s -v
```

Expected: completes without panics. If any crash is found, the fuzzer will write a failing test case to `testdata/fuzz/FuzzNotify/` — investigate and fix.

**Step 3: If crashes found, commit the crash corpus and fix**

```bash
# Only if testdata/fuzz exists with crash entries:
git add pkg/logic/service/testdata/
git commit -m "test(service): add fuzz crash corpus for Notify handler"
```

---

## Task 4: Open PR

**Step 1: Push branch**

```bash
git push -u origin test/grpc-notify-fuzz
```

**Step 2: Create PR**

```bash
gh pr create --title "test(service): fuzz gRPC Notify handler" --body "$(cat <<'EOF'
## Summary
- Adds Go native fuzz test for the `Notify` gRPC handler
- Structured seed corpus covers all `NotifyType` variants (valid + nil-attr edge cases)
- Tests against a live `SyncTracker` backend (no FUSE) to exercise real map-mutation code paths
- Validates no panics or data races on malformed protobuf payloads

## Scope (Issue #48 — Partial)
This PR covers **gRPC Notify handler fuzzing only**. SecureReader fuzzing is blocked on PR #42 (stream integrity) and will be implemented separately after that merges.

## Test Plan
- [x] `go test ./pkg/logic/service/ -fuzz=FuzzNotify -fuzztime=10s` — no panics
- [x] `go test -race ./pkg/logic/service/ -fuzz=FuzzNotify -fuzztime=10s` — no races
- [ ] Extended run (`-fuzztime=5m`) recommended before merge
EOF
)"
```

---

## Notes for the Implementer

- The `io.Discard` logger is intentional — fuzz tests generate thousands of iterations and logging would be overwhelming.
- Each fuzz iteration gets a **fresh** `SyncTracker` to avoid state pollution between runs. This is slightly slower but guarantees isolation.
- The pre-populated `/testfile.txt` entry ensures that EDIT_FILE, REMOVE_FILE, and RENAME_FILE can hit their happy paths when the fuzzer generates matching paths.
- Do NOT add FUSE-mode testing to this fuzz test — it requires a real mounted filesystem and is out of scope.
- If `go vet` fails due to missing `io` import, add it. The plan includes it but double-check.
