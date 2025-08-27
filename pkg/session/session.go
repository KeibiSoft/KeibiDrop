package session

import (
	"net"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/inconshreveable/log15"
)

type SessionState string

const (
	SessionInit           SessionState = "init"
	SessionStatePending   SessionState = "pending"
	SessionStateVerified  SessionState = "verified"
	SessionStateConnected SessionState = "connected"
	SessionStateError     SessionState = "error"
	SessionStateExpired   SessionState = "expired"
)

// Session represents the state of a P2P connection between Alice and Bob.
type Session struct {
	// Known fingerprint of the expected peer, shared out-of-band.
	ExpectedPeerFingerprint string

	OwnKeys        *kbc.OwnKeys
	OwnFingerprint string

	// Populated after receiving peer keys.
	PeerPubKeys *kbc.PeerKeys // "x25519", "mlkem"

	// Symmetric session key.
	SEKInbound  []byte
	SEKOutbound []byte

	// Peer-to-peer TCP connections.
	Session  *SessionSockets
	PeerPort int

	DefaultOutboundPort int
	DefaultInboundPort  int

	GRPCListener net.Listener
	GRPCClient   bindings.KeibiServiceClient

	// Session state and lifecycle
	State       SessionState
	Established time.Time

	Err error

	// Internal timeout deadline
	Deadline time.Time

	logger log15.Logger

	FS *filesystem.FS
}

func InitSession(logger log15.Logger, defaultOutboundPort int, defaultInboundPort int) (*Session, error) {
	logger = logger.New("service", "session")
	kemDecapsulate, kemEncapsulate, err := kbc.GenerateMLKEMKeypair()
	if err != nil {
		logger.Error("Failed to generate session mlkem keys", "error", err)
		return nil, err
	}

	ecdPriv, ecdPub, err := kbc.GenerateX25519Keypair()
	if err != nil {
		logger.Error("Failed to generate session x25519 keys", "error", err)
		return nil, err
	}

	ownKeys := kbc.OwnKeys{
		MlKemPrivate:  kemDecapsulate,
		MlKemPublic:   kemEncapsulate,
		X25519Private: ecdPriv,
		X25519Public:  ecdPub,
	}

	ownFingerprint, err := ownKeys.Fingerprint()
	if err != nil {
		logger.Error("Failed to compute fingerprint", "error", err)
		return nil, err
	}

	return &Session{
		logger:              logger,
		OwnKeys:             &ownKeys,
		OwnFingerprint:      ownFingerprint,
		State:               SessionInit,
		DefaultOutboundPort: defaultOutboundPort,
		DefaultInboundPort:  defaultInboundPort,
	}, nil
}

func (s *Session) GetFingerPrint() string {
	return s.OwnFingerprint
}

// SessionSockets holds a duplex connection for peer communication.
type SessionSockets struct {
	Inbound  *SecureConn // Bob -> Alice
	Outbound *SecureConn // Alice -> Bob
}

func NewSessionSockets(connIn, connOut net.Conn, sekIn, sekOut []byte) *SessionSockets {
	return &SessionSockets{
		Inbound:  NewSecureConn(connIn, sekIn),
		Outbound: NewSecureConn(connOut, sekOut),
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
	// We're in an error path; failing here is pointless. If you want strict handling, log it instead.
	_ = s.Transition(SessionStateError) // errors ignored to prevent panic
	s.Err = err

	if s.Session != nil {
		if s.Session.Inbound != nil {
			_ = s.Session.Inbound.Close()
		}
		if s.Session.Outbound != nil {
			_ = s.Session.Outbound.Close()
		}
	}
}

// IsVerified returns true if the fingerprint matched and session is accepted.
func (s *Session) IsVerified() bool {
	return s.State == SessionStateVerified || s.State == SessionStateConnected
}

// ReadyForEncryption returns true when both connections and SEKs are set.
func (s *Session) ReadyForEncryption() bool {
	return s.State == SessionStateConnected &&
		s.SEKInbound != nil &&
		s.SEKOutbound != nil &&
		s.Session != nil &&
		s.Session.Inbound != nil &&
		s.Session.Outbound != nil
}
