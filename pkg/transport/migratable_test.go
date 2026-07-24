package transport

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestDialQUICMigratable checks that after Migrate the local address changes and the
// same stream keeps carrying bytes, with no re-dial.
func TestDialQUICMigratable(t *testing.T) {
	ln, err := QUIC().Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mc, err := DialQUICMigratable(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer mc.Close()

	echo := func(msg string) {
		t.Helper()
		if _, err := mc.Write([]byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(mc, buf); err != nil {
			t.Fatalf("read %q: %v", msg, err)
		}
		if string(buf) != msg {
			t.Fatalf("echo mismatch: got %q want %q", buf, msg)
		}
	}

	echo("before-migration")

	oldAddr, newAddr, err := mc.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if oldAddr.String() == newAddr.String() {
		t.Fatalf("migration did not change the local address: %v", newAddr)
	}
	t.Logf("migrated %v -> %v", oldAddr, newAddr)

	echo("after-migration") // same conn, same stream, new path
}
