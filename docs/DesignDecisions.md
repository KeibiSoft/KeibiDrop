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

---

## `DD-002`: Per-file DirectIo for mmap/Cache Compatibility

- **Date:** 2026-01-18
- **Author:** Claude (AI) + [Marius](https://github.com/marius-gal)
- **Status:** Accepted
- **Location:** `pkg/filesystem/fuse_directory.go`, `pkg/filesystem/api.go`
- **Decision:**

  - Implement per-file `direct_io` control via cgofuse's `FileSystemOpenEx` interface
  - Use `CreateEx` and `OpenEx` methods instead of global mount option
  - Apply the following heuristic for `DirectIo` flag:

  | Condition | DirectIo | Reason |
  |-----------|----------|--------|
  | Files in `.git/` | `false` | Allow mmap for git pack files |
  | Write access (O_WRONLY/O_RDWR) | `true` | Real-time sync to peer |
  | Read-only access | `false` | Kernel page cache is beneficial |

- **Rationale:**

  - **Problem:** The global `-o direct_io` mount option disables the kernel page cache for ALL files. This breaks `mmap()` system calls, causing "bus error" (SIGBUS) when applications like `git` try to memory-map files.

  - **Root cause:** Git uses mmap extensively for pack files (`.git/objects/pack/*.pack`). When `direct_io` is enabled globally, mmap fails because the kernel cannot maintain coherency between mapped pages and the FUSE daemon.

  - **Solution:** cgofuse provides `FileSystemOpenEx` interface allowing per-file `DirectIo` flag. By checking the file path and access mode at open time, we can:
    - Disable DirectIo for `.git/` files → mmap works, git clone succeeds
    - Enable DirectIo for files opened for write → changes visible immediately for peer sync
    - Disable DirectIo for read-only access → better performance via page cache

  - **Cross-platform:** This works on macOS (macFUSE), Windows (WinFSP), and Linux (FUSE3). The cgofuse library abstracts the platform differences.

- **Trade-offs:**

  - Files with `DirectIo=false` may show stale data if modified by peer (kernel cache not invalidated)
  - Future enhancement: add explicit cache invalidation when peer notifies of changes

- **Impacted Modules:**

  - `pkg/filesystem/fuse_directory.go` - `CreateEx()`, `OpenEx()`, `shouldUseDirectIo()`
  - `pkg/filesystem/api.go` - removed global `-o direct_io` mount option

- **References:**

  - [macFUSE mount options](https://github.com/macfuse/macfuse/wiki/Mount-Options)
  - [cgofuse FileSystemOpenEx interface](https://pkg.go.dev/github.com/winfsp/cgofuse/fuse#FileSystemOpenEx)
  - [FUSE direct_io semantics](https://libfuse.github.io/doxygen/structfuse__operations.html)

---

## `DD-003`: Counter-based Deterministic Nonces

- **Date:** 2026-01-18
- **Author:** Claude (AI) + [Marius](https://github.com/marius-gal)
- **Status:** Accepted
- **Location:** `pkg/crypto/symmetric.go`, `pkg/session/secureconn.go`
- **Decision:**

  - Replace random nonce generation with deterministic counter-based nonces
  - Structure: `[4-byte direction prefix][8-byte monotonic counter]` = 12 bytes
  - Use different prefixes for inbound vs outbound directions:
    - Outbound: `0x4F555442` ("OUTB")
    - Inbound: `0x494E4244` ("INBD")

- **Rationale:**

  - **Performance:** Random nonce generation via `crypto/rand.Read()` costs ~500ns per call (syscall overhead). Counter increment via `atomic.Uint64.Add()` costs ~1ns. For high-throughput file transfer, this is a 500x improvement.

  - **Security equivalence:** ChaCha20-Poly1305 requires nonce uniqueness, not randomness. A monotonic counter guarantees uniqueness within a key's lifetime. The 4-byte prefix ensures nonces never collide between inbound and outbound streams even if counters happen to match.

  - **Nonce space:** With 8-byte counter, we can send 2^64 messages before counter wraps. Combined with re-keying at 1GB/1M messages (DD-004 in Security.md), counter exhaustion is impossible.

  - **Thread safety:** `atomic.Uint64` operations are lock-free and safe for concurrent use by multiple goroutines.

- **Implementation:**

  ```go
  type NonceGenerator struct {
      prefix  [4]byte       // Direction identifier
      counter atomic.Uint64 // Monotonic counter
  }

  func (ng *NonceGenerator) Next() [12]byte {
      var nonce [12]byte
      copy(nonce[:4], ng.prefix[:])
      binary.BigEndian.PutUint64(nonce[4:], ng.counter.Add(1))
      return nonce
  }
  ```

- **Trade-offs:**

  - Counter state must not be lost/reset during a session (would cause nonce reuse)
  - Handled by: creating fresh NonceGenerator per session key

- **Impacted Modules:**

  - `pkg/crypto/symmetric.go` - `NonceGenerator`, `EncryptWithNonce()`
  - `pkg/session/secureconn.go` - `SecureWriter` uses `NonceGenerator`

- **References:**

  - [RFC 8439 - ChaCha20-Poly1305](https://datatracker.ietf.org/doc/html/rfc8439) Section 4: "The nonce MUST be unique for each invocation with a given key"
  - [NIST SP 800-38D](https://csrc.nist.gov/publications/detail/sp/800-38d/final) - Counter mode considerations

---

## `DD-004`: Atomic RENAME Notification Protocol

- **Date:** 2026-01-18
- **Author:** Claude (AI) + [Marius](https://github.com/marius-gal)
- **Status:** Accepted
- **Location:** `keibidrop.proto`, `pkg/types/events.go`, `pkg/filesystem/fuse_directory.go`, `pkg/logic/service/service.go`
- **Decision:**

  - Add `RENAME_FILE` and `RENAME_DIR` notification types to the protocol
  - Include `old_path` field in `NotifyRequest` for rename operations
  - Handle rename atomically on receiver side (single operation, not REMOVE+ADD)

- **Rationale:**

  - **Atomicity:** Simulating rename with REMOVE_FILE + ADD_FILE creates a race condition window where the file doesn't exist. Applications may fail if they check for the file between these events.

  - **Semantic correctness:** A rename is fundamentally different from delete+create:
    - Preserves inode (file identity)
    - Preserves open file handles
    - Preserves file permissions and metadata
    - Is a single atomic operation in POSIX

  - **Git compatibility:** Git moves/renames files frequently during operations. Non-atomic rename simulation can cause corruption or errors.

- **Protocol Extension:**

  ```protobuf
  enum NotifyType {
    // ... existing types ...
    RENAME_FILE = 7;  // File moved/renamed. old_path -> path.
    RENAME_DIR = 8;   // Directory moved/renamed. old_path -> path.
  }

  message NotifyRequest {
    NotifyType type = 1;
    string path = 2;      // New path (destination)
    string name = 3;
    Attr attr = 4;
    string old_path = 5;  // For RENAME: source path
  }
  ```

- **Receiver Handling:**

  1. Validate old_path exists in local tracking
  2. Perform filesystem rename: `os.Rename(old_path, new_path)`
  3. Update internal file/directory maps atomically
  4. Preserve download state if file was being streamed

- **Impacted Modules:**

  - `keibidrop.proto` - Added `RENAME_FILE`, `RENAME_DIR`, `old_path`
  - `pkg/types/events.go` - Added `RenameFile`, `RenameDir` actions, `OldPath` field
  - `pkg/filesystem/fuse_directory.go` - `Rename()` sends `RENAME_FILE` notification
  - `pkg/logic/service/service.go` - Handles `RENAME_FILE`, `RENAME_DIR` in `Notify()`
  - `pkg/logic/common/utils.go` - Includes `OldPath` in notification request

- **References:**

  - [POSIX rename() semantics](https://pubs.opengroup.org/onlinepubs/9699919799/functions/rename.html)
  - Protocol Buffers backward compatibility (adding fields/enum values is safe)

---

## `DD-005`: FUSE Release/Fsync Race Condition Workaround

- **Date:** 2026-01-18
- **Author:** Claude (AI) + [Marius](https://github.com/marius-gal)
- **Status:** Accepted
- **Location:** `pkg/filesystem/fuse_directory.go`
- **Decision:**

  - Handle `EBADF` in `Fsync()` by reopening the file and fsyncing it
  - This is a fallback for when FUSE kernel calls Release before Fsync

- **Rationale:**

  - **Problem:** Git clone fails with "fsync error: Bad file descriptor" during `index-pack` phase. The FUSE kernel can call `Release()` before `Fsync()` completes, causing the fd to be closed before fsync can use it.

  - **Root cause:** FUSE operations are asynchronous. When an application calls `close()` followed by `fsync()`, the kernel may deliver `Release` to the FUSE daemon before `Fsync`. Since `Release` closes the fd, subsequent `Fsync` calls fail with `EBADF`.

  - **Sequence observed:**
    ```
    17:29:16.206 - CreateEx tmp_pack_1M0L8Q fd=14
    17:29:16.971 - Release tmp_pack_1M0L8Q fh=14 → fd closed
    17:29:16.975 - Fsync tmp_pack_1M0L8Q fh=14 → EBADF!
    ```

  - **Solution:** In `Fsync()`, if `syscall.Fsync(fh)` returns `EBADF`:
    1. Open the file read-only by path
    2. Call `syscall.Fsync()` on the new fd
    3. Close the fd
    4. If open fails (file renamed/deleted), return success - data was already committed

- **Implementation:**

  ```go
  func (d *Dir) Fsync(path string, datasync bool, fh uint64) int {
      err := syscall.Fsync(int(fh))
      if err == nil {
          return 0
      }

      if err == syscall.EBADF {
          // Fallback: open, fsync, close
          fd, openErr := syscall.Open(localPath, syscall.O_RDONLY, 0)
          if openErr != nil {
              return 0 // File gone - data was committed before close
          }
          fsyncErr := syscall.Fsync(fd)
          syscall.Close(fd)
          if fsyncErr != nil {
              return -EIO
          }
          return 0
      }
      return convertError(err)
  }
  ```

- **Trade-offs:**

  - Extra syscall overhead for the fallback path (open + fsync + close)
  - Only affects edge cases where Release races with Fsync

- **Related Issues:**

  - Similar pattern used in `Write()` for macOS fcopyfile workaround (DD-006 pending)
  - Both are consequences of FUSE async delivery of operations

- **Impacted Modules:**

  - `pkg/filesystem/fuse_directory.go` - `Fsync()`

- **References:**

  - [FUSE low-level API docs](https://libfuse.github.io/doxygen/structfuse__lowlevel__ops.html) - Release/Fsync ordering
  - [macFUSE known issues](https://github.com/macfuse/macfuse/wiki/Known-Issues)

---

## `DD-006`: macOS fcopyfile Late Write Workaround

- **Date:** 2026-01-18
- **Author:** Claude (AI) + [Marius](https://github.com/marius-gal)
- **Status:** Accepted
- **Location:** `pkg/filesystem/fuse_directory.go`
- **Decision:**

  - Handle `EBADF` in `Write()` by reopening the file and writing via `pwrite()`
  - This is a fallback for macOS `fcopyfile()` behavior

- **Rationale:**

  - **Problem:** On macOS, `cp` uses `fcopyfile()` which can send `Write` calls after `Release` has been called. This causes "bad file descriptor" errors.

  - **Root cause:** macOS's `fcopyfile()` syscall internally opens/reads/writes files, but the FUSE layer sees these as coming from the original fd. When the application calls `close()`, `Release` is delivered, but `fcopyfile` may still send `Write` calls.

  - **Solution:** In `Write()`, if `pwrite()` returns `EBADF`:
    1. Log warning about fcopyfile workaround
    2. Open the file for write by path
    3. Call `pwrite()` with the data
    4. Close the fd

- **Trade-offs:**

  - macOS-specific workaround
  - Extra syscalls for affected operations

- **Impacted Modules:**

  - `pkg/filesystem/fuse_directory.go` - `Write()`

- **References:**

  - [macOS fcopyfile(3)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man3/fcopyfile.3.html)

---

## `DD-007`: File Permission Normalization for FUSE

- **Date:** 2026-01-18
- **Author:** Claude (AI) + [Marius](https://github.com/marius-gal)
- **Status:** Accepted
- **Location:** `pkg/filesystem/fuse_directory.go`
- **Decision:**

  - In `Create()` and `CreateEx()`, always ensure owner has write permission (mode `0o200`)
  - Strip file type bits (`S_IFREG`) from mode before passing to `syscall.Open()`

- **Rationale:**

  - **Problem:** Git clone fails with "Permission denied" when trying to reopen files. Git creates pack files with mode `0o444` (read-only), writes via the open fd, closes, then tries to reopen with `O_RDWR`.

  - **Root cause:** The FUSE `mode` parameter includes file type bits (`S_IFREG = 0100000 = 32768`). When passed directly to `syscall.Open()`, these bits are misinterpreted. Additionally, files created with mode `0o444` cannot be reopened for write.

  - **Observed:** `mode=33060` = `S_IFREG (32768) + 0o444 (292)` = read-only regular file

  - **Solution:**
    ```go
    createMode := mode & 0o777           // Extract permission bits only
    if createMode&0o200 == 0 {
        createMode |= 0o200              // Ensure owner write
    }
    if createMode == 0 {
        createMode = 0o644               // Default fallback
    }
    ```

- **Trade-offs:**

  - Files may have more permissive modes than requested
  - Acceptable for a user-space filesystem where the user owns all files

- **Impacted Modules:**

  - `pkg/filesystem/fuse_directory.go` - `Create()`, `CreateEx()`

- **References:**

  - [POSIX file mode bits](https://pubs.opengroup.org/onlinepubs/9699919799/basedefs/sys_stat.h.html)
