module github.com/KeibiSoft/KeibiDrop/scripts/wan/netbench

go 1.26.9

require (
	github.com/KeibiSoft/KeibiDrop v0.0.0
	golang.org/x/net v0.60.0
	golang.org/x/sys v0.48.0
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/quic-go/quic-go v0.60.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/KeibiSoft/KeibiDrop => ../../..

replace github.com/quic-go/quic-go => github.com/KeibiSoft/quic-go v0.0.0-20260719194007-24ecf34ccaf9
