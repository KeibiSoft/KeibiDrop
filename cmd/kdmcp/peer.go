// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: One kdmcp process = one KeibiDrop peer identity = one session.
// ABOUTME: Tool implementations live here; they call pkg/logic/common directly.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/cmd/internal/checkfuse"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

// Stable error codes. An agent branches on these; the message is for a human
// reading the transcript.
const (
	codeInvalidArgument = "invalid_argument"
	codeNotConnected    = "not_connected"
	codeNotFound        = "not_found"
	codeTimeout         = "timeout"
	codeBusy            = "busy"
	codeRefused         = "refused"
	codeForbidden       = "forbidden"
	codeUnsupported     = "unsupported"
	codeInternal        = "internal"
)

type toolError struct {
	Code    string
	Message string
}

func toolErr(code, format string, a ...any) *toolError {
	return &toolError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// defaultJoinTimeout matches the schema: kd_join blocks up to 30s by default.
const defaultJoinTimeout = 30 * time.Second

type transfer struct {
	ID    string
	Name  string
	Path  string
	Total uint64

	done    atomic.Bool
	errText atomic.Value // string
}

type peer struct {
	kd  *common.KeibiDrop
	cfg config.Config

	// shareRoot is the only directory kd_send_file may read from. Empty means
	// sending is disabled.
	shareRoot string

	// events carries engine notices to kd_wait_event. Buffered so a burst
	// during connect is not lost while the agent is busy elsewhere.
	events chan string

	mu         sync.Mutex
	connecting bool // a background Connect retry loop is in flight
	transfers  map[string]*transfer
	seq        atomic.Uint64
}

// newPeer provisions the local environment and brings the engine up. This is
// the "auto install" step: an agent pointed at kdmcp gets its directories
// created, its identity generated and its session ready without a human
// running anything first.
func newPeer(ctx context.Context) (*peer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	_ = config.WriteDefault()
	if err := config.EnsureDirectories(cfg); err != nil {
		return nil, fmt.Errorf("directories: %w", err)
	}

	relayURL, err := url.Parse(cfg.Relay)
	if err != nil {
		return nil, fmt.Errorf("invalid relay %q: %w", cfg.Relay, err)
	}

	// Stdout is the JSON-RPC channel. Every log byte goes to the log file, or
	// to stderr when there is none; writing to stdout would corrupt the
	// protocol stream.
	logWriter := os.Stderr
	if cfg.LogFile != "" {
		if f, err := os.OpenFile(filepath.Clean(cfg.LogFile),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			logWriter = f
		}
	}
	logger := slog.New(slog.NewTextHandler(logWriter,
		&slog.HandlerOptions{Level: slog.LevelInfo})).With("component", "kdmcp")

	isFUSE := checkfuse.IsFUSEPresent() && !cfg.NoFUSE

	kd, err := common.NewKeibiDrop(ctx, logger, isFUSE, relayURL,
		cfg.InboundPort, cfg.OutboundPort, cfg.MountPath, cfg.SavePath,
		cfg.PrefetchOnOpen, cfg.PushOnWrite)
	if err != nil {
		return nil, fmt.Errorf("engine init: %w", err)
	}
	kd.BridgeAddr = cfg.BridgeAddr
	kd.StrictMode = cfg.StrictMode
	kd.PrefetchAutoMB = cfg.PrefetchAutoMB
	kd.ReadAheadWindowMB = cfg.ReadAheadWindowMB
	kd.ScanSharedOnStart = cfg.ScanSharedOnStart
	kd.ShareReadOnly = cfg.ShareReadOnly
	kd.MountReadOnly = cfg.MountReadOnly
	kd.PreserveMetadata = cfg.PreserveMetadata

	if !cfg.Incognito {
		// PassphraseProtect is deliberately not honored here: it reads from a
		// TTY, and an MCP server has none. Identity stays unencrypted at rest
		// or the agent hangs forever on a prompt nobody can answer.
		if err := kd.EnablePersistentIdentity(config.ConfigDir(), common.EnableOpts{}); err != nil {
			logger.Warn("persistent identity unavailable, using ephemeral", "error", err)
		}
	}

	p := &peer{
		kd:        kd,
		cfg:       cfg,
		transfers: map[string]*transfer{},
		events:    make(chan string, 128),
	}
	p.shareRoot = resolveShareRoot()

	// The engine reports session changes here. An agent reads them with
	// kd_wait_event instead of polling kd_status in a loop. A full queue drops
	// its oldest entry: the newest state is the one worth keeping, and
	// kd_status always holds the authoritative answer.
	kd.OnEvent = func(evt string) {
		select {
		case p.events <- evt:
		default:
			select {
			case <-p.events:
			default:
			}
			select {
			case p.events <- evt:
			default:
			}
		}
	}

	go kd.ProbeInboundReachability(ctx)
	go kd.Run()

	return p, nil
}

// resolveShareRoot reads KD_SHARE_ROOT and resolves it through symlinks once,
// at startup. Empty means sending is disabled, which is the default: a server
// that can send files off the machine is an exfiltration primitive, so it
// stays off until an operator names exactly one directory.
func resolveShareRoot() string {
	raw := strings.TrimSpace(os.Getenv("KD_SHARE_ROOT"))
	if raw == "" {
		return ""
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// A root that does not exist yet is created, so an agent's first run
		// works without a human running mkdir.
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(abs, 0o750); mkErr == nil {
				if r2, err2 := filepath.EvalSymlinks(abs); err2 == nil {
					return r2
				}
			}
		}
		return ""
	}
	return resolved
}

// safeSendPath resolves p and confirms it stays inside the share root.
// Symlinks are resolved before the check, so a link inside the root pointing
// at /etc/shadow is refused.
func (p *peer) safeSendPath(raw string) (string, *toolError) {
	resolved, isDir, terr := p.resolveSendTarget(raw)
	if terr != nil {
		return "", terr
	}
	if isDir {
		return "", toolErr(codeInvalidArgument, "%s is a directory; send a file", raw)
	}
	return resolved, nil
}

// resolveSendTarget is safeSendPath widened to accept a directory, so a whole
// tree can be offered in one call. Containment is checked the same way.
func (p *peer) resolveSendTarget(raw string) (path string, isDir bool, terr *toolError) {
	resolved, err := p.containedPath(raw)
	if err != nil {
		return "", false, err
	}
	fi, statErr := os.Stat(resolved)
	if statErr != nil {
		return "", false, toolErr(codeNotFound, "no such file: %s", raw)
	}
	return resolved, fi.IsDir(), nil
}

// containedPath resolves raw and confirms it stays inside the share root,
// following symlinks first. It says nothing about what kind of thing it found.
func (p *peer) containedPath(raw string) (string, *toolError) {
	if p.shareRoot == "" {
		return "", toolErr(codeForbidden,
			"sending is disabled: set KD_SHARE_ROOT to the one directory this server may send from")
	}
	if raw == "" {
		return "", toolErr(codeInvalidArgument, "path is required")
	}

	abs := raw
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(p.shareRoot, raw)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", toolErr(codeNotFound, "no such file: %s", raw)
		}
		return "", toolErr(codeInvalidArgument, "cannot resolve %s: %v", raw, err)
	}
	if !p.insideRoot(resolved) {
		return "", toolErr(codeForbidden, "path is outside KD_SHARE_ROOT: %s", raw)
	}
	return resolved, nil
}

// insideRoot reports whether resolved sits at or under the share root.
func (p *peer) insideRoot(resolved string) bool {
	if resolved == p.shareRoot {
		return true
	}
	rel, err := filepath.Rel(p.shareRoot, resolved)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sendTree offers every file under dir, keeping the directory structure. The
// peer sees the tree and reads a file's bytes when it opens that file, so the
// cost of offering a large tree is the walk, not the data.
//
// This announces one file at a time, which fits a file or a small folder. The
// engine's batched announce is reachable only through ScanAndShareSaveDir,
// which scans the save directory, so a large folder belongs on that path:
// point save_path at it with scan_shared_on_start set.
func (p *peer) sendTree(dir, remoteName string) (any, *toolError) {
	base := remoteName
	if base == "" {
		base = filepath.Base(dir)
	}
	if terr := safeName(base); terr != nil {
		return nil, terr
	}

	started := time.Now()
	var files, skipped, links int
	var bytes uint64
	var firstErr error

	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			skipped++
			return nil // an unreadable corner does not abandon the rest
		}
		if d.IsDir() {
			return nil
		}
		// KeibiDrop carries regular files. A symlink is skipped rather than
		// followed: following one would send the target's bytes under the
		// link's name and lose the fact that it was a link. The peer receives
		// a hard link as an ordinary file, so a file with several names
		// arrives as several files.
		if d.Type()&os.ModeSymlink != 0 {
			links++
			return nil
		}
		fi, statErr := os.Stat(path)
		if statErr != nil || !fi.Mode().IsRegular() {
			skipped++
			return nil
		}

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			skipped++
			return nil
		}
		// The peer sees slash-separated paths whatever the local separator is.
		remote := base + "/" + filepath.ToSlash(rel)

		if addErr := p.kd.AddFileAs(path, remote); addErr != nil {
			skipped++
			if firstErr == nil {
				firstErr = addErr
			}
			return nil
		}
		files++
		bytes += uint64(fi.Size())
		return nil
	})

	if walkErr != nil && files == 0 {
		return nil, toolErr(codeInternal, "walking %s: %v", dir, walkErr)
	}
	if files == 0 && firstErr != nil {
		return nil, classify(firstErr)
	}

	out := map[string]any{
		"queued":      true,
		"remote_name": base,
		"files":       files,
		"bytes":       bytes,
		"elapsed_s":   fmt.Sprintf("%.2f", time.Since(started).Seconds()),
		"note": "the tree is announced, not copied; the peer reads a file's bytes " +
			"when it opens that file",
	}
	if skipped > 0 {
		out["skipped"] = skipped
		out["skipped_reason"] = "unreadable, or not a regular file"
	}
	if links > 0 {
		out["symlinks_skipped"] = links
		out["symlinks_note"] = "KeibiDrop carries regular files; symbolic links are left behind"
	}
	return out, nil
}

// safeName rejects a peer-supplied name that would escape the save directory.
// The name comes off the wire, so it is never trusted as a path.
func safeName(name string) *toolError {
	if name == "" {
		return toolErr(codeInvalidArgument, "name is required")
	}
	if filepath.IsAbs(name) || strings.Contains(name, "..") {
		return toolErr(codeForbidden, "refusing a name that escapes the save directory: %s", name)
	}
	return nil
}

// --- Tools ---

func (h *handler) invoke(name string, args json.RawMessage) (any, *toolError) {
	p := h.kd
	switch name {
	case "kd_setup":
		return p.setup()
	case "kd_create_share":
		return p.createShare()
	case "kd_accept_peer":
		return p.acceptPeer(args)
	case "kd_join":
		return p.join(args)
	case "kd_status":
		return p.status(), nil
	case "kd_list_files":
		return p.listFiles(args)
	case "kd_send_file":
		return p.sendFile(args)
	case "kd_pull_file":
		return p.pullFile(args)
	case "kd_transfer_status":
		return p.transferStatus(args)
	case "kd_wait_event":
		return p.waitEvent(args)
	case "kd_credit":
		return p.credit()
	case "kd_buy_credit":
		return p.buyCredit()
	case "kd_add_credit":
		return p.addCredit(args)
	case "kd_peers":
		return p.peers()
	case "kd_save_peer":
		return p.savePeer(args)
	case "kd_connect_peer":
		return p.connectPeer(args)
	case "kd_disconnect":
		return p.disconnect()
	default:
		return nil, toolErr(codeUnsupported, "unknown tool: %s", name)
	}
}

func (p *peer) code() string {
	fp, _ := p.kd.ExportFingerprint()
	return fp
}

func (p *peer) setup() (any, *toolError) {
	mode := "on_demand"
	hint := "The peer's files will be readable at mount_path; only the bytes you read cross the wire."
	if !p.kd.IsFUSE {
		mode = "copy"
		hint = "FUSE is not available, so use kd_pull_file to copy a file before reading it. " +
			fuseInstallHint()
	}

	sending := "disabled: set KD_SHARE_ROOT to enable kd_send_file"
	if p.shareRoot != "" {
		sending = p.shareRoot
	}

	return map[string]any{
		"ready":        true,
		"version":      buildVersion(),
		"my_code":      p.code(),
		"mode":         mode,
		"fuse":         p.kd.IsFUSE,
		"mount_path":   p.cfg.MountPath,
		"save_path":    p.cfg.SavePath,
		"share_root":   sending,
		"config_dir":   config.ConfigDir(),
		"relay":        p.cfg.Relay,
		"inbound_port": p.cfg.InboundPort,
		"integrity":    p.integrityModes(),
		"note":         hint,
		"next":         "Creator: kd_create_share. Joiner: kd_join with the creator's code.",
	}, nil
}

// integrityModes reports the settings that decide whether what an agent reads
// can stand in for the origin. They are off by default, so work that depends
// on them has to turn them on and can check here that it worked.
func (p *peer) integrityModes() map[string]any {
	m := map[string]any{
		"preserve_metadata":    p.cfg.PreserveMetadata,
		"share_read_only":      p.cfg.ShareReadOnly,
		"mount_read_only":      p.cfg.MountReadOnly,
		"scan_shared_on_start": p.cfg.ScanSharedOnStart,
		"incognito":            p.cfg.Incognito,
		"strict_mode":          p.cfg.StrictMode,
	}
	if p.cfg.PreserveMetadata {
		// Stated precisely on purpose. Birth time is announced and visible
		// through the mount but is not written to disk, because most systems
		// do not allow it, and no tool can set ctime: the kernel owns it.
		m["preserve_metadata_covers"] = "permission bits, mtime and atime"
		if runtimeGOOS() == "darwin" {
			// Linux uses O_NOATIME and Windows a handle sentinel while serving
			// a read. macOS has no per-handle equivalent, so the source
			// volume's own policy decides and plain APFS moves atime on read.
			m["atime_caveat"] = "on macOS the source volume decides: put save_path on a noatime volume " +
				"or reading a file here moves its atime"
		}
	}
	if p.cfg.ScanSharedOnStart {
		// Pointing save_path at a folder that already holds data and turning
		// this on offers that whole folder the moment a session starts, in
		// place. The engine batches the announcements, so this is the route
		// for a folder with a large number of files.
		m["note"] = "save_path is offered to the peer as soon as a session starts, in place"
	} else {
		m["note"] = "files already sitting in save_path are left alone; set " +
			"KEIBIDROP_SCAN_SHARED_ON_START=1 to offer that whole folder when a session starts"
	}
	return m
}

func (p *peer) createShare() (any, *toolError) {
	// This mints nothing and starts nothing: the code IS this peer's identity
	// fingerprint, which exists from process start. Connecting begins once the
	// other side's code arrives, because the engine decides who listens and
	// who dials by comparing the two codes. Starting a rendezvous before that
	// comparison is possible only picks a role that may be wrong.
	return map[string]any{
		"code":         p.code(),
		"fingerprint":  p.code(),
		"expires_hint": "codes are this peer's identity; they last as long as the process",
		"next": "Give code to the other peer and ask for theirs, then call kd_accept_peer " +
			"with it. KeibiDrop authenticates both directions, so neither side connects " +
			"on one code alone.",
	}, nil
}

// startConnect sets the peer code and brings the session up. Connect() picks
// creator or joiner by comparing the two codes, so both peers can call this
// with no coordination and exactly one of them listens.
func (p *peer) startConnect(code string) *toolError {
	code = strings.TrimSpace(code)
	if code == "" {
		return toolErr(codeInvalidArgument, "code is required")
	}
	if code == p.code() {
		return toolErr(codeInvalidArgument, "that is this peer's own code; pass the other peer's code")
	}
	if err := p.kd.AddPeerFingerprint(code); err != nil {
		return toolErr(codeInvalidArgument, "%v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.connecting || p.kd.IsRunning() {
		return nil // already under way, or already up: both are success
	}
	p.connecting = true

	go func() {
		// Retry while the deadline holds. The peer may not have called its own
		// accept yet, and a rendezvous that starts too early fails fast rather
		// than waiting; retrying is what makes the two calls order-independent.
		deadline := time.Now().Add(connectBudget)
		for time.Now().Before(deadline) {
			if err := p.kd.Connect(); err == nil || p.kd.IsRunning() {
				break
			}
			time.Sleep(2 * time.Second)
		}
		p.mu.Lock()
		p.connecting = false
		p.mu.Unlock()
	}()
	return nil
}

// connectBudget bounds the background retry loop. Long enough for a human to
// paste a code into the other agent, short enough that a dead pairing does not
// hold the session forever.
const connectBudget = 5 * time.Minute

func (p *peer) acceptPeer(args json.RawMessage) (any, *toolError) {
	var a struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(args, &a)

	if p.kd.IsRunning() {
		return map[string]any{"accepted": true, "connected": true}, nil
	}
	if terr := p.startConnect(a.Code); terr != nil {
		return nil, terr
	}
	return map[string]any{
		"accepted": true,
		"my_code":  p.code(),
		"next":     "poll kd_status until connected is true",
	}, nil
}

func (p *peer) join(args json.RawMessage) (any, *toolError) {
	var a struct {
		Code     string `json:"code"`
		TimeoutS int    `json:"timeout_s"`
	}
	_ = json.Unmarshal(args, &a)

	// Idempotent: joining while connected reports current state.
	if p.kd.IsRunning() {
		return p.status(), nil
	}
	if terr := p.startConnect(a.Code); terr != nil {
		return nil, terr
	}

	timeout := defaultJoinTimeout
	if a.TimeoutS > 0 {
		timeout = time.Duration(a.TimeoutS) * time.Second
	}
	deadline := time.Now().Add(timeout)
	for !p.kd.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}

	st := p.status()
	if m, ok := st.(map[string]any); ok && m["connected"] == false {
		m["next"] = "give my_code to the other peer so they can call kd_accept_peer, then poll kd_status"
	}
	return st, nil
}

func (p *peer) status() any {
	pfp, _ := p.kd.GetPeerFingerprint()
	if pfp == "TOFU" {
		pfp = ""
	}
	sent, recv := p.kd.WireStatsTotal()

	p.mu.Lock()
	active := 0
	for _, t := range p.transfers {
		if !t.done.Load() {
			active++
		}
	}
	p.mu.Unlock()

	connected := p.kd.IsRunning()
	readAt := p.cfg.SavePath
	if p.kd.IsFUSE && connected {
		readAt = p.kd.ToMount
	}

	p.kd.SyncTracker.LocalFilesMu.RLock()
	localN := len(p.kd.SyncTracker.LocalFiles)
	p.kd.SyncTracker.LocalFilesMu.RUnlock()
	p.kd.SyncTracker.RemoteFilesMu.RLock()
	remoteN := len(p.kd.SyncTracker.RemoteFiles)
	p.kd.SyncTracker.RemoteFilesMu.RUnlock()

	return map[string]any{
		"connected":          connected,
		"state":              p.kd.ConnectionStatus(),
		"my_code":            p.code(),
		"peer":               pfp,
		"mode":               p.kd.ConnectionMode,
		"bytes_out":          sent,
		"bytes_in":           recv,
		"active_transfers":   active,
		"local_files":        localN,
		"remote_files":       remoteN,
		"fuse":               p.kd.IsFUSE,
		"mount_path":         p.kd.ToMount,
		"save_path":          p.cfg.SavePath,
		"read_peer_files_at": readAt,
		// Non-zero means the peer refused local changes, so this mount is
		// showing bytes the origin does not hold. It is the signal that
		// matters when the mount is standing in for the original data.
		"peer_refused_writes": p.kd.PeerRefusedWrites(),
	}
}

func (p *peer) listFiles(args json.RawMessage) (any, *toolError) {
	var a struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal(args, &a)
	want := strings.ToLower(strings.TrimSpace(a.Source))
	if want == "" {
		want = "all"
	}
	if want != "all" && want != "local" && want != "remote" {
		return nil, toolErr(codeInvalidArgument, "source must be local, remote or all")
	}

	type entry struct {
		Name   string `json:"name"`
		Size   uint64 `json:"size"`
		Dir    bool   `json:"dir"`
		Source string `json:"source"`
	}
	entries := []entry{}

	if want == "all" || want == "local" {
		p.kd.SyncTracker.LocalFilesMu.RLock()
		for k, v := range p.kd.SyncTracker.LocalFiles {
			entries = append(entries, entry{Name: k, Size: v.Size, Source: "local"})
		}
		p.kd.SyncTracker.LocalFilesMu.RUnlock()
	}
	if want == "all" || want == "remote" {
		p.kd.SyncTracker.RemoteFilesMu.RLock()
		for k, v := range p.kd.SyncTracker.RemoteFiles {
			entries = append(entries, entry{Name: k, Size: v.Size, Source: "remote"})
		}
		p.kd.SyncTracker.RemoteFilesMu.RUnlock()
	}

	return map[string]any{
		"entries": entries,
		"note":    "last-known list from the session tracker; it does not trigger a sync",
	}, nil
}

func (p *peer) sendFile(args json.RawMessage) (any, *toolError) {
	var a struct {
		Path       string `json:"path"`
		RemoteName string `json:"remote_name"`
	}
	_ = json.Unmarshal(args, &a)

	resolved, isDir, terr := p.resolveSendTarget(a.Path)
	if terr != nil {
		return nil, terr
	}
	if !p.kd.IsRunning() {
		return nil, toolErr(codeNotConnected, "no session; pair first with kd_create_share or kd_join")
	}
	if isDir {
		return p.sendTree(resolved, strings.TrimSpace(a.RemoteName))
	}

	remoteName := strings.TrimSpace(a.RemoteName)
	var err error
	if remoteName == "" {
		err = p.kd.AddFile(resolved)
		remoteName = filepath.Base(resolved)
	} else {
		if terr := safeName(remoteName); terr != nil {
			return nil, terr
		}
		err = p.kd.AddFileAs(resolved, remoteName)
	}
	if err != nil {
		return nil, classify(err)
	}

	var size int64
	if fi, statErr := os.Stat(resolved); statErr == nil {
		size = fi.Size()
	}
	return map[string]any{
		"queued":      true,
		"remote_name": remoteName,
		"size":        size,
		"note":        "metadata announced; content streams when the peer reads it",
	}, nil
}

func (p *peer) pullFile(args json.RawMessage) (any, *toolError) {
	var a struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &a)
	name := strings.TrimSpace(a.Name)
	if terr := safeName(name); terr != nil {
		return nil, terr
	}
	if !p.kd.IsRunning() {
		return nil, toolErr(codeNotConnected, "no session; pair first with kd_create_share or kd_join")
	}

	size, ok := p.remoteSize(name)
	if !ok {
		return nil, toolErr(codeNotFound, "the peer is not offering %q; check kd_list_files", name)
	}

	// Writes land under KD_SAVE_PATH only. safeName already rejected traversal.
	dest := filepath.Join(p.cfg.SavePath, filepath.Base(name))

	t := &transfer{
		ID:    "t" + strconv.FormatUint(p.seq.Add(1), 10),
		Name:  name,
		Path:  dest,
		Total: size,
	}
	t.errText.Store("")

	p.mu.Lock()
	p.transfers[t.ID] = t
	p.mu.Unlock()

	go func() {
		if err := p.kd.PullFile(name, dest); err != nil {
			t.errText.Store(err.Error())
		}
		t.done.Store(true)
	}()

	return map[string]any{
		"transfer_id": t.ID,
		"name":        name,
		"local_path":  dest,
		"total":       size,
		"next":        "poll kd_transfer_status with transfer_id",
	}, nil
}

func (p *peer) remoteSize(name string) (uint64, bool) {
	p.kd.SyncTracker.RemoteFilesMu.RLock()
	defer p.kd.SyncTracker.RemoteFilesMu.RUnlock()
	if f, ok := p.kd.SyncTracker.RemoteFiles[name]; ok {
		return f.Size, true
	}
	if f, ok := p.kd.SyncTracker.RemoteFiles["/"+name]; ok {
		return f.Size, true
	}
	return 0, false
}

func (p *peer) transferStatus(args json.RawMessage) (any, *toolError) {
	var a struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(args, &a)

	p.mu.Lock()
	t, ok := p.transfers[strings.TrimSpace(a.TransferID)]
	p.mu.Unlock()
	if !ok {
		return nil, toolErr(codeNotFound, "unknown transfer_id: %s", a.TransferID)
	}

	errText, _ := t.errText.Load().(string)
	done := t.done.Load()

	// The engine reports a fraction and returns a negative sentinel for a name
	// it no longer tracks; there is no bytes-level API to read instead.
	frac := p.kd.GetDownloadProgress(t.Name)
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
		"local_path":  t.Path,
		"done":        done,
		"bytes":       bytes,
		"total":       t.Total,
		"percent":     pct,
	}
	if errText != "" {
		out["error"] = map[string]any{"code": classifyText(errText), "message": errText}
	}
	return out, nil
}

// --- Relay credit ---

// Credit buys priority on the bridge, which is the relay-assisted path used
// when two peers cannot reach each other directly. A peer with no credit still
// transfers: bridged traffic falls to the free tier, which is slower only when
// the bridge is under load. A direct connection never touches this.

func (p *peer) credit() (any, *toolError) {
	st := p.kd.TokensCreditStatus()
	out := map[string]any{
		"wallet_gb": st.WalletGB,
		"level":     st.Level,
		"buy_url":   st.BuyURL,
		"packs":     p.kd.TokensSummaries(),
		"applies_to": "bridged transfers only; a direct connection never spends credit, " +
			"and with no credit bridged transfers use the free tier",
	}
	if st.Notice != "" {
		out["notice"] = st.Notice
	}
	if p.kd.IsRunning() {
		b := p.kd.BridgeInfo()
		out["session"] = map[string]any{
			"mode":       p.kd.ConnectionMode,
			"paid":       b.Paid,
			"contention": b.Contention,
			"session_gb": b.SessionGB,
		}
	}
	return out, nil
}

// buyCredit hands back a checkout link. Payment is taken by Stripe on that
// page. While this server keeps running it watches for the resulting code and
// adds it on its own, then a tokens_added event fires, which kd_wait_event
// reports. A human has to open the link and pay; no tool here takes money.
func (p *peer) buyCredit() (any, *toolError) {
	return map[string]any{
		"url": p.kd.TokensBuyStart(),
		"next": "open the link and pay; the pack is added to this peer while this server runs, " +
			"and kd_wait_event reports tokens_added when it lands",
		"note": "payment is handled by Stripe on that page",
	}, nil
}

func (p *peer) addCredit(args json.RawMessage) (any, *toolError) {
	var a struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(args, &a)
	code := strings.TrimSpace(a.Code)
	if code == "" {
		return nil, toolErr(codeInvalidArgument, "code is required")
	}
	gb, err := p.kd.TokensAdd(code)
	if err != nil {
		return nil, classify(err)
	}
	return map[string]any{
		"added_gb":  gb,
		"wallet_gb": p.kd.TokensCreditStatus().WalletGB,
		"warning": "a pack code works like cash: whoever holds it can spend it, " +
			"and it cannot be recovered once lost",
	}, nil
}

// --- Saved peers ---

// errNoAddressBook is what every saved-peer tool returns under KD_INCOGNITO.
// Incognito keeps nothing on disk, which is the point of it, so there is
// nowhere to save a peer and no identity that would still be recognised next
// time.
func (p *peer) addressBook() *toolError {
	if p.kd.AddressBook == nil {
		return toolErr(codeUnsupported,
			"saved peers need a stored identity; this server runs with KD_INCOGNITO set")
	}
	return nil
}

func (p *peer) peers() (any, *toolError) {
	if terr := p.addressBook(); terr != nil {
		return nil, terr
	}
	list := p.kd.AddressBook.List()
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		e := map[string]any{
			"name":   c.Name,
			"code":   c.Fingerprint,
			"online": p.kd.CheckContactPresence(c.Fingerprint),
		}
		if !c.LastSeen.IsZero() {
			e["last_seen"] = c.LastSeen.Format(time.RFC3339)
		}
		out = append(out, e)
	}
	return map[string]any{
		"peers": out,
		"note":  "connect to one of these with kd_connect_peer; no code exchange is needed",
	}, nil
}

func (p *peer) savePeer(args json.RawMessage) (any, *toolError) {
	if terr := p.addressBook(); terr != nil {
		return nil, terr
	}
	var a struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &a)
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return nil, toolErr(codeInvalidArgument, "name is required")
	}
	if !p.kd.IsRunning() {
		return nil, toolErr(codeNotConnected, "there is no connected peer to save")
	}
	if err := p.kd.SaveCurrentPeerAsContact(name); err != nil {
		return nil, classify(err)
	}
	pfp, _ := p.kd.GetPeerFingerprint()
	return map[string]any{
		"saved": name,
		"code":  pfp,
		"next":  "kd_connect_peer with this name reconnects later without exchanging codes",
	}, nil
}

// connectPeer reconnects to a peer already in the address book. Both sides
// keep each other's code from the first pairing, so this needs nothing from
// the human.
func (p *peer) connectPeer(args json.RawMessage) (any, *toolError) {
	if terr := p.addressBook(); terr != nil {
		return nil, terr
	}
	var a struct {
		Name     string `json:"name"`
		TimeoutS int    `json:"timeout_s"`
	}
	_ = json.Unmarshal(args, &a)
	want := strings.TrimSpace(a.Name)
	if want == "" {
		return nil, toolErr(codeInvalidArgument, "name is required")
	}
	if p.kd.IsRunning() {
		return p.status(), nil
	}

	// Accept either the saved name or the code itself.
	code := ""
	for _, c := range p.kd.AddressBook.List() {
		if c.Name == want || c.Fingerprint == want {
			code = c.Fingerprint
			break
		}
	}
	if code == "" {
		return nil, toolErr(codeNotFound, "no saved peer called %q; list them with kd_peers", want)
	}

	if terr := p.startConnect(code); terr != nil {
		return nil, terr
	}

	timeout := defaultJoinTimeout
	if a.TimeoutS > 0 {
		timeout = time.Duration(a.TimeoutS) * time.Second
	}
	deadline := time.Now().Add(timeout)
	for !p.kd.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
	return p.status(), nil
}

// maxWaitEvent bounds a single wait so the call cannot outlive an MCP client's
// own tool-call timeout.
const maxWaitEvent = 120 * time.Second

// waitEvent blocks until the engine reports something or the wait runs out.
// This is how a headless agent follows a session without calling kd_status in
// a loop: call it, get woken when something happens, act, call it again.
func (p *peer) waitEvent(args json.RawMessage) (any, *toolError) {
	var a struct {
		TimeoutS int `json:"timeout_s"`
	}
	_ = json.Unmarshal(args, &a)

	wait := 30 * time.Second
	if a.TimeoutS > 0 {
		wait = time.Duration(a.TimeoutS) * time.Second
	}
	if wait > maxWaitEvent {
		wait = maxWaitEvent
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case evt := <-p.events:
		name, detail := evt, ""
		// Several engine events use a "prefix:value" shape. Split it so the
		// agent can match on the name and read the detail separately.
		if i := strings.IndexByte(evt, ':'); i > 0 {
			name, detail = evt[:i], evt[i+1:]
		}
		out := map[string]any{
			"event":     name,
			"raw":       evt,
			"connected": p.kd.IsRunning(),
		}
		if detail != "" {
			out["detail"] = detail
		}
		return out, nil
	case <-timer.C:
		return map[string]any{
			"event":     "",
			"timed_out": true,
			"connected": p.kd.IsRunning(),
			"note":      "nothing happened within the wait; call again to keep waiting",
		}, nil
	}
}

func (p *peer) disconnect() (any, *toolError) {
	p.mu.Lock()
	p.connecting = false
	p.mu.Unlock()

	p.kd.NotifyDisconnect()
	_ = p.kd.UnmountFilesystem()
	p.kd.Stop()
	return map[string]any{"ok": true, "my_code": p.code()}, nil
}

func (p *peer) shutdown() {
	p.kd.NotifyDisconnect()
	_ = p.kd.UnmountFilesystem()
	p.kd.Shutdown()
}

// classify maps an engine error to a stable tool error.
func classify(err error) *toolError {
	switch {
	case errors.Is(err, common.ErrInvalidSession),
		errors.Is(err, common.ErrSessionNotEstablished),
		errors.Is(err, common.ErrNilPointer):
		return toolErr(codeNotConnected, "%v", err)
	case errors.Is(err, common.ErrTimeoutReached):
		return toolErr(codeTimeout, "%v", err)
	case errors.Is(err, common.ErrNotFound):
		return toolErr(codeNotFound, "%v", err)
	case errors.Is(err, common.ErrAlreadyRunning):
		return toolErr(codeBusy, "%v", err)
	case errors.Is(err, common.ErrFingerprintMismatch),
		errors.Is(err, common.ErrIdenticalFingerprints):
		return toolErr(codeRefused, "%v", err)
	}
	return &toolError{Code: classifyText(err.Error()), Message: err.Error()}
}

func classifyText(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "no such file"), strings.Contains(m, "not found"),
		strings.Contains(m, "not shared"), strings.Contains(m, "enoent"):
		return codeNotFound
	case strings.Contains(m, "timeout"), strings.Contains(m, "deadline exceeded"):
		return codeTimeout
	case strings.Contains(m, "already in progress"), strings.Contains(m, "already running"):
		return codeBusy
	case strings.Contains(m, "invalid session"), strings.Contains(m, "not connected"):
		return codeNotConnected
	case strings.Contains(m, "mismatch"), strings.Contains(m, "refused"):
		return codeRefused
	case strings.Contains(m, "invalid"), strings.Contains(m, "usage:"):
		return codeInvalidArgument
	}
	return codeInternal
}

// fuseInstallHint names the one command that turns copy mode into on-demand
// mount mode on this platform.
func fuseInstallHint() string {
	switch runtimeGOOS() {
	case "darwin":
		return "Install macFUSE to enable the on-demand mount: brew install --cask macfuse"
	case "linux":
		return "Install libfuse to enable the on-demand mount: apt install fuse3 (or the distro equivalent)"
	case "windows":
		return "Install WinFsp 2.2 or newer to enable the on-demand mount: https://winfsp.dev"
	}
	return "Install a FUSE provider to enable the on-demand mount."
}
