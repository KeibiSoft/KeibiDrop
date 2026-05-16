// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Tests that wireReconnectEvents pushes reconnecting/reconnected/gave_up events.
// ABOUTME: Verifies mobile apps receive the same events as desktop (rustbridge).
package common

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
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

			if len(*events) != 1 || (*events)[0] != tc.want {
				t.Fatalf("expected [%s], got %v", tc.want, *events)
			}
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

	if !originalCalled {
		t.Fatal("original OnReconnected callback was not called")
	}
	if len(*events) != 1 || (*events)[0] != "reconnected:" {
		t.Fatalf("expected [reconnected:], got %v", *events)
	}
}

func TestWireReconnectEvents_NilReconnectManagerIsNoop(t *testing.T) {
	kd := &KeibiDrop{
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	kd.wireReconnectEvents()
}
