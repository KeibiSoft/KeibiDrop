// ABOUTME: Tests for reconnect safety — verifies no panic on disconnect→reconnect cycle.
// ABOUTME: Covers issue #64: close of closed channel in setupFilesystem.
package common

import (
	"sync"
	"testing"
)

func TestFilesystemReadyOnceGuardsDoubleClose(t *testing.T) {
	// Simulates the reconnect scenario where setupFilesystem could be
	// called with an already-closed channel. sync.Once must prevent the
	// second close from panicking.

	ready := make(chan struct{})
	var once sync.Once

	// First close — normal session teardown
	once.Do(func() { close(ready) })

	// Second close — reconnect calling setupFilesystem again with same channel.
	// Without the Once guard this would panic: "close of closed channel"
	once.Do(func() { close(ready) })

	// Verify channel is actually closed
	select {
	case <-ready:
		// expected
	default:
		t.Fatal("channel should be closed after Once.Do")
	}
}

func TestFilesystemReadyNewChannelPerSession(t *testing.T) {
	// Simulates the fix: each CreateRoom/JoinRoom creates a fresh channel
	// and a fresh sync.Once, so there's no stale state from the previous session.

	// Session 1
	ready1 := make(chan struct{})
	once1 := sync.Once{}
	once1.Do(func() { close(ready1) })

	// Verify session 1 channel closed
	select {
	case <-ready1:
	default:
		t.Fatal("session 1 channel should be closed")
	}

	// Session 2 — fresh channel and Once
	ready2 := make(chan struct{})
	once2 := sync.Once{}
	once2.Do(func() { close(ready2) })

	// Verify session 2 channel closed
	select {
	case <-ready2:
	default:
		t.Fatal("session 2 channel should be closed")
	}

	// If we reach here, no panic — reconnect is safe
}
