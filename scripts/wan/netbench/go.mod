module github.com/KeibiSoft/KeibiDrop/scripts/wan/netbench

go 1.25.14

require (
	github.com/KeibiSoft/KeibiDrop v0.0.0
	golang.org/x/net v0.56.0
	golang.org/x/sys v0.47.0
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/quic-go/quic-go v0.60.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/grpc v1.82.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/KeibiSoft/KeibiDrop => ../../..

replace github.com/quic-go/quic-go => github.com/KeibiSoft/quic-go v0.0.0-20260719194007-24ecf34ccaf9
