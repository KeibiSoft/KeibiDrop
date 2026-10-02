// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Windows stubs: CPU via GetProcessTimes; GSO/GRO/batch/per-socket CC
// ABOUTME: report unsupported so rows are labeled, never silently wrong.

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

var errUnsupported = errors.New("unsupported on windows")

func cpuTimes() (userNS, sysNS int64) {
	var creation, exit, kernel, user windows.Filetime
	h := windows.CurrentProcess()
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, 0
	}
	// Filetime counts 100ns units.
	ft := func(f windows.Filetime) int64 {
		return (int64(f.HighDateTime)<<32 | int64(f.LowDateTime)) * 100
	}
	return ft(user), ft(kernel)
}

func rawFD(c syscall.Conn) (windows.Handle, error) {
	sc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd windows.Handle
	err = sc.Control(func(f uintptr) { fd = windows.Handle(f) })
	return fd, err
}

func setCC(c syscall.Conn, name string) (string, error) { return "", errUnsupported }
func getCC(c syscall.Conn) string                       { return "" }

func sockBufs(c syscall.Conn) (snd, rcv int) {
	fd, err := rawFD(c)
	if err != nil {
		return -1, -1
	}
	snd, err = windows.GetsockoptInt(fd, windows.SOL_SOCKET, windows.SO_SNDBUF)
	if err != nil {
		snd = -1
	}
	rcv, err = windows.GetsockoptInt(fd, windows.SOL_SOCKET, windows.SO_RCVBUF)
	if err != nil {
		rcv = -1
	}
	return snd, rcv
}

func setGSO(c syscall.Conn, segSize int) error                         { return errUnsupported }
func setGRO(c syscall.Conn) error                                      { return errUnsupported }
func recvGRO(c syscall.Conn, p, oob []byte) (n, stride int, err error) { return 0, 0, errUnsupported }

const (
	gsoSupported   = false
	batchSupported = false
	groSupported   = false
	ccSupported    = false
)
