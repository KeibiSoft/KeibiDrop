package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func randomBytes(t *testing.T, size int) []byte {
	b := make([]byte, size)
	_, err := rand.Read(b)
	require.NoError(t, err, "failed to generate random bytes")
	return b
}

func TestSymmetricEncryption(t *testing.T) {
	req := require.New(t)
	kek := randomBytes(t, seedSize)
	plaintext := []byte("this is a secret message")

	ciphertext, err := Encrypt(kek, plaintext)
	req.NoError(err, "Encrypt failed")

	decrypted, err := Decrypt(kek, ciphertext)
	req.NoError(err, "Decrypt failed")

	req.Equal(plaintext, decrypted, "Decrypted plaintext does not match original")
}

func TestAsymmetricKeyExchange(t *testing.T) {
	req := require.New(t)

	// ML-KEM
	privAlice, pubAlice, err := GenerateMLKEMKeypair()
	req.NoError(err)

	seed1, ct1 := pubAlice.Encapsulate()
	ssAlice, err := privAlice.Decapsulate(ct1)
	req.NoError(err)
	req.Equal(seed1, ssAlice, "Kyber shared secrets do not match")
}

func TestHybridKeyDerivation(t *testing.T) {
	req := require.New(t)

	sharedX := randomBytes(t, seedSize)
	sharedKEM := randomBytes(t, seedSize)

	kek1, err := DeriveChaCha20Key(sharedX, sharedKEM)
	req.NoError(err, "DeriveChaCha20Key failed")

	kek2, err := DeriveChaCha20Key(sharedX, sharedKEM)
	req.NoError(err, "DeriveChaCha20Key failed")

	req.Equal(kek1, kek2, "KEK derivation is not deterministic")
}

func TestProtocolEndToEndStream(t *testing.T) {
	req := require.New(t)

	// Generate real key pairs for Alice and Bob
	alicePrivMLKEM, alicePubMLKEM, _ := GenerateMLKEMKeypair()
	alicePrivCurve, alicePubCurve, _ := GenerateX25519Keypair()

	bobPrivCurve, bobPubCurve, _ := GenerateX25519Keypair()

	// Bob encapsulates
	seedKEM, ctKEM := alicePubMLKEM.Encapsulate()
	seedCurve := randomBytes(t, seedSize)
	ctCurve, err := X25519Decapsulate(seedCurve, bobPrivCurve, alicePubCurve)
	req.NoError(err)

	// Alice decapsulates
	recoveredKEM, err := alicePrivMLKEM.Decapsulate(ctKEM)
	req.NoError(err)
	recoveredCurve, err := X25519Decapsulate(ctCurve, alicePrivCurve, bobPubCurve)
	req.NoError(err)

	// Derive KEKs
	kekBob, err := DeriveChaCha20Key(seedCurve, seedKEM)
	req.NoError(err)
	kekAlice, err := DeriveChaCha20Key(recoveredCurve, recoveredKEM)
	req.NoError(err)
	req.Equal(kekAlice, kekBob, "KEK mismatch")

	// Prepare 553 KiB of data
	src := make([]byte, 553*1024)
	_, err = rand.Read(src)
	req.NoError(err)

	// Encrypt with Bob's KEK
	ciphertext, err := Encrypt(kekBob, src)
	req.NoError(err)

	// Decrypt with Alice's KEK
	plaintext, err := Decrypt(kekAlice, ciphertext)
	req.NoError(err)
	req.Equal(src, plaintext, "Decrypted stream does not match original")
}

func TestProtocolMimic(t *testing.T) {
	req := require.New(t)

	// Alice (responder)
	_, pubAliceMLKEM, _ := GenerateMLKEMKeypair()
	privAliceX, pubAliceX, _ := GenerateX25519Keypair()

	fpAlice, err := ProtocolFingerprintV0(map[string][]byte{
		"x25519": pubAliceX.Bytes(),
		"mlkem":  pubAliceMLKEM.Bytes(),
	})
	req.NoError(err, "Alice fingerprint generation failed")

	// Bob (initiator)
	seed := randomBytes(t, seedSize)
	ct, err := X25519Encapsulate(seed, privAliceX, pubAliceX)
	req.NoError(err)

	recovered, err := X25519Decapsulate(ct, privAliceX, pubAliceX)
	req.NoError(err)
	req.Equal(seed, recovered, "Decrypted seed mismatch")

	// Fingerprint check
	fpAliceCheck, err := ProtocolFingerprintV0(map[string][]byte{
		"x25519": pubAliceX.Bytes(),
		"mlkem":  pubAliceMLKEM.Bytes(),
	})
	req.NoError(err)
	req.Equal(fpAlice, fpAliceCheck, "Fingerprint mismatch")

	t.Logf("Fingerprint: %s", base64.RawURLEncoding.EncodeToString([]byte(fpAlice)))
}
