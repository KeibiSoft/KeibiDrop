package common

import (
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

func (kd *KeibiDrop) ExportFingerprint() (string, error) {
	logger := kd.logger.New("method", "export-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	fp := kd.session.OwnFingerprint

	logger.Info("Success", "fingerprint", fp)

	return fp, nil
}

func (kd *KeibiDrop) AddPeerFingerprint(fp string) error {
	logger := kd.logger.New("method", "add-per-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	err := ValidateFingerprint(fp)
	if err != nil {
		logger.Error("Failed to validate fingerprint", "error", err)
		return err
	}

	kd.session.ExpectedPeerFingerprint = fp

	return nil
}

func (kd *KeibiDrop) RegisterRoomToRelay() error {
	logger := kd.logger.New("method", "register-room-to-relay")
	if kd.relayClient == nil || kd.session == nil || kd.session.OwnKeys == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	ownFp := kd.session.OwnFingerprint

	pkMap, err := kd.session.OwnKeys.ExportPubKeysAsMap()
	if err != nil {
		logger.Error("Failed to export own keys", "error", err)
		return err
	}

	peerReg := PeerRegistration{
		Fingerprint: ownFp,
		Listen: &ConnectionHint{
			IPv6:  true,
			IP:    kd.localIPv6IP,
			Proto: "tcp",
			Port:  kd.inboundPort,
		},
		PublicKeys: pkMap,
		Timestamp:  time.Now().UnixNano(),
	}

	resp, err := PostJSONWithURL(kd.relayClient, kd.relayEndoint, peerReg, RegisterErrorMapper)
	if err != nil {
		logger.Error("Failed to register", "error", err)
		// TODO: On the caller of this method; handle the retry logic, and appropriate display of message.
		return err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		logger.Warn("We got a weird status code", "code", resp.StatusCode)
	}

	logger.Info("Success")

	return nil
}

func (kd *KeibiDrop) CreateRoom() error {
	logger := kd.logger.New("method", "create-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	// This is the "Alice" flow

	err := kd.RegisterRoomToRelay()
	if err != nil {
		return err
	}

	// Get input to for expected fingerprint.

	elapsed := 0
	for {
		if elapsed >= 10*60-5 {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			time.Sleep(time.Second)
			elapsed++
			continue
		}
		break
	}

	// Listen for the Bob Connection.

	// TODO: Add retries
	conn, err := kd.listener.Accept()
	if err != nil {
		logger.Error("Failed to accept", "error", err)
		return err
	}

	// TODO: Add uniform error, use errors.Is(err, ErrCustomDefinedError) to do the retry logic.
	err = session.PerformInboundHandshake(kd.session, conn)
	if err != nil {
		return err
	}

	// TODO: Add the port for inbound bob.
	upgConn, err := session.PerformOutboundHandshake(kd.session, conn.RemoteAddr().String())
	if err != nil {
		return err
	}

	_ = upgConn // This conn should be added in the session in the above method.

	// From now on we use HTTP with JSON for data exchange.

	logger.Info("Success")
	return nil
}

func (kd *KeibiDrop) JoinRoom(fp string) error {
	logger := kd.logger.New("method", "join-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	// This is the "Bob" flow

	// TODO: Exact reverse flow from the above method.

	logger.Info("Success", "fingerprint", fp)
	return nil
}

func (kd *KeibiDrop) MountFilesystem() error {
	kd.logger.Info("Mounting virtual filesystem")
	return nil
}

func (kd *KeibiDrop) UnmountFilesystem() error {
	kd.logger.Info("Unmounting virtual filesystem")
	return nil
}

func (kd *KeibiDrop) ResetSession() {
	kd.logger.Info("Resetting session state")

	// You probably want to close any existing net.Conn, etc.
}

func (kd *KeibiDrop) RegenerateKeys() error {
	kd.logger.Info("Regenerating ephemeral keys")

	return nil
}
