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
