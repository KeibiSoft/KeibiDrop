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
	go test -timeout 30s -v ./...

build-static-rust-bridge:
	go build -buildmode=c-archive -o libkeibidrop.a ./rustbridge

# Regenerate Rust bindings from the C header (requires bindgen)
# Install: cargo install bindgen-cli
rust-bindings: protoc build-static-rust-bridge
	bindgen libkeibidrop.h -o rust/src/bindings.rs

# One-step build: Go static lib → bindgen → Rust release binary
# bindgen runs automatically in rust/build.rs (platform-correct, no manual step)
build-rust: protoc build-static-rust-bridge
	cd rust && cargo build --release

# Preview Slint UI (requires slint-viewer)
# Install: cargo install slint-viewer
slint-preview:
	slint-viewer rust/src/ui.slint
