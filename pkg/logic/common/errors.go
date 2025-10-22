package common

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrNilPointer                    = errors.New("nil pointer")
	ErrEmptyFingerprint              = errors.New("fingerprint is empty")
	ErrInvalidLength                 = errors.New("invalid length")
	ErrRelayAtMaximumCapacity        = errors.New("relay at maximum capacity")
	ErrRateLimitHit                  = errors.New("relay rate limit hit, retry in 5 minutes")
	ErrMissingFingerprint            = errors.New("missing fingerprint header")
	ErrInvalidPayload                = errors.New("invalid registration payload")
	ErrMissingKeys                   = errors.New("missing public keys")
	ErrInvalidFingerprint            = errors.New("invalid fingerprint format")
	ErrServerError                   = errors.New("server error")
	ErrTemporaryRetry                = errors.New("temporary network issue")
	ErrTimeoutReached                = errors.New("timeout reached")
	ErrFingerprintMismatch           = errors.New("fingerprint missmatch")
	ErrRelayAtFullCapacityRetryLater = errors.New("relay at full capacity, retry later")
	ErrNotFound                      = errors.New("not found")
	ErrInvalidResponse               = errors.New("invalid response")
	ErrInvalidIP                     = errors.New("invalid IP")
	ErrSessionNotEstablished         = errors.New("session not established")
	ErrFilesystemAlreadyMounted      = errors.New("filesystem already mounted")
	ErrNilFilesystem                 = errors.New("filesystem not mounted")
	ErrAlreadyRunning                = errors.New("already running")
	ErrInvalidSession                = errors.New("invalid sesssion")
	ErrServerAtCapacity              = errors.New("relay server at capacity, please try again in 5 minutes")
)

func RegisterErrorMapper(statusCode int, err error) error {
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	switch statusCode {
	case http.StatusBadRequest:
		return ErrInvalidPayload
	case http.StatusConflict:
		return ErrTemporaryRetry
	case http.StatusInternalServerError:
		return ErrServerError
	case http.StatusTooManyRequests:
		return ErrRateLimitHit
	case http.StatusServiceUnavailable:
		return ErrServerAtCapacity
	default:
		return fmt.Errorf("unexpected HTTP status %d", statusCode)
	}
}
