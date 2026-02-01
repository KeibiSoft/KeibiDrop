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
	"log/slog"
	"net/url"
	"os"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

var kd *common.KeibiDrop
var cancel context.CancelFunc

//export KD_Initialize
func KD_Initialize(relayURL *C.char, inbound, outbound C.int, toMount, toSave *C.char, useFUSE C.int, prefetchOnOpen C.int, pushOnWrite C.int) C.int {
	r := C.GoString(relayURL)
	m := C.GoString(toMount)
	s := C.GoString(toSave)
	fuse := useFUSE != 0
	prefetch := prefetchOnOpen != 0
	push := pushOnWrite != 0

	parsed, err := url.Parse(r)
	if err != nil {
		return -1
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "rustbridge")

	ctx, c := context.WithCancel(context.Background())
	cancel = c

	instance, err := common.NewKeibiDrop(ctx, logger, fuse, parsed, int(inbound), int(outbound), m, s, prefetch, push)
	if err != nil {
		logger.Error("Failed to create KeibiDrop instance", "error", err)
		return -2
	}
	kd = instance
	go kd.Run()
	return 0
}

//export KD_CreateRoom
func KD_CreateRoom() C.int {
	if kd == nil {
		return -1
	}
	if err := kd.CreateRoom(); err != nil {
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

//export KD_Stop
func KD_Stop() {
	if cancel != nil {
		cancel()
	}
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
	remote, _ := kd.ListFiles()
	return C.int(len(remote))
}

//export KD_GetFileName
func KD_GetFileName(index C.int) *C.char {
	if kd == nil {
		return nil
	}
	remote, _ := kd.ListFiles()
	if int(index) >= len(remote) {
		return nil
	}
	return C.CString(remote[index])
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
	remote, _ := kd.ListFiles()
	if int(index) >= len(remote) {
		return -2
	}
	if err := kd.PullFile(remote[index], C.GoString(localPath)); err != nil {
		return -3
	}
	return 0
}

func main() {}
