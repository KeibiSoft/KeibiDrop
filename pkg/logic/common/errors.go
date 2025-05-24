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
	ErrRateLimitHit                  = errors.New("relay rate limit hit, retry in 15 minutes")
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
)

func RegisterErrorMapper(statusCode int, err error) error {
	if err != nil {
		// Networking or JSON failure
		return fmt.Errorf("request failed: %w", err)
	}

	switch statusCode {
	case http.StatusBadRequest:
		// Optional: parse error body to be more specific
		return ErrInvalidPayload
	case http.StatusConflict:
		return ErrTemporaryRetry // For example
	case http.StatusInternalServerError:
		return ErrServerError
	default:
		return fmt.Errorf("unexpected HTTP status %d", statusCode)
	}
}
