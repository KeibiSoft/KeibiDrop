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
	logger := s.logger.New("method", "transition")

	if s.State == next {
		return nil
	}
	validNext := allowedTransitions[s.State]
	for _, state := range validNext {
		if state == next {
			s.logger.Info("Session state transition", "from", s.State, "to", next)
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
	logger := s.logger.New("method", "validate-ready")

	if s.State != SessionStateConnected {
		err := fmt.Errorf("invalid session state: %s (expected 'connected')", s.State)
		logger.Error("Session not ready: wrong state", "state", s.State, "error", err)
		return err
	}
	if s.KEK == nil {
		err := fmt.Errorf("KEK not derived")
		logger.Error("Session not ready: missing KEK", "error", err)
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
	logger := s.logger.New("method", "validate-peer")

	if s.State != SessionStateVerified && s.State != SessionStateConnected {
		err := fmt.Errorf("peer not verified (state: %s)", s.State)
		logger.Error("Session not ready: wrong state", "error", err)
		return err
	}
	if len(s.PeerPubKeys) == 0 {
		err := fmt.Errorf("missing peer public keys")
		logger.Error("Session not ready: missing public keys", "error", err)
		return err
	}
	if s.PeerFingerprint == "" {
		err := fmt.Errorf("missing peer fingerprint")
		logger.Error("Session not ready: missing fingerprint", "error", err)
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
