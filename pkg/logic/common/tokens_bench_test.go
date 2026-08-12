package common

import (
	"net"
	"testing"
	"time"
)

// The only token cost on the data path is payConn's byte counting (one
// atomic add per Read/Write plus a mutex check on Read). These benches
// measure that against the same loop on a bare conn.

type nopConn struct{}

func (nopConn) Read(b []byte) (int, error)         { return len(b), nil }
func (nopConn) Write(b []byte) (int, error)        { return len(b), nil }
func (nopConn) Close() error                       { return nil }
func (nopConn) LocalAddr() net.Addr                { return nil }
func (nopConn) RemoteAddr() net.Addr               { return nil }
func (nopConn) SetDeadline(t time.Time) error      { return nil }
func (nopConn) SetReadDeadline(t time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(t time.Time) error { return nil }

func BenchmarkPayConnRead64K(b *testing.B) {
	ts := &tokenSession{}
	p := newPayConn(nopConn{}, ts)
	p.ackPending = false
	buf := make([]byte, 64*1024)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBareConnRead64K(b *testing.B) {
	var c net.Conn = nopConn{}
	buf := make([]byte, 64*1024)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}
