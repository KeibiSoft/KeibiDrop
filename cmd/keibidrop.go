// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"

	"github.com/KeibiSoft/KeibiDrop/cmd/internal/checkfuse"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/ui"
)

const NO_FUSE_ENV = "NO_FUSE"

func getenv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func main() {
	// Default to production relay unless overridden
	relayURL := getenv("KEIBIDROP_RELAY", "https://keibidroprelay.keibisoft.com")

	parsedURL, err := url.Parse(relayURL)
	if err != nil {
		log.Fatalf("invalid KEIBIDROP_RELAY URL: %v", err)
	}

	fmt.Println("Connecting to relay:", parsedURL.String())

	common.PrintBanner()

	// text output, level=DEBUG
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	logger := slog.New(handler).With("component", "cli")

	// Explicitly pass NO FUSE
	_, noFUSE := os.LookupEnv(NO_FUSE_ENV)
	isFuse := checkfuse.IsFUSEPresent()
	logger.Info("Is FUSE present", "val", isFuse)

	logger.Info("Do not use FUSE", "val", noFUSE)
	finalVal := isFuse && !noFUSE

	ui.Launch(logger, finalVal)
}
