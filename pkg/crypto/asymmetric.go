package crypto

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"hash"
	"io"

	"golang.org/x/crypto/hkdf"
)

func GenerateMLKEMKeypair() (*mlkem.DecapsulationKey1024, *mlkem.EncapsulationKey1024, error) {
	priv, err := mlkem.GenerateKey1024()
	if err != nil {
		return nil, nil, err
	}
	pub := priv.EncapsulationKey()
	return priv, pub, nil
}

func GenerateX25519Keypair() (*ecdh.PrivateKey, *ecdh.PublicKey, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(nil) // nil = crypto/rand.Reader
	if err != nil {
		return nil, nil, err
	}
	pub := priv.PublicKey()
	return priv, pub, nil
}

func ValidateSeed(s []byte) error {
	if len(s) == 0 {
		return errors.New("shared seed must not be empty")
	}

	if len(s) < 64 {
		return errors.New("shared seed too small ")
	}

	return nil
}

func deriveKeyInternal(hash func() hash.Hash, label string, size int, secrets ...[]byte) ([]byte, error) {
	total := 0
	for _, s := range secrets {
		if err := ValidateSeed(s); err != nil {
			return nil, err
		}

		total += len(s)
	}

	seed := make([]byte, total)
	offset := 0
	for _, s := range secrets {
		copy(seed[offset:], s)
		offset += len(s)
	}

	hkdfStream := hkdf.New(hash, seed, nil, []byte(label))
	key := make([]byte, size)
	if _, err := io.ReadFull(hkdfStream, key); err != nil {
		return nil, err
	}
	return key, nil
}

// DeriveChaCha20Key derives a 32-byte symmetric key using SHA-512 over the given secrets.
func DeriveChaCha20Key(sharedSecrets ...[]byte) ([]byte, error) {
	return deriveKeyInternal(sha512.New, "KeibiDrop-ChaCha20-Poly1305-KEK", KeySize, sharedSecrets...)
}

func Fingerprint(pub []byte) string {
	sum := sha512.Sum512(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ProtocolFingerprintV0 computes a stable fingerprint hash of ordered public keys.
func ProtocolFingerprintV0(pubkeys map[string][]byte) (string, error) {
	// Deterministic key order
	orderedKeys := []string{"x25519", "mlkem"}

	totalLen := 0
	for _, key := range orderedKeys {
		val, ok := pubkeys[key]
		if !ok || len(val) == 0 {
			return "", errors.New("missing or empty public key: " + key)
		}
		totalLen += len(val)
	}

	concat := make([]byte, totalLen)
	offset := 0
	for _, key := range orderedKeys {
		val := pubkeys[key]
		copy(concat[offset:], val)
		offset += len(val)
	}

	sum := sha512.Sum512(concat)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
