// ABOUTME: Tests for kd CLI helpers and dispatch argument validation.
// ABOUTME: Covers isShowAll and dispatch error paths for missing args.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
)

// isLoopbackAddr must refuse any off-box bind for the debug pprof endpoint, so KD_PPROF can never
// expose profiles beyond the local host.
func TestIsLoopbackAddr(t *testing.T) {
	type loopbackCase struct {
		addr string
		want bool
	}
	cases := []loopbackCase{
		{"127.0.0.1:6060", true},
		{"[::1]:6060", true},
		{"localhost:6060", true},
		{"0.0.0.0:6060", false}, // binds all interfaces = off-box
		{"192.168.1.10:6060", false},
		{"example.com:6060", false}, // non-literal host we cannot verify = refuse
		{"6060", false},             // not host:port
		{"", false},
	}
	// Name the empty case. An empty subtest name auto-numbers to #00.
	name := func(c loopbackCase) string {
		if c.addr == "" {
			return "empty"
		}
		return c.addr
	}
	testkit.RunTable(t, cases, name, func(_ *testing.T, c loopbackCase) error {
		return fp.Equal("isLoopbackAddr", isLoopbackAddr(c.addr), c.want)
	})
}

var testPortBase int32 = 26900

func nextTestPort() int {
	return int(atomic.AddInt32(&testPortBase, 2))
}

func newTestKD(t *testing.T) *common.KeibiDrop {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	relay, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := nextTestPort()
	kd, err := common.NewKeibiDropWithIP(
		ctx, logger, false, relay,
		port, port+1, "", t.TempDir(),
		false, false, "::1",
	)
	require.NoError(t, err, "NewKeibiDropWithIP")
	return kd
}

func dispatchTest(kd *common.KeibiDrop, cmd string, args ...string) Response {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
	return dispatch(kd, Request{Command: cmd, Args: args}, cancel, ln)
}

func TestIsShowAll(t *testing.T) {
	type showAllCase struct {
		name string
		args []string
		want bool
	}
	cases := []showAllCase{
		{"no args", nil, true},
		{"empty slice", []string{}, true},
		{"explicit all", []string{"all"}, true},
		{"single field", []string{"fingerprint"}, false},
		{"compound peer ip", []string{"peer", "ip"}, false},
		{"all with extra", []string{"all", "extra"}, false},
		{"capitalised", []string{"All"}, false},
	}
	testkit.RunTable(t, cases, func(c showAllCase) string { return c.name },
		func(_ *testing.T, c showAllCase) error {
			return fp.Equal("isShowAll", isShowAll(c.args), c.want)
		})
}

func TestDispatch_MissingArgs(t *testing.T) {
	kd := newTestKD(t)
	type missingArgsCase struct {
		cmd  string
		args []string
	}
	cases := []missingArgsCase{
		{"register", nil},
		{"add", nil},
		{"pull", nil},
		{"add-contact", []string{"name-only"}},
		{"remove-contact", nil},
		{"quick-connect", nil},
		{"save-contact", nil},
		{"unshare", nil},
		{"add-as", []string{"only-one"}},
		{"cancel-download", nil},
		{"progress", nil},
	}
	testkit.RunTable(t, cases, func(c missingArgsCase) string { return c.cmd },
		func(_ *testing.T, c missingArgsCase) error {
			resp := dispatchTest(kd, c.cmd, c.args...)
			return fp.False(c.cmd+" with missing args should fail", resp.OK)
		})
}

func TestDispatch_UnknownCommand(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "bogus-command")
	require.False(t, resp.OK, "unknown command should fail")
}

func TestDispatch_Version(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "version")
	require.True(t, resp.OK, "version should succeed: %s", resp.Error)
	var data map[string]string
	require.NoError(t, json.Unmarshal(resp.Data, &data))
	_, ok := data["version"]
	require.True(t, ok, "version response missing 'version' field")
}

func TestDispatch_PollEventEmpty(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "poll-event")
	require.True(t, resp.OK, "poll-event should succeed: %s", resp.Error)
	var data map[string]string
	require.NoError(t, json.Unmarshal(resp.Data, &data))
	require.Equal(t, "", data["event"])
}

func TestDispatch_PollEventWithData(t *testing.T) {
	kd := newTestKD(t)
	eventCh <- "reconnected:"
	resp := dispatchTest(kd, "poll-event")
	require.True(t, resp.OK, "poll-event should succeed: %s", resp.Error)
	var data map[string]string
	_ = json.Unmarshal(resp.Data, &data)
	require.Equal(t, "reconnected:", data["event"])
}

func TestDispatch_PeerInfo(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "peer-info")
	require.True(t, resp.OK, "peer-info should succeed: %s", resp.Error)
	var data map[string]any
	require.NoError(t, json.Unmarshal(resp.Data, &data))
	_, ok := data["connection_mode"]
	require.True(t, ok, "missing connection_mode")
}

func TestDispatch_IncognitoQuery(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "incognito")
	require.True(t, resp.OK, "incognito query should succeed: %s", resp.Error)
}

func TestDispatch_UnshareExistingFile(t *testing.T) {
	kd := newTestKD(t)
	kd.SyncTracker.LocalFiles["test.txt"] = &synctracker.File{
		Name: "test.txt",
	}
	resp := dispatchTest(kd, "unshare", "test.txt")
	require.True(t, resp.OK, "unshare should succeed: %s", resp.Error)
	kd.SyncTracker.LocalFilesMu.RLock()
	_, exists := kd.SyncTracker.LocalFiles["test.txt"]
	kd.SyncTracker.LocalFilesMu.RUnlock()
	require.False(t, exists, "file should be removed from tracker")
}

func TestDispatch_UnshareNonexistent(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "unshare", "ghost.txt")
	require.False(t, resp.OK, "unshare nonexistent should fail")
}

func TestDispatch_ListPrunesStale(t *testing.T) {
	kd := newTestKD(t)
	kd.SyncTracker.LocalFiles["gone.txt"] = &synctracker.File{
		Name:           "gone.txt",
		RealPathOfFile: "/nonexistent/gone.txt",
	}
	resp := dispatchTest(kd, "list")
	require.True(t, resp.OK, "list should succeed: %s", resp.Error)
	kd.SyncTracker.LocalFilesMu.RLock()
	_, exists := kd.SyncTracker.LocalFiles["gone.txt"]
	kd.SyncTracker.LocalFilesMu.RUnlock()
	require.False(t, exists, "stale file should be pruned by list")
}

func TestDispatch_Status(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "status")
	require.True(t, resp.OK, "status should succeed: %s", resp.Error)
	var data map[string]any
	_ = json.Unmarshal(resp.Data, &data)
	_, hasFingerprint := data["fingerprint"]
	require.True(t, hasFingerprint, "status missing fingerprint")
	_, hasMode := data["connection_mode"]
	require.True(t, hasMode, "status missing connection_mode")
}

// TestDispatch_DiscoverDisconnectConcurrent races "discover" against "disconnect" dispatches.
// cmdDiscover sleeps for seconds between checking daemonDisc and calling methods on it; a
// concurrent "disconnect" nils daemonDisc in that window. Pre-fix this is a plain unguarded
// package global: -race reports a DATA RACE, and/or a goroutine hits the nil-deref and its
// recover() surfaces the panic via t.Errorf instead of crashing the whole test binary.
// "stop" shares disconnect's exact daemonDisc-nilling code, so it is not raced separately here.
// Only one discoverer: cmdDiscover also unconditionally writes kd.IsLocalMode (a plain bool on
// common.KeibiDrop, out of scope for this task), so two concurrent "discover" calls on the
// same kd race on that unrelated field regardless of discMu.
// One discoverer against several disconnectors still fully covers the daemonDisc racing pair.
func TestDispatch_DiscoverDisconnectConcurrent(t *testing.T) {
	kd := newTestKD(t)

	const discoverers = 1
	const disconnectors = 4

	var start sync.WaitGroup
	start.Add(1)
	var discoverWG sync.WaitGroup
	var disconnectWG sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < discoverers; i++ {
		discoverWG.Add(1)
		go func() {
			defer discoverWG.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("discover panicked: %v", r)
				}
			}()
			start.Wait()
			dispatchTest(kd, "discover")
		}()
	}

	// Disconnect keeps racing for as long as any discover call is still in flight, so the
	// multi-second sleep window inside cmdDiscover is covered without a guessed duration.
	go func() {
		discoverWG.Wait()
		close(stop)
	}()

	for i := 0; i < disconnectors; i++ {
		disconnectWG.Add(1)
		go func() {
			defer disconnectWG.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("disconnect panicked: %v", r)
				}
			}()
			start.Wait()
			for {
				select {
				case <-stop:
					return
				default:
					dispatchTest(kd, "disconnect")
				}
			}
		}()
	}

	start.Done()
	discoverWG.Wait()
	disconnectWG.Wait()
}
