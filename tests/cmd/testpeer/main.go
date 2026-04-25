// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// testpeer is a scriptable KeibiDrop peer for multi-process integration tests.
// It reads line-based commands from stdin and writes responses to stdout.
// Configuration is via environment variables.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

func main() {
	relayURL := os.Getenv("RELAY_URL")
	inbound, _ := strconv.Atoi(os.Getenv("INBOUND_PORT"))
	outbound, _ := strconv.Atoi(os.Getenv("OUTBOUND_PORT"))
	mountDir := os.Getenv("MOUNT_DIR")
	saveDir := os.Getenv("SAVE_DIR")
	useFUSE := os.Getenv("USE_FUSE") == "1"
	logFile := os.Getenv("LOG_FILE")

	var wr *os.File = os.Stderr
	if logFile != "" {
		f, err := os.OpenFile(filepath.Clean(logFile),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			wr = f
			defer f.Close()
		}
	}
	handler := slog.NewTextHandler(wr, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "testpeer",
		"fuse", useFUSE, "in", inbound, "out", outbound)

	parsed, err := url.Parse(relayURL)
	if err != nil {
		fmt.Println("ERR:invalid relay URL: " + err.Error())
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := common.NewKeibiDropWithIP(ctx, logger, useFUSE, parsed,
		inbound, outbound, mountDir, saveDir, false, false, "::1")
	if err != nil {
		fmt.Println("ERR:init failed: " + err.Error())
		os.Exit(1)
	}

	go kd.Run()
	fmt.Println("READY")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		args := strings.Fields(line)
		if len(args) == 0 {
			continue
		}

		switch args[0] {
		case "fingerprint":
			fp, err := kd.ExportFingerprint()
			if err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("FP:" + fp)
			}

		case "register":
			if len(args) < 2 {
				fmt.Println("ERR:usage: register <fingerprint>")
				continue
			}
			if err := kd.AddPeerFingerprint(args[1]); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "create":
			if err := kd.CreateRoom(); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("CONNECTED")
			}

		case "join":
			if err := kd.JoinRoom(); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("CONNECTED")
			}

		case "add":
			if len(args) < 2 {
				fmt.Println("ERR:usage: add <path>")
				continue
			}
			if err := kd.AddFile(strings.Join(args[1:], " ")); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "list":
			remote, local := kd.ListFiles()
			for _, f := range remote {
				fmt.Println("REMOTE:" + f)
			}
			for _, f := range local {
				fmt.Println("LOCAL:" + f)
			}
			fmt.Println("END")

		case "wait_file":
			if len(args) < 3 {
				fmt.Println("ERR:usage: wait_file <name> <timeout_sec>")
				continue
			}
			timeout, _ := strconv.Atoi(args[2])
			if timeout <= 0 {
				timeout = 15
			}
			deadline := time.Now().Add(time.Duration(timeout) * time.Second)
			found := false
			for time.Now().Before(deadline) {
				p := filepath.Join(mountDir, args[1])
				if _, err := os.Stat(p); err == nil {
					found = true
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if found {
				fmt.Println("OK")
			} else {
				fmt.Println("ERR:timeout waiting for " + args[1])
			}

		case "read_file":
			if len(args) < 2 {
				fmt.Println("ERR:usage: read_file <name>")
				continue
			}
			p := filepath.Join(mountDir, args[1])
			data, err := os.ReadFile(p)
			if err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Printf("DATA:%d:%s\n", len(data), string(data))
			}

		case "write_file":
			// write_file <name> <content>
			if len(args) < 3 {
				fmt.Println("ERR:usage: write_file <name> <content>")
				continue
			}
			p := filepath.Join(mountDir, args[1])
			content := strings.Join(args[2:], " ")
			if err := os.WriteFile(p, []byte(content), 0644); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "pull":
			if len(args) < 3 {
				fmt.Println("ERR:usage: pull <remote> <local>")
				continue
			}
			if err := kd.PullFile(args[1], args[2]); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "mkdir_p":
			// mkdir_p <relative-path> — create directory tree on the mount (recursive)
			if len(args) < 2 {
				fmt.Println("ERR:usage: mkdir_p <path>")
				continue
			}
			p := filepath.Join(mountDir, strings.Join(args[1:], " "))
			if err := os.MkdirAll(p, 0o755); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "list_dir":
			// list_dir <relative-path> — list direct children of a directory on the mount.
			// Prints ENTRY:<name> per child, then END.
			if len(args) < 2 {
				fmt.Println("ERR:usage: list_dir <path>")
				continue
			}
			p := filepath.Join(mountDir, strings.Join(args[1:], " "))
			entries, err := os.ReadDir(p)
			if err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				for _, e := range entries {
					fmt.Println("ENTRY:" + e.Name())
				}
				fmt.Println("END")
			}

		case "exec":
			// exec <dir-relative-to-mount> <command> [args...]
			// Runs a shell command inside the FUSE mount. Returns EXEC:<exit_code>:<output>
			if len(args) < 3 {
				fmt.Println("ERR:usage: exec <dir> <command> [args...]")
				continue
			}
			dir := filepath.Join(mountDir, args[1])
			cmd := exec.Command(args[2], args[3:]...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
			// Replace newlines with \n literal for single-line protocol
			escaped := strings.ReplaceAll(strings.TrimRight(string(out), "\n"), "\n", "\\n")
			fmt.Printf("EXEC:%d:%s\n", exitCode, escaped)

		case "quit":
			_ = kd.UnmountFilesystem()
			cancel()
			fmt.Println("BYE")
			return

		default:
			fmt.Println("ERR:unknown command: " + args[0])
		}
	}
}
