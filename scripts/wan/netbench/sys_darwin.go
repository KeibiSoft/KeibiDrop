// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Darwin stubs: CPU times work; GSO/GRO/per-socket CC do not exist here
// ABOUTME: and report unsupported so rows are labeled, never silently wrong.

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

var errUnsupported = errors.New("unsupported on darwin")

func cpuTimes() (userNS, sysNS int64) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	return ru.Utime.Nano(), ru.Stime.Nano()
}

func rawFD(c syscall.Conn) (int, error) {
	sc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd int
	err = sc.Control(func(f uintptr) { fd = int(f) })
	return fd, err
}

func setCC(c syscall.Conn, name string) (string, error) { return "", errUnsupported }
func getCC(c syscall.Conn) string                       { return "" }

func sockBufs(c syscall.Conn) (snd, rcv int) {
	fd, err := rawFD(c)
	if err != nil {
		return -1, -1
	}
	snd, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF)
	if err != nil {
		snd = -1
	}
	rcv, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF)
	if err != nil {
		rcv = -1
	}
	return snd, rcv
}

func setGSO(c syscall.Conn, segSize int) error                       { return errUnsupported }
func setGRO(c syscall.Conn) error                                    { return errUnsupported }
func recvGRO(c syscall.Conn, p, oob []byte) (n, stride int, err error) { return 0, 0, errUnsupported }

const (
	gsoSupported   = false
	batchSupported = false
	groSupported   = false
	ccSupported    = false
)
