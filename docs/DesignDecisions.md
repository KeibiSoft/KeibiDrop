# Design Decisions

This document tracks key architectural decisions made during the development of KeibiDrop.  
Each decision is assigned a unique ID for reference in commits, issues, and code comments.

---

## `DD-001`: Secure Session State Enforcement

- **Date:** 2025-05-09
- **Author:** [Andrei Cristian](https://github.com/ac999)
- **Status:** Accepted
- **Location:** `pkg/session/guard.go`
- **Decision:**

  - Use a hybrid model for session lifecycle control:
    - All state changes go through `Transition(to string)`
    - Sensitive actions (e.g., SEK derivation, file transfer) must call `Validate*()` guards

- **Rationale:**

  - Ensures valid transitions and defends against invalid usage across the codebase.
  - Combines auditability with safety and avoids scattered state handling logic.

- **Impacted Modules:**

  - `pkg/session`
  - `cmd/keibidrop.go` (when wiring session logic)

- **See Also:**

  - [Secure Session State Machine](./diagrams/Secure%20Session%20State%20Machine.png)
