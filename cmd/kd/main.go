// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: kd is a non-interactive CLI for KeibiDrop, designed for AI agents.
// ABOUTME: "kd start" runs a daemon; all other commands talk to it via Unix socket.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"time"

	"github.com/KeibiSoft/KeibiDrop/cmd/internal/checkfuse"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/discovery"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

// socketPath returns the Unix socket path for daemon<->client communication.
func socketPath() string {
	if s := os.Getenv("KD_SOCKET"); s != "" {
		return s
	}
	return filepath.Join(os.TempDir(), "kd.sock")
}

// --- JSON protocol ---

type Request struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type Response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func okResponse(data any) Response {
	b, _ := json.Marshal(data)
	return Response{OK: true, Data: b}
}

func errResponse(msg string) Response {
	return Response{OK: false, Error: msg}
}

// --- Daemon ---

func runDaemon() {
	sock := socketPath()

	// Clean up stale socket
	if _, err := os.Stat(sock); err == nil {
		// Try connecting to see if another daemon is running
		conn, err := net.Dial("unix", sock)
		if err == nil {
			_ = conn.Close()
			fmt.Fprintf(os.Stderr, `{"ok":false,"error":"daemon already running at %s"}`+"\n", sock)
			os.Exit(1)
		}
		_ = os.Remove(sock)
	}

	// Load config (defaults → config file → env vars).
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"config: %s"}`+"\n", err)
		os.Exit(1)
	}
	_ = config.WriteDefault()
	if err := config.EnsureDirectories(cfg); err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"directories: %s"}`+"\n", err)
		os.Exit(1)
	}

	relayURL, err := url.Parse(cfg.Relay)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"invalid relay: %s"}`+"\n", err)
		os.Exit(1)
	}

	isFuse := checkfuse.IsFUSEPresent() && !cfg.NoFUSE
	isLocal := os.Getenv("KD_LOCAL") != ""

	// Setup logger
	logWriter := os.Stderr
	if cfg.LogFile != "" {
		f, err := os.OpenFile(filepath.Clean(cfg.LogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			logWriter = f
			defer f.Close()
		}
	}
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelDebug})).With("component", "kd")

	// Create KeibiDrop instance
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := common.NewKeibiDrop(ctx, logger, isFuse, relayURL,
		cfg.InboundPort, cfg.OutboundPort, cfg.MountPath, cfg.SavePath,
		cfg.PrefetchOnOpen, cfg.PushOnWrite)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"init failed: %s"}`+"\n", err)
		os.Exit(1) //nolint:gocritic
	}
	kd.IsLocalMode = isLocal
	kd.BridgeAddr = cfg.BridgeAddr
	kd.StrictMode = cfg.StrictMode
	go kd.Run()

	// Listen on Unix socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"socket listen: %s"}`+"\n", err)
		os.Exit(1)
	}
	defer ln.Close()
	defer os.Remove(sock)

	// Handle signals for clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fp, _ := kd.ExportFingerprint()
	startData := map[string]any{
		"socket":      sock,
		"fingerprint": fp,
		"relay":       cfg.Relay,
		"ip":          kd.LocalIPv6IP,
		"fuse":        isFuse,
		"save_path":   cfg.SavePath,
		"mount_path":  cfg.MountPath,
		"log_file":    cfg.LogFile,
		"config_path": config.ConfigPath(),
	}
	if isLocal {
		addr, _ := common.GetLinkLocalAddress(kd.InboundPort())
		startData["local_mode"] = true
		startData["local_address"] = addr
	}
	b, _ := json.Marshal(Response{OK: true, Data: mustMarshal(startData)})
	fmt.Println(string(b))

	var wg sync.WaitGroup
	go func() {
		<-sigCh
		logger.Info("Shutting down")
		kd.NotifyDisconnect()
		_ = kd.UnmountFilesystem()
		kd.Shutdown()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			break // listener closed
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConn(conn, kd, cancel, ln)
		}()
	}
	wg.Wait()
}

func handleConn(conn net.Conn, kd *common.KeibiDrop, cancel context.CancelFunc, ln net.Listener) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}

	var req Request
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		writeResponse(conn, errResponse("invalid request JSON"))
		return
	}

	resp := dispatch(kd, req, cancel, ln)
	writeResponse(conn, resp)
}

func dispatch(kd *common.KeibiDrop, req Request, cancel context.CancelFunc, ln net.Listener) Response {
	switch req.Command {
	case "show":
		return cmdShow(kd, req.Args)

	case "register":
		if len(req.Args) < 1 {
			return errResponse("usage: kd register <fingerprint-or-address>")
		}
		if kd.IsLocalMode {
			err := kd.SetPeerDirectAddress(req.Args[0])
			if err != nil {
				return errResponse(err.Error())
			}
			return okResponse(map[string]string{"registered_address": req.Args[0]})
		}
		err := kd.AddPeerFingerprint(req.Args[0])
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"registered": req.Args[0]})

	case "discover":
		return cmdDiscover(kd)

	case "create":
		return cmdCreateOrJoin(kd, "create")

	case "join":
		return cmdCreateOrJoin(kd, "join")

	case "connect":
		return cmdConnect(kd)

	case "add":
		if len(req.Args) < 1 {
			return errResponse("usage: kd add <filepath>")
		}
		err := kd.AddFile(req.Args[0])
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"added": req.Args[0]})

	case "list":
		return cmdList(kd)

	case "pull":
		if len(req.Args) < 1 {
			return errResponse("usage: kd pull <remote-name> [local-path]")
		}
		remoteName := req.Args[0]
		localPath := remoteName
		if len(req.Args) >= 2 {
			localPath = req.Args[1]
		}
		err := kd.PullFile(remoteName, localPath)
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"pulled": remoteName, "to": localPath})

	case "status":
		return cmdStatus(kd)

	case "disconnect":
		kd.NotifyDisconnect()
		_ = kd.UnmountFilesystem()
		kd.Stop()
		fp, _ := kd.ExportFingerprint()
		return okResponse(map[string]string{
			"status":          "disconnected",
			"new_fingerprint": fp,
		})

	case "export-logs":
		dest := "keibidrop-sanitized.log"
		if len(req.Args) > 0 {
			dest = req.Args[0]
		}
		logCfg, err := config.Load()
		if err != nil {
			return errResponse(err.Error())
		}
		if err := common.SanitizeLogsToFile(logCfg.LogFile, dest); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"path": dest})

	case "stop", "quit":
		kd.NotifyDisconnect()
		_ = kd.UnmountFilesystem()
		kd.Shutdown()
		go func() {
			_ = ln.Close()
		}()
		return okResponse(map[string]string{"status": "stopped"})

	default:
		return errResponse(fmt.Sprintf("unknown command: %s", req.Command))
	}
}

// isShowAll reports whether `kd show <args>` should show every field.
// Both `kd show` (no args) and `kd show all` qualify.
func isShowAll(args []string) bool {
	return len(args) == 0 || (len(args) == 1 && args[0] == "all")
}

func cmdShow(kd *common.KeibiDrop, args []string) Response {
	data := map[string]string{}

	showAll := isShowAll(args)

	what := ""
	if len(args) > 0 {
		what = strings.Join(args, " ")
	}

	if showAll || what == "fingerprint" {
		if kd.IsLocalMode {
			addr, err := common.GetLinkLocalAddress(kd.InboundPort())
			if err != nil {
				return errResponse(err.Error())
			}
			data["local_address"] = addr
		} else {
			fp, err := kd.ExportFingerprint()
			if err != nil {
				return errResponse(err.Error())
			}
			data["fingerprint"] = fp
		}
	}
	if showAll || what == "ip" {
		data["ip"] = kd.LocalIPv6IP
	}
	if showAll || what == "peer" || what == "peer fingerprint" {
		pfp, _ := kd.GetPeerFingerprint()
		data["peer_fingerprint"] = pfp
	}
	if showAll || what == "peer ip" {
		data["peer_ip"] = kd.PeerIPv6IP
	}
	if showAll || what == "relay" {
		data["relay"] = kd.RelayEndoint.String()
	}
	if showAll || what == "status" {
		data["connected"] = fmt.Sprintf("%v", kd.IsRunning())
		data["connection_status"] = kd.ConnectionStatus()
	}
	if showAll || what == "config" {
		cfg, _ := config.Load()
		data["config_path"] = config.ConfigPath()
		data["relay"] = cfg.Relay
		data["save_path"] = cfg.SavePath
		data["mount_path"] = cfg.MountPath
		data["log_file"] = cfg.LogFile
		data["inbound_port"] = fmt.Sprintf("%d", cfg.InboundPort)
		data["outbound_port"] = fmt.Sprintf("%d", cfg.OutboundPort)
		data["bridge_addr"] = cfg.BridgeAddr
		data["no_fuse"] = fmt.Sprintf("%v", cfg.NoFUSE)
		data["strict_mode"] = fmt.Sprintf("%v", cfg.StrictMode)
	}

	if len(data) == 0 {
		return errResponse(fmt.Sprintf("unknown show target: %s", what))
	}
	return okResponse(data)
}

func cmdDiscover(kd *common.KeibiDrop) Response {
	kd.IsLocalMode = true
	disc := discovery.New(kd.InboundPort(), slog.Default())
	_ = disc.Start()
	defer disc.Stop()

	// Wait for beacons
	time.Sleep(6 * time.Second)
	peers := disc.Peers()
	if len(peers) == 0 {
		time.Sleep(4 * time.Second)
		peers = disc.Peers()
	}

	result := make([]map[string]string, 0, len(peers))
	for _, p := range peers {
		result = append(result, map[string]string{
			"name": p.Name,
			"addr": p.Addr,
		})
	}
	return okResponse(map[string]any{
		"my_name": disc.Name(),
		"peers":   result,
	})
}

func cmdCreateOrJoin(kd *common.KeibiDrop, mode string) Response {
	if kd.OpInProgress.Add(1) != 1 {
		kd.OpInProgress.Add(-1)
		return errResponse("create/join already in progress")
	}
	defer kd.OpInProgress.Add(-1)

	var err error
	if mode == "create" {
		err = kd.CreateRoom()
	} else {
		err = kd.JoinRoom()
	}
	if err != nil {
		return errResponse(err.Error())
	}

	return okResponse(map[string]string{
		"status":  "connected",
		"peer_ip": kd.PeerIPv6IP,
	})
}

func cmdConnect(kd *common.KeibiDrop) Response {
	if kd.OpInProgress.Add(1) != 1 {
		kd.OpInProgress.Add(-1)
		return errResponse("operation already in progress")
	}
	defer kd.OpInProgress.Add(-1)

	if err := kd.Connect(); err != nil {
		return errResponse(err.Error())
	}

	return okResponse(map[string]string{
		"status":  "connected",
		"peer_ip": kd.PeerIPv6IP,
		"mode":    kd.ConnectionMode,
	})
}

func cmdList(kd *common.KeibiDrop) Response {
	type fileInfo struct {
		Name   string `json:"name"`
		Size   uint64 `json:"size"`
		Path   string `json:"path"`
		Source string `json:"source"` // "local" or "remote"
	}

	var files []fileInfo

	kd.SyncTracker.LocalFilesMu.RLock()
	for k, v := range kd.SyncTracker.LocalFiles {
		files = append(files, fileInfo{
			Name:   k,
			Size:   v.Size,
			Path:   v.RealPathOfFile,
			Source: "local",
		})
	}
	kd.SyncTracker.LocalFilesMu.RUnlock()

	kd.SyncTracker.RemoteFilesMu.RLock()
	for k, v := range kd.SyncTracker.RemoteFiles {
		files = append(files, fileInfo{
			Name:   k,
			Size:   v.Size,
			Path:   v.RealPathOfFile,
			Source: "remote",
		})
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()

	if files == nil {
		files = []fileInfo{}
	}
	return okResponse(map[string]any{"files": files})
}

func cmdStatus(kd *common.KeibiDrop) Response {
	fp, _ := kd.ExportFingerprint()
	pfp, _ := kd.GetPeerFingerprint()

	data := map[string]any{
		"running":           kd.IsRunning(),
		"connection_status": kd.ConnectionStatus(),
		"fingerprint":       fp,
		"peer_fingerprint":  pfp,
		"ip":                kd.LocalIPv6IP,
		"peer_ip":           kd.PeerIPv6IP,
		"connection_mode":   kd.ConnectionMode,
		"relay":             kd.RelayEndoint.String(),
		"fuse":              kd.IsFUSE,
		"mount_path":        kd.ToMount,
		"save_path":         kd.ToSave,
	}

	// File counts
	kd.SyncTracker.LocalFilesMu.RLock()
	data["local_files"] = len(kd.SyncTracker.LocalFiles)
	kd.SyncTracker.LocalFilesMu.RUnlock()

	kd.SyncTracker.RemoteFilesMu.RLock()
	data["remote_files"] = len(kd.SyncTracker.RemoteFiles)
	kd.SyncTracker.RemoteFilesMu.RUnlock()

	return okResponse(data)
}

// --- Client ---

func runClient(cmd string, args []string) {
	sock := socketPath()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"daemon not running (socket: %s)"}`+"\n", sock)
		os.Exit(1)
	}
	defer conn.Close()

	req := Request{Command: cmd, Args: args}
	b, _ := json.Marshal(req)
	_, _ = fmt.Fprintf(conn, "%s\n", b)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for large responses
	if scanner.Scan() {
		fmt.Println(scanner.Text())
	}
}

// --- Helpers ---

func writeResponse(conn net.Conn, resp Response) {
	b, _ := json.Marshal(resp)
	_, _ = fmt.Fprintf(conn, "%s\n", b)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func printHelp() {
	help := `kd - KeibiDrop CLI for agents

USAGE:
  kd start                       Start daemon (foreground). Configure via env vars.
  kd stop                        Stop the daemon.
  kd show [what]                 Show info (fingerprint, ip, peer, relay, status, or all).
  kd register <fingerprint>      Register peer's fingerprint.
  kd create                      Create a room (you are the initiator).
  kd join                        Join a room (peer must have created first).
  kd add <filepath>              Share a file with the peer.
  kd list                        List all shared files (local + remote).
  kd pull <name> [local-path]    Download a remote file.
  kd status                      Connection status and session info.
  kd disconnect                  Disconnect (keys rotate, ready for new session).
  kd stop                        Shutdown daemon.
  kd help                        Show this help.

ENVIRONMENT (for "kd start"):
  KD_RELAY           Relay URL        (default: https://keibidroprelay.keibisoft.com)
  KD_INBOUND_PORT    Listen port      (default: 26431)
  KD_OUTBOUND_PORT   Outbound port    (default: 26432)
  KD_SAVE_PATH       Where to save received files
  KD_MOUNT_PATH      FUSE mount point
  KD_NO_FUSE         Set to disable FUSE (any value)
  KD_LOG_FILE        Log file path    (default: stderr)
  KD_SOCKET          Unix socket path (default: /tmp/kd.sock)

MODES:
  FUSE mode:    Files appear in KD_MOUNT_PATH as a virtual folder.
                Read/write files directly from the mount (like a shared drive).
                After connecting, "kd status" shows the mount_path.

  no-FUSE mode: Use "kd add <file>" to share and "kd pull <name> [path]" to download.
                Set KD_NO_FUSE=1 to use this mode.

EXAMPLE (agent workflow, no-FUSE):
  # Terminal 1: start daemon
  KD_SAVE_PATH=./received KD_NO_FUSE=1 kd start

  # Terminal 2 (or from agent):
  kd show fingerprint              # get your fingerprint, send to peer
  kd register <peer-fingerprint>   # paste peer's fingerprint
  kd create                        # or "kd join" if peer creates
  kd add ./myfile.pdf              # share a file
  kd list                          # see remote files
  kd pull somefile.txt ./local.txt # download from peer
  kd disconnect                    # done, keys rotated
  kd stop                          # shutdown daemon

EXAMPLE (agent workflow, FUSE):
  # Terminal 1: start daemon with FUSE mount
  KD_SAVE_PATH=./saved KD_MOUNT_PATH=./mount kd start

  # Terminal 2 (or from agent):
  kd show fingerprint              # get your fingerprint, send to peer
  kd register <peer-fingerprint>   # paste peer's fingerprint
  kd create                        # or "kd join" if peer creates
  # After connecting, peer's files appear in ./mount/
  ls ./mount/                      # see shared files from peer
  cat ./mount/readme.txt           # read a remote file directly
  cp ./myfile.pdf ./mount/         # share a file (copy into mount)
  kd disconnect                    # done
  kd stop                          # shutdown daemon

All output is JSON: {"ok":true,"data":{...}} or {"ok":false,"error":"..."}
Use "kd status" after connecting to see mount_path, save_path, and peer info.`

	fmt.Println(help)
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(0)
	}

	cmd := os.Args[1]
	switch cmd {
	case "start":
		runDaemon()
	case "help", "--help", "-h":
		printHelp()
	default:
		runClient(cmd, os.Args[2:])
	}
}
