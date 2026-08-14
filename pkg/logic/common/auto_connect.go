// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// autoConnectTuning holds the watchdog intervals. Tests shrink them.
type autoConnectTuning struct {
	poll           time.Duration // liveness poll while connected or deferring
	initialBackoff time.Duration // first retry delay after a failed dial
	maxBackoff     time.Duration // backoff cap
	rearmGrace     time.Duration // continuous downtime before a re-dial after a session existed
}

// defaultAutoConnectTuning: the rearm grace stays above the ReconnectManager
// retry window, so the watchdog never dials while reconnect still owns the
// session.
var defaultAutoConnectTuning = autoConnectTuning{
	poll:           5 * time.Second,
	initialBackoff: 5 * time.Second,
	maxBackoff:     2 * time.Minute,
	rearmGrace:     90 * time.Second,
}

// ResolveContact maps a saved contact name (case-insensitive) or fingerprint
// to the contact's fingerprint.
func (kd *KeibiDrop) ResolveContact(nameOrFingerprint string) (string, error) {
	if kd.AddressBook == nil {
		return "", fmt.Errorf("no address book: auto_connect_peer needs a persistent identity (incognito off)")
	}
	for _, c := range kd.AddressBook.List() {
		if strings.EqualFold(c.Name, nameOrFingerprint) {
			return c.Fingerprint, nil
		}
	}
	if c := kd.AddressBook.Lookup(nameOrFingerprint); c != nil {
		return c.Fingerprint, nil
	}
	return "", fmt.Errorf("auto_connect_peer %q is not a saved contact; add it with add-contact or save-contact", nameOrFingerprint)
}

// StartAutoConnect resolves AutoConnectPeer and arms the connect watchdog.
// A no-op when the field is empty. Returns an error when the value does not
// resolve to a saved contact, so frontends can surface a clear message at
// startup instead of a silent dead loop.
func (kd *KeibiDrop) StartAutoConnect(ctx context.Context) error {
	target := kd.AutoConnectPeer
	if target == "" {
		return nil
	}
	fp, err := kd.ResolveContact(target)
	if err != nil {
		kd.logger.Error("Auto-connect disabled", "peer", target, "error", err)
		return err
	}
	logger := kd.logger.With("method", "auto-connect", "peer", target)
	logger.Info("Auto-connect armed")
	go kd.autoConnectLoop(ctx, defaultAutoConnectTuning,
		func() error { return kd.ConnectToContact(fp) },
		kd.IsRunning,
		kd.reconnectBusy)
	return nil
}

// reconnectBusy reports whether the reconnect machinery currently owns the
// session: while it retries or waits for the peer, the watchdog must not dial.
func (kd *KeibiDrop) reconnectBusy() bool {
	kd.mu.Lock()
	rm := kd.ReconnectManager
	kd.mu.Unlock()
	if rm == nil {
		return false
	}
	switch rm.State() {
	case session.ReconnectStateReconnecting, session.ReconnectStateWaitingPeer:
		return true
	default:
		return false
	}
}

// autoConnectLoop keeps one contact connected: dial with exponential backoff,
// then sit idle while the session runs. Transient drops belong to the
// ReconnectManager; the loop re-dials only after the session has been down a
// full rearm grace with no reconnect in progress (peer-initiated disconnect or
// reconnect gave up).
func (kd *KeibiDrop) autoConnectLoop(ctx context.Context, tun autoConnectTuning,
	dial func() error, isRunning func() bool, busy func() bool) {
	logger := kd.logger.With("method", "auto-connect")
	backoff := tun.initialBackoff
	hadSession := false
	var idleSince time.Time

	for ctx.Err() == nil {
		switch {
		case isRunning():
			hadSession = true
			idleSince = time.Time{}
			backoff = tun.initialBackoff
			if !sleepCtx(ctx, tun.poll) {
				return
			}
		case busy():
			idleSince = time.Time{}
			if !sleepCtx(ctx, tun.poll) {
				return
			}
		case hadSession && idleSince.IsZero():
			idleSince = time.Now()
			if !sleepCtx(ctx, tun.poll) {
				return
			}
		case hadSession && time.Since(idleSince) < tun.rearmGrace:
			if !sleepCtx(ctx, tun.poll) {
				return
			}
		default:
			logger.Info("Auto-connect dialing")
			if err := dial(); err != nil {
				logger.Warn("Auto-connect attempt failed", "error", err, "retry_in", backoff)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff *= 2
				if backoff > tun.maxBackoff {
					backoff = tun.maxBackoff
				}
				continue
			}
			idleSince = time.Time{}
		}
	}
}

// sleepCtx sleeps d or returns false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
