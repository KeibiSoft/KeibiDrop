# KeibiDrop v0.1.0 Implementation Plan

## Phase 0: Establish Baseline
- [x] **0.1** Run `make test` on current `main` — confirm green
- [ ] **0.2** Run `make lint && make sec` — confirm clean
- [ ] **0.3** Build full binary: `make build-static-rust-bridge && cd rust && cargo build --release`
- [x] **0.4** Manual smoke test (Alice + Bob via relay)
  - [x] Start Relay
  - [x] Alice: Create Room
  - [x] Bob: Join Room
  - [x] Alice -> Bob: Transfer file
  - [x] Bob -> Alice: Transfer file
  - [x] Disconnect

## Phase 1: Security PRs — Wave 1 (Independent, Safe)

### 1.1 — PR #55: Fuzz gRPC Notify handler (Branch: `test/grpc-notify-fuzz`)
- [x] **1.1.a** Verify branch: fuzz test run 30s — no findings
- [x] **1.1.b** E2E Verification: Alice→Bob and Bob→Alice file transfer (incl. 35MB binary)
- [x] **1.1.c** Merged via GitHub PR #55
- [x] **1.1.d** Post-merge verify: `go test -timeout 300s ./...` — all green

### 1.2 — PR #41: Path traversal fix via SecureJoin [CRITICAL] (Branch: `security/fix-path-traversal`)
- [x] **1.2.a** Verify branch: `git checkout security/fix-path-traversal && make test`
- [x] **1.2.b** E2E Verification on branch: Build + Smoke Test (Alice + Bob)
- [x] **1.2.c** Merged via GitHub PR #41
- [x] **1.2.d** Post-merge verify: `go test -timeout 300s ./...` — all green

### 1.3 — PR #40: Handle RNG errors in GenerateSeed() [MEDIUM] (Branch: `security/fix-rng-error`)
- [ ] **1.3.a** Verify branch: `git checkout security/fix-rng-error && make test`
- [ ] **1.3.b** E2E Verification on branch: Build + Smoke Test (Alice + Bob)
- [ ] **1.3.c** Merge: `git checkout main && git merge security/fix-rng-error`
- [ ] **1.3.d** Post-merge verify: `make test`

### 1.4 — PR #42: Stream integrity + AEAD key encapsulation [MEDIUM] (Branch: `security/fix-stream-integrity`)
- [ ] **1.4.a** Verify branch: `git checkout security/fix-stream-integrity && make test`
- [ ] **1.4.b** E2E Verification on branch: Build + Smoke Test (Alice + Bob)
- [ ] **1.4.c** Merge: `git checkout main && git merge security/fix-stream-integrity`
- [ ] **1.4.d** Post-merge verify: `make test`

### Wave 1 Gate
- [ ] **1.5** Full test suite: `make test && make lint && make sec`
- [ ] **1.6** Full rebuild: `make build-static-rust-bridge && cd rust && cargo build --release`
- [ ] **1.7** Final Wave 1 E2E smoke test
