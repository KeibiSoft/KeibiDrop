// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: kdmcp is a stdio MCP server exposing KeibiDrop to AI agents.
// ABOUTME: It imports pkg/logic/common directly; it never shells out to kd.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

func buildVersion() string { return common.Version }

func runtimeGOOS() string { return runtime.GOOS }

func main() {
	switch {
	case len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "-h" || os.Args[1] == "help"):
		usage()
		return
	case len(os.Args) > 1 && os.Args[1] == "version":
		fmt.Println(common.Version)
		return
	case len(os.Args) > 1 && os.Args[1] == "install":
		printInstall()
		return
	case len(os.Args) > 1 && os.Args[1] != "serve":
		fmt.Fprintf(os.Stderr, "unknown argument %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	os.Exit(run())
}

// run returns instead of exiting so cancel and shutdown always execute.
func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := newPeer(ctx)
	if err != nil {
		// Startup failures go to stderr: stdout carries only JSON-RPC frames.
		fmt.Fprintf(os.Stderr, "kdmcp: %v\n", err)
		return 1
	}
	defer p.shutdown()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		p.shutdown()
		os.Exit(0)
	}()

	if err := serve(os.Stdin, os.Stdout, &handler{kd: p}); err != nil {
		fmt.Fprintf(os.Stderr, "kdmcp: %v\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Print(`kdmcp - KeibiDrop as an MCP server

Move files directly between two machines, encrypted end to end, with no cloud
storage in between. One kdmcp process is one KeibiDrop peer.

USAGE:
  kdmcp                Run the server on stdio (what an MCP client launches).
  kdmcp serve          The same thing, named explicitly.
  kdmcp install        Print the MCP client config snippet for this machine.
  kdmcp version        Print the version.

ENVIRONMENT:
  KD_SHARE_ROOT        The one directory kd_send_file may send from. Unset means
                       sending is disabled and only receiving works. Symlinks are
                       resolved before the check.
  KD_SAVE_PATH         Where pulled files are written.
  KD_MOUNT_PATH        FUSE mount point. With FUSE present, the peer's files are
                       readable here and only the bytes you read cross the wire.
  KD_NO_FUSE           Set to force copy mode instead of the on-demand mount.
  KD_RELAY             Relay URL for peer discovery.
  KD_INBOUND_PORT      Listen port, 26000-27000. Give each peer on one host its own.
  KD_OUTBOUND_PORT     Outbound port, 26000-27000. Likewise.
  KEIBIDROP_CONFIG_DIR Config and identity directory. Give each peer on one host
                       its own, or they share an identity and cannot connect.
  KD_INCOGNITO         Ephemeral identity, nothing saved to disk.

Directories are created on start; there is no separate install step.
`)
}

// printInstall emits a ready-to-paste MCP client config. It names this
// binary's real path, so the snippet works wherever the binary landed.
func printInstall() {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "kdmcp"
	}
	home, _ := os.UserHomeDir()

	cfg := map[string]any{
		"mcpServers": map[string]any{
			"keibidrop": map[string]any{
				"command": exe,
				"env": map[string]string{
					"KD_SHARE_ROOT": home + "/KeibiDrop/Share",
					"KD_SAVE_PATH":  home + "/KeibiDrop/Received",
					"KD_MOUNT_PATH": home + "/KeibiDrop/Mount",
				},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")

	fmt.Fprintln(os.Stderr, "Add this to your MCP client config, then restart the client.")
	fmt.Fprintln(os.Stderr, "Claude Code: claude mcp add-json keibidrop '<the keibidrop object below>'")
	fmt.Fprintln(os.Stderr, "KD_SHARE_ROOT is the only directory this server may send from; narrow it.")
	fmt.Fprintln(os.Stderr)
	fmt.Println(string(b))
}
