package tests

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// The invite link is the code inside a URL fragment. Every surface reaches the
// engine through AddPeerFingerprint, so the link form must register there.
func TestAddPeerFingerprint_AcceptsInviteLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, err := url.Parse("http://127.0.0.1:1")
	require.NoError(t, err)

	dir := t.TempDir()
	kd, err := common.NewKeibiDropWithIP(ctx, logger, false, relayURL,
		31871, 31872, dir, dir, false, true, "::1")
	require.NoError(t, err)

	peer, err := kd.ExportFingerprint()
	require.NoError(t, err)

	require.NoError(t, kd.AddPeerFingerprint("https://keibidrop.com/join#"+peer))
	got, err := kd.GetPeerFingerprint()
	require.NoError(t, err)
	require.Equal(t, peer, got, "invite link did not register the code it carries")

	require.NoError(t, kd.AddPeerFingerprint("  "+peer+"\n"))
	got, err = kd.GetPeerFingerprint()
	require.NoError(t, err)
	require.Equal(t, peer, got, "a pasted code with whitespace did not register")

	require.Error(t, kd.AddPeerFingerprint("https://keibidrop.com/join#nope"),
		"a link with a bad code was accepted")
}
