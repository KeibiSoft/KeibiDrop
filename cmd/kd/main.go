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
	"golang.org/x/term"
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

// promptPassphraseFromTTY reads a passphrase from the controlling terminal
// without echoing the input. Used for Tier 2 (passphrase-protect) key loading.
//
// Uses os.Stdin.Fd() rather than syscall.Stdin so the call is portable —
// syscall.Stdin is an int on Unix but a Handle (uintptr) on Windows, and
// would not compile or behave correctly across both. os.Stdin.Fd() returns
// the platform-appropriate file descriptor / handle that term.ReadPassword
// knows how to handle.
func promptPassphraseFromTTY() (string, error) {
	fmt.Fprint(os.Stderr, "Passphrase: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(pw), nil
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

	if !cfg.Incognito {
		opts := common.EnableOpts{
			PassphraseProtect: cfg.PassphraseProtect,
		}
		if cfg.PassphraseProtect {
			opts.PassphraseProvider = promptPassphraseFromTTY
		}
		if err := kd.EnablePersistentIdentity(config.ConfigDir(), opts); err != nil {
			logger.Warn("Failed to enable persistent identity, using ephemeral", "error", err)
		}
	}

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

	case "contacts":
		if kd.AddressBook == nil {
			return errResponse("no address book (incognito mode)")
		}
		contacts := kd.AddressBook.List()
		type contactInfo struct {
			Name        string `json:"name"`
			Fingerprint string `json:"fingerprint"`
			LastSeen    string `json:"last_seen,omitempty"`
		}
		out := make([]contactInfo, len(contacts))
		for i, c := range contacts {
			ci := contactInfo{Name: c.Name, Fingerprint: c.Fingerprint}
			if !c.LastSeen.IsZero() {
				ci.LastSeen = c.LastSeen.Format(time.RFC3339)
			}
			out[i] = ci
		}
		return okResponse(out)

	case "add-contact":
		if len(req.Args) < 2 {
			return errResponse("usage: kd add-contact <name> <fingerprint>")
		}
		if kd.AddressBook == nil {
			return errResponse("no address book (incognito mode)")
		}
		name := req.Args[0]
		fp := strings.Join(req.Args[1:], "")
		if err := kd.AddressBook.Add(name, fp); err != nil {
			return errResponse(err.Error())
		}
		if err := kd.AddressBook.Save(); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"added": name})

	case "remove-contact":
		if len(req.Args) < 1 {
			return errResponse("usage: kd remove-contact <fingerprint>")
		}
		if kd.AddressBook == nil {
			return errResponse("no address book (incognito mode)")
		}
		if err := kd.AddressBook.Remove(req.Args[0]); err != nil {
			return errResponse(err.Error())
		}
		if err := kd.AddressBook.Save(); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"removed": req.Args[0]})

	case "quick-connect":
		if len(req.Args) < 1 {
			return errResponse("usage: kd quick-connect <fingerprint>")
		}
		go func() {
			_ = kd.ConnectToContact(req.Args[0])
		}()
		return okResponse(map[string]string{"status": "connecting"})

	case "save-contact":
		if len(req.Args) < 1 {
			return errResponse("usage: kd save-contact <name>")
		}
		if err := kd.SaveCurrentPeerAsContact(req.Args[0]); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"saved": req.Args[0]})

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
		data["incognito"] = fmt.Sprintf("%v", cfg.Incognito)
		if kd.Identity != nil {
			data["identity_mode"] = "persistent"
		} else {
			data["identity_mode"] = "ephemeral"
		}
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
  kd stop                        Shutdown daemon.
  kd show [what]                 Show info (fingerprint, ip, peer, relay, status, config, or all).
  kd register <fingerprint>      Register peer's fingerprint.
  kd connect                     Connect (auto role via fingerprint tiebreak).
  kd create                      Create a room (you are the initiator).
  kd join                        Join a room (peer must have created first).
  kd add <filepath>              Share a file with the peer.
  kd list                        List all shared files (local + remote).
  kd pull <name> [local-path]    Download a remote file.
  kd status                      Connection status and session info.
  kd disconnect                  Disconnect and reset session.
  kd contacts                    List saved contacts (JSON array).
  kd add-contact <name> <fp>     Save a contact.
  kd remove-contact <fp>         Remove a saved contact.
  kd quick-connect <fp>          Connect to a saved contact (1-click).
  kd save-contact <name>         Save current peer as contact.
  kd help                        Show this help.

ENVIRONMENT (for "kd start"):
  KD_RELAY                Relay URL        (default: https://keibidroprelay.keibisoft.com)
  KD_INBOUND_PORT         Listen port      (default: 26431)
  KD_OUTBOUND_PORT        Outbound port    (default: 26432)
  KD_SAVE_PATH            Where to save received files
  KD_MOUNT_PATH           FUSE mount point
  KD_NO_FUSE              Set to disable FUSE (any value)
  KD_LOG_FILE             Log file path    (default: stderr)
  KD_INCOGNITO            Force ephemeral mode, no identity saved (any value)
  KD_PASSPHRASE_PROTECT   Prompt for passphrase to encrypt identity (any value)
  KD_SOCKET               Unix socket path (default: /tmp/kd.sock)

EXAMPLE (first connection):
  KD_SAVE_PATH=./received KD_NO_FUSE=1 kd start
  kd show fingerprint              # send this to your peer
  kd register <peer-fingerprint>   # paste theirs
  kd connect                       # both peers run this
  kd add ./myfile.pdf              # share a file
  kd list                          # see remote files
  kd pull somefile.txt ./local.txt # download
  kd save-contact Alice            # save peer for next time
  kd disconnect
  kd stop

EXAMPLE (reconnect with saved contact):
  KD_SAVE_PATH=./received KD_NO_FUSE=1 kd start
  kd contacts                      # list saved contacts
  kd quick-connect <fingerprint>   # 1-click, no code exchange
  kd list
  kd disconnect
  kd stop

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
