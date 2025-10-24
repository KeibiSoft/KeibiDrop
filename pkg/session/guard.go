// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"fmt"
)

// Valid transitions: from → list of allowed targets
var allowedTransitions = map[SessionState][]SessionState{
	SessionStatePending:   {SessionStateVerified, SessionStateError, SessionStateExpired},
	SessionStateVerified:  {SessionStateConnected, SessionStateError, SessionStateExpired},
	SessionStateConnected: {SessionStateError, SessionStateExpired},
	SessionStateError:     {},
	SessionStateExpired:   {},
}

// Transition safely updates the session state, if allowed.
func (s *Session) Transition(next SessionState) error {
	logger := s.logger.With("method", "transition")

	if s.State == next {
		return nil
	}
	validNext := allowedTransitions[s.State]
	for _, state := range validNext {
		if state == next {
			logger.Info("Session state transition", "from", s.State, "to", next)
			s.State = next
			return nil
		}
	}
	err := fmt.Errorf("invalid session state transition: %s → %s", s.State, next)
	logger.Error("Session not ready", "error", err)
	return err
}

// ValidateReady ensures session is fully ready for encryption and data transfer.
func (s *Session) ValidateReady() error {
	logger := s.logger.With("method", "validate-ready")

	if s.State != SessionStateConnected {
		err := fmt.Errorf("invalid session state: %s (expected 'connected')", s.State)
		logger.Error("Session not ready: wrong state", "state", s.State, "error", err)
		return err
	}
	if s.SEKInbound == nil {
		err := fmt.Errorf("inbound SEK not derived")
		logger.Error("Session not ready: missing inbound SEK", "error", err)
		return err
	}
	if s.SEKOutbound == nil {
		err := fmt.Errorf("outbound SEK not derived")
		logger.Error("Session not ready: missing outbound SEK", "error", err)
		return err
	}

	if s.Session == nil || s.Session.Inbound == nil || s.Session.Outbound == nil {
		err := fmt.Errorf("secure connections not initialized")
		logger.Error("Session not ready: SecureConn missing", "error", err)
		return err
	}
	return nil
}

// ValidatePeer ensures peer handshake and verification are complete.
func (s *Session) ValidatePeer() error {
	logger := s.logger.With("method", "validate-peer")

	if s.State != SessionStateVerified && s.State != SessionStateConnected {
		err := fmt.Errorf("peer not verified (state: %s)", s.State)
		logger.Error("Session not ready: wrong state", "error", err)
		return err
	}

	err := s.PeerPubKeys.Validate()
	if err != nil {
		logger.Error("Failed to validate peer public keys", "error", err)
		return err
	}

	return nil
}

// ValidateExpired moves session to 'expired' state if deadline passed.
func (s *Session) ValidateExpired() {
	if s.State == SessionStateConnected || s.State == SessionStateError || s.State == SessionStateExpired {
		return
	}
	if s.IsExpired() {
		_ = s.Transition(SessionStateExpired)
	}
}
