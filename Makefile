COMMIT  := $(shell git rev-parse HEAD)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
LDFLAGS := -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=$(VERSION) \
           -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=$(COMMIT)
DIST    := dist
GOARCH  := $(shell go env GOARCH)
GOOS    := $(shell go env GOOS)

# ── Build ──────────────────────────────────────────────────

build-cli:
	go build -ldflags="$(LDFLAGS)" -o keibidrop-cli cmd/cli/keibidrop-cli.go

build-kd:
	go build -ldflags="$(LDFLAGS)" -o kd ./cmd/kd

build-static-rust-bridge:
	go build -buildmode=c-archive -o libkeibidrop.a ./rustbridge

build-rust: protoc build-static-rust-bridge
	cd rust && cargo build --release

build-all: build-rust build-cli build-kd

# ── Test & Lint ────────────────────────────────────────────

test:
	go test -v -count=1 -timeout 180s ./tests/...

lint:
	golangci-lint run ./...

sec:
	gosec -exclude-generated -conf .gosec.json ./...

# ── Protobuf ──────────────────────────────────────────────

install-proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

protoc:
	protoc --go_opt=module=github.com/KeibiSoft/KeibiDrop \
	       --go-grpc_opt=module=github.com/KeibiSoft/KeibiDrop \
	       --go_out=. --go-grpc_out=. keibidrop.proto

# ── Rust helpers ──────────────────────────────────────────

rust-bindings: build-static-rust-bridge
	bindgen libkeibidrop.h -o rust/src/bindings.rs

slint-preview:
	slint-viewer rust/src/ui.slint

# ── Packaging ─────────────────────────────────────────────

$(DIST):
	mkdir -p $(DIST)

# macOS .dmg — contains all three binaries + README
package-macos: $(DIST)
	@echo "Packaging macOS .dmg for $(GOARCH)..."
	mkdir -p $(DIST)/dmg-staging
	cp rust/target/release/keibidrop-rust $(DIST)/dmg-staging/keibidrop
	cp keibidrop-cli $(DIST)/dmg-staging/
	cp kd $(DIST)/dmg-staging/
	cp README.md $(DIST)/dmg-staging/
	cp LICENSE $(DIST)/dmg-staging/
	hdiutil create -volname "KeibiDrop $(VERSION)" \
	  -srcfolder $(DIST)/dmg-staging \
	  -ov -format UDZO \
	  $(DIST)/keibidrop-$(VERSION)-darwin-$(GOARCH).dmg
	rm -rf $(DIST)/dmg-staging
	@echo "Created $(DIST)/keibidrop-$(VERSION)-darwin-$(GOARCH).dmg"

# Linux .tar.gz — universal fallback
package-tar: $(DIST)
	@echo "Packaging .tar.gz for $(GOOS)-$(GOARCH)..."
	mkdir -p $(DIST)/tar-staging/keibidrop-$(VERSION)
	cp rust/target/release/keibidrop-rust $(DIST)/tar-staging/keibidrop-$(VERSION)/keibidrop
	cp keibidrop-cli $(DIST)/tar-staging/keibidrop-$(VERSION)/
	cp kd $(DIST)/tar-staging/keibidrop-$(VERSION)/
	cp README.md LICENSE $(DIST)/tar-staging/keibidrop-$(VERSION)/
	cd $(DIST)/tar-staging && tar czf ../keibidrop-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz keibidrop-$(VERSION)/
	rm -rf $(DIST)/tar-staging
	@echo "Created $(DIST)/keibidrop-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz"

# Linux .deb — requires nfpm (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest)
package-deb: $(DIST)
	@echo "Packaging .deb for $(GOARCH)..."
	VERSION=$(VERSION) GOARCH=$(GOARCH) nfpm package -p deb -f nfpm.yaml -t $(DIST)/
	@echo "Created .deb in $(DIST)/"

# Generate SHA256 checksums for all release artifacts
checksums: $(DIST)
	cd $(DIST) && shasum -a 256 keibidrop-* > SHA256SUMS
	@echo "Created $(DIST)/SHA256SUMS"

clean-dist:
	rm -rf $(DIST)

clean: clean-dist
	rm -f keibidrop-cli kd libkeibidrop.a libkeibidrop.h

# ── Run (dev) ─────────────────────────────────────────────
# Alice: ports 26001/26002    Bob: ports 26003/26004
# Relay: http://localhost:54321 (start your own relay first)

RELAY   ?= http://localhost:54321
SCRIPTS := scripts/dev

# Rust UI
run-alice:          ; NO_FUSE=1  KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26001 OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_rust_ui_nofuse.sh
run-alice-fuse:     ; NO_FUSE=   KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26001 OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_rust_ui_nofuse.sh
run-bob:            ; NO_FUSE=1  KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26003 OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_rust_ui.sh
run-bob-fuse:       ; NO_FUSE=   KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26003 OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_rust_ui.sh

# Go CLI
run-cli-alice:      ; NO_FUSE=1  KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26001 OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_cli.sh
run-cli-alice-fuse: ; NO_FUSE=   KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26001 OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_cli.sh
run-cli-bob:        ; NO_FUSE=1  KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26003 OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_peer_cli.sh
run-cli-bob-fuse:   ; NO_FUSE=   KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26003 OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_peer_cli.sh

# kd daemon (agent)
run-kd-alice:       ; KD_NO_FUSE=1 KD_RELAY=$(RELAY) KD_INBOUND_PORT=26001 KD_OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_kd_alice.sh
run-kd-bob:         ; KD_NO_FUSE=1 KD_RELAY=$(RELAY) KD_INBOUND_PORT=26003 KD_OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_kd_bob.sh

# ── Help ──────────────────────────────────────────────────

help:
	@echo "Build:"
	@echo "  make build-cli              Build interactive CLI"
	@echo "  make build-kd               Build agent daemon"
	@echo "  make build-rust             Build Rust/Slint UI (includes protoc + Go static lib)"
	@echo "  make build-all              Build everything"
	@echo ""
	@echo "Run (dev):                    Alice=26001/26002  Bob=26003/26004"
	@echo "  make run-alice              Rust UI, Alice, no-FUSE"
	@echo "  make run-alice-fuse         Rust UI, Alice, FUSE"
	@echo "  make run-bob                Rust UI, Bob, no-FUSE"
	@echo "  make run-bob-fuse           Rust UI, Bob, FUSE"
	@echo "  make run-cli-alice          Go CLI, Alice, no-FUSE"
	@echo "  make run-cli-alice-fuse     Go CLI, Alice, FUSE"
	@echo "  make run-cli-bob            Go CLI, Bob, no-FUSE"
	@echo "  make run-cli-bob-fuse       Go CLI, Bob, FUSE"
	@echo "  make run-kd-alice           kd daemon, Alice"
	@echo "  make run-kd-bob             kd daemon, Bob"
	@echo ""
	@echo "Test & Lint:"
	@echo "  make test                   Run all integration tests"
	@echo "  make lint                   Run golangci-lint"
	@echo "  make sec                    Run gosec security scanner"
	@echo ""
	@echo "Packaging:"
	@echo "  make package-macos          .dmg for macOS"
	@echo "  make package-tar            .tar.gz archive"
	@echo "  make package-deb            .deb package (needs nfpm)"
	@echo "  make checksums              SHA256SUMS for dist/"
	@echo ""
	@echo "Other:"
	@echo "  make protoc                 Regenerate gRPC stubs"
	@echo "  make rust-bindings          Regenerate Rust FFI bindings"
	@echo "  make clean                  Remove build artifacts"

.PHONY: build-cli build-kd build-static-rust-bridge build-rust build-all \
        test lint sec install-proto protoc rust-bindings slint-preview \
        package-macos package-tar package-deb checksums clean-dist clean \
        run-alice run-alice-fuse run-bob run-bob-fuse \
        run-cli-alice run-cli-alice-fuse run-cli-bob run-cli-bob-fuse \
        run-kd-alice run-kd-bob help
