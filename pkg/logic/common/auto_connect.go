// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
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
	// One loop per process. A second arm (a saved contact while the startup
	// arm runs) would dial beside the first; the running loop keeps its target
	// for this session, as KD_SetAutoConnectPeer documents.
	if !kd.autoConnectArmed.CompareAndSwap(false, true) {
		logger.Info("Auto-connect already armed, keeping its target for this session")
		return nil
	}
	logger.Info("Auto-connect armed")
	kd.autoConnectPaused.Store(false)
	go kd.autoConnectLoop(ctx, defaultAutoConnectTuning,
		func() error { return kd.watchdogDial(fp) },
		kd.IsRunning,
		kd.reconnectBusy)
	return nil
}

// watchdogDial is the loop's connect. It never displaces a connect a person
// or an agent started, and it joins one already running to its own peer.
func (kd *KeibiDrop) watchdogDial(fp string) error {
	if kd.AddressBook == nil || kd.AddressBook.Lookup(fp) == nil {
		return fmt.Errorf("contact not found in address book")
	}
	if err := kd.setPeerFingerprint(fp, originWatchdog); err != nil {
		return err
	}
	return kd.connect(originWatchdog)
}

// PauseAutoConnect parks the watchdog after a person cancelled its dial from
// the connect screen. It resumes once a session exists again, so a drop after
// a manual connect is still followed; a fresh StartAutoConnect arms unpaused.
func (kd *KeibiDrop) PauseAutoConnect() {
	if kd.autoConnectArmed.Load() {
		kd.autoConnectPaused.Store(true)
		kd.logger.Info("Auto-connect paused until the next session", "method", "auto-connect")
	}
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
// full rearm grace with no reconnect in progress (reconnect gave up). A peer
// that said goodbye leaves no reconnect to defer to, so the loop redials on
// the next poll: measured, the grace alone cost 90 of the 112 s a laptop took
// to come back after a clean box restart.
func (kd *KeibiDrop) autoConnectLoop(ctx context.Context, tun autoConnectTuning,
	dial func() error, isRunning func() bool, busy func() bool) {
	logger := kd.logger.With("method", "auto-connect")
	// The loop ends only with its context. Say so, and let the armed flag
	// follow: a dead loop that still read as armed made the state line promise
	// "will keep trying" and refused a re-arm (0.4.8, 2026-09-16).
	defer func() {
		logger.Info("Auto-connect watchdog stopped", "reason", ctx.Err())
		kd.autoConnectArmed.Store(false)
	}()
	backoff := tun.initialBackoff
	hadSession := false
	deferred := false // logged once per stretch of dials refused as in-flight
	var idleSince time.Time

	for ctx.Err() == nil {
		switch {
		case isRunning():
			hadSession = true
			idleSince = time.Time{}
			backoff = tun.initialBackoff
			kd.autoConnectPaused.Store(false)
			if !sleepCtx(ctx, tun.poll) {
				return
			}
		case kd.autoConnectPaused.Load():
			// A person cancelled the dial. Their word stands until a session
			// exists again.
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
		case hadSession && !kd.peerSaidGoodbye.Load() && time.Since(idleSince) < tun.rearmGrace:
			if !sleepCtx(ctx, tun.poll) {
				return
			}
		default:
			// The goodbye is consumed here, not on every running poll: a poll that
			// landed between the peer's goodbye and the session's end cleared the
			// flag and cost the full rearm grace (measured 2026-09-09 on the box:
			// 95 s to redial after a clean disconnect, against 8 s the times the
			// poll fell on the other side of that gap).
			kd.peerSaidGoodbye.Store(false)
			logger.Info("Auto-connect dialing")
			if err := dial(); err != nil {
				if errors.Is(err, ErrConnectInProgress) {
					// Another caller holds the connect (a click, an agent). Not a
					// failed dial: poll again, and keep the backoff where it was.
					if !deferred {
						logger.Info("Auto-connect deferred: a connect is already in flight")
						deferred = true
					}
					if !sleepCtx(ctx, tun.poll) {
						return
					}
					continue
				}
				deferred = false
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
			deferred = false
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
