// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
	ErrIdenticalFingerprints         = errors.New("own and peer fingerprints are identical")
)

func RegisterErrorMapper(statusCode int, err error) error {
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	switch statusCode {
	case http.StatusNotFound:
		return ErrNotFound
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
