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
	"log"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KeibiSoft/KeibiDrop/cmd/internal/checkfuse"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	prompt "github.com/c-bata/go-prompt"
	"github.com/fatih/color"
)

const KEIBIDROP_RELAY_ENV = "KEIBIDROP_RELAY"
const INBOUND_PORT_ENV = "INBOUND_PORT"
const OUTBOUND_PORT_ENV = "OUTBOUND_PORT"
const TO_MOUNT_PATH_ENV = "TO_MOUNT_PATH"
const TO_SAVE_PATH_ENV = "TO_SAVE_PATH"
const LOG_FILE_ENV = "LOG_FILE"
const NO_FUSE_ENV = "NO_FUSE"

type cliContext struct {
	kd *common.KeibiDrop
}

func getenv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func initRelay() *url.URL {
	relayURL := getenv(KEIBIDROP_RELAY_ENV, "https://keibidroprelay.keibisoft.com")
	parsedURL, err := url.Parse(relayURL)
	if err != nil {
		log.Fatalf("invalid KEIBIDROP_RELAY URL: %v", err)
	}

	fmt.Println("Connecting to relay:", parsedURL.String())
	return parsedURL
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
			fmt.Println("Usage: show <fingerprint|ip|peer fingerprint|peer ip>")
			return
		}
		handleShow(c.kd, strings.Join(args[1:], " "))

	case "register":
		if len(args) != 3 || args[1] != "peer" {
			fmt.Println("Usage: register peer <fingerprint>")
			return
		}
		registerPeer(c.kd, args[2])

	case "create":
		createRoom(c.kd)

	case "join":
		joinRoom(c.kd)

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

	case "exit", "quit":
		c.kd.UnmountFilesystem()
		fmt.Println("Goodbye.")
		os.Exit(0)

	default:
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
		{Text: "register peer", Description: "Register peer fingerprint"},
		{Text: "create", Description: "Create a room"},
		{Text: "join", Description: "Join a room by fingerprint"},
		{Text: "reset", Description: "Reset session, rotate keys"},
		{Text: "add", Description: "Add file or folder to share"},
		{Text: "list", Description: "List shared files"},
		{Text: "pull", Description: "Pull file/folder from peer"},
		{Text: "delete", Description: "Stop sharing a file/folder"},
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
register peer <fingerprint>  Register a peer's fingerprint
create                       Create a room
join <fingerprint>           Join a room by peer fingerprint
reset                        Reset session and rotate keys
add <filepath>               Share a file or directory
list                         List shared files and their locations
pull <remote> <local>        Copy file/folder from peer to local path
delete <filepath>            Unshare a file or folder
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
		fmt.Println("Room created and peer connected: ", kd.PeerIPv6IP)
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

		fmt.Printf("Room: %v, joined successfully\n", kd.PeerIPv6IP)
	}()
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

func deleteFile(kd *common.KeibiDrop, path string) {
	_ = kd
	fmt.Println("[TODO] Unshared:", path)
}

func main() {
	relayURL := initRelay()
	var wr *os.File = os.Stderr

	logFileStr := os.Getenv(LOG_FILE_ENV)
	if logFileStr != "" {
		f, err := os.OpenFile(filepath.Clean(logFileStr),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			slog.Warn("Failed to open log file, defaulting to stderr",
				"path", logFileStr,
				"env", LOG_FILE_ENV,
				"error", err)
		} else {
			wr = f
		}
	}

	// text output, level=DEBUG
	handler := slog.NewTextHandler(wr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	logger := slog.New(handler).With("component", "cli")

	inboundStr := os.Getenv(INBOUND_PORT_ENV)
	outboundStr := os.Getenv(OUTBOUND_PORT_ENV)
	inbound := config.InboundPort
	if inboundStr != "" {
		inPort, err := strconv.Atoi(inboundStr)
		if err != nil {
			logger.Error("Invalid inbound port", "provided", inboundStr)
			os.Exit(1)
		}
		inbound = inPort
	}
	outbound := config.OutboundPort
	if outboundStr != "" {
		outPort, err := strconv.Atoi(outboundStr)
		if err != nil {
			logger.Error("Invalid outbound port", "provided", outboundStr)
			os.Exit(1)
		}
		outbound = outPort
	}

	toMount := os.Getenv(TO_MOUNT_PATH_ENV)
	toSave := os.Getenv(TO_SAVE_PATH_ENV)

	// Explicitly pass NO FUSE
	_, noFUSE := os.LookupEnv(NO_FUSE_ENV)

	isFuse := checkfuse.IsFUSEPresent()
	logger.Info("Is FUSE present", "val", isFuse)

	logger.Info("Do not use FUSE", "val", noFUSE)

	finalVal := isFuse && !noFUSE

	kdctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kd, err := common.NewKeibiDrop(kdctx, logger, finalVal, relayURL, inbound, outbound, toMount, toSave)
	if err != nil {
		logger.Error("Failed to start keibidrop", "error", err)
		os.Exit(1)
	}

	go kd.Run()

	ctx := &cliContext{kd: kd}

	common.PrintBanner()

	p := prompt.New(
		ctx.executor,
		ctx.completer,
		prompt.OptionPrefix("keibidrop> "),
		prompt.OptionTitle("keibidrop-cli"),
	)

	p.Run()
}
