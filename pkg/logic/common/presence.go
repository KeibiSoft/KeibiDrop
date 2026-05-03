// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// StartPresenceHeartbeat sends periodic presence heartbeats for all contacts.
// Runs until ctx is cancelled. Should be called after EnablePersistentIdentity.
func (kd *KeibiDrop) StartPresenceHeartbeat(ctx context.Context) {
	if kd.Identity == nil || kd.AddressBook == nil || kd.Incognito {
		return
	}

	logger := kd.logger.With("method", "presence-heartbeat")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Immediate first tick.
	kd.sendPresenceForAll(logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			kd.sendPresenceForAll(logger)
		}
	}
}

func (kd *KeibiDrop) sendPresenceForAll(logger interface{ Info(string, ...any) }) {
	contacts := kd.AddressBook.List()
	for _, c := range contacts {
		token, err := kbc.DerivePresenceKey(kd.Identity.Fingerprint, c.Fingerprint)
		if err != nil {
			continue
		}
		_ = kd.postPresence(token)
	}
	if len(contacts) > 0 {
		logger.Info("Presence heartbeat sent", "contacts", len(contacts))
	}
}

// CheckContactPresence returns true if the contact was seen online recently.
func (kd *KeibiDrop) CheckContactPresence(fingerprint string) bool {
	if kd.Identity == nil || kd.RelayEndoint == nil {
		return false
	}
	token, err := kbc.DerivePresenceKey(kd.Identity.Fingerprint, fingerprint)
	if err != nil {
		return false
	}
	return kd.getPresence(token)
}

func (kd *KeibiDrop) postPresence(token []byte) error {
	if kd.RelayEndoint == nil {
		return fmt.Errorf("no relay configured")
	}

	url := kd.RelayEndoint.JoinPath("/presence").String()
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(token))

	resp, err := kd.relayClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (kd *KeibiDrop) getPresence(token []byte) bool {
	if kd.RelayEndoint == nil {
		return false
	}

	url := kd.RelayEndoint.JoinPath("/presence").String()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(token))

	resp, err := kd.relayClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
