// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.
// A contact saved a moment ago shows online on the other side at once: the
// save posts presence instead of leaving it to the next heartbeat tick, which
// left a fresh contact absent for up to 30 s (contact runner, 2026-09-30).

package common

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

func TestSaveCurrentPeerAsContact_PostsPresenceAtOnce(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/presence" {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	relay, err := url.Parse(srv.URL)
	require.NoError(t, err)
	dir := t.TempDir()
	key, err := identity.GenerateMasterKey()
	require.NoError(t, err)
	src, err := identity.NewMasterKeySource(identity.KeySourceOpts{ConfigDir: dir, ExternalMaster: key})
	require.NoError(t, err)
	ab, err := identity.LoadAddressBook(dir, src)
	require.NoError(t, err)

	kd := newBareKD()
	kd.Identity = &identity.DeviceIdentity{Fingerprint: "FP-SELF-0001"}
	kd.AddressBook = ab
	kd.RelayEndoint = relay
	kd.relayClient = srv.Client()
	kd.session = &session.Session{ExpectedPeerFingerprint: "FP-PEER-0002", PeerIsPersistent: true}

	require.NoError(t, kd.SaveCurrentPeerAsContact("box"))
	require.Eventually(t, func() bool { return posts.Load() >= 1 },
		5*time.Second, 20*time.Millisecond, "the save posts presence for the new contact")
}
