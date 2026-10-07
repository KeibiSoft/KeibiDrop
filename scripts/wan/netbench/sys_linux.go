// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Linux syscall helpers: per-socket congestion control, UDP GSO/GRO,
// ABOUTME: effective socket buffers, process CPU times.

package main

import (
	"encoding/binary"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

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
	var opErr error
	err = sc.Control(func(f uintptr) { fd = int(f) })
	if err != nil {
		return 0, err
	}
	return fd, opErr
}

func setCC(c syscall.Conn, name string) (string, error) {
	fd, err := rawFD(c)
	if err != nil {
		return "", err
	}
	if err := unix.SetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_CONGESTION, name); err != nil {
		return "", fmt.Errorf("set TCP_CONGESTION=%s: %w", name, err)
	}
	return getCC(c), nil
}

func getCC(c syscall.Conn) string {
	fd, err := rawFD(c)
	if err != nil {
		return ""
	}
	v, err := unix.GetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_CONGESTION)
	if err != nil {
		return ""
	}
	return v
}

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

func setGSO(c syscall.Conn, segSize int) error {
	fd, err := rawFD(c)
	if err != nil {
		return err
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_UDP, unix.UDP_SEGMENT, segSize)
}

func setGRO(c syscall.Conn) error {
	fd, err := rawFD(c)
	if err != nil {
		return err
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_UDP, unix.UDP_GRO, 1)
}

// recvGRO reads one possibly-coalesced UDP buffer and returns payload plus the
// GRO segment stride (0 when the kernel did not coalesce).
func recvGRO(c syscall.Conn, p, oob []byte) (n, stride int, err error) {
	fd, ferr := rawFD(c)
	if ferr != nil {
		return 0, 0, ferr
	}
	n, oobn, _, _, err := unix.Recvmsg(fd, p, oob, 0)
	if err != nil {
		return 0, 0, err
	}
	if oobn > 0 {
		msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
		if perr == nil {
			for _, m := range msgs {
				if m.Header.Level == unix.SOL_UDP && m.Header.Type == unix.UDP_GRO && len(m.Data) >= 2 {
					stride = int(binary.NativeEndian.Uint16(m.Data))
				}
			}
		}
	}
	return n, stride, nil
}

const (
	gsoSupported   = true
	batchSupported = true
	groSupported   = true
	ccSupported    = true
)
