// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: netbench measures raw transport lanes (kernel TCP, UDP plain/batch/GSO,
// ABOUTME: QUIC via pkg/transport) between two hosts. Client dials; -dir picks the
// ABOUTME: data direction on the client-dialed session, so a host that blocks all
// ABOUTME: inbound (Singapore) can still measure both directions.

package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type Config struct {
	Mode     string
	Listen   string
	UDPPort  int
	Connect  string
	Lane     string
	Dir      string
	Bytes    int64
	Duration time.Duration
	RateMbit float64
	Dgram    int
	Write    int
	Batch    int
	GSOSegs  int
	GRO      bool
	SockBuf  int
	CC       string
	MaxWall  time.Duration
	Trials   int
	Path     string
	Label    string
	JSONL    string
	RunID    string
	PIDFile  string
}

func main() {
	var cfg Config
	var bytesStr string
	flag.StringVar(&cfg.Mode, "mode", "client", "server|client")
	flag.StringVar(&cfg.Listen, "listen", ":44300", "server control/data TCP listen address")
	flag.IntVar(&cfg.UDPPort, "udp-port", 44301, "server UDP/QUIC port")
	flag.StringVar(&cfg.Connect, "connect", "", "client: server host:port (control)")
	flag.StringVar(&cfg.Lane, "lane", "tcp", "tcp|udp|udp-batch|udp-gso|quic|quic-secure")
	flag.StringVar(&cfg.Dir, "dir", "up", "up = client sends, down = server sends")
	flag.StringVar(&bytesStr, "bytes", "1073741824", "bytes per trial (0 with -duration)")
	flag.DurationVar(&cfg.Duration, "duration", 0, "fixed-time trial (paced UDP sweep points)")
	flag.Float64Var(&cfg.RateMbit, "rate-mbit", 0, "pace UDP at this rate; 0 = unpaced")
	flag.IntVar(&cfg.Dgram, "dgram", 1200, "UDP datagram payload size")
	flag.IntVar(&cfg.Write, "write", 65536, "stream write size")
	flag.IntVar(&cfg.Batch, "batch", 32, "sendmmsg/recvmmsg batch size")
	flag.IntVar(&cfg.GSOSegs, "gso-segs", 32, "UDP_SEGMENT segments per super-buffer")
	flag.BoolVar(&cfg.GRO, "gro", false, "enable UDP_GRO on the receiver (linux)")
	flag.IntVar(&cfg.SockBuf, "sockbuf", 0, "SO_SNDBUF/SO_RCVBUF request; 0 = OS default")
	flag.StringVar(&cfg.CC, "cc", "", "sender TCP congestion control (linux, per-socket)")
	flag.DurationVar(&cfg.MaxWall, "max-wall", 120*time.Second, "hard per-trial cutoff")
	flag.IntVar(&cfg.Trials, "trials", 3, "trials per invocation")
	flag.StringVar(&cfg.Path, "path", "", "path tag for the jsonl row, e.g. mac-tm")
	flag.StringVar(&cfg.Label, "label", "", "freeform row label")
	flag.StringVar(&cfg.JSONL, "jsonl", "", "append rows here; empty = stdout")
	flag.StringVar(&cfg.RunID, "run-id", "", "run id; default = start timestamp")
	flag.StringVar(&cfg.PIDFile, "pidfile", "", "server: write pid here")
	flag.Parse()

	b, err := strconv.ParseInt(bytesStr, 10, 64)
	if err != nil {
		log.Fatalf("bad -bytes: %v", err)
	}
	cfg.Bytes = b
	if cfg.RunID == "" {
		cfg.RunID = time.Now().UTC().Format("20060102T150405Z")
	}
	log.SetFlags(log.LstdFlags | log.LUTC)

	switch cfg.Mode {
	case "server":
		if err := runServer(cfg); err != nil {
			log.Fatalf("server: %v", err)
		}
	case "client":
		if cfg.Connect == "" {
			log.Fatal("client needs -connect host:port")
		}
		if err := runClient(cfg); err != nil {
			log.Fatalf("client: %v", err)
		}
	default:
		log.Fatalf("unknown -mode %q", cfg.Mode)
	}
}

func specFrom(cfg Config, trialID uint64) *TrialSpec {
	return &TrialSpec{
		Lane: cfg.Lane, Dir: cfg.Dir, Bytes: cfg.Bytes,
		DurationMS: int(cfg.Duration / time.Millisecond),
		RateMbit:   cfg.RateMbit, Dgram: cfg.Dgram, Write: cfg.Write,
		Batch: cfg.Batch, GSOSegs: cfg.GSOSegs, GRO: cfg.GRO,
		SockBuf: cfg.SockBuf, CC: cfg.CC,
		MaxWallMS: int(cfg.MaxWall / time.Millisecond), TrialID: trialID,
	}
}

// ---------- server ----------

type server struct {
	cfg Config
	mu  sync.Mutex
	// pending TCP data conns by trial id, delivered by the accept loop.
	pending map[uint64]chan net.Conn
}

func runServer(cfg Config) error {
	if cfg.PIDFile != "" {
		if err := os.WriteFile(cfg.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			return err
		}
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("netbench server listening on %s (udp/quic on :%d) os=%s ncpu=%d",
		cfg.Listen, cfg.UDPPort, runtime.GOOS, runtime.NumCPU())
	s := &server{cfg: cfg, pending: make(map[uint64]chan net.Conn)}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.dispatch(c)
	}
}

// dispatch peeks the first byte: '{' = JSON control conn, 'D' = TCP data conn.
func (s *server) dispatch(c net.Conn) {
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := br.Peek(1)
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	wc := &peekedConn{Conn: c, r: br}
	switch b[0] {
	case '{':
		s.handleControl(wc)
	case 'D':
		hdr := make([]byte, 9)
		if _, err := readFull(wc, hdr); err != nil {
			_ = c.Close()
			return
		}
		id := beUint64(hdr[1:9])
		s.mu.Lock()
		ch := s.pending[id]
		s.mu.Unlock()
		if ch == nil {
			_ = c.Close()
			return
		}
		select {
		case ch <- wc:
		case <-time.After(10 * time.Second):
			_ = c.Close()
		}
	default:
		// Internet scanners hit this port; drop them silently.
		_ = c.Close()
	}
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func beUint64(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

func (s *server) handleControl(c net.Conn) {
	defer c.Close()
	cc := newCtrl(c)
	for {
		m, err := cc.recv(10 * time.Minute)
		if err != nil {
			return
		}
		switch m.Cmd {
		case "hello":
			host, _ := os.Hostname()
			_ = cc.send(ctrlMsg{Cmd: "hello", Info: map[string]string{
				"os": runtime.GOOS, "ncpu": strconv.Itoa(runtime.NumCPU()), "host": host,
			}})
		case "ping":
			_ = cc.send(ctrlMsg{Cmd: "pong", T: m.T})
		case "trial":
			if m.Spec == nil {
				return
			}
			if err := s.serveTrial(cc, m.Spec); err != nil {
				log.Printf("trial %d %s/%s: %v", m.Spec.TrialID, m.Spec.Lane, m.Spec.Dir, err)
				return
			}
		case "bye":
			return
		}
	}
}

func waitFor(cc *ctrlConn, cmd string, timeout time.Duration) (ctrlMsg, error) {
	deadline := time.Now().Add(timeout)
	for {
		m, err := cc.recv(time.Until(deadline))
		if err != nil {
			return m, err
		}
		if m.Cmd == cmd {
			return m, nil
		}
		if m.Cmd == "bye" {
			return m, fmt.Errorf("peer said bye while waiting for %s", cmd)
		}
	}
}

func (s *server) serveTrial(cc *ctrlConn, spec *TrialSpec) error {
	trialWait := time.Duration(spec.MaxWallMS)*time.Millisecond + 90*time.Second
	switch {
	case spec.Lane == "tcp":
		ch := make(chan net.Conn, 1)
		s.mu.Lock()
		s.pending[spec.TrialID] = ch
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.pending, spec.TrialID)
			s.mu.Unlock()
		}()
		if err := cc.send(ctrlMsg{Cmd: "ready"}); err != nil {
			return err
		}
		var dconn net.Conn
		select {
		case dconn = <-ch:
		case <-time.After(30 * time.Second):
			return fmt.Errorf("no data conn for trial %d", spec.TrialID)
		}
		defer dconn.Close()
		return s.streamPhase(cc, spec, dconn, trialWait)

	case spec.isQUIC():
		ln, err := quicListen(fmt.Sprintf(":%d", s.cfg.UDPPort))
		if err != nil {
			return err
		}
		defer ln.Close()
		if err := cc.send(ctrlMsg{Cmd: "ready"}); err != nil {
			return err
		}
		dconn, err := quicAccept(ln, spec, 30*time.Second)
		if err != nil {
			return err
		}
		defer dconn.Close()
		return s.streamPhase(cc, spec, dconn, trialWait)

	case spec.isUDP():
		uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: s.cfg.UDPPort})
		if err != nil {
			return err
		}
		defer uc.Close()
		if err := cc.send(ctrlMsg{Cmd: "ready"}); err != nil {
			return err
		}
		// Hello phase: learn the client's public 5-tuple through its NAT/firewall.
		clientAddr, err := waitHello(uc, spec.TrialID, 30*time.Second)
		if err != nil {
			return err
		}
		// Rebind the port CONNECTED to the learned peer: unconnected sends pay a
		// per-packet policy evaluation on macOS (76x pps measured). Wait out the
		// client's remaining hellos so none hits the close/rebind gap and bounces
		// an ICMP refusal into the client's connected socket.
		time.Sleep(300 * time.Millisecond)
		_ = uc.Close()
		ucc, err := net.DialUDP("udp", &net.UDPAddr{Port: s.cfg.UDPPort}, clientAddr)
		if err != nil {
			return fmt.Errorf("udp reconnect to %s: %w", clientAddr, err)
		}
		defer ucc.Close()
		sbuf, rbuf := applyBufs(ucc, spec.SockBuf)
		if err := cc.send(ctrlMsg{Cmd: "udp_ready"}); err != nil {
			return err
		}
		if spec.Dir == "down" {
			rep := udpSend(&udpSender{c: ucc}, spec)
			rep.SockbufSnd, rep.SockbufRcv = sbuf, rbuf
			if err := cc.send(ctrlMsg{Cmd: "sender_report", Report: &rep}); err != nil {
				return err
			}
			_, err := waitFor(cc, "trial_done", trialWait)
			return err
		}
		st := &udpRecvState{}
		repCh := make(chan SideReport, 1)
		go func() { repCh <- udpRecv(ucc, spec, st, nil) }()
		m, err := waitFor(cc, "sender_report", trialWait)
		if err != nil {
			return err
		}
		st.senderTotal.Store(m.Report.Dgrams)
		st.senderSeen.Store(true)
		rep := <-repCh
		rep.SockbufSnd, rep.SockbufRcv = sbuf, rbuf
		if err := cc.send(ctrlMsg{Cmd: "receiver_report", Report: &rep}); err != nil {
			return err
		}
		_, err = waitFor(cc, "trial_done", trialWait)
		return err
	}
	return fmt.Errorf("unknown lane %q", spec.Lane)
}

// streamPhase runs the tcp/quic data phase on an established conn, server side.
func (s *server) streamPhase(cc *ctrlConn, spec *TrialSpec, dconn net.Conn, trialWait time.Duration) error {
	sbuf, rbuf := applyBufs(underlying(dconn), spec.SockBuf)
	if spec.Dir == "down" {
		ccEff := applyCC(underlying(dconn), spec.CC)
		rep := streamSend(dconn, spec)
		rep.SockbufSnd, rep.SockbufRcv = sbuf, rbuf
		rep.CCEff = ccEff
		if err := cc.send(ctrlMsg{Cmd: "sender_report", Report: &rep}); err != nil {
			return err
		}
		_, err := waitFor(cc, "trial_done", trialWait)
		return err
	}
	senderDone := make(chan int64, 1)
	repCh := make(chan SideReport, 1)
	go func() { repCh <- streamRecv(dconn, spec, senderDone) }()
	m, err := waitFor(cc, "sender_report", trialWait)
	if err != nil {
		return err
	}
	senderDone <- m.Report.Bytes
	rep := <-repCh
	rep.SockbufSnd, rep.SockbufRcv = sbuf, rbuf
	if err := cc.send(ctrlMsg{Cmd: "receiver_report", Report: &rep}); err != nil {
		return err
	}
	_, err = waitFor(cc, "trial_done", trialWait)
	return err
}

func waitHello(uc *net.UDPConn, trialID uint64, timeout time.Duration) (*net.UDPAddr, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 64)
	for time.Now().Before(deadline) {
		_ = uc.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, addr, err := uc.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return nil, err
		}
		if n >= 9 && buf[0] == 'H' && beUint64(buf[1:9]) == trialID {
			_ = uc.SetReadDeadline(time.Time{})
			return addr, nil
		}
	}
	return nil, fmt.Errorf("no hello for trial %d", trialID)
}

// ---------- client ----------

func runClient(cfg Config) error {
	host, _, err := net.SplitHostPort(cfg.Connect)
	if err != nil {
		return fmt.Errorf("bad -connect: %w", err)
	}
	c, err := net.DialTimeout("tcp", cfg.Connect, 15*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	cc := newCtrl(c)
	if err := cc.send(ctrlMsg{Cmd: "hello"}); err != nil {
		return err
	}
	hello, err := waitFor(cc, "hello", 15*time.Second)
	if err != nil {
		return err
	}
	serverOS := hello.Info["os"]

	rtt := time.Duration(1<<62 - 1)
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		if err := cc.send(ctrlMsg{Cmd: "ping", T: t0.UnixNano()}); err != nil {
			return err
		}
		if _, err := waitFor(cc, "pong", 10*time.Second); err != nil {
			return err
		}
		if d := time.Since(t0); d < rtt {
			rtt = d
		}
	}
	log.Printf("control up: server os=%s host=%s rtt=%.2fms", serverOS, hello.Info["host"], float64(rtt)/1e6)

	for trial := 1; trial <= cfg.Trials; trial++ {
		spec := specFrom(cfg, uint64(time.Now().UnixNano()))
		r, err := clientTrial(cc, cfg, spec, host, serverOS, rtt)
		if err != nil {
			return fmt.Errorf("trial %d: %w", trial, err)
		}
		r.Trial = trial
		if err := appendJSONL(cfg.JSONL, r); err != nil {
			return err
		}
		log.Printf("trial %d/%d %s/%s: %.1f Mbit/s loss=%.2f%% bound=%s",
			trial, cfg.Trials, cfg.Lane, cfg.Dir, r.GoodputMbit, r.LossPct, r.Bound)
		time.Sleep(2 * time.Second)
	}
	_ = cc.send(ctrlMsg{Cmd: "bye"})
	return nil
}

func clientTrial(cc *ctrlConn, cfg Config, spec *TrialSpec, host, serverOS string, rtt time.Duration) (row, error) {
	trialWait := time.Duration(spec.MaxWallMS)*time.Millisecond + 90*time.Second
	if err := cc.send(ctrlMsg{Cmd: "trial", Spec: spec}); err != nil {
		return row{}, err
	}
	if _, err := waitFor(cc, "ready", 30*time.Second); err != nil {
		return row{}, err
	}

	var localRep, peerRep SideReport
	var sbuf, rbuf int
	var ccEff string

	switch {
	case spec.isStream():
		var dconn net.Conn
		var err error
		if spec.Lane == "tcp" {
			dconn, err = net.DialTimeout("tcp", cfg.Connect, 15*time.Second)
			if err != nil {
				return row{}, err
			}
			hdr := make([]byte, 9)
			hdr[0] = 'D'
			for i := 0; i < 8; i++ {
				hdr[1+i] = byte(spec.TrialID >> (56 - 8*i))
			}
			if _, err := dconn.Write(hdr); err != nil {
				_ = dconn.Close()
				return row{}, err
			}
		} else {
			dconn, err = quicDial(fmt.Sprintf("%s:%d", host, cfg.UDPPort), spec)
			if err != nil {
				return row{}, err
			}
			if spec.Dir == "down" {
				// quic-go materializes the stream at the server's Accept only when
				// data first flows; a pure receiver must prime it or accept starves.
				if _, err := dconn.Write([]byte{0}); err != nil {
					_ = dconn.Close()
					return row{}, fmt.Errorf("quic stream prime: %w", err)
				}
			}
		}
		defer dconn.Close()
		sbuf, rbuf = applyBufs(underlying(dconn), spec.SockBuf)
		if spec.Dir == "up" {
			ccEff = applyCC(underlying(dconn), spec.CC)
			localRep = streamSend(dconn, spec)
			if err := cc.send(ctrlMsg{Cmd: "sender_report", Report: &localRep}); err != nil {
				return row{}, err
			}
			m, err := waitFor(cc, "receiver_report", trialWait)
			if err != nil {
				return row{}, err
			}
			peerRep = *m.Report
		} else {
			senderDone := make(chan int64, 1)
			repCh := make(chan SideReport, 1)
			go func() { repCh <- streamRecv(dconn, spec, senderDone) }()
			m, err := waitFor(cc, "sender_report", trialWait)
			if err != nil {
				return row{}, err
			}
			peerRep = *m.Report
			senderDone <- peerRep.Bytes
			localRep = <-repCh
		}

	case spec.isUDP():
		raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, cfg.UDPPort))
		if err != nil {
			return row{}, err
		}
		// Connected socket from birth: see udpSender for why connected matters.
		uc, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			return row{}, err
		}
		defer uc.Close()
		sbuf, rbuf = applyBufs(uc, spec.SockBuf)
		hello := make([]byte, 9)
		hello[0] = 'H'
		for i := 0; i < 8; i++ {
			hello[1+i] = byte(spec.TrialID >> (56 - 8*i))
		}
		for i := 0; i < 5; i++ {
			_, _ = uc.Write(hello)
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := waitFor(cc, "udp_ready", 30*time.Second); err != nil {
			return row{}, err
		}
		if spec.Dir == "up" {
			localRep = udpSend(&udpSender{c: uc}, spec)
			if err := cc.send(ctrlMsg{Cmd: "sender_report", Report: &localRep}); err != nil {
				return row{}, err
			}
			m, err := waitFor(cc, "receiver_report", trialWait)
			if err != nil {
				return row{}, err
			}
			peerRep = *m.Report
		} else {
			st := &udpRecvState{}
			repCh := make(chan SideReport, 1)
			go func() { repCh <- udpRecv(uc, spec, st, func() { _, _ = uc.Write([]byte{'K'}) }) }()
			m, err := waitFor(cc, "sender_report", trialWait)
			if err != nil {
				return row{}, err
			}
			peerRep = *m.Report
			st.senderTotal.Store(peerRep.Dgrams)
			st.senderSeen.Store(true)
			localRep = <-repCh
		}
	default:
		return row{}, fmt.Errorf("unknown lane %q", spec.Lane)
	}

	if err := cc.send(ctrlMsg{Cmd: "trial_done"}); err != nil {
		return row{}, err
	}
	return buildRow(cfg, spec, localRep, peerRep, serverOS, rtt, sbuf, rbuf, ccEff), nil
}

// underlying unwraps server-side peekedConn (and any RawSocketConn wrapper) down
// to the socket-backed conn, so sockopt work (TCP_CONGESTION, SO_*BUF) reaches a
// real fd instead of silently reporting unsupported.
func underlying(c net.Conn) net.Conn {
	if p, ok := c.(*peekedConn); ok {
		return p.Conn
	}
	type rawer interface{ RawSocketConn() net.Conn }
	if r, ok := c.(rawer); ok {
		return r.RawSocketConn()
	}
	return c
}

func buildRow(cfg Config, spec *TrialSpec, local, peer SideReport, serverOS string, rtt time.Duration, sbuf, rbuf int, ccEffClient string) row {
	clientSends := spec.Dir == "up"
	snd, rcv := local, peer
	if !clientSends {
		snd, rcv = peer, local
	}
	r := row{
		RunID: cfg.RunID, Path: cfg.Path, Label: cfg.Label,
		Lane: spec.Lane, Dir: spec.Dir, Secure: spec.Lane == "quic-secure",
		BytesTarget: spec.Bytes, BytesOK: rcv.Bytes,
		Reorder: rcv.Reorder, RTTms: float64(rtt) / 1e6,
		Dgram: spec.Dgram, Write: spec.Write, Batch: spec.Batch,
		GSOSegs: spec.GSOSegs, GRO: spec.GRO,
		RateTarget: spec.RateMbit, SockbufReq: spec.SockBuf,
		CCReq: spec.CC, OSSnd: snd.OS, OSRcv: rcv.OS,
		Bound: snd.Bound, Status: "ok",
		ErrSnd: snd.Err, ErrRcv: rcv.Err,
	}
	if snd.Err != "" || rcv.Err != "" {
		r.Status = "error"
	}
	wall := rcv.WallNS
	if spec.isUDP() {
		wall = snd.WallNS
	}
	if wall > 0 {
		r.WallS = float64(wall) / 1e9
		r.GoodputMbit = float64(rcv.Bytes) * 8 / (float64(wall) / 1e9) / 1e6
		r.MBps = float64(rcv.Bytes) / (float64(wall) / 1e9) / 1e6
	}
	if snd.WallNS > 0 {
		r.RateSent = float64(snd.Bytes) * 8 / (float64(snd.WallNS) / 1e9) / 1e6
		r.PPSSnd = float64(snd.Dgrams) / (float64(snd.WallNS) / 1e9)
	}
	if rcv.WallNS > 0 {
		r.PPSRcv = float64(rcv.Dgrams) / (float64(rcv.WallNS) / 1e9)
	}
	if spec.isUDP() && snd.Dgrams > 0 {
		lost := float64(snd.Dgrams) - float64(rcv.Dgrams)
		if lost < 0 {
			lost = 0
		}
		r.LossPct = lost / float64(snd.Dgrams) * 100
	}
	r.CPUSndUserS = float64(snd.CPUUserNS) / 1e9
	r.CPUSndSysS = float64(snd.CPUSysNS) / 1e9
	r.CPURcvUserS = float64(rcv.CPUUserNS) / 1e9
	r.CPURcvSysS = float64(rcv.CPUSysNS) / 1e9
	if gib := float64(rcv.Bytes) / (1 << 30); gib > 0.01 {
		r.CPUSndPerGiB = (r.CPUSndUserS + r.CPUSndSysS) / gib
		r.CPURcvPerGiB = (r.CPURcvUserS + r.CPURcvSysS) / gib
	}
	// Effective sockbuf/cc: report the sender side's socket.
	if clientSends {
		r.SockbufSnd, r.SockbufRcv = sbuf, rbuf
		r.CCEff = ccEffClient
	} else {
		r.SockbufSnd, r.SockbufRcv = peer.SockbufSnd, peer.SockbufRcv
		r.CCEff = peer.CCEff
	}
	// Batch capability lives on the sender; other fallbacks may come from either side.
	if spec.Lane == "udp-batch" {
		r.Fallback = snd.Fallback
	} else if snd.Fallback != "" {
		r.Fallback = snd.Fallback
	} else {
		r.Fallback = rcv.Fallback
	}
	return r
}
