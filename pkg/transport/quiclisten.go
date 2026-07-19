package transport

import (
	"context"
	"net"

	"github.com/quic-go/quic-go"
)

// quicListener adapts a *quic.Listener to net.Listener so grpc.Server.Serve can
// consume it. Each accepted QUIC connection contributes its first stream as one
// net.Conn (one stream = one conn, mirroring KeibiDrop's per-direction socket).
//
// Streams are accepted in a per-connection goroutine so that a client which
// connects but stalls before opening its stream cannot block Accept for other
// clients — the one place the naive "Accept then AcceptStream inline" version
// would head-of-line block.
type quicListener struct {
	ln     *quic.Listener
	tr     *quic.Transport // owned; Close stops the transport
	udp    net.PacketConn  // the UDP socket; quic-go does NOT close a user-provided Conn, so we do
	conns  chan net.Conn
	errc   chan error
	closed chan struct{}
}

func newQUICListener(ln *quic.Listener, tr *quic.Transport, udp net.PacketConn) net.Listener {
	l := &quicListener{
		ln:     ln,
		tr:     tr,
		udp:    udp,
		conns:  make(chan net.Conn),
		errc:   make(chan error, 1),
		closed: make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *quicListener) acceptLoop() {
	for {
		conn, err := l.ln.Accept(context.Background())
		if err != nil {
			select {
			case l.errc <- err:
			case <-l.closed:
			}
			return
		}
		go func(conn *quic.Conn) {
			stream, err := conn.AcceptStream(context.Background())
			if err != nil {
				_ = conn.CloseWithError(0, "")
				return
			}
			select {
			case l.conns <- newQUICConn(conn, stream):
			case <-l.closed:
				_ = conn.CloseWithError(0, "")
			}
		}(conn)
	}
}

func (l *quicListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errc:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *quicListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	err := l.ln.Close()
	if l.tr != nil {
		_ = l.tr.Close() // stop the transport
	}
	if l.udp != nil {
		_ = l.udp.Close() // release the UDP socket so the port is immediately reusable
	}
	return err
}

func (l *quicListener) Addr() net.Addr { return l.ln.Addr() }
