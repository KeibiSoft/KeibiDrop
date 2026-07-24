package transport

import (
	"context"
	"net"

	"google.golang.org/grpc"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// secureListener runs the PQC handshake (server role) on each accepted conn before
// handing it up, so gRPC runs over a post-quantum-secured net.Conn. A failed handshake
// drops that conn and keeps serving.
type secureListener struct {
	net.Listener
}

func (l secureListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		sc, err := Secure(context.Background(), c, RoleServer)
		if err != nil {
			_ = c.Close()
			continue
		}
		return sc, nil
	}
}

// Thin wrappers: gRPC over the PQC handshake + SecureConn over QUIC.
func startQUICServerSecure(addr string, svc pb.BenchServiceServer, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	return ServeGRPC(QUIC(), addr, svc, true, opts...)
}

func dialQUICSecure(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return DialGRPC(QUIC(), addr, true, opts...)
}
