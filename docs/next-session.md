# Next Session Handoff

## Status
Last completed: fix-test-suite — full test suite made green (zero failures) on `test/grpc-notify-fuzz`
Next task: 1.1 — Verify fuzz branch, E2E smoke test, merge to main
Plan file: TODO.md

## What Was Done
- Fixed AB-BA deadlock in no-FUSE gRPC Read handler (release `LocalFilesMu.RLock` immediately after path lookup)
- Fixed `LargeBinaryFromRemote` corruption: added `sync.Mutex` to `ImplRemoteFileStream.ReadAt` to serialize concurrent Send+Recv pairs
- Fixed `REMOVE_FILE` notify: removes local placeholder file from disk using `RemoteFiles` for path lookup
- Fixed `RENAME_FILE` notify: renames local placeholder file on disk
- Fixed `REMOVE_DIR` notify: calls `os.RemoveAll` on local directory
- Fixed `rmdirInternal`: removed erroneous `!isRemoteDir` guard that suppressed peer notifications for locally-created dirs
- Added `attr_timeout=0,entry_timeout=0` to FUSE mount options so kernel dentry cache doesn't serve stale entries
- Rewrote `TestKeibiDropFlow` to use `SetupPeerPairWithTimeout` mock relay harness (removed hardcoded Mac paths)
- Replaced hardcoded Mac paths in `fuse_operations_test.go` with `t.TempDir()` + dynamic ports + `NewKeibiDropWithIP`
- Hardened port availability check in `harness_test.go` for both IPv4 and IPv6
- All tests pass: `go test -timeout 300s ./...` — 176s, zero failures
- Committed as: `fix(tests): make full test suite pass with zero failures`
- Branch: `test/grpc-notify-fuzz`

## What's Left (ordered)
- [ ] **1.1.a** Verify fuzz: `go test -fuzz=FuzzNotifyHandler -fuzztime=30s ./pkg/logic/service/`
- [ ] **1.1.b** E2E smoke test on branch: Build + Alice + Bob file transfer both ways
- [ ] **1.1.c** Merge to main: `git checkout main && git merge test/grpc-notify-fuzz`
- [ ] **1.1.d** Post-merge verify: `make test`
- [ ] **1.2** PR #41: Path traversal fix via SecureJoin (Branch: `security/fix-path-traversal`)
- [ ] **1.3** PR #40: Handle RNG errors in GenerateSeed() (Branch: `security/fix-rng-error`)
- [ ] **1.4** PR #42: Stream integrity + AEAD key encapsulation (Branch: `security/fix-stream-integrity`)
- [ ] **1.5** Wave 1 Gate: `make test && make lint && make sec`

## Task Specs

### 1.1 — Fuzz branch verification and merge to main

**Goal**: Confirm `test/grpc-notify-fuzz` is solid, then land it on main.

Steps:
1. **1.1.a** On branch `test/grpc-notify-fuzz`, run:
   ```
   go test -fuzz=FuzzNotifyHandler -fuzztime=30s ./pkg/logic/service/
   ```
   Confirm no crashes or findings.
2. **1.1.b** E2E smoke test: build binary, start relay, start Alice + Bob, transfer a file each way.
   ```bash
   make build-static-rust-bridge && cd rust && cargo build --release
   ```
   Launch commands are in MEMORY.md.
3. **1.1.c** Merge to main:
   ```bash
   git checkout main && git merge test/grpc-notify-fuzz
   ```
4. **1.1.d** Post-merge full test suite:
   ```bash
   make test
   ```

### 1.2 — PR #41: Path traversal fix

Branch: `security/fix-path-traversal`
- `git checkout security/fix-path-traversal && make test`
- E2E smoke test
- `git checkout main && git merge security/fix-path-traversal`
- `make test`

### 1.3 — PR #40: RNG error handling

Branch: `security/fix-rng-error`
- Same verify-then-merge pattern as 1.2.

### 1.4 — PR #42: Stream integrity + AEAD

Branch: `security/fix-stream-integrity`
- Same verify-then-merge pattern as 1.2.

## Instructions for the Next Session

1. Read this file completely before doing anything.
2. We are on branch `test/grpc-notify-fuzz`. All tests pass.
3. Start with **1.1.a**: `go test -fuzz=FuzzNotifyHandler -fuzztime=30s ./pkg/logic/service/`
4. Then **1.1.b**: E2E smoke test (Alice + Bob file transfer both ways).
5. Then **1.1.c**: merge to main.
6. Then **1.1.d**: `make test` on main.
7. If any step fails, fix it before proceeding.
8. After 1.1 is complete, proceed with 1.2 (same pattern).
9. Update this handoff file and commit: `chore(handoff): update next-session after 1.1`
10. Push.
11. Tell the user the task is done and suggest running /clear.

> After completing your task, update docs/next-session.md following the Session Handoff skill pattern (skill name: session-handoff). Mark your task done, advance the next task, copy in the next task spec, commit with `chore(handoff): update next-session after <task-id>`, and push.
