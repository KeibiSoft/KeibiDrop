// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KeibiSoft/KeibiDrop/cmd/internal/checkfuse"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/discovery"
	"github.com/KeibiSoft/KeibiDrop/pkg/feedback"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	prompt "github.com/c-bata/go-prompt"
	"github.com/fatih/color"
	"golang.org/x/term"
)

var eventCh = make(chan string, 64)

// Local mode discovery state. The names give a deterministic role tiebreak.
var (
	cliLocalMyName       string
	cliLocalPeerName     string
	cliDiscoveredPeerMap = map[string]string{} // addr → name
)

func pushEvent(evt string) {
	printCreditEvent(evt)
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

// printCreditEvent surfaces credit and bridge warnings in the REPL as they
// happen. The core emits each threshold crossing once (latched, re-armed on
// top-up), so this stays one line per crossing, never a stream. Routine
// events stay in the poll-event queue.
func printCreditEvent(evt string) {
	switch {
	case strings.HasPrefix(evt, "tokens_low:"):
		color.Yellow("\n[credit] Relay credit low: %s GB left. Top up with: tokens buy", strings.TrimPrefix(evt, "tokens_low:"))
	case strings.HasPrefix(evt, "tokens_critical:"):
		color.Red("\n[credit] Relay credit nearly gone: %s GB. At zero, bridged transfers drop to the free tier (slower when the bridge is busy). Top up with: tokens buy", strings.TrimPrefix(evt, "tokens_critical:"))
	case strings.HasPrefix(evt, "tokens_exhausted:"):
		color.Red("\n[credit] Relay credit exhausted. Bridged transfers now ride the free tier (slower when the bridge is busy; direct links unaffected). Top up with: tokens buy")
	case strings.HasPrefix(evt, "tokens_chain_done:"):
		color.Yellow("\n[credit] A token chain finished; the next funded chain pays from here.")
	case strings.HasPrefix(evt, "tokens_added:"):
		color.Green("\n[credit] Token code added to the wallet.")
	case strings.HasPrefix(evt, "relay_busy:"):
		color.Yellow("\n[bridge] %s", strings.TrimPrefix(evt, "relay_busy:"))
	}
}

type cliContext struct {
	kd *common.KeibiDrop
}

func (c *cliContext) executor(in string) {
	if c.kd == nil {
		fmt.Println("Error: KeibiDrop not initialized")
		return
	}

	in = strings.TrimSpace(in)
	args := strings.Fields(in)
	if len(args) == 0 {
		return
	}

	switch args[0] {
	case "help":
		printHelp()

	case "version":
		common.PrintBanner()

	case "feedback":
		msg := strings.TrimSpace(strings.Join(args[1:], " "))
		if msg == "" {
			fmt.Println("Usage: feedback <message>")
			fmt.Println("Include an email address in the message if you want a reply.")
			return
		}
		if err := feedback.Send(feedback.Report{
			Message: msg,
			Version: common.Version,
			Surface: "cli",
		}); err != nil {
			fmt.Println("Could not send:", err)
			fmt.Println("You can email marius@keibisoft.com instead.")
			return
		}
		fmt.Println("Sent. Thanks.")

	case "show":
		if len(args) < 2 {
			fmt.Println("Usage: show <fingerprint|ip|peer fingerprint|peer ip|config>")
			return
		}
		target := strings.Join(args[1:], " ")
		if target == "config" {
			cfg, _ := config.Load()
			fmt.Printf("Config file: %s\n", config.ConfigPath())
			fmt.Printf("Relay:       %s\n", cfg.Relay)
			fmt.Printf("Save path:   %s\n", cfg.SavePath)
			fmt.Printf("Mount path:  %s\n", cfg.MountPath)
			fmt.Printf("Log file:    %s\n", cfg.LogFile)
			fmt.Printf("Inbound:     %d\n", cfg.InboundPort)
			fmt.Printf("Outbound:    %d\n", cfg.OutboundPort)
			fmt.Printf("Bridge:      %s\n", cfg.BridgeAddr)
			fmt.Printf("No FUSE:     %v\n", cfg.NoFUSE)
			fmt.Printf("Strict:      %v\n", cfg.StrictMode)
			fmt.Printf("Incognito:   %v\n", cfg.Incognito)
			if c.kd.Identity != nil {
				fmt.Printf("Identity:    persistent (%s...)\n", c.kd.Identity.Fingerprint[:12])
			} else {
				fmt.Printf("Identity:    ephemeral\n")
			}
			if c.kd.AddressBook != nil {
				fmt.Printf("Contacts:    %d saved\n", c.kd.AddressBook.Count())
			}
			return
		}
		handleShow(c.kd, target)

	case "register":
		if len(args) != 2 {
			fmt.Println("Usage: register <fingerprint>")
			return
		}
		registerPeer(c.kd, args[1])

	case "discover":
		discoverPeers(c.kd)

	case "create":
		createRoom(c.kd)

	case "join":
		joinRoom(c.kd)

	case "connect":
		connectPeer(c.kd)

	case "reset":
		resetSession(c.kd)

	case "add":
		if len(args) != 2 {
			fmt.Println("Usage: add <filepath>")
			return
		}
		addFile(c.kd, args[1])

	case "list":
		listFiles(c.kd)

	case "pull":
		if len(args) != 3 {
			fmt.Println("Usage: pull <remote path> <local path>")
			return
		}
		pullFile(c.kd, args[1], args[2])

	case "delete":
		if len(args) != 2 {
			fmt.Println("Usage: delete <filepath>")
			return
		}
		deleteFile(c.kd, args[1])

	case "disconnect":
		disconnectRoom(c.kd)

	case "contacts":
		if c.kd.AddressBook == nil {
			fmt.Println("No address book (incognito mode)")
			return
		}
		contacts := c.kd.AddressBook.List()
		if len(contacts) == 0 {
			fmt.Println("No saved contacts.")
			return
		}
		for i, ct := range contacts {
			status := "offline"
			if !ct.LastSeen.IsZero() {
				status = "last seen " + ct.LastSeen.Format("2006-01-02 15:04")
			}
			fp := ct.Fingerprint
			if len(fp) > 12 {
				fp = fp[:12] + "..."
			}
			fmt.Printf("  [%d] %s  %s  (%s)\n", i+1, ct.Name, fp, status)
		}

	case "add-contact":
		if len(args) < 3 {
			fmt.Println("Usage: add-contact <name> <fingerprint>")
			return
		}
		if c.kd.AddressBook == nil {
			fmt.Println("No address book (incognito mode)")
			return
		}
		name := args[1]
		fp := strings.Join(args[2:], " ")
		if err := c.kd.AddressBook.Add(name, fp); err != nil {
			fmt.Println("Error:", err)
			return
		}
		if err := c.kd.AddressBook.Save(); err != nil {
			fmt.Println("Error saving:", err)
			return
		}
		fmt.Printf("Contact '%s' added.\n", name)

	case "remove-contact":
		if len(args) != 2 {
			fmt.Println("Usage: remove-contact <fingerprint>")
			return
		}
		if c.kd.AddressBook == nil {
			fmt.Println("No address book (incognito mode)")
			return
		}
		if err := c.kd.AddressBook.Remove(args[1]); err != nil {
			fmt.Println("Error:", err)
			return
		}
		if err := c.kd.AddressBook.Save(); err != nil {
			fmt.Println("Error saving:", err)
			return
		}
		fmt.Println("Contact removed.")

	case "quick-connect":
		if len(args) != 2 {
			fmt.Println("Usage: quick-connect <fingerprint>")
			return
		}
		go func() {
			if err := c.kd.ConnectToContact(args[1]); err != nil {
				fmt.Println("Error:", err)
			} else {
				fmt.Println("Connected to contact!")
			}
		}()

	case "save-contact":
		if len(args) != 2 {
			fmt.Println("Usage: save-contact <name>")
			return
		}
		if err := c.kd.SaveCurrentPeerAsContact(args[1]); err != nil {
			fmt.Println("Error:", err)
		} else {
			fmt.Printf("Peer saved as '%s'.\n", args[1])
		}

	case "add-as":
		if len(args) < 3 {
			fmt.Println("Usage: add-as <local-path> <remote-name>")
			return
		}
		if err := c.kd.AddFileAs(filepath.Clean(args[1]), args[2]); err != nil {
			fmt.Println("Error:", err)
			return
		}
		fmt.Printf("Shared '%s' as '%s'\n", args[1], args[2])

	case "unshare":
		if len(args) != 2 {
			fmt.Println("Usage: unshare <filename>")
			return
		}
		deleteFile(c.kd, args[1])

	case "cancel-download":
		if len(args) != 2 {
			fmt.Println("Usage: cancel-download <name>")
			return
		}
		if err := c.kd.CancelDownload(args[1]); err != nil {
			fmt.Println("Error:", err)
		} else {
			fmt.Println("Download cancelled:", args[1])
		}

	case "progress":
		if len(args) != 2 {
			fmt.Println("Usage: progress <name>")
			return
		}
		p := c.kd.GetDownloadProgress(args[1])
		if p < 0 {
			fmt.Println("No active download:", args[1])
		} else {
			fmt.Printf("%s: %d%%\n", args[1], int(p*100))
		}

	case "incognito":
		if len(args) < 2 {
			if c.kd.Incognito {
				fmt.Println("incognito: on")
			} else {
				fmt.Println("incognito: off")
			}
			return
		}
		enable := args[1] == "on" || args[1] == "true" || args[1] == "1"
		newFP, err := c.kd.ToggleIncognito(enable, config.ConfigDir())
		if err != nil {
			fmt.Println("Error:", err)
			return
		}
		if newFP == "" {
			newFP, _ = c.kd.ExportFingerprint()
		}
		fmt.Println("Incognito:", args[1])
		fmt.Println("Fingerprint:", newFP)

	case "tokens":
		sub := "list"
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "add":
			if len(args) != 3 {
				fmt.Println("Usage: tokens add <code>")
				return
			}
			gb, err := c.kd.TokensAdd(args[2])
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			fmt.Printf("Added %.0f GiB of relay credit.\n", gb)
			fmt.Println("Keep the code safe: it is like cash - anyone holding it can spend it, and it cannot be recovered if lost.")
		case "list":
			printCreditStatus(c.kd.TokensCreditStatus())
			printTokenSummaries(c.kd.TokensSummaries())
		case "balance":
			fmt.Println("Checking balances with the relay...")
			printTokenSummaries(c.kd.TokensRefreshBalances())
			printCreditStatus(c.kd.TokensCreditStatus())
		case "buy":
			fmt.Println("Buy relay credit (prepaid, anonymous, no account, no expiry):")
			fmt.Println("  " + c.kd.TokensBuyStart())
			fmt.Println("Open that link and pay. The code adds itself here after payment,")
			fmt.Println("as long as this CLI stays running. Manual fallback: tokens add <code>")
		default:
			fmt.Println("Usage: tokens [list|add <code>|balance|buy]")
		}

	case "peer-info":
		pfp, _ := c.kd.GetPeerFingerprint()
		fmt.Printf("Peer fingerprint: %s\n", pfp)
		fmt.Printf("Peer IP:          %s\n", c.kd.PeerIPv6IP)
		fmt.Printf("Connection mode:  %s\n", c.kd.ConnectionMode)
		if bi := c.kd.BridgeInfo(); bi.Via != "" {
			fmt.Printf("Via bridge:       %s\n", bi.Via)
		}
		fmt.Printf("Peer persistent:  %v\n", c.kd.IsPeerPersistent())

	case "status":
		fp, _ := c.kd.ExportFingerprint()
		pfp, _ := c.kd.GetPeerFingerprint()
		fmt.Printf("Running:          %v\n", c.kd.IsRunning())
		fmt.Printf("Connection:       %s\n", c.kd.ConnectionStatus())
		fmt.Printf("Fingerprint:      %s\n", fp)
		fmt.Printf("Peer fingerprint: %s\n", pfp)
		fmt.Printf("IP:               %s\n", c.kd.LocalIPv6IP)
		fmt.Printf("Peer IP:          %s\n", c.kd.PeerIPv6IP)
		fmt.Printf("Mode:             %s\n", c.kd.ConnectionMode)
		if bi := c.kd.BridgeInfo(); bi.Via != "" {
			cls := "free tier"
			if bi.Paid {
				cls = "paid priority"
			}
			if bi.Contention || bi.Busy {
				cls += ", relay busy"
			}
			fmt.Printf("Via bridge:       %s (%s)\n", bi.Via, cls)
			if bi.Paid {
				fmt.Printf("Relay credit:     %.1f GiB left, %.2f GiB used this session\n", bi.WalletGB, bi.SessionGB)
			} else if bi.WalletGB > 0 {
				fmt.Printf("Relay credit:     %.1f GiB left\n", bi.WalletGB)
			}
		}
		fmt.Printf("Relay:            %s\n", c.kd.RelayEndoint)
		fmt.Printf("FUSE:             %v\n", c.kd.IsFUSE)
		c.kd.SyncTracker.LocalFilesMu.RLock()
		localCount := len(c.kd.SyncTracker.LocalFiles)
		c.kd.SyncTracker.LocalFilesMu.RUnlock()
		c.kd.SyncTracker.RemoteFilesMu.RLock()
		remoteCount := len(c.kd.SyncTracker.RemoteFiles)
		c.kd.SyncTracker.RemoteFilesMu.RUnlock()
		fmt.Printf("Local files:      %d\n", localCount)
		fmt.Printf("Remote files:     %d\n", remoteCount)

	case "poll-event":
		select {
		case evt := <-eventCh:
			fmt.Println("Event:", evt)
		default:
			fmt.Println("No events")
		}

	case "export-logs", "sanitize-logs":
		dest := "keibidrop-sanitized.log"
		if len(args) > 1 {
			dest = args[1]
		}
		logCfg, err := config.Load()
		if err != nil {
			fmt.Println("Error loading config:", err)
			return
		}
		if err := common.SanitizeLogsToFile(logCfg.LogFile, dest); err != nil {
			fmt.Println("Error:", err)
		} else {
			fmt.Println("Sanitized logs written to", dest)
		}

	case "exit", "quit":
		c.kd.NotifyDisconnect()
		_ = c.kd.UnmountFilesystem()
		fmt.Println("Goodbye.")
		os.Exit(0)

	default:
		// TODO: Add a shell passthrough mode like tmate.
		// Uncomment the block to run shell commands without a / prefix.
		// Add the imports "os/exec" and "runtime".
		/*
			// Cross-platform shell execution
			var cmd *exec.Cmd
			switch runtime.GOOS {
			case "windows":
				cmd = exec.Command("cmd", "/c", in)
			default: // macOS, Linux
				cmd = exec.Command("sh", "-c", in)
			}
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Stdin = os.Stdin
			if err := cmd.Run(); err != nil {
				color.Red("Command failed: %v", err)
			}
			return
		*/
		color.Red("Unknown command: %s", args[0])
	}
}

func (c *cliContext) completer(d prompt.Document) []prompt.Suggest {
	s := []prompt.Suggest{
		{Text: "help", Description: "Show help message"},
		{Text: "version", Description: "Show banner, version and commit hash"},
		{Text: "status", Description: "Full connection and session status"},
		{Text: "show", Description: "Show local or peer info"},
		{Text: "show relay", Description: "Show the connected relay URL"},
		{Text: "show peer", Description: "Show the peer fingerprint"},
		{Text: "show fingerprint", Description: "Show our fingerprint"},
		{Text: "show config", Description: "Show full configuration"},
		{Text: "register", Description: "Register peer fingerprint"},
		{Text: "discover", Description: "Discover peers on local network"},
		{Text: "create", Description: "Create a room"},
		{Text: "join", Description: "Join a room by fingerprint"},
		{Text: "connect", Description: "Connect (auto role via fingerprint)"},
		{Text: "disconnect", Description: "Disconnect from peer and reset session"},
		{Text: "add", Description: "Add file or folder to share"},
		{Text: "add-as", Description: "Share with custom name: add-as <path> <name>"},
		{Text: "unshare", Description: "Stop sharing a file"},
		{Text: "list", Description: "List shared files"},
		{Text: "pull", Description: "Pull file/folder from peer"},
		{Text: "delete", Description: "Stop sharing a file/folder"},
		{Text: "cancel-download", Description: "Cancel an active download"},
		{Text: "progress", Description: "Download progress: progress <name>"},
		{Text: "peer-info", Description: "Peer details and connection mode"},
		{Text: "incognito", Description: "Query or toggle incognito mode"},
		{Text: "tokens", Description: "Relay credit: list, add <code>, balance, buy"},
		{Text: "poll-event", Description: "Pop next event from event queue"},
		{Text: "contacts", Description: "List saved contacts"},
		{Text: "add-contact", Description: "Add contact: add-contact <name> <fp>"},
		{Text: "remove-contact", Description: "Remove contact by fingerprint"},
		{Text: "quick-connect", Description: "Connect to saved contact by fingerprint"},
		{Text: "save-contact", Description: "Save current peer as contact"},
		{Text: "export-logs", Description: "Export sanitized logs"},
		{Text: "exit", Description: "Exit the CLI"},
	}
	return prompt.FilterHasPrefix(s, d.GetWordBeforeCursor(), true)
}

// printCreditStatus shows the credit level once per command, colored by
// severity. Pull-based: no periodic output, no log lines.
func printCreditStatus(st common.CreditStatus) {
	switch st.Level {
	case "critical", "empty":
		color.Red("Relay credit: %.1f GB (%s)", st.WalletGB, st.Level)
		color.Red("  %s", st.Notice)
	case "low":
		color.Yellow("Relay credit: %.0f GB (low)", st.WalletGB)
		color.Yellow("  %s", st.Notice)
	default:
		fmt.Printf("Relay credit: %.0f GB\n", st.WalletGB)
	}
}

func printTokenSummaries(sums []common.TokenChainSummary) {
	if len(sums) == 0 {
		fmt.Println("No relay credit yet. Direct and local-network transfers are always free;")
		fmt.Println("prepaid credit buys priority relay bandwidth for when peers can't connect directly.")
		fmt.Println("Get a code at " + common.TokensBuyURL + " then paste it: tokens add <code>")
		return
	}
	var total float64
	for _, s := range sums {
		state := ""
		if s.Dead || s.UnitsLeft == 0 {
			state = "  (spent)"
		}
		fmt.Printf("  %s  %.1f/%.0f GiB left%s\n", s.Code, s.GBLeft, s.GBTotal, state)
		if !s.Dead {
			total += s.GBLeft
		}
	}
	fmt.Printf("Total relay credit: %.1f GiB\n", total)
	fmt.Println("Top up: tokens buy")
}

func printHelp() {
	fmt.Println(`
help                         Show this help message
version                      Show banner and version
feedback <message>           Send a problem report to the developers
status                       Full connection and session status
show fingerprint             Show your fingerprint
show ip                      Show your IP
show peer fingerprint        Show peer's fingerprint
show peer ip                 Show peer's IP
show relay                   Show the currently connected relay URL
show config                  Show full configuration
register <fingerprint>       Register a peer's fingerprint
discover                     Discover peers on local network
connect                      Connect (auto role via fingerprint)
create                       Create a room
join                         Join a room
disconnect                   Disconnect from peer and reset session
add <filepath>               Share a file or directory
add-as <path> <remote-name>  Share with a custom remote name
unshare <filename>           Stop sharing a file
list                         List shared files and their locations
pull <remote> <local>        Copy file/folder from peer to local path
cancel-download <name>       Cancel an active download
progress <name>              Download progress (0-100%)
peer-info                    Peer details and connection mode
incognito [on|off]           Query or toggle incognito mode
tokens                       List relay credit (prepaid priority bandwidth)
tokens add <code>            Paste a purchased or gifted token code
tokens balance               Refresh balances from the relay
tokens buy                   Where to buy relay credit
poll-event                   Pop next event from event queue
contacts                     List saved contacts
add-contact <name> <fp>      Save a contact
remove-contact <fp>          Remove a saved contact
quick-connect <fp>           Connect to a saved contact
save-contact <name>          Save current peer as contact
export-logs [dest]           Export sanitized logs
exit                         Quit the CLI`)
}

func handleShow(kd *common.KeibiDrop, what string) {
	if kd == nil {
		fmt.Println("Error: KeibiDrop not initialized")
	}
	switch what {
	case "fingerprint":
		fp, err := kd.ExportFingerprint()
		if err != nil {
			fmt.Println("Error:", err)
			return
		}
		fmt.Println("Your fingerprint:", fp)

	case "ip":
		fmt.Println("Your IP:", kd.LocalIPv6IP)
	case "peer fingerprint":
		pfp, _ := kd.GetPeerFingerprint()
		fmt.Println("Peer fingerprint:", pfp)
	case "peer ip":
		fmt.Println("Peer IP:", kd.PeerIPv6IP)
	case "relay":
		fmt.Println("Relay:", kd.RelayEndoint)
	default:
		fmt.Println("Unknown show command.")
	}
}

func discoverPeers(kd *common.KeibiDrop) {
	kd.IsLocalMode = true
	disc := discovery.New(kd.InboundPort(), slog.Default())
	_ = disc.Start()
	defer disc.Stop()

	cliLocalMyName = disc.Name()
	fmt.Printf("You appear as: %s\n", cliLocalMyName)
	fmt.Println("Searching for nearby KeibiDrop devices (10s)...")

	time.Sleep(6 * time.Second)

	peers := disc.Peers()
	if len(peers) == 0 {
		time.Sleep(4 * time.Second)
		peers = disc.Peers()
	}

	if len(peers) == 0 {
		fmt.Println("No devices found on this network.")
		return
	}

	cliDiscoveredPeerMap = make(map[string]string, len(peers))
	fmt.Printf("\nFound %d device(s):\n", len(peers))
	for i, p := range peers {
		cliDiscoveredPeerMap[p.Addr] = p.Name
		fmt.Printf("  [%d] %s  (%s)\n", i+1, p.Name, p.Addr)
	}
	fmt.Println("\nTo connect: register <address> then connect")
	fmt.Println("Example:  register " + peers[0].Addr)
}

func registerPeer(kd *common.KeibiDrop, fp string) {
	if kd.IsLocalMode {
		if err := kd.SetPeerDirectAddress(fp); err != nil {
			fmt.Println("Error:", err)
			return
		}
		if name, ok := cliDiscoveredPeerMap[fp]; ok {
			cliLocalPeerName = name
		}
		fmt.Println("Registered LAN address:", fp)
		return
	}
	err := kd.AddPeerFingerprint(fp)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("Peer registered:", fp)
}

func createRoom(kd *common.KeibiDrop) {
	if kd.OpInProgress.Add(1) != 1 {
		kd.OpInProgress.Add(-1)
		fmt.Println("Create/Join Room already in progress...")
		return
	}

	go func() {
		defer kd.OpInProgress.Add(-1)
		err := kd.CreateRoom()
		if err != nil {
			fmt.Println("Error: ", err)
			return
		}
		fmt.Printf("Room created and peer connected: %s (mode: %s)\n", kd.PeerIPv6IP, kd.ConnectionMode)
	}()
}

func joinRoom(kd *common.KeibiDrop) {
	if kd.OpInProgress.Add(1) != 1 {
		kd.OpInProgress.Add(-1)
		fmt.Println("Create/Join Room already in progress...")
		return
	}

	go func() {
		defer kd.OpInProgress.Add(-1)
		err := kd.JoinRoom()
		if err != nil {
			if errors.Is(err, common.ErrRateLimitHit) {
				fmt.Printf(`This is a free public relay, you can use it around 3 times per 5 minute interval: %e\n`, err)
				return
			}

			if errors.Is(err, common.ErrServerAtCapacity) {
				fmt.Printf(`The free public relay is at it's capacity, please retry in 5 minutes: %e\n`, err)
				return
			}

			fmt.Println("Error: ", err)
			return
		}

		fmt.Printf("Room: %v, joined successfully (mode: %s)\n", kd.PeerIPv6IP, kd.ConnectionMode)
	}()
}

func connectPeer(kd *common.KeibiDrop) {
	if kd.OpInProgress.Add(1) != 1 {
		kd.OpInProgress.Add(-1)
		fmt.Println("Operation already in progress...")
		return
	}

	// In local mode, use the name tiebreak. Mobile and the Rust UI use the same rule.
	if kd.IsLocalMode && cliLocalMyName != "" && cliLocalPeerName != "" {
		iAmCreator := common.DecideLocalRole(cliLocalMyName, cliLocalPeerName, kd.PeerIPv6IP)
		role := "joiner"
		if iAmCreator {
			role = "creator"
		}
		fmt.Printf("Connecting as %s (me=%s, peer=%s)\n", role, cliLocalMyName, cliLocalPeerName)
		go func() {
			defer kd.OpInProgress.Add(-1)
			var err error
			if iAmCreator {
				err = kd.CreateRoom()
			} else {
				err = kd.JoinRoom()
			}
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			fmt.Printf("Connected to peer: %s (mode: %s)\n", kd.PeerIPv6IP, kd.ConnectionMode)
		}()
		return
	}

	go func() {
		defer kd.OpInProgress.Add(-1)
		err := kd.Connect()
		if err != nil {
			fmt.Println("Error:", err)
			return
		}
		fmt.Printf("Connected to peer: %s (mode: %s)\n", kd.PeerIPv6IP, kd.ConnectionMode)
	}()
	fmt.Println("Connecting... (role determined by fingerprint comparison)")
}

func resetSession(kd *common.KeibiDrop) {
	// kd.ResetSession()
	fmt.Println("Session reset")
}

func addFile(kd *common.KeibiDrop, p string) {
	err := kd.AddFile(filepath.Clean(p))
	if err != nil {
		fmt.Println(fmt.Errorf("failed to add the file `%v` to the shared list: %e", p, err))
		return
	}
	fmt.Printf("File `%v` added\n", p)
}

func listFiles(kd *common.KeibiDrop) {
	fmt.Println("Listing shared files...")

	remote, local := kd.ListFiles()
	for _, s := range remote {
		fmt.Println(s)
	}
	for _, s := range local {
		fmt.Println(s)
	}
	if len(remote) == 0 && len(local) == 0 {
		fmt.Println("No tracked files.")
	}
}
func pullFile(kd *common.KeibiDrop, remote, local string) {
	err := kd.PullFile(remote, local)
	if err != nil {
		fmt.Printf("Failed to pull remote file: %e\n", err)
		return
	}

	fmt.Printf("Pulled '%s' to '%s'\n", remote, local)
}

func disconnectRoom(kd *common.KeibiDrop) {
	kd.NotifyDisconnect()
	_ = kd.UnmountFilesystem()
	kd.Stop()
	fp, err := kd.ExportFingerprint()
	if err != nil {
		color.Red("Failed to get new fingerprint: %v", err)
		return
	}
	fmt.Println("Disconnected. New fingerprint:", fp)
}

func deleteFile(kd *common.KeibiDrop, path string) {
	if err := kd.UnshareFile(path); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("Unshared:", path)
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

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Write default config on first run.
	_ = config.WriteDefault()

	if err := config.EnsureDirectories(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create directories: %v\n", err)
		os.Exit(1)
	}

	relayURL, err := url.Parse(cfg.Relay)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid relay URL %q: %v\n", cfg.Relay, err)
		os.Exit(1)
	}
	fmt.Println("Connecting to relay:", relayURL.String())

	wr := os.Stderr
	if cfg.LogFile != "" {
		f, err := os.OpenFile(filepath.Clean(cfg.LogFile),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			slog.Warn("Failed to open log file, defaulting to stderr",
				"path", cfg.LogFile, "error", err)
		} else {
			wr = f
		}
	}
	handler := slog.NewTextHandler(wr, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "cli")

	useFUSE := checkfuse.IsFUSEPresent() && !cfg.NoFUSE
	logger.Info("FUSE", "present", checkfuse.IsFUSEPresent(), "disabled", cfg.NoFUSE, "using", useFUSE)
	logger.Info("Config", "relay", cfg.Relay, "save", cfg.SavePath, "mount", cfg.MountPath,
		"inbound", cfg.InboundPort, "outbound", cfg.OutboundPort, "log", cfg.LogFile)

	kdctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kd, err := common.NewKeibiDrop(kdctx, logger, useFUSE, relayURL,
		cfg.InboundPort, cfg.OutboundPort, cfg.MountPath, cfg.SavePath,
		cfg.PrefetchOnOpen, cfg.PushOnWrite)
	if err != nil {
		logger.Error("Failed to start keibidrop", "error", err)
		color.Red("Fatal: %v", err)
		os.Exit(1) //nolint:gocritic
	}
	kd.AutoCache = cfg.LiveCollab // live_collab sets macFUSE auto_cache for same-size live edits on macOS.
	kd.PrefetchAutoMB = cfg.PrefetchAutoMB
	kd.ScanSharedOnStart = cfg.ScanSharedOnStart
	kd.ShareReadOnly = cfg.ShareReadOnly
	kd.MountReadOnly = cfg.MountReadOnly
	kd.PreserveMetadata = cfg.PreserveMetadata
	// The daemon sets these from config; the CLI used to skip them, leaving it
	// with no relay fallback at all.
	kd.BridgeAddr = cfg.BridgeAddr
	kd.StrictMode = cfg.StrictMode
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

	kd.OnEvent = pushEvent
	go kd.Run()

	if cfg.AutoConnectPeer != "" {
		kd.AutoConnectPeer = cfg.AutoConnectPeer
		if err := kd.StartAutoConnect(kdctx); err != nil {
			color.Red("auto_connect_peer: %v", err)
		} else {
			fmt.Printf("Auto-connect: %s\n", cfg.AutoConnectPeer)
		}
	}

	ctx := &cliContext{kd: kd}

	common.PrintBanner()
	fmt.Printf("Config: %s\n", config.ConfigPath())
	fmt.Printf("Log:    %s\n", cfg.LogFile)
	fmt.Printf("Save:   %s\n", cfg.SavePath)
	if useFUSE {
		fmt.Printf("Mount:  %s\n", cfg.MountPath)
	}
	handleShow(kd, "fingerprint")

	p := prompt.New(
		ctx.executor,
		ctx.completer,
		prompt.OptionPrefix("keibidrop> "),
		prompt.OptionTitle("keibidrop-cli"),
	)

	p.Run()
}
