# KeibiDrop Security Audit Report - February 26, 2026

## Executive Summary
A security review of the KeibiDrop codebase has identified several critical vulnerabilities affecting the confidentiality, integrity, and availability of the system. These findings match several categories in the OWASP Top 10, including Cryptographic Failures and Broken Access Control.

---

## Findings

### 1. Path Traversal (Arbitrary File Write / Broken Access Control)
**ID:** KD-2026-001  
**Category:** Injection (OWASP A03:2021) / Broken Access Control (OWASP A01:2021)  
**Severity:** Critical

#### Description
The gRPC `Notify` service processes paths received from remote peers without sufficient sanitization. The implementation uses `filepath.Join` and `filepath.Clean`, which do not prevent directory traversal if the resulting path is not validated against a base directory.

#### Affected Code
File: `pkg/logic/service/service.go`
```go
case bindings.NotifyType_ADD_DIR:
    err := kd.FS.Root.MkdirFromPeer(req.Path, 0755)
```

File: `pkg/filesystem/fuse_directory.go`
```go
func (d *Dir) mkdirInternal(path string, mode uint32, notifyPeer bool) (errCode int) {
    cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
    err := syscall.Mkdir(cleanPath, mode)
```

#### Impact
A malicious peer can escape the designated download folder and create or overwrite arbitrary files on the local system (e.g., `~/.ssh/authorized_keys`, `~/.bashrc`), leading to full system compromise.

#### Recommendation
Validate that the resolved path is a child of the base download directory.
```go
func isSecurePath(base, path string) bool {
    rel, err := filepath.Rel(base, path)
    if err != nil {
        return false
    }
    return !strings.HasPrefix(rel, "..") && rel != ".."
}
```

---

### 2. Nonce Reuse in ChaCha20-Poly1305 (Cryptographic Failure)
**ID:** KD-2026-002  
**Category:** Cryptographic Failures (OWASP A02:2021)  
**Severity:** Critical

#### Description
Both peers in a secure connection initialize their `SecureWriter` with the same hardcoded nonce prefix (`NoncePrefixOutbound`). This results in both directions of the full-duplex connection using identical nonces. While separate keys are currently used for inbound and outbound traffic, this architecture is fragile and violates the principle of using unique nonces per key-usage domain, creating a high risk of catastrophic failure if the key derivation is ever unified or refactored.

#### Affected Code
File: `pkg/session/secureconn.go`
```go
func NewSecureWriter(w io.Writer, kek []byte) *SecureWriter {
    return &SecureWriter{
        w:     w,
        kek:   kek,
        nonce: kbc.NewNonceGenerator(NoncePrefixOutbound), // Hardcoded 0x4F555442
    }
}
```

#### Impact
If the keys for inbound and outbound traffic were ever the same (e.g., during a botched rekeying or session resumption), nonce reuse would allow an attacker to recover the keystream and forge messages.

#### Recommendation
Differentiate the nonce prefixes based on the role (Initiator/Responder) or directionality (Inbound/Outbound) to ensure nonces never overlap even if keys were identical.
