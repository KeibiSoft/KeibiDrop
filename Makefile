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
	gosec -exclude-generated ./...

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
package-macos: build-all $(DIST)
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
package-tar: build-all $(DIST)
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
package-deb: build-all $(DIST)
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

.PHONY: build-cli build-kd build-static-rust-bridge build-rust build-all \
        test lint sec install-proto protoc rust-bindings slint-preview \
        package-macos package-tar package-deb checksums clean-dist clean
