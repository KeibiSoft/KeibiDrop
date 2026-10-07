// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

// A peer without a mount accepts a folder notification instead of failing the batch.
func TestNotify_AddDirWithoutFilesystemIsAccepted(t *testing.T) {
	svc := newPathSafetyTestService()
	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_DIR,
		Path: "/plain",
		Name: "plain",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
}
