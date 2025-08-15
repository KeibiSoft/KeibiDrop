package common

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/inconshreveable/log15"
)

type KeibiDrop struct {
	logger       log15.Logger
	relayClient  *http.Client
	RelayEndoint *url.URL

	session *session.Session

	PeerIPv6IP string

	LocalIPv6IP string
	inboundPort int
	listener    net.Listener
}

// Factory-style constructor
func NewKeibiDrop(logger log15.Logger, relayURL *url.URL, inboundPort int, defaultOutboundPort int) (*KeibiDrop, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	session, err := session.InitSession(logger, defaultOutboundPort, inboundPort)
	if err != nil {
		logger.Error("Failed to init session", "error", err)
		return nil, err
	}

	addr := net.JoinHostPort("::", strconv.Itoa(inboundPort))
	listener, err := net.Listen("tcp6", addr)
	if err != nil {
		return nil, err
	}

	ipv6, err := GetGlobalIPv6()
	if err != nil {
		logger.Error("Failed to get local IPv6", "error", err)
		return nil, err
	}

	kd := &KeibiDrop{
		logger:       logger,
		relayClient:  client,
		RelayEndoint: relayURL,
		session:      session,
		LocalIPv6IP:  ipv6,
		inboundPort:  inboundPort,
		listener:     listener,
	}

	return kd, nil
}

type PeerRegistration struct {
	Fingerprint string            `json:"fingerprint"`
	PublicKeys  map[string]string `json:"public_keys"` // base64 encoded
	Listen      *ConnectionHint   `json:"listen"`
	Reverse     *ConnectionHint   `json:"reverse,omitempty"`
	Timestamp   int64             `json:"timestamp"`
}

type ConnectionHint struct {
	IP    string `json:"ip"`             // public IP address (either v4 or v6)
	Port  int    `json:"port"`           // where peer is listening
	IPv6  bool   `json:"ipv6"`           // does this prefer IPv6?
	Proto string `json:"proto"`          // e.g., "tcp"
	Note  string `json:"note,omitempty"` // optional: NAT behavior, etc.
}

// Map server status errors to semantic errors.
type ErrorMapperFunc func(statusCode int, err error) error
