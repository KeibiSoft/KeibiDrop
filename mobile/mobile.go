// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	keibidrop "github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

type API struct {
	KeibiDrop        *keibidrop.KeibiDrop
	running          *bool
	ctxCancel        context.CancelFunc
	logger           *slog.Logger
	op               *opState
	opTimeoutSeconds int
}

func (api *API) Initialize(useLogFile bool, logFilePath string, relayURL string, inboundPort int, outboundPort int) error {
	var wr *os.File = os.Stderr
	if logFilePath != "" {
		f, err := os.OpenFile(filepath.Clean(logFilePath),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			slog.Warn("Failed to open log file, defaulting to stderr",
				"path", logFilePath,
				"error", err)
		} else {
			wr = f
		}
	}

	// TODO: remove this.
	wr = os.Stdout

	// text output, level=DEBUG
	handler := slog.NewTextHandler(wr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	logger := slog.New(handler).With("component", "mobile")

	parsedURL, err := url.Parse(relayURL)
	if err != nil {
		logger.Error("Invalid KEIBIDROP_RELAY URL", "error", err)
		return err
	}

	kdctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kd, err := common.NewKeibiDrop(kdctx, logger, false, parsedURL, inboundPort, outboundPort, "", "")
	if err != nil {
		logger.Error("Failed to start keibidrop", "error", err)
		return err
	}

	api.ctxCancel = cancel
	api.KeibiDrop = kd
	api.logger = logger
	running := false
	api.running = &running

	api.op = newOpState()
	api.opTimeoutSeconds = 10 * 60 // 10 minutes default

	return nil
}

// Blocking, add it as a long running process.
func (api *API) Start() error {
	if api.KeibiDrop == nil || api.running == nil {
		slog.Error("Failed to start, API not initialized")
		return fmt.Errorf("faield to start, API not initialized")
	}
	if *api.running {
		api.logger.Error("API already running")
		return fmt.Errorf("failed to stard API already running")
	}

	go api.KeibiDrop.Run()
	for {
		time.Sleep(time.Second)
		if !*api.running {
			return nil
		}
	}
}

func (api *API) Stop() error {
	if api.KeibiDrop == nil || api.running == nil {
		slog.Error("Failed to stop, API not initialized")
		return fmt.Errorf("failed to stop, API not initialized")
	}
	api.ctxCancel()
	*api.running = false
	return nil
}

// The actual logic.

func (api *API) CreateRoom() error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	return api.KeibiDrop.CreateRoom()
}

// JoinRoom connects to a peer room using fingerprint.
func (api *API) JoinRoom() error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	return api.KeibiDrop.JoinRoom()
}

// RegisterPeer registers a peer fingerprint.
func (api *API) RegisterPeer(fingerprint string) error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	return api.KeibiDrop.AddPeerFingerprint(fingerprint)
}

// AddFile shares a local file or directory.
func (api *API) AddFile(path string) error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	return api.KeibiDrop.AddFile(path)
}

// PullFile copies a file from peer.
func (api *API) PullFile(remote string, local string) error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	return api.KeibiDrop.PullFile(remote, local)
}

// Fingerprint returns local fingerprint.
func (api *API) Fingerprint() (string, error) {
	if api.KeibiDrop == nil {
		return "", fmt.Errorf("api not initialized")
	}
	return api.KeibiDrop.ExportFingerprint()
}

// PeerFingerprint returns peer fingerprint.
func (api *API) PeerFingerprint() string {
	if api.KeibiDrop == nil {
		return ""
	}
	fp, _ := api.KeibiDrop.GetPeerFingerprint()
	return fp
}

// RelayEndpoint returns the relay URL string.
func (api *API) RelayEndpoint() string {
	if api.KeibiDrop == nil {
		return ""
	}
	return api.KeibiDrop.RelayEndoint.String()
}

// Async room Create/ Connect

func (api *API) CreateRoomAsync() error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	if api.op == nil {
		api.op = newOpState()
	}
	status, _ := api.op.get()
	if status == OpStatusRunning {
		return fmt.Errorf("operation already in progress")
	}
	api.op.set(OpStatusRunning, "creating room")
	api.KeibiDrop.OpInProgress.Add(1)
	go func() {
		defer api.KeibiDrop.OpInProgress.Add(-1)
		err := api.CreateRoom()
		if err != nil {
			api.op.set(OpStatusFailed, err.Error())
			return
		}
		api.op.set(OpStatusSucceeded, "peer connected: "+api.KeibiDrop.PeerIPv6IP)
	}()
	return nil
}

func (api *API) JoinRoomAsync(peerFingerprint string) error {
	if api.KeibiDrop == nil {
		return fmt.Errorf("api not initialized")
	}
	if api.op == nil {
		api.op = newOpState()
	}
	status, _ := api.op.get()
	if status == OpStatusRunning {
		return fmt.Errorf("operation already in progress")
	}
	api.op.set(OpStatusRunning, "joining room")
	api.KeibiDrop.OpInProgress.Add(1)
	go func() {
		defer api.KeibiDrop.OpInProgress.Add(-1)
		err := api.JoinRoom()
		if err != nil {
			api.op.set(OpStatusFailed, err.Error())
			return
		}
		api.op.set(OpStatusSucceeded, "joined: "+api.KeibiDrop.PeerIPv6IP)
	}()

	return nil
}

func (api *API) CancelOp() error {
	if api.op == nil {
		return fmt.Errorf("nil op")
	}
	api.op.set(OpStatusFailed, "cancelled by user")
	return api.Stop()
}

// GetOpStatus returns (status, message) for the current op.
func (api *API) GetOpStatus() *OpStatus {
	if api.op == nil {
		return &OpStatus{
			Status: OpStatusIdle,
		}
	}
	// timeout handling: if running for longer than opTimeoutSeconds, mark timeout
	status, msg := api.op.get()
	if status == OpStatusRunning {
		// copy to avoid race on opTimeoutSeconds
		timeout := 600 // default 10 minutes
		if api.opTimeoutSeconds != 0 {
			timeout = api.opTimeoutSeconds
		}
		// get duration
		api.op.mu.Lock()
		start := api.op.startedAt
		api.op.mu.Unlock()
		if time.Since(start) > time.Duration(timeout)*time.Second {
			api.op.set(OpStatusTimeout, "operation timed out")
			status, msg = api.op.get()
		}
	}
	return &OpStatus{
		Status:  status,
		Message: msg,
	}
}
