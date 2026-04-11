// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

/*
#include <stdint.h>
*/
import "C"

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

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

// Error reporting: thread-safe last error string.
var (
	lastErrorMu sync.Mutex
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

// Event queue: bounded channel for UI events.
var eventChan = make(chan string, 64)

//export KD_Initialize
func KD_Initialize(relayURL *C.char, inbound, outbound C.int, toMount, toSave *C.char, useFUSE C.int, prefetchOnOpen C.int, pushOnWrite C.int) C.int {
	// Load config defaults, then override with explicit parameters from Rust UI.
	cfg, _ := config.Load()

	r := C.GoString(relayURL)
	m := C.GoString(toMount)
	s := C.GoString(toSave)
	fuse := useFUSE != 0
	prefetch := prefetchOnOpen != 0
	push := pushOnWrite != 0

	// Fill in defaults for empty values.
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

	// Setup log file from config if available.
	var logWriter = os.Stdout
	if cfg.LogFile != "" {
		if f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			logWriter = f
		}
	}
	handler := slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "rustbridge")

	// Ensure directories exist.
	_ = config.EnsureDirectories(cfg)

	ctx, c := context.WithCancel(context.Background())

	instance, err := common.NewKeibiDrop(ctx, logger, fuse, parsed, int(inbound), int(outbound), m, s, prefetch, push)
	if err != nil {
		logger.Error("Failed to create KeibiDrop instance", "error", err)
		setLastError(err)
		return -2
	}
	kd = instance
	kd.Cancel = c
	kd.OnEvent = pushEvent
	go kd.Run()
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

//export KD_UnmountFilesystem
func KD_UnmountFilesystem() {
	if kd != nil {
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
	// Map: 0=healthy→2, 1=degraded→3, 2=disconnected→0
	switch kd.HealthMonitor.Health() {
	case 0:
		return 2 // connected
	case 1:
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

	// Wire reconnect manager events.
	if kd.ReconnectManager != nil {
		origOnReconnecting := kd.ReconnectManager.OnReconnecting
		kd.ReconnectManager.OnReconnecting = func() {
			if origOnReconnecting != nil {
				origOnReconnecting()
			}
			pushEvent("reconnecting:")
		}

		origOnReconnected := kd.ReconnectManager.OnReconnected
		kd.ReconnectManager.OnReconnected = func() {
			if origOnReconnected != nil {
				origOnReconnected()
			}
			pushEvent("reconnected:")
		}

		origOnGaveUp := kd.ReconnectManager.OnGaveUp
		kd.ReconnectManager.OnGaveUp = func() {
			if origOnGaveUp != nil {
				origOnGaveUp()
			}
			pushEvent("gave_up:")
		}
	}
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

//export KD_GetVersion
func KD_GetVersion() *C.char {
	return C.CString(common.Version + " (" + common.CommitHash + ")")
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

func main() {}
