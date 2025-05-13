COMMIT := $(shell git rev-parse HEAD)
VERSION := 0.0.1

lint:
	golangci-lint run ./...

sec:
	gosec ./...

build:
	go build -ldflags="-X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=$(VERSION) -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=$(COMMIT)" cmd/keibidrop.go
