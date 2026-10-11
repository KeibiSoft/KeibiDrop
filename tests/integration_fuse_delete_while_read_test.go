// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSEtoFUSE_OwnerDeletesWhileFriendReads deletes a file, then its folder,
// while the friend still has the file open. The friend's read stream holds the
// owner's file open on the owner's side. On Windows the serving handle had no
// delete sharing, so the owner's delete failed with "being used by another
// process" (2026-10-11, rm -r of a git repository). POSIX never refused it.
func TestFUSEtoFUSE_OwnerDeletesWhileFriendReads(t *testing.T) {
	p := connectFUSEPeers(t, os.Getenv("KEIBIDROP_LIVE_COLLAB") == "1")
	req := require.New(t)

	req.Equal("OK", p.alice.send(t, "mkdir_p held", 10*time.Second))
	req.Equal("OK", p.alice.send(t, "write_file held/a.txt the owner's bytes", 10*time.Second))
	req.Equal("OK", p.bob.send(t, "wait_file held/a.txt 15", 20*time.Second))

	// Bob opens the file and reads from it: his stream to Alice stays up for
	// as long as he holds it.
	f, err := os.Open(filepath.Join(p.bobMount, "held", "a.txt"))
	req.NoError(err)
	defer func() { _ = f.Close() }()
	buf := make([]byte, 9)
	_, err = f.ReadAt(buf, 0)
	req.NoError(err)
	req.Equal("the owner", string(buf))

	req.Equal("OK", p.alice.send(t, "remove held/a.txt", 10*time.Second), "Alice deletes while Bob reads")
	req.Equal("OK", p.alice.send(t, "remove held", 10*time.Second), "Alice removes the emptied folder")
}
