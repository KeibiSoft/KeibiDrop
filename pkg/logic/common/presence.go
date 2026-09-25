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
// It runs until the caller cancels ctx. Call it after EnablePersistentIdentity.
func (kd *KeibiDrop) StartPresenceHeartbeat(ctx context.Context) {
	if kd.Identity == nil || kd.AddressBook == nil || kd.Incognito {
		return
	}

	logger := kd.logger.With("method", "presence-heartbeat")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Send the first heartbeat immediately.
	kd.sendPresenceForAll(logger)

	for {
		select {
		case <-ctx.Done():
			// Contacts stop seeing this peer as online from here. Say so: a
			// heartbeat that went silent read as a hang (0.4.8, 2026-09-16).
			logger.Info("Presence heartbeat stopped", "reason", ctx.Err())
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

// relayPresenceTTL is how long the relay keeps a heartbeat (its presenceTTL).
// A contact is "absent" only after longer than this with nothing observed, so
// a single missed beat never counts.
const relayPresenceTTL = 60 * time.Second

// contactPresent asks the relay and remembers a yes. asked is false when the
// relay could not be reached at all, which is not the same answer as "absent":
// the memory is what makes the bridge-leg gate safe, and an unanswered
// question must never cost a NAS its leg.
func (kd *KeibiDrop) contactPresent(fingerprint string) (present, asked bool) {
	present, asked = kd.presenceOf(fingerprint)
	if present {
		kd.presenceSeen.Store(fingerprint, time.Now().Unix())
	}
	return present, asked
}

// lastSeenPresent returns when the relay last reported this contact online in
// this process, and whether that ever happened.
func (kd *KeibiDrop) lastSeenPresent(fingerprint string) (time.Time, bool) {
	v, ok := kd.presenceSeen.Load(fingerprint)
	if !ok {
		return time.Time{}, false
	}
	at, ok := v.(int64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(at, 0), true
}

// shouldParkBridgeLeg reports whether this rendezvous round parks a paid leg
// on the bridge. A leg parked for an absent contact expires after ten minutes
// and costs the bridge a release and re-claim of its prepaid chain. The leg is
// skipped only for a saved contact on a 0.4.9 or newer build, seen present once
// in this process and unseen by the relay for longer than its presence TTL.
func (kd *KeibiDrop) shouldParkBridgeLeg(peerFP string) (bool, string) {
	switch {
	case kd.IsLocalMode, peerFP == "", peerFP == "TOFU":
		return true, ""
	case kd.AddressBook == nil || kd.AddressBook.Lookup(peerFP) == nil:
		return true, "" // a first-time peer: unchanged
	case !kd.presenceReliableFor(peerFP):
		return true, "" // pre-0.4.9: presence dies with the heartbeat
	case kd.RelayEndoint == nil || kd.Identity == nil:
		return true, ""
	}
	present, asked := kd.contactPresent(peerFP)
	if present || !asked {
		// Present, or the relay did not answer. Either way, park.
		return true, ""
	}
	seenAt, ever := kd.lastSeenPresent(peerFP)
	if !ever {
		// Never seen online: the peer may not post presence at all.
		return true, ""
	}
	if time.Since(seenAt) <= relayPresenceTTL {
		return true, "" // inside the TTL: one missed beat
	}
	return false, "Peer not present on the relay; not parking a bridge leg"
}

// presenceReliableFor reports whether the contact's build keeps posting
// presence while online: its last handshake here declared a cipher.
func (kd *KeibiDrop) presenceReliableFor(fingerprint string) bool {
	_, ok := kd.presenceReliable.Load(fingerprint)
	return ok
}

// CheckContactPresence reports whether the relay saw the contact online recently.
func (kd *KeibiDrop) CheckContactPresence(fingerprint string) bool {
	present, _ := kd.presenceOf(fingerprint)
	return present
}

// presenceOf is CheckContactPresence plus whether the relay answered at all.
func (kd *KeibiDrop) presenceOf(fingerprint string) (present, asked bool) {
	if kd.Identity == nil || kd.RelayEndoint == nil {
		return false, false
	}
	token, err := kbc.DerivePresenceKey(fingerprint, kd.Identity.Fingerprint)
	if err != nil {
		return false, false
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

// getPresence returns the relay's answer and whether there was one. A
// transport failure or a 5xx is not an answer, and callers that act on absence
// must be able to tell the two apart.
func (kd *KeibiDrop) getPresence(token []byte) (present, asked bool) {
	if kd.RelayEndoint == nil {
		return false, false
	}

	url := kd.RelayEndoint.JoinPath("/presence").String()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(token))

	resp, err := kd.relayClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		return false, false
	}
	return resp.StatusCode == http.StatusOK, true
}
