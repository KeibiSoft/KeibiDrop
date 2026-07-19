package transport

import (
	"context"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// Transport establishes gRPC-ready net.Conns over a wire protocol. Two
// implementations: TCP (the fast bulk data plane) and QUIC (migration-capable —
// the always-on control channel and the degraded-link fallback). One interface so
// gRPC, the benchmarks, and eventually KeibiDrop treat both uniformly, and the
// choice of transport is a single value rather than a forked code path.
type Transport interface {
	// Dial connects to addr and returns a net.Conn ready to carry gRPC.
	Dial(ctx context.Context, addr string) (net.Conn, error)
	// Listen returns a net.Listener yielding gRPC-ready conns.
	Listen(addr string) (net.Listener, error)
}

// TCP returns a plain-TCP transport.
func TCP() Transport { return tcpTransport{} }

// QUIC returns a QUIC transport (one bidirectional stream per connection = one
// net.Conn, mirroring KeibiDrop's per-direction socket).
func QUIC() Transport { return quicTransport{} }

type tcpTransport struct{}

func (tcpTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (tcpTransport) Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

type quicTransport struct{}

func (quicTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := quic.DialAddr(ctx, addr, clientTLSConfig(), quicConfig())
	if err != nil {
		return nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	return newQUICConn(conn, stream), nil
}

func (quicTransport) Listen(addr string) (net.Listener, error) {
	ln, err := quic.ListenAddr(addr, serverTLSConfig(), quicConfig())
	if err != nil {
		return nil, err
	}
	return newQUICListener(ln), nil
}

// quicConfig is applied to both ends so throughput isn't window-limited and the
// comparison against TCP is fair. Matched on both sides. MaxIdleTimeout is a
// sensible dead-peer-detection value for a P2P app; KeepAlivePeriod keeps healthy
// connections alive well within it.
func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 20 * time.Second,
		KeepAlivePeriod:                5 * time.Second,
		InitialStreamReceiveWindow:     1 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 1 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
	}
}

// ServeGRPC serves svc over the transport at addr ("127.0.0.1:0" for an ephemeral
// port). If secure, each accepted conn gets the PQC handshake before gRPC. Returns
// the grpc server (call Stop) and the bound address.
func ServeGRPC(tr Transport, addr string, svc pb.BenchServiceServer, secure bool, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	ln, err := tr.Listen(addr)
	if err != nil {
		return nil, nil, err
	}
	if secure {
		ln = secureListener{ln}
	}
	s := grpc.NewServer(opts...)
	pb.RegisterBenchServiceServer(s, svc)
	go func() { _ = s.Serve(ln) }()
	return s, ln.Addr(), nil
}

// DialGRPC dials svc over the transport at addr. If secure, the PQC handshake runs
// (client role) before gRPC sees the conn. The passthrough:/// target keeps
// grpc.NewClient off the dns resolver (it defaults to dns, unlike the old Dial).
func DialGRPC(tr Transport, addr string, secure bool, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		c, err := tr.Dial(ctx, addr)
		if err != nil {
			return nil, err
		}
		if secure {
			sc, err := Secure(ctx, c, RoleClient)
			if err != nil {
				_ = c.Close()
				return nil, err
			}
			return sc, nil
		}
		return c, nil
	}
	all := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	}, opts...)
	return grpc.NewClient("passthrough:///"+addr, all...)
}

// Thin wrappers preserving the signatures the tests and benchmarks use.
func startQUICServer(addr string, svc pb.BenchServiceServer, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	return ServeGRPC(QUIC(), addr, svc, false, opts...)
}

func startTCPServer(addr string, svc pb.BenchServiceServer, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	return ServeGRPC(TCP(), addr, svc, false, opts...)
}

func dialQUIC(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return DialGRPC(QUIC(), addr, false, opts...)
}

func dialTCP(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return DialGRPC(TCP(), addr, false, opts...)
}
