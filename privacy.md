# Privacy Policy – KeibiDrop Client

Last updated: 10.05.2025

KeibiDrop is an open-source peer-to-peer file sharing client developed by **KeibiSoft SRL**. This document outlines the privacy expectations for users of the KeibiDrop client and its default relay infrastructure.

---

## 1. No Telemetry, No Analytics (Client)

The KeibiDrop client does **not collect**, **transmit**, or **store** any usage data, telemetry, or analytics. It runs locally on your device and communicates directly with peers or a relay server if configured.

We do not receive or track what you do in the client. You're on your own out there.

---

## 2. End-to-End Encryption

All file transfers are **end-to-end encrypted** between the sender and recipient using modern cryptography (including post-quantum algorithms, where applicable). Only the recipient can decrypt the content.

We cannot and do not decrypt or inspect transferred data.

---

## 3. When Using the Default KeibiSoft Relay Server

By default, KeibiDrop uses a public relay server operated by **KeibiSoft SRL** to facilitate metadata exchange (not file transfer).

If you use this default relay:
- **Your IP address will be logged** for abuse prevention and system monitoring.
- **Basic metrics** are collected, such as:
  - Total requests received
  - Number of unique IPs in the last 24h, 30d, and 365d
  - Top IPs by request count
  - Country of origin (based on IP geolocation)

This data is not linked to personal identity and is **not shared or sold**. It exists so we can keep the service from turning into a spam tunnel.

If you don’t like this, you’re free to run your own relay or use a trusted one of your choosing.

---

## 4. No Centralized Logging (Client)

The client itself does not connect to or report back to KeibiSoft. We don’t want your data, and we don’t want to be responsible for it.

---

## 5. Third-Party Libraries

KeibiDrop may include open source dependencies. Each may have its own license or policy.

---

## 6. Community Contributions

If you submit code to the KeibiDrop project, your contributions will be public and attributed to your GitHub account (or whichever identity you use). See `CONTRIBUTING.md` for more info.

---

## 7. Changes

This privacy policy may change. If it does, we’ll update this file. We will not send an email, a push notification, or a man with a clipboard.

---


© [2025] KeibiSoft SRL. We protect your privacy by mostly not caring about your data.
