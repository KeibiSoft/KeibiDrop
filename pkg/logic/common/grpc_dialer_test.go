// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// The session socket goes to grpc-go once. A second call is grpc-go redialing
// after the transport died, and it gets told so rather than the dead socket.
func TestOneShotDialer_HandsTheSocketOverOnce(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	dial := oneShotDialer(a)

	got, err := dial(context.Background(), "keibipipe")
	require.NoError(t, err)
	require.Same(t, a, got)

	got, err = dial(context.Background(), "keibipipe")
	require.ErrorIs(t, err, errSessionSocketConsumed)
	require.Nil(t, got)
}
