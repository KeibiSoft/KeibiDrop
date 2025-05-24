COMMIT := $(shell git rev-parse HEAD)
VERSION := 0.0.1

lint:
	golangci-lint run ./...

sec:
	gosec ./...

build:
	go build -ldflags="-X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=$(VERSION) -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=$(COMMIT)" cmd/keibidrop.go

install-proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

protoc:
	protoc --go_opt=module=github.com/KeibiSoft/KeibiDrop --go-grpc_opt=module=github.com/KeibiSoft/KeibiDrop --go_out=. --go-grpc_out=. keibidrop.proto

