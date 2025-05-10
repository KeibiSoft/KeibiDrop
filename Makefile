lint:
	golangci-lint run ./...

sec:
	gosec ./...

build:
	go build cmd/keibidrop.go
