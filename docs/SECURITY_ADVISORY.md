# KeibiDrop Security Advisory: Cryptographic Protocol Weaknesses

**Date:** March 3, 2026  
**Status:** CONFIRMED (Empirically Verified)  
**Affected Versions:** Current Development Branch  
**Severity:** CRITICAL

## Executive Summary
A security audit of the `pkg/crypto` and `pkg/session` packages has identified three critical cryptographic flaws that compromise the confidentiality and integrity of data transferred via KeibiDrop. These vulnerabilities include a catastrophic failure in random number generation, a lack of stream-level integrity in file transfers, and a malleable key encapsulation mechanism.

---

## 1. [CRITICAL] Predictable Seed Generation on RNG Failure
**Vulnerability ID:** KD-SEC-2026-001  
**Category:** Cryptographic Failures (OWASP A02:2021)

### Description
The `GenerateSeed()` function in `pkg/crypto/utils.go` ignores the error returned by `RandomBytes`. If the system's entropy pool is exhausted or file descriptors for `/dev/urandom` cannot be opened, the function silently returns a 32-byte slice of zeros.

### Affected Code
```go
// pkg/crypto/utils.go
func GenerateSeed() []byte {
	res, _ := RandomBytes(seedSize) // Error ignored!
	return res
}
```

### Exploit Scenario
An attacker on a shared host or a malicious background process exhausts the system's file descriptors. When KeibiDrop starts a new session, `rand.Read` fails. The session proceeds using `0000...0000` as the secret seed. The attacker intercepts the network traffic and, knowing the "secret" is all zeros, decrypts the entire session in real-time.

---

## 2. [HIGH] Lack of Stream Integrity (Chunk Reordering/Truncation)
**Vulnerability ID:** KD-SEC-2026-002  
**Category:** Software and Data Integrity Failures (OWASP A08:2021)

### Description
The `EncryptChunked` and `DecryptChunked` functions in `pkg/crypto/symmetric.go` treat each file block as an independent encryption operation. Each block uses a random nonce but does not include any sequence information or "binding" (AAD) to ensure blocks are decrypted in the same order they were encrypted.

### Affected Code
```go
// pkg/crypto/symmetric.go
encryptedChunk, err := Encrypt(kek, buf[:n]) // No sequence binding
```

### Exploit Scenario
A Man-in-the-Middle (MITM) attacker intercepts an encrypted file transfer. They can swap the first and second 128KB blocks of the file. `DecryptChunked` will successfully authenticate both blocks (because they are valid ChaCha20-Poly1305 ciphertexts) and write the corrupted file to disk. This could be used to corrupt executables or modify the meaning of configuration files.

---

## 3. [MEDIUM] Malleable X25519 Key Encapsulation
**Vulnerability ID:** KD-SEC-2026-003  
**Category:** Insecure Design (OWASP A04:2021)

### Description
`X25519Encapsulate` uses a raw XOR stream cipher to "encrypt" the seed against an HKDF mask. It lacks any Message Authentication Code (MAC). Consequently, the ciphertext is malleable: flipping a bit in the ciphertext results in a predictable bit-flip in the recovered seed.

### Affected Code
```go
// pkg/crypto/asymmetric.go
for i := 0; i < seedSize; i++ {
	ct[i] = seed[i] ^ mask[i] // No authentication
}
```

### Exploit Scenario
An attacker intercepts the handshake. They flip bits in the encrypted seed. While the subsequent file decryption will fail (because the derived key will be wrong), the receiver's `X25519Decapsulate` function returns **Success**, potentially allowing the attacker to probe the system or induce specific error states in the protocol logic.

---

## Reproduction
Working exploit demonstrations are available in `pkg/crypto/repro_test.go`. Run them with:
`go test -v -run TestRepro ./pkg/crypto/`
