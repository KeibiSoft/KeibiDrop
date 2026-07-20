package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// MigratableConn is a QUIC-backed net.Conn dialed on an explicit quic.Transport, so
// the underlying connection can MIGRATE to a fresh local UDP socket (the Wi-Fi -> LTE
// case) while the stream — and everything layered on it (SecureConn, gRPC) — survives
// untouched. The plain Transport.Dial uses quic.DialAddr, whose connection cannot
// AddPath; the app-level control channel dials through this instead.
type MigratableConn struct {
	net.Conn // the one-stream adapter (quicConn)

	qconn      *quic.Conn
	serverAddr *net.UDPAddr

	mu         sync.Mutex
	transports []*quic.Transport // kept alive: quic-go ties the conn's lifetime to them
}

// DialQUICMigratable dials addr over QUIC on an explicit transport and returns the
// stream as a migratable net.Conn.
func DialQUICMigratable(ctx context.Context, addr string) (*MigratableConn, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", addr, err)
	}
	tr, err := newUDPTransport(serverAddr)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	qc, err := tr.Dial(ctx, serverAddr, clientTLSConfig(), quicConfig())
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		_ = qc.CloseWithError(0, "")
		_ = tr.Close()
		return nil, err
	}
	return &MigratableConn{
		Conn:       newQUICConn(qc, st),
		qconn:      qc,
		serverAddr: serverAddr,
		transports: []*quic.Transport{tr},
	}, nil
}

// Migrate moves the connection to a fresh local UDP socket and returns the old and
// new local addresses. The stream keeps working across the switch. The caller must
// send traffic right after (e.g. a ping) so the peer migrates its send path too.
func (m *MigratableConn) Migrate(ctx context.Context) (oldAddr, newAddr net.Addr, err error) {
	oldAddr = m.qconn.LocalAddr()
	tr, err := migratePath(ctx, m.qconn, m.serverAddr)
	if err != nil {
		return oldAddr, nil, err
	}
	m.mu.Lock()
	m.transports = append(m.transports, tr)
	m.mu.Unlock()
	// Report the new socket's own address: qconn.LocalAddr() only reflects the new
	// path once traffic has flowed on it (hence "send right after").
	return oldAddr, tr.Conn.LocalAddr(), nil
}

// Close tears down the stream, the connection, and every transport it ever used.
func (m *MigratableConn) Close() error {
	err := m.Conn.Close()
	m.mu.Lock()
	trs := m.transports
	m.transports = nil
	m.mu.Unlock()
	for _, tr := range trs {
		_ = tr.Close()
	}
	return err
}

// migratePath is the one migration mechanism (shared by Manager.Migrate and
// MigratableConn.Migrate): bind a fresh local socket toward serverAddr, AddPath,
// Probe, let the PATH_CHALLENGE/RESPONSE ACKs settle, Switch. Returns the new
// transport, which the caller must keep alive for the connection's lifetime.
func migratePath(ctx context.Context, qconn *quic.Conn, serverAddr *net.UDPAddr) (*quic.Transport, error) {
	newTr, err := newUDPTransport(serverAddr)
	if err != nil {
		return nil, fmt.Errorf("migration socket: %w", err)
	}
	path, err := qconn.AddPath(newTr)
	if err != nil {
		_ = newTr.Close()
		return nil, fmt.Errorf("AddPath: %w", err)
	}
	if err := path.Probe(ctx); err != nil {
		_ = newTr.Close()
		return nil, fmt.Errorf("Probe: %w", err)
	}
	time.Sleep(30 * time.Millisecond) // let PATH_CHALLENGE/RESPONSE ACKs settle
	if err := path.Switch(); err != nil {
		_ = newTr.Close()
		return nil, fmt.Errorf("Switch: %w", err)
	}
	return newTr, nil
}
