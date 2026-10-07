// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Data-phase senders and receivers. Stream lanes (tcp/quic) share one
// ABOUTME: pair; UDP lanes add seq headers, pacing, loss accounting, GSO/batch.

package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	udpHeader = 16 // seq uint64 + unix-nanos int64
	finSeq    = ^uint64(0)
)

// trialClock bounds a data phase: target bytes, optional fixed duration, hard wall.
type trialClock struct {
	start    time.Time
	deadline time.Time
	duration time.Duration
	bytes    int64
}

func newClock(spec *TrialSpec) *trialClock {
	c := &trialClock{start: time.Now(), bytes: spec.Bytes}
	if spec.MaxWallMS > 0 {
		c.deadline = c.start.Add(time.Duration(spec.MaxWallMS) * time.Millisecond)
	}
	if spec.DurationMS > 0 {
		c.duration = time.Duration(spec.DurationMS) * time.Millisecond
	}
	return c
}

// done returns the bound that fired, or "" to continue.
func (c *trialClock) done(sent int64) string {
	if c.duration > 0 {
		if time.Since(c.start) >= c.duration {
			return "duration"
		}
	} else if c.bytes > 0 && sent >= c.bytes {
		return "bytes"
	}
	if !c.deadline.IsZero() && time.Now().After(c.deadline) {
		return "max-wall"
	}
	return ""
}

// pacer holds the sender to rateMbit with ~1ms bursts. Sub-ms sleeps oversleep
// badly (macOS timer coalescing), so it checks the absolute schedule every ~1ms
// worth of packets and sleeps only when >=2ms ahead, undershooting by 1ms; the
// absolute schedule absorbs the remaining slop.
func pacer(rateMbit float64, dgram int, start time.Time) func(n uint64) {
	if rateMbit <= 0 {
		return func(uint64) {}
	}
	perPktNS := float64(dgram*8) / (rateMbit * 1e6) * 1e9
	checkEvery := uint64(1)
	if per := 1e6 / perPktNS; per > 1 {
		checkEvery = uint64(per)
	}
	return func(n uint64) {
		if n%checkEvery != 0 {
			return
		}
		next := start.Add(time.Duration(float64(n) * perPktNS))
		if d := time.Until(next); d > 2*time.Millisecond {
			time.Sleep(d - time.Millisecond)
		}
	}
}

type closeWriter interface{ CloseWrite() error }

// streamSend pushes spec.Bytes over a stream conn in spec.Write-sized writes.
func streamSend(c net.Conn, spec *TrialSpec) SideReport {
	rep := SideReport{OS: hereOS()}
	buf := make([]byte, spec.Write)
	clock := newClock(spec)
	u0, s0 := cpuTimes()
	for {
		bound := clock.done(rep.Bytes)
		if bound != "" {
			rep.Bound = bound
			break
		}
		n := len(buf)
		if spec.Bytes > 0 && rep.Bytes+int64(n) > spec.Bytes {
			n = int(spec.Bytes - rep.Bytes)
		}
		wn, err := c.Write(buf[:n])
		rep.Bytes += int64(wn)
		if err != nil {
			rep.Err = err.Error()
			rep.Bound = "error"
			break
		}
	}
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
	u1, s1 := cpuTimes()
	rep.CPUUserNS, rep.CPUSysNS = u1-u0, s1-s0
	rep.WallNS = int64(time.Since(clock.start))
	return rep
}

// streamRecv drains a stream conn. want>0 stops at that byte count; otherwise it
// reads to EOF. senderDone delivers the sender's actual byte count mid-flight when
// the sender hit a wall bound.
func streamRecv(c net.Conn, spec *TrialSpec, senderDone <-chan int64) SideReport {
	rep := SideReport{OS: hereOS()}
	buf := make([]byte, 1<<20)
	want := spec.Bytes
	if spec.DurationMS > 0 {
		want = 0
	}
	clock := newClock(spec)
	u0, s0 := cpuTimes()
	hardStop := time.Now().Add(time.Duration(spec.MaxWallMS)*time.Millisecond + 30*time.Second)
	for {
		select {
		case actual := <-senderDone:
			if actual >= 0 {
				want = actual
			}
			senderDone = nil
		default:
		}
		if want > 0 && rep.Bytes >= want {
			rep.Bound = "bytes"
			break
		}
		if time.Now().After(hardStop) {
			rep.Bound = "max-wall"
			break
		}
		_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := c.Read(buf)
		rep.Bytes += int64(n)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, io.EOF) {
				rep.Bound = "eof"
			} else {
				rep.Err = err.Error()
				rep.Bound = "error"
			}
			break
		}
	}
	u1, s1 := cpuTimes()
	rep.CPUUserNS, rep.CPUSysNS = u1-u0, s1-s0
	rep.WallNS = int64(time.Since(clock.start))
	return rep
}

// udpSender wraps a CONNECTED *net.UDPConn. Connected is load-bearing on macOS:
// unconnected sendto pays a per-packet policy evaluation (~250us measured on the
// bench Mac, a 76x pps difference); connect() caches the verdict per flow.
type udpSender struct {
	c *net.UDPConn
}

func (u *udpSender) send(b []byte) (int, error) { return u.c.Write(b) }

// udpSend paces datagrams with per-packet seq headers. Lane variants: plain
// (one sendmsg per dgram), batch (sendmmsg via x/net), gso (UDP_SEGMENT
// super-buffers carrying per-dgram headers at stride boundaries).
func udpSend(u *udpSender, spec *TrialSpec) SideReport {
	rep := SideReport{OS: hereOS()}
	clock := newClock(spec)
	pace := pacer(spec.RateMbit, spec.Dgram, clock.start)
	u0, s0 := cpuTimes()

	var seq uint64
	switch spec.Lane {
	case "udp-batch":
		if !batchSupported {
			rep.Fallback = "xnet-loop-emulation"
		}
		pc := ipv4.NewPacketConn(u.c)
		batch := spec.Batch
		if batch < 1 {
			batch = 1
		}
		msgs := make([]ipv4.Message, batch)
		bufs := make([][]byte, batch)
		for i := range msgs {
			bufs[i] = make([]byte, spec.Dgram)
			msgs[i].Buffers = [][]byte{bufs[i]}
			// Addr stays nil: the socket is connected.
		}
		for {
			if b := clock.done(rep.Bytes); b != "" {
				rep.Bound = b
				break
			}
			for i := 0; i < batch; i++ {
				binary.BigEndian.PutUint64(bufs[i][0:8], seq+uint64(i))
				binary.BigEndian.PutUint64(bufs[i][8:16], uint64(time.Now().UnixNano()))
			}
			n, err := pc.WriteBatch(msgs, 0)
			if err != nil {
				rep.Err = err.Error()
				rep.Bound = "error"
				break
			}
			seq += uint64(n)
			rep.Dgrams += uint64(n)
			rep.Bytes += int64(n * spec.Dgram)
			pace(seq)
		}
	case "udp-gso":
		if !gsoSupported {
			rep.Err = "udp-gso unsupported on " + runtime.GOOS
			rep.Bound = "error"
			return rep
		}
		if err := setGSO(u.c, spec.Dgram); err != nil {
			rep.Err = "setGSO: " + err.Error()
			rep.Bound = "error"
			return rep
		}
		segs := spec.GSOSegs
		if segs < 1 {
			segs = 1
		}
		if segs*spec.Dgram > 65000 {
			segs = 65000 / spec.Dgram
		}
		super := make([]byte, segs*spec.Dgram)
		for {
			if b := clock.done(rep.Bytes); b != "" {
				rep.Bound = b
				break
			}
			for i := 0; i < segs; i++ {
				off := i * spec.Dgram
				binary.BigEndian.PutUint64(super[off:off+8], seq+uint64(i))
				binary.BigEndian.PutUint64(super[off+8:off+16], uint64(time.Now().UnixNano()))
			}
			_, err := u.send(super)
			if err != nil {
				rep.Err = err.Error()
				rep.Bound = "error"
				break
			}
			seq += uint64(segs)
			rep.Dgrams += uint64(segs)
			rep.Bytes += int64(len(super))
			pace(seq)
		}
	default: // plain udp
		buf := make([]byte, spec.Dgram)
		for {
			if b := clock.done(rep.Bytes); b != "" {
				rep.Bound = b
				break
			}
			binary.BigEndian.PutUint64(buf[0:8], seq)
			binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
			n, err := u.send(buf)
			if err != nil {
				rep.Err = err.Error()
				rep.Bound = "error"
				break
			}
			seq++
			rep.Dgrams++
			rep.Bytes += int64(n)
			pace(seq)
		}
	}

	// FIN burst: seq=MaxUint64, second field carries total dgrams sent so the
	// receiver can compute loss even when some FINs drop.
	fin := make([]byte, udpHeader)
	binary.BigEndian.PutUint64(fin[0:8], finSeq)
	binary.BigEndian.PutUint64(fin[8:16], rep.Dgrams)
	for i := 0; i < 10; i++ {
		_, _ = u.send(fin)
		time.Sleep(20 * time.Millisecond)
	}
	u1, s1 := cpuTimes()
	rep.CPUUserNS, rep.CPUSysNS = u1-u0, s1-s0
	rep.WallNS = int64(time.Since(clock.start))
	return rep
}

// udpRecvState is shared between the datagram loop and the control goroutine.
type udpRecvState struct {
	senderTotal atomic.Uint64 // dgrams the sender claims; 0 = unknown yet
	senderSeen  atomic.Bool
}

// udpRecv counts datagrams until FIN + drain, sender report + drain, or hard stop.
// keepalive (nil ok): send a 1-byte dgram to the peer every 5s to hold stateful
// firewalls open (dir=down through NAT/Windows firewall).
func udpRecv(c *net.UDPConn, spec *TrialSpec, st *udpRecvState, keepalive func()) SideReport {
	rep := SideReport{OS: hereOS()}
	useGRO := spec.GRO && groSupported && spec.Lane == "udp-gso"
	if spec.GRO && !useGRO {
		rep.Fallback = "gro-unavailable"
	}
	if useGRO {
		if err := setGRO(c); err != nil {
			useGRO = false
			rep.Fallback = "gro-setsockopt-failed"
		}
	}
	var buf []byte
	var oob []byte
	if useGRO {
		buf = make([]byte, 1<<16)
		oob = make([]byte, 256)
	} else {
		buf = make([]byte, 65535)
	}
	useBatch := spec.Lane == "udp-batch" && batchSupported
	var pc *ipv4.PacketConn
	var msgs []ipv4.Message
	if useBatch {
		pc = ipv4.NewPacketConn(c)
		msgs = make([]ipv4.Message, 32)
		for i := range msgs {
			msgs[i].Buffers = [][]byte{make([]byte, 65535)}
		}
	}

	start := time.Now()
	u0, s0 := cpuTimes()
	hardStop := start.Add(time.Duration(spec.MaxWallMS)*time.Millisecond + 30*time.Second)
	var lastSeq uint64
	var finTotal uint64
	var lastData time.Time = start
	lastKeep := time.Now()
	var lastArm time.Time

	account := func(p []byte) {
		if len(p) < udpHeader {
			return
		}
		seq := binary.BigEndian.Uint64(p[0:8])
		if seq == finSeq {
			finTotal = binary.BigEndian.Uint64(p[8:16])
			return
		}
		if seq < lastSeq {
			rep.Reorder++
		}
		lastSeq = seq
		rep.Dgrams++
		rep.Bytes += int64(len(p))
		lastData = time.Now()
	}

	for {
		if time.Now().After(hardStop) {
			rep.Bound = "max-wall"
			break
		}
		// Finish when the sender is known-done and the pipe has drained 2s.
		total := st.senderTotal.Load()
		if finTotal > 0 {
			total = finTotal
		}
		if total > 0 && (rep.Dgrams >= total || time.Since(lastData) > 2*time.Second) {
			rep.Bound = "drained"
			break
		}
		if total == 0 && st.senderSeen.Load() && time.Since(lastData) > 2*time.Second {
			rep.Bound = "drained"
			break
		}
		if keepalive != nil && time.Since(lastKeep) > 5*time.Second {
			keepalive()
			lastKeep = time.Now()
		}
		// Re-arming the read deadline per packet costs a timer syscall per dgram;
		// arm it coarsely instead.
		if time.Since(lastArm) > 500*time.Millisecond {
			_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
			lastArm = time.Now()
		}
		switch {
		case useGRO:
			n, stride, err := recvGRO(c, buf, oob)
			if err != nil {
				if isTimeout(err) {
					continue
				}
				rep.Err = err.Error()
				rep.Bound = "error"
				goto out
			}
			if stride <= 0 {
				stride = n
			}
			for off := 0; off < n; off += stride {
				end := off + stride
				if end > n {
					end = n
				}
				account(buf[off:end])
			}
		case useBatch:
			n, err := pc.ReadBatch(msgs, 0)
			if err != nil {
				if isTimeout(err) {
					continue
				}
				rep.Err = err.Error()
				rep.Bound = "error"
				goto out
			}
			for i := 0; i < n; i++ {
				account(msgs[i].Buffers[0][:msgs[i].N])
			}
		default:
			n, _, err := c.ReadFromUDP(buf)
			if err != nil {
				if isTimeout(err) {
					continue
				}
				rep.Err = err.Error()
				rep.Bound = "error"
				goto out
			}
			account(buf[:n])
		}
	}
out:
	u1, s1 := cpuTimes()
	rep.CPUUserNS, rep.CPUSysNS = u1-u0, s1-s0
	rep.WallNS = int64(time.Since(start))
	return rep
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.EAGAIN)
}

// applyBufs applies the requested socket buffer and reads back effective values.
type bufConn interface {
	SetReadBuffer(int) error
	SetWriteBuffer(int) error
}

func applyBufs(c net.Conn, req int) (snd, rcv int) {
	if bc, ok := c.(bufConn); ok && req > 0 {
		_ = bc.SetReadBuffer(req)
		_ = bc.SetWriteBuffer(req)
	}
	if sc, ok := c.(syscall.Conn); ok {
		return sockBufs(sc)
	}
	return -1, -1
}

func applyCC(c net.Conn, name string) string {
	if name == "" {
		if sc, ok := c.(syscall.Conn); ok && ccSupported {
			return getCC(sc)
		}
		return ""
	}
	sc, ok := c.(syscall.Conn)
	if !ok || !ccSupported {
		return "unsupported"
	}
	eff, err := setCC(sc, name)
	if err != nil {
		return "unsupported"
	}
	return eff
}
