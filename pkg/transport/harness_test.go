package transport

import (
	"net"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// transport is one of the two wirings under test: QUIC or plain TCP. The start
// and dial signatures are identical for both, so every test and benchmark runs
// over both by iterating this table.
type transport struct {
	name  string
	start func(string, pb.BenchServiceServer, ...grpc.ServerOption) (*grpc.Server, net.Addr, error)
	dial  func(string, ...grpc.DialOption) (*grpc.ClientConn, error)
}

var transports = []transport{
	{"quic", startQUICServer, dialQUIC},
	{"tcp", startTCPServer, dialTCP},
}

// newClient starts a server (default service) and connects a client over this
// transport, returning the stub and a cleanup func.
func (tr transport) newClient(t testing.TB) (pb.BenchServiceClient, func()) {
	return tr.newClientSvc(t, benchService{})
}

// newClientSvc is newClient with a caller-supplied service, so tests can install
// hooks (e.g. observe when a handler returns).
func (tr transport) newClientSvc(t testing.TB, svc pb.BenchServiceServer) (pb.BenchServiceClient, func()) {
	t.Helper()
	srv, addr, err := tr.start("127.0.0.1:0", svc)
	if err != nil {
		t.Fatalf("[%s] start server: %v", tr.name, err)
	}
	cc, err := tr.dial(addr.String())
	if err != nil {
		srv.Stop()
		t.Fatalf("[%s] dial: %v", tr.name, err)
	}
	return pb.NewBenchServiceClient(cc), func() {
		dbg("cleanup: cc.Close START")
		_ = cc.Close()
		dbg("cleanup: cc.Close DONE; srv.Stop START")
		srv.Stop()
		dbg("cleanup: srv.Stop DONE")
	}
}
