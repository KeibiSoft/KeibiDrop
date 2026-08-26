// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests cancelRemoveOnLocalContent: local add/edit/rename events cancel a
// ABOUTME: buffered peer REMOVE, other actions don't, and a nil KDSvc is safe.

package common

import (
	"context"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
)

func armPendingRemove(t *testing.T, filePath string) *service.KeibidropServiceImpl {
	t.Helper()
	st := synctracker.NewSyncTracker()
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[filePath] = &synctracker.File{Name: filePath, RelativePath: filePath, Size: 4}
	st.RemoteFilesMu.Unlock()

	svc := &service.KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: st,
	}
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)
	return svc
}

func trackedIn(svc *service.KeibidropServiceImpl, filePath string) bool {
	svc.SyncTracker.RemoteFilesMu.RLock()
	defer svc.SyncTracker.RemoteFilesMu.RUnlock()
	_, ok := svc.SyncTracker.RemoteFiles[filePath]
	return ok
}

// TestCancelRemoveOnLocalContent_EditCancels: a local EDIT event inside the
// window cancels the buffered peer REMOVE, so the entry survives.
func TestCancelRemoveOnLocalContent_EditCancels(t *testing.T) {
	const filePath = "e.txt"
	svc := armPendingRemove(t, filePath)
	kd := &KeibiDrop{KDSvc: svc}

	kd.cancelRemoveOnLocalContent(types.EditFile, filePath)

	time.Sleep(1300 * time.Millisecond)
	require.True(t, trackedIn(svc, filePath), "local edit must cancel the buffered remove")
}

// TestCancelRemoveOnLocalContent_NonContentActionsDoNot: a local REMOVE event
// must not cancel the buffered peer REMOVE; it still executes.
func TestCancelRemoveOnLocalContent_NonContentActionsDoNot(t *testing.T) {
	const filePath = "r.txt"
	svc := armPendingRemove(t, filePath)
	kd := &KeibiDrop{KDSvc: svc}

	kd.cancelRemoveOnLocalContent(types.RemoveFile, filePath)
	kd.cancelRemoveOnLocalContent(types.CancelPendingNotify, filePath)

	testkit.Eventually(t, 5*time.Second, 25*time.Millisecond, func() bool {
		return !trackedIn(svc, filePath)
	}, "non-content local actions must not cancel the remove")
}

// TestCancelRemoveOnLocalContent_NilSvcSafe: no service wired yet must not panic.
func TestCancelRemoveOnLocalContent_NilSvcSafe(t *testing.T) {
	kd := &KeibiDrop{}
	require.NotPanics(t, func() {
		kd.cancelRemoveOnLocalContent(types.AddFile, "x.txt")
	})
}
