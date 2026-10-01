// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Control-channel protocol and result types for netbench. One JSON
// ABOUTME: message per line on the client-dialed control conn; one JSONL row per trial.

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"
)

type TrialSpec struct {
	Lane       string  `json:"lane"` // tcp | udp | udp-batch | udp-gso | quic | quic-secure
	Dir        string  `json:"dir"`  // up = client sends, down = server sends
	Bytes      int64   `json:"bytes"`
	DurationMS int     `json:"duration_ms"`
	RateMbit   float64 `json:"rate_mbit"`
	Dgram      int     `json:"dgram"`
	Write      int     `json:"write"`
	Batch      int     `json:"batch"`
	GSOSegs    int     `json:"gso_segs"`
	GRO        bool    `json:"gro"`
	SockBuf    int     `json:"sockbuf"`
	CC         string  `json:"cc"`
	MaxWallMS  int     `json:"max_wall_ms"`
	TrialID    uint64  `json:"trial_id"`
}

func (s *TrialSpec) isUDP() bool  { return s.Lane == "udp" || s.Lane == "udp-batch" || s.Lane == "udp-gso" }
func (s *TrialSpec) isQUIC() bool {
	return s.Lane == "quic" || s.Lane == "quic-secure" || s.Lane == "quic-conn"
}
func (s *TrialSpec) isStream() bool { return s.Lane == "tcp" || s.isQUIC() }

type SideReport struct {
	OS         string `json:"os"`
	Bytes      int64  `json:"bytes"`
	Dgrams     uint64 `json:"dgrams"`
	WallNS     int64  `json:"wall_ns"`
	Reorder    uint64 `json:"reorder"`
	CPUUserNS  int64  `json:"cpu_user_ns"`
	CPUSysNS   int64  `json:"cpu_sys_ns"`
	SockbufSnd int    `json:"sockbuf_snd"`
	SockbufRcv int    `json:"sockbuf_rcv"`
	CCEff      string `json:"cc_eff,omitempty"`
	Fallback   string `json:"fallback,omitempty"`
	Bound      string `json:"bound,omitempty"` // bytes | duration | max-wall
	Err        string `json:"err,omitempty"`
}

type ctrlMsg struct {
	Cmd    string            `json:"cmd"`
	Spec   *TrialSpec        `json:"spec,omitempty"`
	Report *SideReport       `json:"report,omitempty"`
	Info   map[string]string `json:"info,omitempty"`
	T      int64             `json:"t,omitempty"`
}

type ctrlConn struct {
	c   net.Conn
	enc *json.Encoder
	dec *json.Decoder
}

func newCtrl(c net.Conn) *ctrlConn {
	return &ctrlConn{c: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}
}

func (cc *ctrlConn) send(m ctrlMsg) error { return cc.enc.Encode(&m) }

func (cc *ctrlConn) recv(timeout time.Duration) (ctrlMsg, error) {
	var m ctrlMsg
	if timeout > 0 {
		_ = cc.c.SetReadDeadline(time.Now().Add(timeout))
		defer cc.c.SetReadDeadline(time.Time{})
	}
	err := cc.dec.Decode(&m)
	return m, err
}

// row is one JSONL result line. Client-side merge of the spec and both side reports.
type row struct {
	RunID        string  `json:"run_id"`
	Path         string  `json:"path"`
	Label        string  `json:"label,omitempty"`
	Lane         string  `json:"lane"`
	Dir          string  `json:"dir"`
	Trial        int     `json:"trial"`
	Secure       bool    `json:"secure"`
	BytesTarget  int64   `json:"bytes_target"`
	BytesOK      int64   `json:"bytes_ok"`
	WallS        float64 `json:"wall_s"`
	GoodputMbit  float64 `json:"goodput_mbit"`
	MBps         float64 `json:"mbps"`
	LossPct      float64 `json:"loss_pct"`
	Reorder      uint64  `json:"reorder"`
	PPSSnd       float64 `json:"pps_snd"`
	PPSRcv       float64 `json:"pps_rcv"`
	RTTms        float64 `json:"rtt_ms"`
	CPUSndUserS  float64 `json:"cpu_snd_user_s"`
	CPUSndSysS   float64 `json:"cpu_snd_sys_s"`
	CPURcvUserS  float64 `json:"cpu_rcv_user_s"`
	CPURcvSysS   float64 `json:"cpu_rcv_sys_s"`
	CPUSndPerGiB float64 `json:"cpu_snd_per_gib_s"`
	CPURcvPerGiB float64 `json:"cpu_rcv_per_gib_s"`
	Dgram        int     `json:"dgram"`
	Write        int     `json:"write"`
	Batch        int     `json:"batch"`
	GSOSegs      int     `json:"gso_segs"`
	GRO          bool    `json:"gro"`
	RateTarget   float64 `json:"rate_mbit_target"`
	RateSent     float64 `json:"rate_mbit_sent"`
	SockbufReq   int     `json:"sockbuf_req"`
	SockbufSnd   int     `json:"sockbuf_snd_eff"`
	SockbufRcv   int     `json:"sockbuf_rcv_eff"`
	CCReq        string  `json:"cc_req,omitempty"`
	CCEff        string  `json:"cc_eff,omitempty"`
	OSSnd        string  `json:"os_snd"`
	OSRcv        string  `json:"os_rcv"`
	Fallback     string  `json:"fallback,omitempty"`
	Bound        string  `json:"bound"`
	Status       string  `json:"status"`
	ErrSnd       string  `json:"err_snd,omitempty"`
	ErrRcv       string  `json:"err_rcv,omitempty"`
}

func appendJSONL(path string, r row) error {
	if path == "" {
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(&r)
}

func hereOS() string { return runtime.GOOS }
