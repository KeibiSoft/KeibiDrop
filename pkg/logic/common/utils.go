package common

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

func (kd *KeibiDrop) registerRoomToRelay() error {
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
			IP:    kd.LocalIPv6IP,
			Proto: "tcp",
			Port:  kd.inboundPort,
		},
		PublicKeys: pkMap,
		Timestamp:  time.Now().UnixNano(),
	}

	path, err := url.JoinPath(kd.RelayEndoint.String(), "register")
	if err != nil {
		logger.Error("Failed to add register path", "error", err)
		return err
	}

	registerUrl, err := url.Parse(path)
	if err != nil {
		logger.Error("Failed to parse url", "error", err)
		return err
	}

	resp, err := PostJSONWithURL(kd.relayClient, registerUrl, map[string]string{"Authorization": "Bearer " + ownFp}, peerReg, RegisterErrorMapper)
	if err != nil {
		logger.Error("Failed to register", "error", err)
		// TODO: On the caller of this method; handle the retry logic, and appropriate display of message.
		return err
	}

	// Server reached its limit. Retry in 10 minutes or later.
	if resp.StatusCode == http.StatusServiceUnavailable {
		logger.Warn("Relay server at full capacity retry in 10 minutes")
		return ErrRelayAtFullCapacityRetryLater
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		logger.Warn("We got a weird status code", "code", resp.StatusCode)
	}

	_ = resp.Body.Close()

	logger.Info("Success")

	return nil
}

func (kd *KeibiDrop) getRoomFromRelay(outOfBandFingerPrint string) error {
	logger := kd.logger.New("method", "get-room-from-relay")
	if kd.relayClient == nil || kd.session == nil || kd.session.OwnKeys == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	path, err := url.JoinPath(kd.RelayEndoint.String(), "fetch")
	if err != nil {
		logger.Error("Failed to add fetch path", "error", err)
		return err
	}

	fetchUrl, err := url.Parse(path)
	if err != nil {
		logger.Error("Failed to parse url", "error", err)
		return err
	}

	resp, err := GetJSONWithURL(kd.relayClient, fetchUrl, map[string]string{"Authorization": "Bearer " + outOfBandFingerPrint}, RegisterErrorMapper)
	if err != nil {
		logger.Error("Failed to fetch", "error", err)
		// TODO: On the caller of this method; handle the retry logic, and appropriate display of message.
		return err
	}

	if resp.StatusCode == http.StatusNotFound {
		logger.Warn("Not found")
		return ErrNotFound
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		logger.Warn("We got a weird status code", "code", resp.StatusCode)
	}

	peerReg := &PeerRegistration{}

	if err := json.NewDecoder(resp.Body).Decode(peerReg); err != nil {
		logger.Error("Failed to decode JSON", "error", err)
		return err
	}

	if peerReg.Listen == nil {
		logger.Error("Invalid listen details")
		return ErrInvalidResponse
	}

	if subtle.ConstantTimeCompare([]byte(peerReg.Fingerprint), []byte(outOfBandFingerPrint)) != 1 {
		logger.Warn("Fingerprint mismatch")
		return ErrFingerprintMismatch
	}

	peerKeysMap := make(map[string][]byte)
	for k, v := range peerReg.PublicKeys {
		asByte, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			logger.Error("Failed to decode peer public key", "alg", k, "error", err)
			return err
		}
		peerKeysMap[k] = asByte
	}

	peerKeys, err := crypto.ParsePeerKeys(peerKeysMap)
	if err != nil {
		logger.Error("Failed to parse peer keys", "error", err)
		return err
	}

	err = peerKeys.Validate()
	if err != nil {
		logger.Error("Failed to validate peer keys", "error", err)
		return err
	}

	computedFp, err := peerKeys.Fingerprint()
	if err != nil {
		logger.Error("Failed to compute peer fingerprint", "error", err)
		return err
	}

	if subtle.ConstantTimeCompare([]byte(computedFp), []byte(outOfBandFingerPrint)) != 1 {
		logger.Warn("Fingerprint mismatch")
		return ErrFingerprintMismatch
	}

	kd.session.PeerPubKeys = peerKeys
	if peerReg.Listen.Port < 26000 || peerReg.Listen.Port > 27000 {
		logger.Warn("Provided outbound port is out of known range, defaulting to config", "provided-port", peerReg.Listen.Port, "default-to", config.OutboundPort)
		peerReg.Listen.Port = config.OutboundPort
	}

	kd.session.PeerPort = peerReg.Listen.Port
	if !isValidIPv6(peerReg.Listen.IP) {
		logger.Warn("Invalid peer IP", "got", peerReg.Listen.IP, "error", ErrInvalidIP)
		return ErrInvalidIP
	}

	kd.PeerIPv6IP = peerReg.Listen.IP

	logger.Info("Success")

	return nil
}

func isValidIPv6(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	// TODO: Comment
	return ip != nil && ip.To4() == nil
	return ip != nil && ip.To4() == nil && !ip.IsLoopback() && !ip.IsPrivate()
}
