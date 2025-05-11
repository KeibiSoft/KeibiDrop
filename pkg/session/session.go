package session

import (
	"net"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/inconshreveable/log15"
)

const (
	SessionStatePending   = "pending"
	SessionStateVerified  = "verified"
	SessionStateConnected = "connected"
	SessionStateError     = "error"
)

// Session represents the state of a P2P connection between Alice and Bob.
type Session struct {
	// Known fingerprint of the expected peer, shared out-of-band
	ExpectedPeerFingerprint string

	OwnKeys kbc.OwnKeys
	// Populated after receiving peer keys
	PeerFingerprint string
	PeerPubKeys     map[string][]byte // "x25519", "mlkem"

	// Symmetric session key
	KEK []byte

	// TCP connections
	Session *SessionSockets

	// Session state and lifecycle
	State       string
	Established time.Time
	Err         error

	// Internal timeout deadline
	Deadline time.Time

	logger log15.Logger
}

// SessionSockets holds a duplex connection for peer communication.
type SessionSockets struct {
	Inbound  *SecureConn // Bob -> Alice
	Outbound *SecureConn // Alice -> Bob
}

func NewSessionSockets(connIn, connOut net.Conn, kekIn, kekOut []byte) *SessionSockets {
	return &SessionSockets{
		Inbound:  NewSecureConn(connIn, kekIn),
		Outbound: NewSecureConn(connOut, kekOut),
	}
}

// NewSession initializes a new session with a timeout deadline.
func NewSession(logger log15.Logger, expectedFingerprint string, timeout time.Duration) *Session {
	return &Session{
		ExpectedPeerFingerprint: expectedFingerprint,
		State:                   SessionStatePending,
		Established:             time.Now(),
		Deadline:                time.Now().Add(timeout),
		logger:                  logger,
	}
}

// IsExpired returns true if the session has passed its allowed timeout.
func (s *Session) IsExpired() bool {
	return time.Now().After(s.Deadline)
}

// MarkError marks the session as errored and closes any open connections.
func (s *Session) MarkError(err error) {
	s.State = SessionStateError
	s.Err = err

	if s.Session != nil {
		if s.Session.Inbound != nil {
			s.Session.Inbound.Close()
		}
		if s.Session.Outbound != nil {
			s.Session.Outbound.Close()
		}
	}
}

// IsVerified returns true if the fingerprint matched and session is accepted.
func (s *Session) IsVerified() bool {
	return s.State == SessionStateVerified || s.State == SessionStateConnected
}

// ReadyForEncryption returns true when both connections and KEK are set.
func (s *Session) ReadyForEncryption() bool {
	return s.State == SessionStateConnected &&
		s.KEK != nil &&
		s.Session != nil &&
		s.Session.Inbound != nil &&
		s.Session.Outbound != nil
}
