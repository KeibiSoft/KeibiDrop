package transport

import (
	"net"

	"github.com/quic-go/quic-go"
)

// quicConn adapts one QUIC bidirectional stream to net.Conn, so gRPC — which
// speaks net.Conn — runs over QUIC unmodified. This is the whole integration
// surface, and it is *thinner* than KeibiDrop's SecureConn: a quic.Stream is
// already a reliable, ordered byte stream, so there is no message framing and no
// leftover-buffering to do (SecureConn needs both because it is AEAD-message
// framed). Read/Write/SetDeadline/SetReadDeadline/SetWriteDeadline are promoted
// straight from the embedded *quic.Stream; only the two address methods (which
// live on *quic.Conn, not the stream) and Close are supplied here.
//
// Model: one QUIC connection carries exactly one stream = one net.Conn. Closing
// the net.Conn therefore tears the whole QUIC connection down.
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

// Close forcefully terminates the connection. This is the sharpest edge of the
// adapter, and getting it wrong deadlocks teardown:
//
//   - net.Conn.Close must end the connection entirely, but quic.Stream.Close() is
//     only a graceful send-side FIN.
//   - Worse, quic-go documents that Stream.Close() "must not be called concurrently
//     with Write" — yet gRPC's loopyWriter may be mid-Write on this stream during
//     teardown (and can be *blocked* there on flow control if a client cancelled a
//     server-stream and stopped reading). Calling Stream.Close() then races Write
//     and never unblocks it, so gRPC's http2Server.Close never finishes closing the
//     conn, and Server.Stop hangs forever.
//
// The correct abort is CancelWrite (RESET_STREAM): it is safe to call concurrently
// with Write and it unblocks a flow-control-blocked Write. Paired with CancelRead
// (STOP_SENDING) and CloseWithError on the one-stream-per-connection, this tears the
// whole thing down without deadlocking. See docs/FINDINGS.md for the full story.
func (c *quicConn) Close() error {
	dbg("quicConn.Close START local=%v remote=%v", c.conn.LocalAddr(), c.conn.RemoteAddr())
	c.Stream.CancelRead(0)  // STOP_SENDING: unblock/kill the read half
	c.Stream.CancelWrite(0) // RESET_STREAM: abort the write half (safe vs concurrent Write)
	err := c.conn.CloseWithError(0, "")
	dbg("quicConn.Close DONE remote=%v err=%v", c.conn.RemoteAddr(), err)
	return err
}
