// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Tests that wireReconnectEvents pushes reconnecting/reconnected/gave_up events.
// ABOUTME: Verifies mobile apps receive the same events as desktop (rustbridge).
package common

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

func collectEvents(kd *KeibiDrop) *[]string {
	var mu sync.Mutex
	events := make([]string, 0, 8)
	kd.OnEvent = func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	return &events
}

func newEventTestKD() *KeibiDrop {
	kd := &KeibiDrop{
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	kd.ReconnectManager = session.NewReconnectManager(nil, kd.logger)
	return kd
}

func TestWireReconnectEvents_EventFired(t *testing.T) {
	tests := []struct {
		name    string
		trigger func(rm *session.ReconnectManager)
		want    string
	}{
		{"reconnecting", func(rm *session.ReconnectManager) { rm.OnReconnecting() }, "reconnecting:"},
		{"reconnected", func(rm *session.ReconnectManager) { rm.OnReconnected() }, "reconnected:"},
		{"gave_up", func(rm *session.ReconnectManager) { rm.OnGaveUp() }, "gave_up:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kd := newEventTestKD()
			events := collectEvents(kd)

			kd.wireReconnectEvents()
			tc.trigger(kd.ReconnectManager)

			require.Equal(t, []string{tc.want}, *events)
		})
	}
}

func TestWireReconnectEvents_OriginalCallbackPreserved(t *testing.T) {
	kd := newEventTestKD()
	events := collectEvents(kd)

	originalCalled := false
	kd.ReconnectManager.OnReconnected = func() { originalCalled = true }

	kd.wireReconnectEvents()
	kd.ReconnectManager.OnReconnected()

	require.True(t, originalCalled, "original OnReconnected callback was not called")
	require.Equal(t, []string{"reconnected:"}, *events)
}

func TestWireReconnectEvents_NilReconnectManagerIsNoop(t *testing.T) {
	kd := &KeibiDrop{
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	kd.wireReconnectEvents()
}

// A give-up with auto-connect armed must end the session, so the watchdog
// redials, and skip the rearm grace. Without it the box sat "running" with no
// link for hours after the laptop slept through the retry budget.
func TestGaveUp_EndsSessionWhenAutoConnectArmed(t *testing.T) {
	kd := newEventTestKD()
	ctx, cancel := context.WithCancel(context.Background())
	kd.Cancel = cancel
	kd.autoConnectArmed.Store(true)
	kd.wireReconnectEvents()

	kd.ReconnectManager.OnGaveUp()

	require.True(t, kd.peerSaidGoodbye.Load(), "the grace must be skipped after a give-up")
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the session context must be cancelled after a give-up")
	}
}

// Without auto-connect a give-up changes nothing: the user decides.
func TestGaveUp_LeavesSessionWithoutAutoConnect(t *testing.T) {
	kd := newEventTestKD()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kd.Cancel = cancel
	kd.wireReconnectEvents()

	kd.ReconnectManager.OnGaveUp()

	require.False(t, kd.peerSaidGoodbye.Load())
	select {
	case <-ctx.Done():
		t.Fatal("a give-up without auto-connect must not end the session")
	case <-time.After(300 * time.Millisecond):
	}
}
