package transport

import (
	"net"

	"github.com/quic-go/quic-go"
)

// quicConn adapts one QUIC bidi stream to net.Conn so gRPC runs over QUIC
// unmodified. One QUIC connection carries one stream; closing the net.Conn
// tears down the whole connection.
type quicConn struct {
	*quic.Stream
	conn *quic.Conn
}

// newQUICConn wraps a stream and its owning connection as a net.Conn.
func newQUICConn(conn *quic.Conn, stream *quic.Stream) net.Conn {
	return &quicConn{Stream: stream, conn: conn}
}

func (c *quicConn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *quicConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// Close tears down the QUIC connection. It uses CancelWrite/CancelRead rather than
// Stream.Close: Stream.Close is a graceful FIN that quic-go forbids concurrently
// with Write, and gRPC's loopyWriter can be mid-Write (blocked on flow control)
// during teardown, which would deadlock Server.Stop. CancelWrite (RESET_STREAM) is
// safe against a concurrent Write and unblocks it.
func (c *quicConn) Close() error {
	c.CancelRead(0)  // STOP_SENDING
	c.CancelWrite(0) // RESET_STREAM
	return c.conn.CloseWithError(0, "")
}
