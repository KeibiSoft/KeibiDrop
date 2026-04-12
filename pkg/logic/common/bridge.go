package common

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// bridgeRoomToken computes a deterministic 32-byte room token from two
// fingerprints. Both peers compute the same token regardless of who is
// the creator vs joiner because the fingerprints are sorted first.
func bridgeRoomToken(ownFingerprint, peerFingerprint string) [32]byte {
	fps := []string{ownFingerprint, peerFingerprint}
	sort.Strings(fps)
	return sha256.Sum256([]byte(fps[0] + fps[1]))
}

// dialBridge connects to the bridge relay and sends the room token.
// connIdx (0 or 1) distinguishes the two connection pairs so they cannot
// mis-pair when one side's connections arrive before the other's.
// Both peers use the same connIdx for the same logical connection:
//   CreateRoom: inConn=0, outConn=1
//   JoinRoom:   outConn=0, inConn=1
func (kd *KeibiDrop) dialBridge(logger *slog.Logger, connIdx uint8) (net.Conn, error) {
	conn, err := session.DialWithStableAddr("tcp", kd.BridgeAddr, 15*time.Second, logger)
	if err != nil {
		return nil, fmt.Errorf("dial bridge: %w", err)
	}

	ownFP := kd.session.OwnFingerprint
	peerFP := kd.session.ExpectedPeerFingerprint
	token := bridgeRoomToken(ownFP, peerFP)
	token[31] ^= connIdx // Distinct token per connection pair to prevent mis-pairing.

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(token[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send room token: %w", err)
	}
	conn.SetWriteDeadline(time.Time{})

	logger.Info("Bridge room token sent", "token", fmt.Sprintf("%x..%x", token[:4], token[28:]))
	return conn, nil
}
