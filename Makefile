COMMIT := $(shell git rev-parse HEAD)
VERSION := 0.0.1

lint:
	golangci-lint run ./...

sec:
	gosec -exclude-generated ./... 

build-gui:
	go build -ldflags="-X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=$(VERSION) -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=$(COMMIT)" cmd/keibidrop.go

build-cli:
	go build -ldflags="-X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=$(VERSION) -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=$(COMMIT)" cmd/cli/keibidrop-cli.go

install-proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

protoc:
	protoc --go_opt=module=github.com/KeibiSoft/KeibiDrop --go-grpc_opt=module=github.com/KeibiSoft/KeibiDrop --go_out=. --go-grpc_out=. keibidrop.proto

test:
	go test -timeout 300s -v ./...

build-static-rust-bridge:
	go build -buildmode=c-archive -o libkeibidrop.a ./rustbridge

# Regenerate Rust bindings from the C header (requires bindgen)
# Install: cargo install bindgen-cli
rust-bindings: build-static-rust-bridge
	bindgen libkeibidrop.h -o rust/src/bindings.rs

# Preview Slint UI (requires slint-viewer)
# Install: cargo install slint-viewer
slint-preview:
	slint-viewer rust/src/ui.slint

# ---------------------------------------------------------------------------
# E2E manual testing helpers
# Usage:
#   make e2e-clean   — kill all instances, unmount FUSE, clear Save dirs
#   make alice       — launch Alice (UI) on ports 26001/26002
#   make bob         — launch Bob   (UI) on ports 26003/26004
#
# Relay must be running at http://127.0.0.1:54321 before starting peers.
# Run each target in its own terminal tab.
# ---------------------------------------------------------------------------

E2E_RELAY   := http://127.0.0.1:54321
E2E_BIN     := ./rust/target/release/keibidrop-rust
E2E_ROOT    := $(CURDIR)

e2e-clean:
	-pkill -9 -f keibidrop-rust
	-fusermount3 -u $(E2E_ROOT)/MountAlice
	-fusermount3 -u $(E2E_ROOT)/MountBob
	mkdir -p $(E2E_ROOT)/MountAlice $(E2E_ROOT)/MountBob \
	          $(E2E_ROOT)/SaveAlice  $(E2E_ROOT)/SaveBob
	rm -rf $(E2E_ROOT)/SaveAlice/* $(E2E_ROOT)/SaveBob/*
	@echo "E2E environment clean."

alice:
	mkdir -p $(E2E_ROOT)/MountAlice $(E2E_ROOT)/SaveAlice
	LOG_FILE="Log_Alice.txt" \
	TO_SAVE_PATH="$(E2E_ROOT)/SaveAlice" \
	TO_MOUNT_PATH="$(E2E_ROOT)/MountAlice" \
	KEIBIDROP_RELAY="$(E2E_RELAY)" \
	INBOUND_PORT=26001 OUTBOUND_PORT=26002 \
	$(E2E_BIN)

bob:
	mkdir -p $(E2E_ROOT)/MountBob $(E2E_ROOT)/SaveBob
	LOG_FILE="Log_Bob.txt" \
	TO_SAVE_PATH="$(E2E_ROOT)/SaveBob" \
	TO_MOUNT_PATH="$(E2E_ROOT)/MountBob" \
	KEIBIDROP_RELAY="$(E2E_RELAY)" \
	INBOUND_PORT=26003 OUTBOUND_PORT=26004 \
	$(E2E_BIN)
