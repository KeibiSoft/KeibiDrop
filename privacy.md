# Privacy Policy - **KEIBI**DROP Client

Last updated: 04.04.2026

**KEIBI**DROP is an open-source peer-to-peer file sharing client developed by **KeibiSoft SRL**. This document outlines the privacy expectations for users of the **KEIBI**DROP client and its default relay infrastructure.

---

## 1. No Telemetry, No Analytics (Client)

The **KEIBI**DROP client does **not collect**, **transmit**, or **store** any usage data, telemetry, or analytics. It runs locally on your device and communicates directly with peers or a relay server if configured.

We do not receive or track what you do in the client. You're on your own out there.

---

## 2. End-to-End Encryption (at transport level)

All file transfers are **end-to-end encrypted** between the sender and recipient using modern cryptography (including post-quantum algorithms, where applicable). Only the recipient can decrypt the content.

We cannot and do not decrypt or inspect transferred data.

---

## 3. When Using the Default KeibiSoft Relay Server

By default, **KEIBI**DROP uses a public relay server operated by **KeibiSoft SRL** to facilitate key exchange (not file transfer).

**The relay cannot read your metadata.** Registration data (fingerprints, public keys, connection hints) is encrypted client-side before being sent to the relay. The relay stores only opaque encrypted blobs indexed by a derived lookup key. It cannot:
- See your fingerprint or public keys
- Read your IP address or port from the registration
- Correlate registrations to identities

If you use this default relay:
- **Your IP address will be logged** at the HTTP layer for abuse prevention and system monitoring.
- **Basic metrics** are collected, such as:
  - Total requests received
  - Number of unique IPs in the last 24h, 30d, and 365d
  - Top IPs by request count
  - Country of origin (based on IP geolocation)

This data is not linked to personal identity and is **not shared or sold**. It exists so we can score `brownie points` and to protect against spam.

If you don't like this, you're free to run your own relay or use a trusted one of your choosing.

---

## 4. Local Identity Storage

By default, KeibiDrop saves your identity and contacts encrypted on disk (`~/.config/keibidrop/`). The encryption key is stored in your OS keychain when available, or in a local file on headless systems. Incognito mode writes nothing to disk. See `Security.md` for details.

---

## 5. Local Logging (Client)

The client writes debug logs to a file on your machine. These logs stay on your device and are never sent to KeibiSoft automatically.

If you run into a crash or bug, we may ask you to send the log file to help us investigate. On mobile, the app has a "Send Logs" button that sanitizes the file before export (see 4b below). On desktop, the log file is at `~/Library/Logs/KeibiDrop/keibidrop.log` (macOS) or `~/.local/share/keibidrop/keibidrop.log` (Linux).

---

## 5b. Diagnostic Logs (Mobile)

The mobile app writes debug logs to a local file on your device. These logs are never sent automatically.

If you choose to send logs (via the "Send Logs" button in the Files tab), they are sanitized before export:
- File names are replaced with `<redacted>`, keeping only the file extension (e.g. `.pdf`, `.mp4`)
- Fingerprint codes and IP addresses are removed
- Standard directory names (Documents, KeibiDrop, Received) are kept for context
- No file contents are ever logged

You control when and how the sanitized log is shared. We receive it only if you send it to us.

Log files are rotated automatically and kept under 5 MB.

---

## 6. Third-Party Libraries

**KEIBI**DROP may include open source dependencies. Each may have its own license or policy.

---

## 7. Community Contributions

If you submit code to the **KEIBI**DROP project, your contributions will be public and attributed to your GitHub account (or whichever identity you use). See `CONTRIBUTING.md` for more info.

---

## 8. Changes

This privacy policy may change. If it does, we’ll update this file. We will not send an email, a push notification, or a man with a clipboard.

---


© [2025] KeibiSoft SRL. We protect your privacy by mostly not caring about your data.
