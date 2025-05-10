package session

import (
	"fmt"
)

// Valid transitions: from → list of allowed targets
var allowedTransitions = map[string][]string{
	SessionStatePending:   {SessionStateVerified, SessionStateError, SessionStateExpired},
	SessionStateVerified:  {SessionStateConnected, SessionStateError, SessionStateExpired},
	SessionStateConnected: {SessionStateError, SessionStateExpired},
	SessionStateError:     {},
	SessionStateExpired:   {},
}

// Transition safely updates the session state, if allowed.
func (s *Session) Transition(next string) error {
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
	return fmt.Errorf("invalid session state transition: %s → %s", s.State, next)
}

// ValidateReady ensures session is fully ready for encryption and data transfer.
func (s *Session) ValidateReady() error {
	if s.State != SessionStateConnected {
		return fmt.Errorf("invalid session state: %s (expected 'connected')", s.State)
	}
	if s.KEK == nil {
		return fmt.Errorf("KEK not derived")
	}
	if s.Session == nil || s.Session.Inbound == nil || s.Session.Outbound == nil {
		return fmt.Errorf("secure connections not initialized")
	}
	return nil
}

// ValidatePeer ensures peer handshake and verification are complete.
func (s *Session) ValidatePeer() error {
	if s.State != SessionStateVerified && s.State != SessionStateConnected {
		return fmt.Errorf("peer not verified (state: %s)", s.State)
	}
	if len(s.PeerPubKeys) == 0 {
		return fmt.Errorf("missing peer public keys")
	}
	if s.PeerFingerprint == "" {
		return fmt.Errorf("missing peer fingerprint")
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
