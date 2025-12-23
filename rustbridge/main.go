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
func KD_Initialize(relayURL *C.char, inbound, outbound C.int, toMount, toSave *C.char, useFUSE C.int) C.int {
	r := C.GoString(relayURL)
	m := C.GoString(toMount)
	s := C.GoString(toSave)
	fuse := useFUSE != 0

	parsed, err := url.Parse(r)
	if err != nil {
		return -1
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "rustbridge")

	ctx, c := context.WithCancel(context.Background())
	cancel = c

	instance, err := common.NewKeibiDrop(ctx, logger, fuse, parsed, int(inbound), int(outbound), m, s)
	if err != nil {
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

func main() {}
