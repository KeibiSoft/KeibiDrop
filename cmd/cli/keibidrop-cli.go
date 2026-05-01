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
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	prompt "github.com/c-bata/go-prompt"
	"github.com/fatih/color"
)

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
		// TODO: Shell passthrough mode (tmate-style)
		// Uncomment to enable shell commands without / prefix
		// Required imports: "os/exec", "runtime"
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
		{Text: "show", Description: "Show local or peer info"},
		{Text: "show relay", Description: "Show the connected relay URL"},
		{Text: "show peer", Description: "Show the peer fingerprint"},
		{Text: "show fingerprint", Description: "Show our fingerprint"},
		{Text: "register", Description: "Register peer fingerprint"},
		{Text: "create", Description: "Create a room"},
		{Text: "join", Description: "Join a room by fingerprint"},
		{Text: "disconnect", Description: "Disconnect from peer and reset session"},
		{Text: "reset", Description: "Reset session, rotate keys"},
		{Text: "add", Description: "Add file or folder to share"},
		{Text: "list", Description: "List shared files"},
		{Text: "pull", Description: "Pull file/folder from peer"},
		{Text: "delete", Description: "Stop sharing a file/folder"},
		{Text: "contacts", Description: "List saved contacts"},
		{Text: "add-contact", Description: "Add contact: add-contact <name> <fp>"},
		{Text: "remove-contact", Description: "Remove contact by fingerprint"},
		{Text: "quick-connect", Description: "Connect to saved contact by fingerprint"},
		{Text: "save-contact", Description: "Save current peer as contact"},
		{Text: "exit", Description: "Exit the CLI"},
	}
	return prompt.FilterHasPrefix(s, d.GetWordBeforeCursor(), true)
}

func printHelp() {
	fmt.Println(`
help                         Show this help message
version                      Show banner and version
show fingerprint             Show your fingerprint
show ip                      Show your IP
show peer fingerprint        Show peer's fingerprint
show peer ip                 Show peer's IP
show relay                   Show the currently connected relay URL
register <fingerprint>       Register a peer's fingerprint
create                       Create a room
join                         Join a room
disconnect                   Disconnect from peer and reset session
reset                        Reset session and rotate keys
add <filepath>               Share a file or directory
list                         List shared files and their locations
pull <remote> <local>        Copy file/folder from peer to local path
delete <filepath>            Unshare a file or folder
contacts                     List saved contacts
add-contact <name> <fp>      Save a contact
remove-contact <fp>          Remove a saved contact
quick-connect <fp>           Connect to a saved contact
save-contact <name>          Save current peer as contact
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

	fmt.Printf("You appear as: %s\n", disc.Name())
	fmt.Println("Searching for nearby KeibiDrop devices (10s)...")

	time.Sleep(6 * time.Second)

	peers := disc.Peers()
	if len(peers) == 0 {
		// Wait a bit more
		time.Sleep(4 * time.Second)
		peers = disc.Peers()
	}

	if len(peers) == 0 {
		fmt.Println("No devices found on this network.")
		return
	}

	fmt.Printf("\nFound %d device(s):\n", len(peers))
	for i, p := range peers {
		fmt.Printf("  [%d] %s  (%s)\n", i+1, p.Name, p.Addr)
	}
	fmt.Println("\nTo connect: register <address> then create/join")
	fmt.Println("Example:  register " + peers[0].Addr)
}

func registerPeer(kd *common.KeibiDrop, fp string) {
	err := kd.AddPeerFingerprint(fp)
	if err != nil {
		fmt.Println("Error: ", err)
		return
	}
	fmt.Println("Peer registed: ", fp)
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

	go func() {
		defer kd.OpInProgress.Add(-1)
		err := kd.Connect()
		if err != nil {
			fmt.Println("Error: ", err)
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
	err := kd.AddFile(p)
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
	_ = kd
	fmt.Println("[TODO] Unshared:", path)
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Write default config on first run.
	_ = config.WriteDefault()

	// Ensure save/mount/log directories exist.
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

	// Setup logger.
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

	if !cfg.Incognito {
		if err := kd.EnablePersistentIdentity(config.ConfigDir()); err != nil {
			logger.Warn("Failed to enable persistent identity, using ephemeral", "error", err)
		}
	}

	go kd.Run()

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
