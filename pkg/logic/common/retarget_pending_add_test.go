// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/stretchr/testify/require"
)

// A temp ADD retargeted by a RENAME becomes a redelivery of the renamed
// version: the rename's path, base and Attr. One stamp per version.
func TestRetargetPendingAdd_TakesRenameStampAndBase(t *testing.T) {
	add := &bindings.NotifyRequest{
		Type:        bindings.NotifyType_ADD_FILE,
		Path:        "/doc.txt.tmp",
		BaseMtimeNs: -1,
		Attr:        &bindings.Attr{Size: 5, ModificationTime: 1_000_000_900, Mode: 0o100644},
	}
	ren := &bindings.NotifyRequest{
		Type:        bindings.NotifyType_RENAME_FILE,
		OldPath:     "/doc.txt.tmp",
		Path:        "/doc.txt",
		BaseMtimeNs: 42,
		Attr:        &bindings.Attr{Size: 7, ModificationTime: 1_000_000_000, Mode: 0o100644, BirthTime: 3},
	}
	retargetPendingAdd(add, ren)
	require.Equal(t, "/doc.txt", add.Path)
	require.Equal(t, int64(42), add.BaseMtimeNs)
	require.Equal(t, uint64(1_000_000_000), add.Attr.ModificationTime, "the ADD must carry the RENAME's stamp")
	require.Equal(t, int64(7), add.Attr.Size)
	require.Equal(t, uint64(3), add.Attr.BirthTime)
	require.NotSame(t, ren.Attr, add.Attr, "the Attr must be a copy: the flush refresh writes into the ADD's")
}

// A RENAME without an Attr (the stat after the disk rename failed) leaves the
// ADD's own Attr in place: the content still reaches the peer.
func TestRetargetPendingAdd_KeepsOwnAttrWithoutRenameAttr(t *testing.T) {
	add := &bindings.NotifyRequest{Path: "/a.tmp", Attr: &bindings.Attr{Size: 5, ModificationTime: 9}}
	ren := &bindings.NotifyRequest{OldPath: "/a.tmp", Path: "/a", BaseMtimeNs: 1}
	retargetPendingAdd(add, ren)
	require.Equal(t, "/a", add.Path)
	require.Equal(t, int64(1), add.BaseMtimeNs)
	require.Equal(t, uint64(9), add.Attr.ModificationTime)
}
