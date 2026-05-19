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
	"sync/atomic"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
)

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
	if err != nil {
		t.Fatalf("NewKeibiDropWithIP: %v", err)
	}
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
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, true},
		{"empty slice", []string{}, true},
		{"explicit all", []string{"all"}, true},
		{"single field", []string{"fingerprint"}, false},
		{"compound peer ip", []string{"peer", "ip"}, false},
		{"all with extra", []string{"all", "extra"}, false},
		{"capitalised", []string{"All"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isShowAll(tc.args); got != tc.want {
				t.Fatalf("isShowAll(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestDispatch_MissingArgs(t *testing.T) {
	kd := newTestKD(t)
	cases := []struct {
		cmd  string
		args []string
	}{
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
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			resp := dispatchTest(kd, tc.cmd, tc.args...)
			if resp.OK {
				t.Fatalf("%s with missing args should fail", tc.cmd)
			}
		})
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "bogus-command")
	if resp.OK {
		t.Fatal("unknown command should fail")
	}
}

func TestDispatch_Version(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "version")
	if !resp.OK {
		t.Fatalf("version should succeed: %s", resp.Error)
	}
	var data map[string]string
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["version"]; !ok {
		t.Fatal("version response missing 'version' field")
	}
}

func TestDispatch_PollEventEmpty(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "poll-event")
	if !resp.OK {
		t.Fatalf("poll-event should succeed: %s", resp.Error)
	}
	var data map[string]string
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["event"] != "" {
		t.Fatalf("expected empty event, got %q", data["event"])
	}
}

func TestDispatch_PollEventWithData(t *testing.T) {
	kd := newTestKD(t)
	eventCh <- "reconnected:"
	resp := dispatchTest(kd, "poll-event")
	if !resp.OK {
		t.Fatalf("poll-event should succeed: %s", resp.Error)
	}
	var data map[string]string
	_ = json.Unmarshal(resp.Data, &data)
	if data["event"] != "reconnected:" {
		t.Fatalf("expected 'reconnected:', got %q", data["event"])
	}
}

func TestDispatch_PeerInfo(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "peer-info")
	if !resp.OK {
		t.Fatalf("peer-info should succeed: %s", resp.Error)
	}
	var data map[string]any
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["connection_mode"]; !ok {
		t.Fatal("missing connection_mode")
	}
}

func TestDispatch_IncognitoQuery(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "incognito")
	if !resp.OK {
		t.Fatalf("incognito query should succeed: %s", resp.Error)
	}
}

func TestDispatch_UnshareExistingFile(t *testing.T) {
	kd := newTestKD(t)
	kd.SyncTracker.LocalFiles["test.txt"] = &synctracker.File{
		Name: "test.txt",
	}
	resp := dispatchTest(kd, "unshare", "test.txt")
	if !resp.OK {
		t.Fatalf("unshare should succeed: %s", resp.Error)
	}
	kd.SyncTracker.LocalFilesMu.RLock()
	_, exists := kd.SyncTracker.LocalFiles["test.txt"]
	kd.SyncTracker.LocalFilesMu.RUnlock()
	if exists {
		t.Fatal("file should be removed from tracker")
	}
}

func TestDispatch_UnshareNonexistent(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "unshare", "ghost.txt")
	if resp.OK {
		t.Fatal("unshare nonexistent should fail")
	}
}

func TestDispatch_ListPrunesStale(t *testing.T) {
	kd := newTestKD(t)
	kd.SyncTracker.LocalFiles["gone.txt"] = &synctracker.File{
		Name:           "gone.txt",
		RealPathOfFile: "/nonexistent/gone.txt",
	}
	resp := dispatchTest(kd, "list")
	if !resp.OK {
		t.Fatalf("list should succeed: %s", resp.Error)
	}
	kd.SyncTracker.LocalFilesMu.RLock()
	_, exists := kd.SyncTracker.LocalFiles["gone.txt"]
	kd.SyncTracker.LocalFilesMu.RUnlock()
	if exists {
		t.Fatal("stale file should be pruned by list")
	}
}

func TestDispatch_Status(t *testing.T) {
	kd := newTestKD(t)
	resp := dispatchTest(kd, "status")
	if !resp.OK {
		t.Fatalf("status should succeed: %s", resp.Error)
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Data, &data)
	if _, ok := data["fingerprint"]; !ok {
		t.Fatal("status missing fingerprint")
	}
	if _, ok := data["connection_mode"]; !ok {
		t.Fatal("status missing connection_mode")
	}
}
