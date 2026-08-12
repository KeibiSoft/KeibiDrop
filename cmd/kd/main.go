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

var eventCh = make(chan string, 64)

// Local mode discovery state. The names give a deterministic role tiebreak.
var (
	localMyName       string
	localPeerName     string
	discoveredPeerMap map[string]string // addr → name
	daemonDisc        *discovery.Service
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

// promptPassphraseFromTTY reads a passphrase from the terminal without echo.
// It uses os.Stdin.Fd(), not syscall.Stdin, because syscall.Stdin is a Handle on Windows.
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

	// Remove a stale socket from a dead daemon.
	if _, err := os.Stat(sock); err == nil {
		// If the dial succeeds, another daemon owns the socket.
		conn, err := net.Dial("unix", sock)
		if err == nil {
			_ = conn.Close()
			fmt.Fprintf(os.Stderr, `{"ok":false,"error":"daemon already running at %s"}`+"\n", sock)
			os.Exit(1)
		}
		_ = os.Remove(sock)
	}

	// Config precedence: defaults, then config file, then env vars.
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

	logWriter := os.Stderr
	if cfg.LogFile != "" {
		f, err := os.OpenFile(filepath.Clean(cfg.LogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			logWriter = f
			defer f.Close()
		}
	}
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelDebug})).With("component", "kd")

	// KD_PPROF starts a pprof server for heap and goroutine profiles.
	// The debug build tag gates it; a plain build gets a no-op.
	maybeStartPprof(logger)

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
	kd.AutoCache = cfg.LiveCollab // live_collab sets macFUSE auto_cache for same-size live edits on macOS.
	kd.PrefetchAutoMB = cfg.PrefetchAutoMB
	kd.ReadAheadWindowMB = cfg.ReadAheadWindowMB
	for _, warn := range cfg.Warnings() {
		logger.Warn("config flag note", "note", warn)
	}

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

	kd.OnEvent = func(evt string) {
		select {
		case eventCh <- evt:
		default:
			select {
			case <-eventCh:
			default:
			}
			eventCh <- evt
		}
	}

	if !cfg.Incognito && kd.Identity != nil && kd.AddressBook != nil && kd.AddressBook.Count() > 0 {
		go kd.StartPresenceHeartbeat(ctx)
	}

	go kd.Run()

	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"ok":false,"error":"socket listen: %s"}`+"\n", err)
		os.Exit(1)
	}
	defer ln.Close()
	defer os.Remove(sock)

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
			// Look up the peer name from discovery results for the role tiebreak.
			if name, ok := discoveredPeerMap[req.Args[0]]; ok {
				localPeerName = name
			} else if len(req.Args) >= 2 {
				localPeerName = req.Args[1]
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

	case "connect-lan":
		if len(req.Args) < 2 {
			return errResponse("usage: connect-lan <peer-name> <peer-addr>")
		}
		peerName := req.Args[0]
		peerAddr := req.Args[1]
		if peerName == "" {
			return errResponse("usage: connect-lan <peer-name> <peer-addr>")
		}
		kd.IsLocalMode = true
		if err := kd.SetPeerDirectAddress(peerAddr); err != nil {
			return errResponse(err.Error())
		}
		// Tiebreak on the advertised daemonDisc name, not a throwaway instance.
		// A name the peer never saw lets both sides pick the same role and deadlock.
		myName := ""
		if daemonDisc != nil {
			myName = daemonDisc.Name()
		} else {
			disc := discovery.New(kd.InboundPort(), slog.Default())
			myName = disc.Name()
			disc.Stop()
		}
		if common.DecideLocalRole(myName, peerName, peerAddr) {
			return cmdCreateOrJoin(kd, "create")
		}
		return cmdCreateOrJoin(kd, "join")

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
		err := kd.AddFile(filepath.Clean(req.Args[0]))
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
			localPath = filepath.Clean(req.Args[1])
		}
		err := kd.PullFile(remoteName, localPath)
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"pulled": remoteName, "to": localPath})

	case "bench-pull":
		if len(req.Args) < 1 {
			return errResponse("usage: kd bench-pull <remote-name> [local-path]")
		}
		remoteName := req.Args[0]
		localPath := filepath.Clean(filepath.Join(os.TempDir(), "kd-bench-"+filepath.Base(remoteName)))
		if len(req.Args) >= 2 {
			localPath = filepath.Clean(req.Args[1])
		}
		kd.SyncTracker.RemoteFilesMu.RLock()
		f, ok := kd.SyncTracker.RemoteFiles[remoteName]
		if !ok {
			f, ok = kd.SyncTracker.RemoteFiles["/"+remoteName]
		}
		var fileSize uint64
		if ok {
			fileSize = f.Size
		}
		kd.SyncTracker.RemoteFilesMu.RUnlock()
		start := time.Now()
		pullErr := kd.PullFile(remoteName, localPath)
		elapsed := time.Since(start)
		var actualSize int64
		if fi, statErr := os.Stat(localPath); statErr == nil {
			actualSize = fi.Size()
		}
		_ = os.Remove(localPath)
		_ = os.Remove(localPath + ".kdbitmap")
		transferred := uint64(actualSize)
		if transferred == 0 {
			transferred = fileSize
		}
		mbps := float64(transferred) / elapsed.Seconds() / (1024 * 1024)
		result := map[string]any{
			"file":        remoteName,
			"size":        fileSize,
			"transferred": transferred,
			"duration":    elapsed.String(),
			"mb_per_s":    fmt.Sprintf("%.1f", mbps),
			"mode":        kd.ConnectionMode,
		}
		if pullErr != nil {
			result["error"] = pullErr.Error()
		}
		return okResponse(result)

	case "status":
		return cmdStatus(kd)

	case "disconnect":
		if daemonDisc != nil {
			daemonDisc.Stop()
			daemonDisc = nil
		}
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
			Online      bool   `json:"online"`
			LastSeen    string `json:"last_seen,omitempty"`
		}
		out := make([]contactInfo, len(contacts))
		for i, c := range contacts {
			ci := contactInfo{
				Name:        c.Name,
				Fingerprint: c.Fingerprint,
				Online:      kd.CheckContactPresence(c.Fingerprint),
			}
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

	case "unshare":
		if len(req.Args) < 1 {
			return errResponse("usage: kd unshare <filename>")
		}
		if err := kd.UnshareFile(req.Args[0]); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"unshared": req.Args[0]})

	case "add-as":
		if len(req.Args) < 2 {
			return errResponse("usage: kd add-as <local-path> <remote-name>")
		}
		if err := kd.AddFileAs(filepath.Clean(req.Args[0]), req.Args[1]); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{
			"added":       req.Args[0],
			"remote_name": req.Args[1],
		})

	case "cancel-download":
		if len(req.Args) < 1 {
			return errResponse("usage: kd cancel-download <name>")
		}
		if err := kd.CancelDownload(req.Args[0]); err != nil {
			return errResponse(err.Error())
		}
		return okResponse(map[string]string{"cancelled": req.Args[0]})

	case "progress":
		if len(req.Args) < 1 {
			return errResponse("usage: kd progress <name>")
		}
		p := kd.GetDownloadProgress(req.Args[0])
		return okResponse(map[string]any{
			"name":     req.Args[0],
			"progress": int(p * 100),
		})

	case "poll-event":
		select {
		case evt := <-eventCh:
			return okResponse(map[string]string{"event": evt})
		default:
			return okResponse(map[string]string{"event": ""})
		}

	case "tokens":
		sub := "list"
		if len(req.Args) > 0 {
			sub = req.Args[0]
		}
		switch sub {
		case "add":
			if len(req.Args) != 2 {
				return errResponse("usage: tokens add <code>")
			}
			gb, err := kd.TokensAdd(req.Args[1])
			if err != nil {
				return errResponse(err.Error())
			}
			return okResponse(map[string]any{
				"added_gb": gb,
				"warning":  "this code is like cash: anyone holding it can spend it, and it cannot be recovered if lost",
			})
		case "balance":
			return okResponse(map[string]any{"chains": kd.TokensRefreshBalances(), "buy_url": common.TokensBuyURL})
		case "list":
			return okResponse(map[string]any{"chains": kd.TokensSummaries(), "buy_url": common.TokensBuyURL})
		case "buy":
			return okResponse(map[string]any{
				"url":  kd.TokensBuyStart(),
				"note": "open the url and pay; the code adds itself while this daemon runs, then a tokens_added event fires",
			})
		default:
			return errResponse("usage: tokens [list|add <code>|balance|buy]")
		}

	case "incognito":
		if len(req.Args) < 1 {
			if kd.Incognito {
				return okResponse(map[string]string{"incognito": "true"})
			}
			return okResponse(map[string]string{"incognito": "false"})
		}
		enable := req.Args[0] == "true" || req.Args[0] == "1" || req.Args[0] == "on"
		newFP, err := kd.ToggleIncognito(enable, config.ConfigDir())
		if err != nil {
			return errResponse(err.Error())
		}
		fp := newFP
		if fp == "" {
			fp, _ = kd.ExportFingerprint()
		}
		return okResponse(map[string]string{
			"incognito":   fmt.Sprintf("%v", enable),
			"fingerprint": fp,
		})

	case "peer-info":
		pfp, _ := kd.GetPeerFingerprint()
		data := map[string]any{
			"peer_fingerprint": pfp,
			"peer_ip":          kd.PeerIPv6IP,
			"connection_mode":  kd.ConnectionMode,
			"peer_persistent":  kd.IsPeerPersistent(),
		}
		if kd.AddressBook != nil && pfp != "" && pfp != "TOFU" {
			data["peer_is_contact"] = kd.AddressBook.Lookup(pfp) != nil
		}
		return okResponse(data)

	case "version":
		return okResponse(map[string]string{
			"version": common.Version,
			"commit":  common.CommitHash,
		})

	case "sanitize-logs":
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

	case "config-path":
		return okResponse(map[string]string{"path": config.ConfigPath()})

	case "log-path":
		cfg, _ := config.Load()
		return okResponse(map[string]string{"path": cfg.LogFile})

	case "file-size":
		if len(req.Args) < 1 {
			return errResponse("usage: kd file-size <name>")
		}
		name := req.Args[0]
		kd.SyncTracker.RemoteFilesMu.RLock()
		f, ok := kd.SyncTracker.RemoteFiles[name]
		if !ok {
			f, ok = kd.SyncTracker.RemoteFiles["/"+name]
		}
		kd.SyncTracker.RemoteFilesMu.RUnlock()
		if !ok {
			return errResponse("file not found: " + name)
		}
		return okResponse(map[string]any{"name": name, "size": f.Size})

	case "local-file-path":
		if len(req.Args) < 1 {
			return errResponse("usage: kd local-file-path <name>")
		}
		name := req.Args[0]
		kd.SyncTracker.LocalFilesMu.RLock()
		f, ok := kd.SyncTracker.LocalFiles[name]
		kd.SyncTracker.LocalFilesMu.RUnlock()
		if !ok {
			return errResponse("file not found: " + name)
		}
		return okResponse(map[string]string{"name": name, "path": f.RealPathOfFile})

	case "stop", "quit":
		if daemonDisc != nil {
			daemonDisc.Stop()
			daemonDisc = nil
		}
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

	// Start discovery once and keep it running. Rustbridge and mobile do the same.
	if daemonDisc == nil {
		daemonDisc = discovery.New(kd.InboundPort(), slog.Default())
		_ = daemonDisc.Start()
		// The first call waits for beacons.
		time.Sleep(6 * time.Second)
	} else {
		// Later calls only refresh the list, so wait less.
		time.Sleep(2 * time.Second)
	}

	peers := daemonDisc.Peers()
	if len(peers) == 0 {
		time.Sleep(4 * time.Second)
		peers = daemonDisc.Peers()
	}

	localMyName = daemonDisc.Name()
	discoveredPeerMap = make(map[string]string, len(peers))

	result := make([]map[string]string, 0, len(peers))
	for _, p := range peers {
		discoveredPeerMap[p.Addr] = p.Name
		result = append(result, map[string]string{
			"name": p.Name,
			"addr": p.Addr,
		})
	}
	return okResponse(map[string]any{
		"my_name": localMyName,
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

	// In local mode, use the name tiebreak. Mobile and the Rust UI use the same rule.
	// The fingerprint tiebreak in Connect() does not work with TOFU.
	if kd.IsLocalMode && localMyName != "" && localPeerName != "" {
		if common.DecideLocalRole(localMyName, localPeerName, kd.PeerIPv6IP) {
			if err := kd.CreateRoom(); err != nil {
				return errResponse(err.Error())
			}
		} else {
			if err := kd.JoinRoom(); err != nil {
				return errResponse(err.Error())
			}
		}
		return okResponse(map[string]string{
			"status":  "connected",
			"peer_ip": kd.PeerIPv6IP,
			"mode":    kd.ConnectionMode,
		})
	}

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

	kd.SyncTracker.PruneStaleLocalFiles()

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
		"writer_epoch":      kd.WriterEpoch(),
		"bridge":            kd.BridgeInfo(),
	}

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

// isLoopbackAddr reports whether addr binds only the loopback interface.
// This keeps the KD_PPROF debug endpoint off external interfaces.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
  kd add-as <path> <remote-name> Share with a custom remote name.
  kd unshare <filename>          Stop sharing a file (keeps it on disk).
  kd list                        List all shared files (local + remote).
  kd pull <name> [local-path]    Download a remote file.
  kd cancel-download <name>      Pause/cancel an active download.
  kd progress <name>             Download progress (0-100).
  kd status                      Connection status and session info.
  kd peer-info                   Peer details and connection mode.
  kd poll-event                  Pop next event from the event queue.
  kd disconnect                  Disconnect and reset session.
  kd contacts                    List saved contacts (with online status).
  kd add-contact <name> <fp>     Save a contact.
  kd remove-contact <fp>         Remove a saved contact.
  kd quick-connect <fp>          Connect to a saved contact (1-click).
  kd save-contact <name>         Save current peer as contact.
  kd incognito [on|off]          Query or toggle incognito mode.
  kd tokens [list]               Relay credit chains and balance (JSON).
  kd tokens add <code>           Paste a prepaid relay token code.
  kd tokens balance              Refresh balances from the relay.
  kd version                     Show version and commit hash.
  kd export-logs [dest]          Export sanitized logs.
  kd sanitize-logs [dest]        Alias for export-logs.
  kd config-path                 Show config file path.
  kd log-path                    Show log file path.
  kd file-size <name>            Get remote file size.
  kd local-file-path <name>      Get local file real path.
  kd bench-pull <name> [path]    Benchmark: pull file, report MB/s, cleanup.
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
