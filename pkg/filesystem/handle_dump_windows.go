// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//go:build windows

package filesystem

import (
	"log/slog"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dumpHandlesFor logs every handle of THIS process whose file name ends with
// pathSuffix. Diagnostic for rename-over-target sharing violations: the handle
// table names the holder we failed to predict. Runs bounded work in the caller
// goroutine of platRename's failure path only.
func dumpHandlesFor(pathSuffix string) {
	go func() {
		defer func() { _ = recover() }()
		const systemExtendedHandleInformation = 64
		ntdll := windows.NewLazySystemDLL("ntdll.dll")
		query := ntdll.NewProc("NtQuerySystemInformation")

		size := uint32(8 << 20)
		var buf []byte
		for i := 0; i < 6; i++ {
			buf = make([]byte, size)
			var ret uint32
			status, _, _ := query.Call(systemExtendedHandleInformation,
				uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret)))
			if status == 0 {
				break
			}
			const statusInfoLengthMismatch = 0xC0000004
			if uint32(status) != statusInfoLengthMismatch {
				slog.Warn("handle dump: NtQuerySystemInformation failed", "status", status)
				return
			}
			size *= 2
			buf = nil
		}
		if buf == nil {
			slog.Warn("handle dump: buffer growth exhausted")
			return
		}

		type handleEntryEx struct {
			Object                uintptr
			UniqueProcessID       uintptr
			HandleValue           uintptr
			GrantedAccess         uint32
			CreatorBackTraceIndex uint16
			ObjectTypeIndex       uint16
			HandleAttributes      uint32
			Reserved              uint32
		}
		count := *(*uintptr)(unsafe.Pointer(&buf[0]))
		basePtr := unsafe.Add(unsafe.Pointer(&buf[0]), 2*int(unsafe.Sizeof(uintptr(0))))
		entrySize := unsafe.Sizeof(handleEntryEx{})
		pid := uintptr(windows.GetCurrentProcessId())

		suffix := strings.ToLower(pathSuffix)
		for i := uintptr(0); i < count; i++ {
			e := (*handleEntryEx)(unsafe.Add(basePtr, int(i*entrySize)))
			if e.UniqueProcessID != pid {
				continue
			}
			// GetFinalPathNameByHandle hangs on pipe handles with this
			// access mask; skip them.
			if e.GrantedAccess == 0x0012019f {
				continue
			}
			var name [512]uint16
			n, err := windows.GetFinalPathNameByHandle(windows.Handle(e.HandleValue), &name[0], uint32(len(name)), 0)
			if err != nil || n == 0 || n > uint32(len(name)) {
				continue
			}
			p := windows.UTF16ToString(name[:n])
			if strings.HasSuffix(strings.ToLower(p), suffix) {
				slog.Warn("rename blocker candidate handle",
					"handle", e.HandleValue, "access", e.GrantedAccess, "path", p)
			}
		}
	}()
}

// DumpHandlesFor is the exported debug entry for the standalone probe.
func DumpHandlesFor(pathSuffix string) { dumpHandlesFor(pathSuffix) }
