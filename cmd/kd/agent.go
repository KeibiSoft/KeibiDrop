// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Additive non-interactive surface for agents: stable error codes,
// ABOUTME: process exit codes, timeouts on blocking verbs, async transfers.

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

// Stable machine-readable error codes. An agent branches on these, never on
// the message text. New codes may be added; existing ones do not change.
const (
	codeOK              = "ok"
	codeInvalidArgument = "invalid_argument"
	codeNotConnected    = "not_connected"
	codeNotFound        = "not_found"
	codeTimeout         = "timeout"
	codeBusy            = "busy"
	codeRefused         = "refused"
	codeUnsupported     = "unsupported"
	codeInternal        = "internal"
)

// Process exit codes, one per error code. 0 means success. These are the
// contract a shell script branches on.
const (
	exitOK              = 0
	exitInternal        = 1
	exitNotConnected    = 2
	exitTimeout         = 3
	exitNotFound        = 4
	exitInvalidArgument = 5
	exitBusy            = 6
	exitRefused         = 7
	exitUnsupported     = 8
)

// exitCodeFor maps an error code to the process exit code.
func exitCodeFor(code string) int {
	switch code {
	case "", codeOK:
		return exitOK
	case codeNotConnected:
		return exitNotConnected
	case codeTimeout:
		return exitTimeout
	case codeNotFound:
		return exitNotFound
	case codeInvalidArgument:
		return exitInvalidArgument
	case codeBusy:
		return exitBusy
	case codeRefused:
		return exitRefused
	case codeUnsupported:
		return exitUnsupported
	default:
		return exitInternal
	}
}

// classifyError maps an engine error to a stable code. The engine returns
// sentinel errors for most failures; the string fallbacks cover the paths that
// still format their own text.
func classifyError(err error) string {
	if err == nil {
		return codeOK
	}
	switch {
	case errors.Is(err, common.ErrInvalidSession),
		errors.Is(err, common.ErrSessionNotEstablished),
		errors.Is(err, common.ErrNilPointer),
		errors.Is(err, common.ErrNilFilesystem):
		return codeNotConnected
	case errors.Is(err, common.ErrTimeoutReached):
		return codeTimeout
	case errors.Is(err, common.ErrNotFound):
		return codeNotFound
	case errors.Is(err, common.ErrAlreadyRunning),
		errors.Is(err, common.ErrFilesystemAlreadyMounted):
		return codeBusy
	case errors.Is(err, common.ErrFingerprintMismatch),
		errors.Is(err, common.ErrIdenticalFingerprints),
		errors.Is(err, common.ErrListenerNotOpen):
		return codeRefused
	case errors.Is(err, common.ErrInvalidLength),
		errors.Is(err, common.ErrInvalidFingerprint),
		errors.Is(err, common.ErrEmptyFingerprint),
		errors.Is(err, common.ErrInvalidIP):
		return codeInvalidArgument
	}
	return classifyMessage(err.Error())
}

// classifyMessage maps error text to a code, for the paths that return a
// formatted error rather than a sentinel.
func classifyMessage(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "no such file"), strings.Contains(m, "not found"),
		strings.Contains(m, "not shared"), strings.Contains(m, "no active download"),
		strings.Contains(m, "enoent"):
		return codeNotFound
	case strings.Contains(m, "already in progress"), strings.Contains(m, "already running"):
		return codeBusy
	case strings.Contains(m, "timeout"), strings.Contains(m, "deadline exceeded"):
		return codeTimeout
	// Before the generic "invalid" arm below: "invalid session" is a session
	// state, not a bad argument, and an agent retries the two differently.
	case strings.Contains(m, "invalid session"), strings.Contains(m, "not connected"),
		strings.Contains(m, "session not established"), strings.Contains(m, "nil pointer"),
		strings.Contains(m, "daemon not running"):
		return codeNotConnected
	case strings.Contains(m, "unknown command"), strings.Contains(m, "unknown show target"):
		return codeUnsupported
	case strings.Contains(m, "usage:"), strings.Contains(m, "invalid"):
		return codeInvalidArgument
	case strings.Contains(m, "mismatch"), strings.Contains(m, "refused"):
		return codeRefused
	}
	return codeInternal
}

// errResponseFor builds a failure response with the code derived from err.
// The "error" field keeps the exact text it had before, so existing parsers
// are unaffected; "code" is new.
func errResponseFor(err error) Response {
	return Response{OK: false, Error: err.Error(), Code: classifyError(err)}
}

// errCoded builds a failure response with an explicit code.
func errCoded(code, msg string) Response {
	return Response{OK: false, Error: msg, Code: code}
}

// --- Transfers ---

// transfer is one async pull. Progress comes from the engine's own tracker,
// so a transfer started here reads the same numbers as "kd progress".
type transfer struct {
	ID        string `json:"transfer_id"`
	Name      string `json:"name"`
	LocalPath string `json:"local_path"`
	Total     uint64 `json:"total"`
	StartedAt string `json:"started_at"`

	done    atomic.Bool
	errText atomic.Value // string
}

var (
	transfersMu sync.Mutex
	transfers   = map[string]*transfer{}
	transferSeq atomic.Uint64
)

// newTransferID returns a short monotonic id. Monotonic, not random: an agent
// reading a log can order them, and there is no secret in a transfer id.
func newTransferID() string {
	return "t" + strconv.FormatUint(transferSeq.Add(1), 10)
}

// remoteFileSize returns the announced size of a remote file, trying the
// leading-slash form the tracker also uses.
func remoteFileSize(kd *common.KeibiDrop, name string) (uint64, bool) {
	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()
	if f, ok := kd.SyncTracker.RemoteFiles[name]; ok {
		return f.Size, true
	}
	if f, ok := kd.SyncTracker.RemoteFiles["/"+name]; ok {
		return f.Size, true
	}
	return 0, false
}

// cmdPullAsync starts a pull in the background and returns a transfer id at
// once. This is what makes a large transfer survivable under an MCP client
// that times out tool calls.
func cmdPullAsync(kd *common.KeibiDrop, args []string) Response {
	if len(args) < 1 {
		return errCoded(codeInvalidArgument, "usage: kd pull-async <remote-name> [local-path]")
	}
	name := args[0]
	localPath := name
	if len(args) >= 2 {
		localPath = filepath.Clean(args[1])
	}
	size, ok := remoteFileSize(kd, name)
	if !ok {
		return errCoded(codeNotFound, "file not found: "+name)
	}

	t := &transfer{
		ID:        newTransferID(),
		Name:      name,
		LocalPath: localPath,
		Total:     size,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	t.errText.Store("")

	transfersMu.Lock()
	transfers[t.ID] = t
	transfersMu.Unlock()

	go func() {
		err := kd.PullFile(name, localPath)
		if err != nil {
			t.errText.Store(err.Error())
		}
		t.done.Store(true)
	}()

	return okResponse(map[string]any{
		"transfer_id": t.ID,
		"name":        t.Name,
		"local_path":  t.LocalPath,
		"total":       t.Total,
		"started_at":  t.StartedAt,
	})
}

// transferSnapshot renders one transfer's current state.
func transferSnapshot(kd *common.KeibiDrop, t *transfer) map[string]any {
	errText, _ := t.errText.Load().(string)
	done := t.done.Load()

	// Progress is a fraction in [0,1]; the engine returns a negative sentinel
	// for an unknown name, which only happens after the tracker forgets it.
	frac := kd.GetDownloadProgress(t.Name)
	var bytes uint64
	pct := 0
	switch {
	case done && errText == "":
		bytes, pct = t.Total, 100
	case frac >= 0:
		bytes = uint64(frac * float64(t.Total))
		pct = int(frac * 100)
	}

	out := map[string]any{
		"transfer_id": t.ID,
		"name":        t.Name,
		"local_path":  t.LocalPath,
		"done":        done,
		"bytes":       bytes,
		"total":       t.Total,
		"percent":     pct,
		"started_at":  t.StartedAt,
	}
	if errText != "" {
		out["error"] = errText
		out["error_code"] = classifyMessage(errText)
	}
	return out
}

func cmdTransferStatus(kd *common.KeibiDrop, args []string) Response {
	if len(args) < 1 {
		return errCoded(codeInvalidArgument, "usage: kd transfer-status <transfer-id>")
	}
	transfersMu.Lock()
	t, ok := transfers[args[0]]
	transfersMu.Unlock()
	if !ok {
		return errCoded(codeNotFound, "unknown transfer id: "+args[0])
	}
	return okResponse(transferSnapshot(kd, t))
}

func cmdTransfers(kd *common.KeibiDrop) Response {
	transfersMu.Lock()
	list := make([]*transfer, 0, len(transfers))
	for _, t := range transfers {
		list = append(list, t)
	}
	transfersMu.Unlock()

	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, transferSnapshot(kd, t))
	}
	return okResponse(map[string]any{"transfers": out})
}

// --- Timeouts on blocking verbs ---

// defaultConnectTimeout caps create/join/connect when the caller gives no
// --timeout. The engine's own budget is common.Timeout (595s), which is a
// human-scale window: one person creates, sends the code by chat, the other
// pastes it. An agent has no such patience, so the default here is short and
// the caller raises it when a human is genuinely in the loop.
const defaultConnectTimeout = 60 * time.Second

// runConnectWithTimeout connects and gives up after d. On expiry it aborts the
// pending rendezvous via NotifyDisconnect, which sets the engine's
// connectAborted latch, so a side left waiting for its peer stops instead of
// holding the OpInProgress guard for the full 595s.
//
// Connect only. It compares the two fingerprints and derives the same role
// split on both machines, so neither side has to be told which one it is.
// Driving the underlying room primitives directly means picking that role by
// hand, and two peers that pick the same one wait for each other forever.
func runConnectWithTimeout(kd *common.KeibiDrop, d time.Duration) Response {
	if kd.OpInProgress.Add(1) != 1 {
		kd.OpInProgress.Add(-1)
		return errCoded(codeBusy, "a connect is already in progress")
	}

	errCh := make(chan error, 1)
	go func() {
		defer kd.OpInProgress.Add(-1)
		errCh <- kd.Connect()
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			return errResponseFor(err)
		}
		return okResponse(map[string]string{
			"status":  "connected",
			"peer_ip": kd.PeerIPv6IP,
			"mode":    kd.ConnectionMode,
		})
	case <-timer.C:
		kd.NotifyDisconnect()
		return errCoded(codeTimeout,
			fmt.Sprintf("connect timed out after %s waiting for the peer", d))
	}
}

// cmdWaitConnected blocks until the session is healthy or the deadline passes.
// It is the one polling loop an agent should not have to write itself.
func cmdWaitConnected(kd *common.KeibiDrop, d time.Duration) Response {
	deadline := time.Now().Add(d)
	for {
		if kd.IsRunning() {
			pfp, _ := kd.GetPeerFingerprint()
			return okResponse(map[string]any{
				"connected":         true,
				"connection_status": kd.ConnectionStatus(),
				"connection_mode":   kd.ConnectionMode,
				"peer_fingerprint":  pfp,
				"peer_ip":           kd.PeerIPv6IP,
			})
		}
		if time.Now().After(deadline) {
			return errCoded(codeTimeout, "not connected after "+d.String())
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// timeoutOrDefault reads the request timeout, falling back to def.
func timeoutOrDefault(req Request, def time.Duration) time.Duration {
	if req.TimeoutS > 0 {
		return time.Duration(req.TimeoutS) * time.Second
	}
	return def
}

// --- Mount ---

// cmdMountInfo reports where the peer's files are readable on this machine and
// whether that path is live yet. With FUSE on, an agent reads the peer's files
// straight off mount_path and only the bytes it touches cross the wire; with
// FUSE off it must pull whole files into save_path first.
func cmdMountInfo(kd *common.KeibiDrop) Response {
	ready := false
	if kd.IsFUSE && kd.ToMount != "" && kd.IsRunning() {
		ready = isLiveMount(kd.ToMount)
	}
	mode := "on_demand"
	if !kd.IsFUSE {
		mode = "copy"
	}
	return okResponse(map[string]any{
		"fuse":       kd.IsFUSE,
		"mode":       mode,
		"mount_path": kd.ToMount,
		"save_path":  kd.ToSave,
		"ready":      ready,
		"note":       mountNote(kd.IsFUSE),
	})
}

func mountNote(isFUSE bool) string {
	if isFUSE {
		return "read the peer's files directly under mount_path; blocks stream on demand"
	}
	return "FUSE is off: use pull-async to copy a file into save_path before reading it"
}

// cmdWaitMount blocks until the mount is live or the deadline passes.
func cmdWaitMount(kd *common.KeibiDrop, d time.Duration) Response {
	if !kd.IsFUSE {
		return errCoded(codeUnsupported, "FUSE is disabled; there is no mount to wait for")
	}
	deadline := time.Now().Add(d)
	for {
		if kd.IsRunning() && kd.ToMount != "" && isLiveMount(kd.ToMount) {
			return okResponse(map[string]any{
				"ready":      true,
				"mount_path": kd.ToMount,
			})
		}
		if time.Now().After(deadline) {
			return errCoded(codeTimeout, "mount not ready after "+d.String())
		}
		time.Sleep(250 * time.Millisecond)
	}
}
