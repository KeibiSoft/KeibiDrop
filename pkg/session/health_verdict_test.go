// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package session

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// deadHeartbeatClient fails every heartbeat, like a peer whose box was killed.
type deadHeartbeatClient struct {
	bindings.KeibiServiceClient
}

func (deadHeartbeatClient) Heartbeat(_ context.Context, _ *bindings.HeartbeatRequest, _ ...grpc.CallOption) (*bindings.HeartbeatResponse, error) {
	return nil, errors.New("connection refused")
}

// A hard drop is declared within about 12 s and a timing-out link within about
// 20 s. The old defaults (five misses five seconds apart) took 25 s, during
// which nothing on any surface moved (BUGS 9).
func TestHealthMonitor_DefaultsDeclareLossFast(t *testing.T) {
	m := NewHealthMonitor(nil, deadHeartbeatClient{}, slog.Default())
	require.LessOrEqual(t, m.Interval*time.Duration(m.MaxFailures), 12*time.Second)
	require.LessOrEqual(t, m.Timeout*time.Duration(m.MaxFailures), 20*time.Second)
	require.GreaterOrEqual(t, m.MaxFailures, 3, "one or two misses on a lossy link must not drop the session")
}

// The verdict fires on the transition to disconnected and then again every
// MaxFailures misses while the link stays down. OnDisconnect may have deferred
// its decision behind a transfer in flight; without the re-fire a transfer stuck
// on a dead link kept the session down for good.
func TestHealthMonitor_RefiresVerdictWhileDown(t *testing.T) {
	m := NewHealthMonitor(nil, deadHeartbeatClient{}, slog.Default())
	m.MaxFailures = 2
	verdicts, changes := 0, 0
	m.OnDisconnect = func() { verdicts++ }
	m.OnHealthChange = func(_, _ ConnectionHealth) { changes++ }

	err := errors.New("connection refused")
	for i := 0; i < 6; i++ {
		m.handleFailure(err)
	}
	require.Equal(t, 3, verdicts, "misses 2, 4 and 6 each carry a verdict")
	require.Equal(t, 1, changes, "the health transition is reported once")
	require.Equal(t, HealthDisconnected, m.Health())
}
