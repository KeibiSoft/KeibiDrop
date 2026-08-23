// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

/*
#include <stdint.h>
#ifndef _WIN32
#include <signal.h>
static void ignore_sigpipe(void) {
    signal(SIGPIPE, SIG_IGN);
}
#else
static void ignore_sigpipe(void) {}
#endif
*/
import "C"

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/discovery"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

func init() {
	C.ignore_sigpipe()
}

var disc *discovery.Service
var discMu sync.Mutex

// sortedRemoteKeys returns remote file map keys in sorted order.
func sortedRemoteKeys() []string {
	keys := make([]string, 0, len(kd.SyncTracker.RemoteFiles))
	for k := range kd.SyncTracker.RemoteFiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedLocalKeys returns local file map keys in sorted order.
func sortedLocalKeys() []string {
	keys := make([]string, 0, len(kd.SyncTracker.LocalFiles))
	for k := range kd.SyncTracker.LocalFiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var kd *common.KeibiDrop

// Last error string. The mutex makes access thread-safe.
var (
	lastErrorMu  sync.Mutex
	lastErrorMsg string
)

func setLastError(err error) {
	lastErrorMu.Lock()
	defer lastErrorMu.Unlock()
	if err != nil {
		lastErrorMsg = err.Error()
	} else {
		lastErrorMsg = ""
	}
}

// eventChan is a bounded queue for UI events.
var eventChan = make(chan string, 64)

//export KD_Initialize
func KD_Initialize(relayURL *C.char, inbound, outbound C.int, toMount, toSave *C.char, useFUSE C.int, prefetchOnOpen C.int, pushOnWrite C.int) C.int {
	// Load config defaults, then override with explicit parameters from Rust UI.
	cfg, _ := config.Load()

	r := C.GoString(relayURL)
	m := C.GoString(toMount)
	s := C.GoString(toSave)
	fuse := useFUSE != 0
	// The two collab-sync flags are tri-state: negative means use the config value.
	// 0 or 1 from older callers still wins, so the C ABI is unchanged.
	prefetch := cfg.PrefetchOnOpen
	if prefetchOnOpen >= 0 {
		prefetch = prefetchOnOpen != 0
	}
	push := cfg.PushOnWrite
	if pushOnWrite >= 0 {
		push = pushOnWrite != 0
	}

	if r == "" {
		r = cfg.Relay
	}
	if m == "" {
		m = cfg.MountPath
	}
	if s == "" {
		s = cfg.SavePath
	}
	if int(inbound) == 0 {
		inbound = C.int(cfg.InboundPort)
	}
	if int(outbound) == 0 {
		outbound = C.int(cfg.OutboundPort)
	}

	parsed, err := url.Parse(r)
	if err != nil {
		setLastError(err)
		return -1
	}

	var logWriter = os.Stdout
	if cfg.LogFile != "" {
		if f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			logWriter = f
		}
	}
	handler := slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "rustbridge")

	_ = config.EnsureDirectories(cfg)

	ctx, c := context.WithCancel(context.Background())

	instance, err := common.NewKeibiDrop(ctx, logger, fuse, parsed, int(inbound), int(outbound), m, s, prefetch, push)
	if err != nil {
		c()
		logger.Error("Failed to create KeibiDrop instance", "error", err)
		setLastError(err)
		return -2
	}
	kd = instance
	kd.Cancel = c
	kd.OnEvent = pushEvent

	if !cfg.Incognito {
		opts := common.EnableOpts{
			PassphraseProtect: cfg.PassphraseProtect,
		}
		if cfg.PassphraseProtect && os.Getenv("KD_PASSPHRASE_FROM_STDIN") == "1" {
			opts.PassphraseProvider = readPassphraseFromStdin
		} else if cfg.PassphraseProtect {
			// The stdin sentinel is not set, so disable tier 2 for this session.
			// The Rust UI has no passphrase FFI yet.
			opts.PassphraseProtect = false
		}
		if err := kd.EnablePersistentIdentity(config.ConfigDir(), opts); err != nil {
			logger.Warn("Failed to enable persistent identity, using ephemeral", "error", err)
			pushEvent("identity_error:" + err.Error())
		}
	} else {
		kd.Incognito = true
	}

	if kd.BridgeAddr == "" && cfg.BridgeAddr != "" {
		kd.BridgeAddr = cfg.BridgeAddr
	}

	// Learn reachability now, so the first connect does not pay the direct-accept stall
	// while discovering that nothing can reach this listener.
	go kd.ProbeInboundReachability(ctx)
	if cfg.StrictMode {
		kd.StrictMode = true
	}
	kd.AutoCache = cfg.LiveCollab // live_collab sets macFUSE auto_cache for same-size live edits on macOS.
	kd.PrefetchAutoMB = cfg.PrefetchAutoMB
	kd.ReadAheadWindowMB = cfg.ReadAheadWindowMB
	kd.ScanSharedOnStart = cfg.ScanSharedOnStart
	kd.ShareReadOnly = cfg.ShareReadOnly
	kd.MountReadOnly = cfg.MountReadOnly
	kd.PreserveMetadata = cfg.PreserveMetadata
	for _, warn := range cfg.Warnings() {
		logger.Warn("config flag note", "note", warn)
	}

	if !kd.Incognito && kd.Identity != nil && kd.AddressBook != nil && kd.AddressBook.Count() > 0 {
		go kd.StartPresenceHeartbeat(ctx)
	}

	go kd.Run()

	if cfg.AutoConnectPeer != "" {
		kd.AutoConnectPeer = cfg.AutoConnectPeer
		if err := kd.StartAutoConnect(ctx); err != nil {
			pushEvent("auto_connect_error:" + err.Error())
		}
	}
	return 0
}

//export KD_CreateRoom
func KD_CreateRoom() C.int {
	if kd == nil {
		return -1
	}
	if err := kd.CreateRoom(); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_JoinRoom
func KD_JoinRoom() C.int {
	if kd == nil {
		return -1
	}
	if err := kd.JoinRoom(); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_Connect
func KD_Connect() C.int {
	if kd == nil {
		return -1
	}
	if err := kd.Connect(); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

// KD_DecideLocalRole reports whether this peer creates (1) or joins (0) in local mode.
// CLI and mobile use the same rule, so all frontends resolve name collisions the same way.
//
//export KD_DecideLocalRole
func KD_DecideLocalRole(myName, peerName, peerAddr *C.char) C.int {
	if common.DecideLocalRole(C.GoString(myName), C.GoString(peerName), C.GoString(peerAddr)) {
		return 1
	}
	return 0
}

//export KD_AddPeerFingerprint
func KD_AddPeerFingerprint(fp *C.char) C.int {
	if kd == nil {
		return -1
	}
	if err := kd.AddPeerFingerprint(C.GoString(fp)); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_GetPeerFingerprint
func KD_GetPeerFingerprint() *C.char {
	if kd == nil {
		return C.CString("")
	}
	fp, _ := kd.GetPeerFingerprint()
	return C.CString(fp)
}

//export KD_AddFile
func KD_AddFile(path *C.char) C.int {
	if kd == nil {
		return -1
	}
	if err := kd.AddFile(C.GoString(path)); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_AddFileAs
func KD_AddFileAs(localPath *C.char, remoteName *C.char) C.int {
	if kd == nil {
		return -1
	}
	if err := kd.AddFileAs(C.GoString(localPath), C.GoString(remoteName)); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_UnshareFile
func KD_UnshareFile(name *C.char) C.int {
	if kd == nil {
		setLastError(fmt.Errorf("not initialized"))
		return -1
	}
	if err := kd.UnshareFile(C.GoString(name)); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

//export KD_ListFiles
func KD_ListFiles() *C.char {
	if kd == nil {
		return C.CString("")
	}
	remote, local := kd.ListFiles()
	out := ""
	for _, f := range remote {
		out += "remote: " + f + "\n"
	}
	for _, f := range local {
		out += "local: " + f + "\n"
	}
	return C.CString(out)
}

//export KD_PullFile
func KD_PullFile(remote, local *C.char) C.int {
	if kd == nil {
		return -1
	}
	if err := kd.PullFile(C.GoString(remote), C.GoString(local)); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_Fingerprint
func KD_Fingerprint() *C.char {
	if kd == nil {
		return C.CString("")
	}
	fp, _ := kd.ExportFingerprint()
	return C.CString(fp)
}

//export KD_PrepareDisconnect
func KD_PrepareDisconnect() {
	if kd != nil {
		kd.StopConnectionResilience()
	}
}

//export KD_UnmountFilesystem
func KD_UnmountFilesystem() {
	if kd != nil {
		kd.StopConnectionResilience()
		_ = kd.UnmountFilesystem()
	}
}

//export KD_Disconnect
func KD_Disconnect() {
	if kd != nil {
		kd.NotifyDisconnect()
		kd.Stop()
	}
}

//export KD_Stop
func KD_Stop() {
	if kd != nil {
		kd.Shutdown()
	}
}

//export KD_CancelDownload
func KD_CancelDownload(remoteName *C.char) C.int {
	if kd == nil {
		setLastError(fmt.Errorf("not initialized"))
		return -1
	}
	name := C.GoString(remoteName)
	if err := kd.CancelDownload(name); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

//export KD_GetDownloadProgress
func KD_GetDownloadProgress(remoteName *C.char) C.int {
	if kd == nil {
		return -1
	}
	name := C.GoString(remoteName)
	p := kd.GetDownloadProgress(name)
	if p < 0 {
		return -1
	}
	return C.int(p * 100) // 0-100 percentage
}

//export KD_PrintBanner
func KD_PrintBanner() {
	common.PrintBanner()
}

//export KD_GetFileCount
func KD_GetFileCount() C.int {
	if kd == nil {
		return 0
	}
	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()
	return C.int(len(kd.SyncTracker.RemoteFiles))
}

//export KD_GetFileName
func KD_GetFileName(index C.int) *C.char {
	if kd == nil {
		return nil
	}
	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()
	keys := sortedRemoteKeys()
	if int(index) >= len(keys) {
		return nil
	}
	return C.CString(keys[int(index)])
}

//export KD_GetLocalFileCount
func KD_GetLocalFileCount() C.int {
	if kd == nil {
		return 0
	}
	kd.SyncTracker.PruneStaleLocalFiles()
	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()
	return C.int(len(kd.SyncTracker.LocalFiles))
}

//export KD_GetLocalFileName
func KD_GetLocalFileName(index C.int) *C.char {
	if kd == nil {
		return nil
	}
	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()
	keys := sortedLocalKeys()
	if int(index) >= len(keys) {
		return nil
	}
	return C.CString(keys[int(index)])
}

//export KD_GetFileSize
func KD_GetFileSize(index C.int) C.long {
	if kd == nil {
		return 0
	}
	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()
	keys := sortedRemoteKeys()
	if int(index) >= len(keys) {
		return 0
	}
	f, ok := kd.SyncTracker.RemoteFiles[keys[int(index)]]
	if !ok {
		return 0
	}
	return C.long(f.Size)
}

//export KD_GetFileSizeByName
func KD_GetFileSizeByName(name *C.char) C.long {
	if kd == nil {
		return 0
	}
	goName := C.GoString(name)
	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()
	// Try exact key first, then with "/" prefix
	f, ok := kd.SyncTracker.RemoteFiles[goName]
	if !ok {
		f, ok = kd.SyncTracker.RemoteFiles["/"+goName]
	}
	if !ok {
		f, ok = kd.SyncTracker.RemoteFiles[strings.TrimPrefix(goName, "/")]
	}
	if !ok {
		return 0
	}
	return C.long(f.Size)
}

//export KD_GetLocalFileRealPath
func KD_GetLocalFileRealPath(name *C.char) *C.char {
	if kd == nil {
		return nil
	}
	goName := C.GoString(name)
	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()
	f, ok := kd.SyncTracker.LocalFiles[goName]
	if !ok {
		return nil
	}
	return C.CString(f.RealPathOfFile)
}

//export KD_GetConnectionStatus
func KD_GetConnectionStatus() C.int {
	if kd == nil {
		return 0
	}
	if kd.HealthMonitor == nil {
		return 2 // no monitor = assume connected
	}
	switch kd.HealthMonitor.Health() {
	case session.HealthHealthy:
		return 2 // connected
	case session.HealthDegraded:
		return 3 // reconnecting
	default:
		return 0 // disconnected
	}
}

//export KD_SaveFileAt
func KD_SaveFileAt(index C.int, localPath *C.char) C.int {
	if kd == nil {
		return -1
	}
	kd.SyncTracker.RemoteFilesMu.RLock()
	keys := sortedRemoteKeys()
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	if int(index) >= len(keys) {
		return -2
	}
	if err := kd.PullFile(keys[int(index)], C.GoString(localPath)); err != nil {
		setLastError(err)
		return -3
	}
	return 0
}

//export KD_SaveFileByName
func KD_SaveFileByName(name *C.char, localPath *C.char) C.int {
	if kd == nil {
		return -1
	}
	goName := C.GoString(name)
	// Try exact key, then with "/" prefix
	kd.SyncTracker.RemoteFilesMu.RLock()
	_, ok := kd.SyncTracker.RemoteFiles[goName]
	if !ok {
		if _, ok2 := kd.SyncTracker.RemoteFiles["/"+goName]; ok2 {
			goName = "/" + goName
		}
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	if err := kd.PullFile(goName, C.GoString(localPath)); err != nil {
		setLastError(err)
		return -3
	}
	return 0
}

//export KD_GetLastError
func KD_GetLastError() *C.char {
	lastErrorMu.Lock()
	defer lastErrorMu.Unlock()
	if lastErrorMsg == "" {
		return nil
	}
	return C.CString(lastErrorMsg)
}

//export KD_GetLastErrorAndClear
func KD_GetLastErrorAndClear() *C.char {
	lastErrorMu.Lock()
	defer lastErrorMu.Unlock()
	if lastErrorMsg == "" {
		return nil
	}
	msg := lastErrorMsg
	lastErrorMsg = ""
	return C.CString(msg)
}

//export KD_PollEvent
func KD_PollEvent() *C.char {
	select {
	case evt := <-eventChan:
		return C.CString(evt)
	default:
		return nil
	}
}

//export KD_SetupEventCallbacks
func KD_SetupEventCallbacks() {
	if kd == nil {
		return
	}

	// Wire health monitor events into the event channel.
	if kd.HealthMonitor != nil {
		origOnHealthChange := kd.HealthMonitor.OnHealthChange
		kd.HealthMonitor.OnHealthChange = func(old, new session.ConnectionHealth) {
			if origOnHealthChange != nil {
				origOnHealthChange(old, new)
			}
			pushEvent(fmt.Sprintf("health_changed:%s:%s", old.String(), new.String()))
		}
	}

	// wireReconnectEvents wires the reconnect manager events through kd.OnEvent.
}

func pushEvent(evt string) {
	select {
	case eventChan <- evt:
	default:
		// Channel full, drop oldest event.
		select {
		case <-eventChan:
		default:
		}
		eventChan <- evt
	}
}

//export KD_SetFUSEMode
func KD_SetFUSEMode(useFUSE C.int) {
	if kd != nil {
		kd.IsFUSE = useFUSE != 0
	}
}

//export KD_GetFUSEMode
func KD_GetFUSEMode() C.int {
	if kd != nil && kd.IsFUSE {
		return 1
	}
	return 0
}

//export KD_SetLocalMode
func KD_SetLocalMode(enabled C.int) {
	if kd != nil {
		kd.IsLocalMode = enabled != 0
	}
}

//export KD_GetLocalMode
func KD_GetLocalMode() C.int {
	if kd != nil && kd.IsLocalMode {
		return 1
	}
	return 0
}

//export KD_GetLinkLocalAddress
func KD_GetLinkLocalAddress() *C.char {
	if kd == nil {
		return C.CString("")
	}
	addr, err := common.GetLinkLocalAddress(kd.InboundPort())
	if err != nil {
		return C.CString("")
	}
	return C.CString(addr)
}

//export KD_SetPeerDirectAddress
func KD_SetPeerDirectAddress(addr *C.char) C.int {
	if kd == nil {
		return -1
	}
	if err := kd.SetPeerDirectAddress(C.GoString(addr)); err != nil {
		setLastError(err)
		return -2
	}
	return 0
}

//export KD_StartDiscovery
func KD_StartDiscovery() {
	discMu.Lock()
	defer discMu.Unlock()
	if disc != nil {
		return
	}
	port := 26001
	if kd != nil {
		port = kd.InboundPort()
		kd.IsLocalMode = true
	}
	disc = discovery.New(port, slog.Default())
	_ = disc.Start()
}

//export KD_StopDiscovery
func KD_StopDiscovery() {
	discMu.Lock()
	defer discMu.Unlock()
	if disc != nil {
		disc.Stop()
		disc = nil
	}
	if kd != nil {
		kd.IsLocalMode = false
	}
}

//export KD_GetDiscoveryName
func KD_GetDiscoveryName() *C.char {
	discMu.Lock()
	defer discMu.Unlock()
	if disc == nil {
		return C.CString("")
	}
	return C.CString(disc.Name())
}

//export KD_GetDiscoveredPeerCount
func KD_GetDiscoveredPeerCount() C.int {
	discMu.Lock()
	defer discMu.Unlock()
	if disc == nil {
		return 0
	}
	return C.int(len(disc.Peers()))
}

//export KD_GetDiscoveredPeerName
func KD_GetDiscoveredPeerName(i C.int) *C.char {
	discMu.Lock()
	defer discMu.Unlock()
	if disc == nil {
		return C.CString("")
	}
	peers := disc.Peers()
	if int(i) < 0 || int(i) >= len(peers) {
		return C.CString("")
	}
	return C.CString(peers[int(i)].Name)
}

//export KD_GetDiscoveredPeerAddr
func KD_GetDiscoveredPeerAddr(i C.int) *C.char {
	discMu.Lock()
	defer discMu.Unlock()
	if disc == nil {
		return C.CString("")
	}
	peers := disc.Peers()
	if int(i) < 0 || int(i) >= len(peers) {
		return C.CString("")
	}
	return C.CString(peers[int(i)].Addr)
}

//export KD_ClearDiscoveredPeers
func KD_ClearDiscoveredPeers() {
	discMu.Lock()
	defer discMu.Unlock()
	if disc != nil {
		disc.ClearPeers()
	}
}

//export KD_GetConnectionMode
func KD_GetConnectionMode() *C.char {
	if kd == nil {
		return C.CString("")
	}
	return C.CString(kd.ConnectionMode)
}

// KD_GetQUICLane reports the QUIC control lane as "up" or "down (<reason>)". On a bridge
// session the lane is what keeps seeks interactive, so its state must be readable.
//
//export KD_GetQUICLane
func KD_GetQUICLane() *C.char {
	if kd == nil {
		return C.CString("")
	}
	return C.CString(kd.QUICLaneStatus())
}

//export KD_SetStrictMode
func KD_SetStrictMode(enabled C.int) {
	if kd != nil {
		kd.StrictMode = enabled != 0
	}
}

//export KD_GetVersion
func KD_GetVersion() *C.char {
	return C.CString(common.Version + " (" + common.CommitHash + ")")
}

// KD_CheckUpdate fetches the newest released version from keibidrop.com
// and returns it when this build is older. Empty string = current build,
// check disabled in config, or site unreachable. The request carries
// nothing about this install. Blocks up to 5s: call off the UI thread.
//
//export KD_CheckUpdate
func KD_CheckUpdate() *C.char {
	cfg, _ := config.Load()
	if !cfg.UpdateCheck {
		return C.CString("")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://keibidrop.com/latest-version.txt")
	if err != nil {
		return C.CString("")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return C.CString("")
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return C.CString("")
	}
	latest := strings.TrimSpace(string(buf))
	if versionNewer(common.Version, latest) {
		return C.CString(latest)
	}
	return C.CString("")
}

// versionNewer reports whether latest is strictly newer than current.
// Versions are x.y.z with an optional v prefix; git-describe suffixes
// (-N-gHASH) are cut. Anything unparsable compares as not newer.
func versionNewer(current, latest string) bool {
	cur := versionParts(current)
	lat := versionParts(latest)
	if cur == nil || lat == nil {
		return false
	}
	for i := range 3 {
		if lat[i] != cur[i] {
			return lat[i] > cur[i]
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ ("); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

//export KD_GetLogPath
func KD_GetLogPath() *C.char {
	cfg, _ := config.Load()
	return C.CString(cfg.LogFile)
}

//export KD_GetConfigPath
func KD_GetConfigPath() *C.char {
	return C.CString(config.ConfigPath())
}

//export KD_GetConfig
func KD_GetConfig() *C.char {
	cfg, _ := config.Load()
	return C.CString(fmt.Sprintf(
		"relay=%s\nsave_path=%s\nmount_path=%s\nlog_file=%s\ninbound_port=%d\noutbound_port=%d\nbridge_addr=%s\nno_fuse=%v\nstrict_mode=%v\nscan_shared_on_start=%v\nauto_connect_peer=%s\nshare_read_only=%v\nmount_read_only=%v\npreserve_metadata=%v",
		cfg.Relay, cfg.SavePath, cfg.MountPath, cfg.LogFile,
		cfg.InboundPort, cfg.OutboundPort, cfg.BridgeAddr,
		cfg.NoFUSE, cfg.StrictMode, cfg.ScanSharedOnStart, cfg.AutoConnectPeer,
		cfg.ShareReadOnly, cfg.MountReadOnly, cfg.PreserveMetadata,
	))
}

//export KD_TokensAdd
func KD_TokensAdd(code *C.char) *C.char {
	if kd == nil {
		return C.CString("ok=0\nerr=core not started")
	}
	gb, err := kd.TokensAdd(C.GoString(code))
	if err != nil {
		return C.CString("ok=0\nerr=" + strings.ReplaceAll(err.Error(), "\n", " "))
	}
	return C.CString(fmt.Sprintf("ok=1\ngb=%.0f", gb))
}

//export KD_TokensStatus
func KD_TokensStatus() *C.char {
	if kd == nil {
		return C.CString("")
	}
	bi := kd.BridgeInfo()
	chains := 0
	for _, c := range kd.TokensSummaries() {
		if !c.Dead && c.UnitsLeft > 0 {
			chains++
		}
	}
	credit := kd.TokensCreditStatus()
	return C.CString(fmt.Sprintf(
		"wallet_gb=%.1f\nchains=%d\nvia=%s\npaid=%v\nbusy=%v\nnotice=%s\nbuy_url=%s\ncredit_level=%s\ncredit_notice=%s",
		bi.WalletGB, chains, bi.Via, bi.Paid, bi.Busy || bi.Contention,
		strings.ReplaceAll(bi.Notice, "\n", " "), common.TokensBuyURL,
		credit.Level, strings.ReplaceAll(credit.Notice, "\n", " ")))
}

// KD_TokensBuyStart returns the buy URL carrying a fresh claim ref and
// starts the background poll that adds the purchased code to the wallet
// (a tokens_added event fires when it lands).
//
//export KD_TokensBuyStart
func KD_TokensBuyStart() *C.char {
	if kd == nil {
		return C.CString("url=" + common.TokensBuyURL)
	}
	return C.CString("url=" + kd.TokensBuyStart())
}

//export KD_SaveConfig
func KD_SaveConfig(relay, savePath, mountPath *C.char) C.int {
	cfg, _ := config.Load()
	if r := C.GoString(relay); r != "" {
		cfg.Relay = r
	}
	if s := C.GoString(savePath); s != "" {
		cfg.SavePath = s
	}
	if m := C.GoString(mountPath); m != "" {
		cfg.MountPath = m
	}
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

// KD_SetAutoConnectPeer persists the connect-at-startup contact (name or
// fingerprint, empty clears) and applies it to the running instance. The
// watchdog arms on the next app start; an already-armed watchdog keeps its
// original target for this session.
//
//export KD_SetAutoConnectPeer
func KD_SetAutoConnectPeer(peer *C.char) C.int {
	cfg, _ := config.Load()
	cfg.AutoConnectPeer = C.GoString(peer)
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return -1
	}
	if kd != nil {
		kd.AutoConnectPeer = cfg.AutoConnectPeer
	}
	return 0
}

// KD_SetNoFUSE persists the FUSE-off preference. No live mirror:
// KD_SetFUSEMode owns the running mode.
//
//export KD_SetNoFUSE
func KD_SetNoFUSE(v C.int) C.int {
	cfg, _ := config.Load()
	cfg.NoFUSE = v != 0
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

// KD_SetShareReadOnly persists share_read_only and applies it to the
// running instance. The gRPC service snapshots it per connect.
//
//export KD_SetShareReadOnly
func KD_SetShareReadOnly(v C.int) C.int {
	cfg, _ := config.Load()
	cfg.ShareReadOnly = v != 0
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return -1
	}
	if kd != nil {
		kd.ShareReadOnly = cfg.ShareReadOnly
	}
	return 0
}

// KD_SetMountReadOnly persists mount_read_only and applies it to the
// running instance. The FUSE root reads it at the next mount.
//
//export KD_SetMountReadOnly
func KD_SetMountReadOnly(v C.int) C.int {
	cfg, _ := config.Load()
	cfg.MountReadOnly = v != 0
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return -1
	}
	if kd != nil {
		kd.MountReadOnly = cfg.MountReadOnly
	}
	return 0
}

// KD_SetPreserveMetadata persists preserve_metadata and applies it to
// the running instance. Files that finish saving after the change use
// the new value.
//
//export KD_SetPreserveMetadata
func KD_SetPreserveMetadata(v C.int) C.int {
	cfg, _ := config.Load()
	cfg.PreserveMetadata = v != 0
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return -1
	}
	if kd != nil {
		kd.PreserveMetadata = cfg.PreserveMetadata
	}
	return 0
}

//export KD_SanitizeLogs
func KD_SanitizeLogs(destPath *C.char) C.int {
	cfg, _ := config.Load()
	logPath := cfg.LogFile
	if logPath == "" {
		setLastError(fmt.Errorf("no log file configured"))
		return -1
	}
	dest := C.GoString(destPath)
	if err := common.SanitizeLogsToFile(logPath, dest); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

// readPassphraseFromStdin reads one stdin line as the passphrase when KD_PASSPHRASE_FROM_STDIN=1.
// A line reader keeps spaces; fmt.Fscan would truncate at the first whitespace.
func readPassphraseFromStdin() (string, error) {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read passphrase from stdin: %w", err)
	}
	pp := strings.TrimRight(line, "\r\n")
	if pp == "" {
		return "", fmt.Errorf("empty passphrase from stdin")
	}
	return pp, nil
}

// ========== IDENTITY & CONTACTS ==========

//export KD_IsIncognito
func KD_IsIncognito() C.int {
	if kd == nil || kd.Incognito {
		return 1
	}
	return 0
}

//export KD_SetIncognito
func KD_SetIncognito(v C.int) *C.char {
	if kd == nil {
		return C.CString("")
	}
	newFP, err := kd.ToggleIncognito(v != 0, config.ConfigDir())
	if err != nil {
		setLastError(err)
	}
	if newFP == "" {
		ptr := KD_Fingerprint()
		return ptr
	}
	return C.CString(newFP)
}

//export KD_SetPassphraseProtect
func KD_SetPassphraseProtect(v C.int) *C.char {
	cfg, _ := config.Load()
	cfg.PassphraseProtect = (v != 0)
	if err := config.Save(cfg); err != nil {
		setLastError(err)
		return C.CString("")
	}
	return C.CString("passphrase-protect toggled")
}

//export KD_IsPeerAlreadyContact
func KD_IsPeerAlreadyContact() C.int {
	if kd == nil || kd.AddressBook == nil {
		return 0
	}
	fp, err := kd.GetPeerFingerprint()
	if err != nil || fp == "" || fp == "TOFU" {
		return 0
	}
	if kd.AddressBook.Lookup(fp) != nil {
		return 1
	}
	return 0
}

//export KD_IsPeerPersistent
func KD_IsPeerPersistent() C.int {
	if kd == nil || !kd.IsPeerPersistent() {
		return 0
	}
	return 1
}

//export KD_GetContactCount
func KD_GetContactCount() C.int {
	if kd == nil || kd.AddressBook == nil {
		return 0
	}
	return C.int(kd.AddressBook.Count())
}

//export KD_GetContactName
func KD_GetContactName(index C.int) *C.char {
	if kd == nil || kd.AddressBook == nil {
		return C.CString("")
	}
	contacts := kd.AddressBook.List()
	i := int(index)
	if i < 0 || i >= len(contacts) {
		return C.CString("")
	}
	return C.CString(contacts[i].Name)
}

//export KD_GetContactFingerprint
func KD_GetContactFingerprint(index C.int) *C.char {
	if kd == nil || kd.AddressBook == nil {
		return C.CString("")
	}
	contacts := kd.AddressBook.List()
	i := int(index)
	if i < 0 || i >= len(contacts) {
		return C.CString("")
	}
	return C.CString(contacts[i].Fingerprint)
}

//export KD_AddContact
func KD_AddContact(name, fingerprint *C.char) C.int {
	if kd == nil || kd.AddressBook == nil {
		setLastError(fmt.Errorf("no address book"))
		return -1
	}
	if err := kd.AddressBook.Add(C.GoString(name), C.GoString(fingerprint)); err != nil {
		setLastError(err)
		return -1
	}
	if err := kd.AddressBook.Save(); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

//export KD_RemoveContact
func KD_RemoveContact(fingerprint *C.char) C.int {
	if kd == nil || kd.AddressBook == nil {
		setLastError(fmt.Errorf("no address book"))
		return -1
	}
	if err := kd.AddressBook.Remove(C.GoString(fingerprint)); err != nil {
		setLastError(err)
		return -1
	}
	if err := kd.AddressBook.Save(); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

//export KD_ConnectToContact
func KD_ConnectToContact(fingerprint *C.char) C.int {
	if kd == nil {
		setLastError(fmt.Errorf("not initialized"))
		return -1
	}
	if err := kd.ConnectToContact(C.GoString(fingerprint)); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

//export KD_GetContactOnline
func KD_GetContactOnline(index C.int) C.int {
	if kd == nil || kd.AddressBook == nil {
		return 0
	}
	contacts := kd.AddressBook.List()
	i := int(index)
	if i < 0 || i >= len(contacts) {
		return 0
	}
	if kd.CheckContactPresence(contacts[i].Fingerprint) {
		return 1
	}
	return 0
}

//export KD_SaveCurrentPeerAsContact
func KD_SaveCurrentPeerAsContact(name *C.char) C.int {
	if kd == nil {
		setLastError(fmt.Errorf("not initialized"))
		return -1
	}
	if err := kd.SaveCurrentPeerAsContact(C.GoString(name)); err != nil {
		setLastError(err)
		return -1
	}
	return 0
}

func main() {}
