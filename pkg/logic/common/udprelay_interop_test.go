package common

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/transport"
	"github.com/stretchr/testify/require"
)

// Interop probe against the REAL KeibiDrop-DataRelay bridge binary.
func TestRealRelayInterop(t *testing.T) {
	addr := os.Getenv("KD_REAL_RELAY")
	if addr == "" {
		t.Skip("set KD_REAL_RELAY")
	}
	relayAddr, err := net.ResolveUDPAddr("udp", addr)
	require.NoError(t, err)

	server, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	client, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t0 := time.Now()
	join := testkit.Go(func() error { return registerUDPRelay(ctx, server, relayAddr, regDatagram(t, "realprobe")) })
	require.NoError(t, registerUDPRelay(ctx, client, relayAddr, regDatagram(t, "realprobe")))
	require.NoError(t, join())
	t.Logf("REAL RELAY paired both rooms in %v", time.Since(t0))

	ln, err := transport.ListenOnConn(server)
	require.NoError(t, err)
	defer ln.Close()

	const msg = "payload through the real relay"
	// Not testkit.Go. The goroutine's cleanup defer blocks on done, which this
	// function closes only after it reads the result. A return runs that defer
	// first, so the join never receives: deadlock. A buffered send does not.
	accepted := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, aErr := ln.Accept()
		if aErr != nil {
			accepted <- aErr
			return
		}
		defer func() { <-done; _ = c.Close() }()
		got := make([]byte, len(msg))
		if _, rErr := io.ReadFull(c, got); rErr != nil {
			accepted <- rErr
			return
		}
		// require.* calls t.FailNow, which is only legal on the test goroutine.
		// Off it, the test HANGS instead of failing. Report through the channel.
		if string(got) != msg {
			accepted <- fmt.Errorf("payload mismatch: got %q, want %q", got, msg)
			return
		}
		_, wErr := c.Write([]byte("ack"))
		accepted <- wErr
	}()

	t1 := time.Now()
	conn, err := transport.DialOnConn(ctx, client, relayAddr)
	require.NoError(t, err, "QUIC handshake through the REAL relay")
	defer conn.Close()
	t.Logf("REAL RELAY QUIC handshake in %v (remote=%s)", time.Since(t1), conn.RemoteAddr())

	_, err = conn.Write([]byte(msg))
	require.NoError(t, err)
	back := make([]byte, 3)
	_, err = io.ReadFull(conn, back)
	require.NoError(t, err)
	require.Equal(t, "ack", string(back))
	require.NoError(t, <-accepted)
	t.Logf("REAL RELAY round-trip OK, total %v", time.Since(t0))
}
