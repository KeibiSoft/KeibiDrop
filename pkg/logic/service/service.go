//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/winfsp/cgofuse/fuse"
	"github.com/zeebo/xxh3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrGRPCNotMounted         = status.Error(codes.FailedPrecondition, "filesystem not mounted")
	ErrGRPCInvalidArgument    = status.Error(codes.InvalidArgument, "invalid argument")
	ErrGRPCFailedPrecondition = status.Error(codes.FailedPrecondition, "failed precondition")
	ErrGRPCAlreadyExists      = status.Error(codes.AlreadyExists, "already exists")
	ErrGRPCNotFound           = status.Error(codes.NotFound, "notFound")
	ErrGRPCReadOnlyShare      = status.Error(codes.PermissionDenied, "share is read-only")
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session      *session.Session
	Logger       *slog.Logger
	SyncTracker  *synctracker.SyncTracker
	OnEvent      func(string)
	OnDisconnect func() // Called (in goroutine) when peer sends DISCONNECT; cancels context to trigger cleanup.

	// ShareReadOnly rejects every inbound mutation (add/edit/remove/rename,
	// file or dir) before it touches disk or the tracker. The origin-side
	// guarantee behind the read_only share mode; DISCONNECT still passes.
	ShareReadOnly bool

	// readOnlyRefusalLogged keeps the refusal log to one line per session.
	readOnlyRefusalLogged atomic.Bool

	// fs is published while the server is already serving: Run() backfills it
	// after the first mount, and mount/unmount swap it mid-session. Handlers
	// load it atomically through FS() instead of reading a bare field.
	fs atomic.Pointer[filesystem.FS]

	// pendingRemoves buffers REMOVE_FILE events for 1000ms. If an ADD_FILE or
	// RENAME_FILE arrives for the same path within that window, the REMOVE is
	// cancelled (it was part of git's atomic .lock→rename dance, not a real deletion).
	pendingRemovesMu sync.Mutex
	pendingRemoves   map[string]*time.Timer
}

// FS returns the current FUSE tree, or nil before the first publish or after
// unmount. Handlers nil-check per call.
func (kd *KeibidropServiceImpl) FS() *filesystem.FS { return kd.fs.Load() }

// SetFS publishes the FUSE tree to running handlers.
func (kd *KeibidropServiceImpl) SetFS(f *filesystem.FS) { kd.fs.Store(f) }

// statFromAttr builds a fuse.Stat_t from a peer-sent Attr. Remote files are
// presented as owned by the current user; Nlink is always 1.
func statFromAttr(a *bindings.Attr) *fuse.Stat_t {
	return &fuse.Stat_t{
		Dev:      a.Dev,
		Ino:      a.Ino,
		Mode:     a.Mode,
		Nlink:    1,
		Uid:      uint32(os.Getuid()),
		Gid:      uint32(os.Getgid()),
		Size:     a.Size,
		Atim:     fuse.NewTimespec(time.Unix(0, int64(a.AccessTime))),
		Mtim:     fuse.NewTimespec(time.Unix(0, int64(a.ModificationTime))),
		Ctim:     fuse.NewTimespec(time.Unix(0, int64(a.ChangeTime))),
		Birthtim: fuse.NewTimespec(time.Unix(0, int64(a.BirthTime))),
		Flags:    a.Flags,
	}
}

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("Debug called. Success!")

	return &bindings.DebugResponse{}, nil
}

func (kd *KeibidropServiceImpl) Notify(_ context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	logger := kd.Logger.With("method", "notify", "req-type", req.Type, "path", req.Path)

	// Handle DISCONNECT before FS checks — it doesn't need a mounted filesystem.
	if req.Type == bindings.NotifyType_DISCONNECT {
		logger.Info("Peer requested graceful disconnect")
		kd.cancelAllPendingRemoves()
		if kd.OnEvent != nil {
			kd.OnEvent("peer_disconnected:")
		}
		// Cancel context in a goroutine to trigger Run()'s ctx.Done() cleanup.
		// Must not call Stop() directly here — it tears down the gRPC server
		// we're currently handling a request on, which would deadlock.
		if kd.OnDisconnect != nil {
			go kd.OnDisconnect()
		}
		return &bindings.NotifyResponse{Status: "ok"}, nil
	}

	// Read-only share: refuse every peer mutation before it reaches disk or
	// the tracker. Log once per session, not once per refused notify.
	if kd.ShareReadOnly {
		if kd.readOnlyRefusalLogged.CompareAndSwap(false, true) {
			logger.Warn("Refusing peer mutation: share is read-only", "first-path", req.Path)
		}
		return nil, ErrGRPCReadOnlyShare
	}

	// Drop macOS/FUSE internal files — ephemeral, never meant to be synced.
	if IsInternalPath(req.Path) {
		// FUSE turns unlink of a still-open file into a rename to
		// .fuse_hiddenN. Drop the hidden name but keep the effect: the
		// source left the folder. Apply it as a buffered remove.
		if req.Type == bindings.NotifyType_RENAME_FILE &&
			strings.Contains(req.Path, ".fuse_hidden") &&
			req.OldPath != "" && !strings.Contains(req.OldPath, ".fuse_hidden") {
			logger.Info("Rename to FUSE hidden name, buffering remove of source", "path", req.OldPath)
			kd.bufferRemove(req.OldPath, logger)
		}
		return &bindings.NotifyResponse{}, nil
	}

	if kd.FS() == nil && kd.SyncTracker == nil {
		logger.Warn("Filesystem not mounted")
		return nil, ErrGRPCNotMounted
	}

	switch req.Type {
	case bindings.NotifyType_UNKNOWN:
		logger.Warn("Unknown notification")
		return nil, ErrGRPCInvalidArgument
	case bindings.NotifyType_ADD_DIR:
		logger.Info("Mkdir called")

		if kd.FS() == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS().Root() == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS().Root().MkdirFromPeer(req.Path, 0755) // Use FromPeer to avoid notification loop.
		if err != 0 {
			return nil, ErrGRPCFailedPrecondition
		}
	case bindings.NotifyType_EDIT_DIR:
		logger.Info("Edit dir is not implemented")
		// TODO: Modify the attr time as per the client payload.
	case bindings.NotifyType_REMOVE_DIR:
		// Peer stopped sharing this directory. Remove from our view but keep any local data.
		logger.Info("Remove dir reference", "path", req.Path)

		if kd.FS() != nil && kd.FS().Root() != nil {
			kd.FS().Root().Adm.Lock()
			delete(kd.FS().Root().AllDirMap, req.Path)
			kd.FS().Root().Adm.Unlock()
			logger.Info("Removed directory reference from view", "path", req.Path)
		}
	case bindings.NotifyType_ADD_FILE:
		// Cancel any pending buffered REMOVE for this path — the file wasn't
		// really deleted, git just did REMOVE→CREATE as part of atomic write.
		kd.cancelPendingRemove(req.Path)

		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCInvalidArgument
		}

		if (kd.FS() == nil || kd.FS().Root() == nil) && kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			defer kd.SyncTracker.RemoteFilesMu.Unlock()
			existing, ok := kd.SyncTracker.RemoteFiles[req.Path]
			if ok {
				// A smaller size is usually a stale temp-file ADD arriving after a
				// rename set the real size; those carry an older mtime, so reject
				// only when older. A real shrink (git HEAD 25->21) is newer: accept.
				// Strict <: equal-mtime shrink is accepted; reject only provably stale ADDs.
				if uint64(req.Attr.Size) < existing.Size &&
					req.Attr.ModificationTime < existing.LastEditTime {
					logger.Info("Ignoring stale ADD_FILE (smaller, older mtime)",
						"path", req.Path, "staleSize", req.Attr.Size, "currentSize", existing.Size)
					return &bindings.NotifyResponse{}, nil
				}
				// Update existing entry (peer overwrote the file).
				existing.Size = uint64(req.Attr.Size)
				existing.LastEditTime = req.Attr.ModificationTime
				existing.Atime = req.Attr.AccessTime
				existing.Mode = req.Attr.Mode
				logger.Info("Updated existing remote file", "path", req.Path, "newSize", req.Attr.Size)
				return &bindings.NotifyResponse{}, nil
			}

			kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
				Name:         req.Name,
				RelativePath: req.Path,
				Size:         uint64(req.Attr.Size),
				LastEditTime: req.Attr.ModificationTime,
				CreatedTime:  req.Attr.BirthTime,
				Atime:        req.Attr.AccessTime,
				Mode:         req.Attr.Mode,
			}

			if kd.OnEvent != nil {
				kd.OnEvent(fmt.Sprintf("file_arrived:%s:%d", req.Name, req.Attr.Size))
			}

			return &bindings.NotifyResponse{}, nil
		}

		if kd.FS() == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS().Root() == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS().Root().AddRemoteFileWithBase(logger, req.Path, req.Name, statFromAttr(req.Attr), req.BaseMtimeNs)
		if err != nil {
			return nil, ErrGRPCInvalidArgument
		}

		// Mirror into SyncTracker so the desktop/mobile file list (which reads
		// SyncTracker.RemoteFiles) shows files added over the FUSE path (#164).
		kd.upsertRemoteInTracker(req)

		if kd.OnEvent != nil {
			name := req.Name
			if name == "" {
				name = filepath.Base(req.Path)
			}
			kd.OnEvent(fmt.Sprintf("file_arrived:%s:%d", name, req.Attr.Size))
		}
	case bindings.NotifyType_EDIT_FILE:
		kd.cancelPendingRemove(req.Path)

		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCFailedPrecondition
		}

		if (kd.FS() == nil || kd.FS().Root() == nil) && kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			defer kd.SyncTracker.RemoteFilesMu.Unlock()
			f, ok := kd.SyncTracker.RemoteFiles[req.Path]
			if !ok {
				name := req.Name
				if name == "" {
					name = filepath.Base(req.Path)
				}
				kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
					Name:         name,
					RelativePath: req.Path,
					Size:         uint64(req.Attr.Size),
					LastEditTime: req.Attr.ModificationTime,
					CreatedTime:  req.Attr.BirthTime,
					Atime:        req.Attr.AccessTime,
					Mode:         req.Attr.Mode,
				}
			} else {
				f.Name = req.Name
				f.RelativePath = req.Path
				f.Size = uint64(req.Attr.Size)
				f.LastEditTime = req.Attr.ModificationTime
				f.CreatedTime = req.Attr.BirthTime
				f.Atime = req.Attr.AccessTime
				f.Mode = req.Attr.Mode
			}

			if kd.OnEvent != nil {
				name := req.Name
				if name == "" {
					name = filepath.Base(req.Path)
				}
				kd.OnEvent(fmt.Sprintf("file_arrived:%s:%d", name, req.Attr.Size))
			}

			return &bindings.NotifyResponse{}, nil
		}

		if kd.FS() == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS().Root() == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS().Root().EditRemoteFileWithBase(logger, req.Path, req.Name, statFromAttr(req.Attr), req.BaseMtimeNs)

		if err != nil {
			return nil, ErrGRPCFailedPrecondition
		}

		// Keep SyncTracker in sync on the FUSE path too (#164).
		kd.upsertRemoteInTracker(req)
	case bindings.NotifyType_REMOVE_FILE:
		// Buffer REMOVE for 1000ms. Git's atomic write pattern is:
		//   REMOVE HEAD → CREATE HEAD → RENAME HEAD.lock (all within <1ms)
		// If an ADD_FILE or RENAME_FILE arrives for the same path within the
		// window, the REMOVE is cancelled — it was part of an atomic write,
		// not a real user deletion.
		logger.Info("Buffering remove (1000ms)", "path", req.Path)
		kd.bufferRemove(req.Path, logger)
	case bindings.NotifyType_RENAME_FILE:
		// Cancel pending removes for both old and new path.
		// Git renames .lock→final: the final path may have a pending REMOVE.
		kd.cancelPendingRemove(req.Path)
		kd.cancelPendingRemove(req.OldPath)

		// Peer renamed/moved a file. OldPath -> Path.
		logger.Info("Rename file", "oldPath", req.OldPath, "newPath", req.Path)

		if kd.FS() != nil && kd.FS().Root() != nil {
			oldDiskPath := filepath.Clean(filepath.Join(kd.FS().Root().RealPathOfFile, req.OldPath))
			newDiskPath := filepath.Clean(filepath.Join(kd.FS().Root().RealPathOfFile, req.Path))

			// A swap whose declared base is older than LOCAL authority must not
			// clobber the disk before the acceptance verdict (which preserves
			// the local bytes as a conflict copy). Git-style flows never trip
			// this: their targets hold no local authority.
			conflictSkip := kd.FS().Root().SwapWouldConflict(req.Path, req.BaseMtimeNs)

			if conflictSkip {
				// The temp's prefetched bytes (if any) refetch under the
				// canonical name after the verdict; drop the litter.
				if rmErr := os.Remove(oldDiskPath); rmErr != nil && !os.IsNotExist(rmErr) {
					logger.Warn("Failed to drop swap temp", "path", oldDiskPath, "error", rmErr)
				}
			} else {
				// Rename the file on disk FIRST, before updating maps.
				// The prefetch deferred cleanup also tries to rename, but it races with
				// this handler — if prefetch finishes before RENAME arrives, the deferred
				// cleanup sees no path change and skips the disk rename.
				if err := os.MkdirAll(filepath.Dir(newDiskPath), 0o755); err != nil {
					logger.Warn("Failed to create dirs for rename", "error", err)
				}
				if err := filesystem.RenameShared(oldDiskPath, newDiskPath); err != nil {
					// Not fatal — file may not exist yet (prefetch still in progress),
					// or prefetch deferred cleanup already moved it.
					if !os.IsNotExist(err) {
						logger.Warn("Disk rename failed", "from", oldDiskPath, "to", newDiskPath, "error", err)
					}
				} else {
					logger.Info("Renamed file on disk", "from", oldDiskPath, "to", newDiskPath)
				}
			}

			// Peek for a local object at the target BEFORE the map re-key:
			// map locks stay sequential, never nested.
			kd.FS().Root().AfmLock.Lock()
			localTarget, hasLocalTarget := kd.FS().Root().AllFileMap[req.Path]
			kd.FS().Root().AfmLock.Unlock()

			// Cancel any in-flight prefetch for the old path before
			// updating maps — prevents it from racing with disk rename
			// or subsequent Open on the new path.
			kd.FS().Root().RemoteFilesLock.Lock()
			file, exists := kd.FS().Root().RemoteFiles[req.OldPath]
			target, targetTracked := kd.FS().Root().RemoteFiles[req.Path]
			// A DISTINCT object already at the target (tracked OR local-only):
			// never blind-replace it — retire the temp's entry and let the
			// acceptance path update the canonical object (split-brain guard,
			// same disease ADD_FILE had).
			collision := exists && ((targetTracked && target != file) ||
				(hasLocalTarget && localTarget != file))
			if exists {
				if file.PrefetchCancel != nil {
					file.PrefetchCancel()
					file.PrefetchCancel = nil
				}
				delete(kd.FS().Root().RemoteFiles, req.OldPath)
				if !collision {
					file.RelativePath = req.Path
					file.Name = filepath.Base(req.Path)
					file.RealPathOfFile = newDiskPath
					kd.FS().Root().RemoteFiles[req.Path] = file
					logger.Info("Renamed remote file reference", "oldPath", req.OldPath, "newPath", req.Path)
				}
			}
			kd.FS().Root().RemoteFilesLock.Unlock()

			// Also update AllFileMap. If the target keeps a distinct object
			// (collision), only retire the old-path entry.
			kd.FS().Root().AfmLock.Lock()
			if f, ok := kd.FS().Root().AllFileMap[req.OldPath]; ok {
				delete(kd.FS().Root().AllFileMap, req.OldPath)
				tgtLocal, hasTgtLocal := kd.FS().Root().AllFileMap[req.Path]
				if !collision && (!hasTgtLocal || tgtLocal == f) {
					f.RelativePath = req.Path
					f.Name = filepath.Base(req.Path)
					f.RealPathOfFile = newDiskPath
					kd.FS().Root().AllFileMap[req.Path] = f
				}
			}
			kd.FS().Root().AfmLock.Unlock()

			// Collision or conflict: the target object is canonical; run the
			// announced state through the acceptance path (watermark, conflict
			// preserve, bitmap reset/reconcile, prefetch).
			if (collision || conflictSkip) && req.Attr != nil {
				_ = kd.FS().Root().AddRemoteFileWithBase(logger, req.Path, filepath.Base(req.Path), statFromAttr(req.Attr), req.BaseMtimeNs)
			}

			// Check if the renamed file needs re-downloading.
			// Cases: (a) file doesn't exist locally (prefetch was still
			// in progress on the old path and got cancelled above),
			// (b) file exists but has wrong size (git index-pack appends
			// 20-byte SHA-1 checksum between the initial write and rename).
			if exists && !collision && !conflictSkip && req.Attr != nil && req.Attr.Size > 0 {
				needsRedownload := false
				localInfo, statErr := os.Stat(newDiskPath)
				if statErr != nil {
					needsRedownload = true
					logger.Info("File missing after rename, scheduling re-download",
						"remoteSize", req.Attr.Size, "path", req.Path)
				} else if localInfo.Size() != req.Attr.Size {
					needsRedownload = true
					logger.Info("Size mismatch after rename, scheduling re-download",
						"localSize", localInfo.Size(), "remoteSize", req.Attr.Size, "path", req.Path)
				}

				if needsRedownload {
					_ = kd.FS().Root().AddRemoteFileWithBase(logger, req.Path, filepath.Base(req.Path), statFromAttr(req.Attr), req.BaseMtimeNs)
				}
			}

			// Handle lock-file → final-file renames (git's .lock pattern).
			// The ADD_FILE for .lock was debounced and arrives later, but the
			// RENAME arrives immediately. If the target already exists in
			// RemoteFiles (e.g., .git/HEAD), trigger re-download now so the
			// peer doesn't read stale content during the debounce window.
			if !exists && !collision && !conflictSkip && req.Attr != nil && req.Attr.Size > 0 {
				// The rename's SOURCE was a transient temp the peer never received
				// (git writes tmp_pack_XXX / *.lock then renames to the FINAL name),
				// so neither map had it and the disk rename above failed with
				// ENOENT — nothing materialized the destination. The RENAME event
				// carries the destination's Attr, so create/refresh it as a remote
				// on-demand file here. Previously this fired ONLY when the target was
				// ALREADY tracked (true for git's HEAD, but NOT for a brand-new pack/
				// idx/index), so those final clone artifacts intermittently never
				// propagated → peer ".git/objects" empty → "bad object HEAD".
				logger.Info("Rename of untracked source: materializing destination",
					"path", req.Path, "size", req.Attr.Size)
				_ = kd.FS().Root().AddRemoteFileWithBase(logger, req.Path, filepath.Base(req.Path), statFromAttr(req.Attr), req.BaseMtimeNs)
			}
		}

		// Handle non-FUSE mode.
		if kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			if f, ok := kd.SyncTracker.RemoteFiles[req.OldPath]; ok {
				delete(kd.SyncTracker.RemoteFiles, req.OldPath)
				f.RelativePath = req.Path
				f.Name = filepath.Base(req.Path)
				if req.Attr != nil && req.Attr.Size > 0 {
					f.Size = uint64(req.Attr.Size)
				}
				kd.SyncTracker.RemoteFiles[req.Path] = f
				logger.Info("Renamed file in sync tracker", "oldPath", req.OldPath, "newPath", req.Path, "size", f.Size)
			} else if req.Attr != nil && req.Attr.Size > 0 {
				// Old path wasn't tracked (temp file notification was debounced away).
				// Create entry with the correct size from the RENAME attr.
				kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
					Name:         filepath.Base(req.Path),
					RelativePath: req.Path,
					Size:         uint64(req.Attr.Size),
					LastEditTime: req.Attr.ModificationTime,
				}
				logger.Info("Created file from RENAME (old path not tracked)", "path", req.Path, "size", req.Attr.Size)
			}
			kd.SyncTracker.RemoteFilesMu.Unlock()
		}
	case bindings.NotifyType_RENAME_DIR:
		// Peer renamed/moved a directory. OldPath -> Path.
		logger.Info("Rename directory", "oldPath", req.OldPath, "newPath", req.Path)

		if kd.FS() != nil && kd.FS().Root() != nil {
			kd.FS().Root().Adm.Lock()
			if dir, ok := kd.FS().Root().AllDirMap[req.OldPath]; ok {
				delete(kd.FS().Root().AllDirMap, req.OldPath)
				dir.RelativePath = req.Path
				dir.Name = filepath.Base(req.Path)
				kd.FS().Root().AllDirMap[req.Path] = dir
				logger.Info("Renamed directory reference", "oldPath", req.OldPath, "newPath", req.Path)
			}
			kd.FS().Root().Adm.Unlock()
		}
	}

	logger.Info("Success")

	return &bindings.NotifyResponse{}, nil
}

// BatchNotify processes multiple notifications in a single RPC call.
// This eliminates per-notification round-trip overhead during large clones.
func (kd *KeibidropServiceImpl) BatchNotify(ctx context.Context, req *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
	logger := kd.Logger.With("method", "batch-notify", "count", len(req.Notifications), "seq", req.Seq)
	logger.Info("Processing batch")

	if kd.ShareReadOnly {
		if kd.readOnlyRefusalLogged.CompareAndSwap(false, true) {
			logger.Warn("Refusing peer mutation batch: share is read-only")
		}
		return nil, ErrGRPCReadOnlyShare
	}

	var processed uint32
	for _, n := range req.Notifications {
		_, err := kd.Notify(ctx, n)
		if err != nil {
			logger.Error("Failed to process notification in batch", "path", n.Path, "type", n.Type, "error", err)
			continue
		}
		processed++
	}

	logger.Info("Batch complete", "processed", processed)
	return &bindings.BatchNotifyResponse{Status: "ok", Processed: processed}, nil
}

// bufferRemove delays a REMOVE_FILE by 1000ms. If cancelPendingRemove is called
// for the same path before the timer fires, the remove is discarded.
func (kd *KeibidropServiceImpl) bufferRemove(path string, logger *slog.Logger) {
	kd.pendingRemovesMu.Lock()
	defer kd.pendingRemovesMu.Unlock()

	if kd.pendingRemoves == nil {
		kd.pendingRemoves = make(map[string]*time.Timer)
	}

	// Cancel any existing pending remove for this path.
	if t, ok := kd.pendingRemoves[path]; ok {
		t.Stop()
	}

	kd.pendingRemoves[path] = time.AfterFunc(1000*time.Millisecond, func() {
		kd.pendingRemovesMu.Lock()
		delete(kd.pendingRemoves, path)
		kd.pendingRemovesMu.Unlock()

		logger.Info("Executing buffered remove (no ADD/RENAME arrived)", "path", path)
		kd.executeRemove(path, logger)
	})
}

// cancelPendingRemove cancels a buffered REMOVE_FILE if one exists.
// Called when ADD_FILE, EDIT_FILE, or RENAME_FILE arrives for the same path,
// indicating the REMOVE was part of git's atomic .lock→rename dance.
func (kd *KeibidropServiceImpl) cancelPendingRemove(path string) {
	kd.pendingRemovesMu.Lock()
	defer kd.pendingRemovesMu.Unlock()

	if kd.pendingRemoves == nil {
		return
	}
	if t, ok := kd.pendingRemoves[path]; ok {
		t.Stop()
		delete(kd.pendingRemoves, path)
		kd.Logger.Info("Cancelled buffered remove (ADD/RENAME arrived)", "path", path)
	}
}

// cancelAllPendingRemoves stops every buffered remove (on disconnect). The
// SyncTracker and FS outlive the session; a timer surviving into the next
// session would remove a freshly re-synced file.
func (kd *KeibidropServiceImpl) cancelAllPendingRemoves() {
	kd.pendingRemovesMu.Lock()
	defer kd.pendingRemovesMu.Unlock()
	for _, t := range kd.pendingRemoves {
		t.Stop()
	}
	kd.pendingRemoves = nil
}

// executeRemove performs the actual REMOVE_FILE logic (previously inline in Notify).
func (kd *KeibidropServiceImpl) executeRemove(path string, logger *slog.Logger) {
	if kd.FS() != nil && kd.FS().Root() != nil {
		hasOpenHandles := false
		kd.FS().Root().AfmLock.Lock()
		file, exists := kd.FS().Root().AllFileMap[path]
		if exists && file != nil {
			openCount := file.CountOpenDescriptors()
			if openCount > 0 {
				file.PeerStoppedSharing = true
				hasOpenHandles = true
				logger.Info("File has open handles, marking for removal after download", "path", path, "openHandles", openCount)
			} else {
				delete(kd.FS().Root().AllFileMap, path)
			}
		}
		kd.FS().Root().AfmLock.Unlock()

		kd.FS().Root().RemoteFilesLock.Lock()
		if rf, rfOk := kd.FS().Root().RemoteFiles[path]; rfOk {
			if rf.PrefetchCancel != nil {
				rf.PrefetchCancel()
				rf.PrefetchCancel = nil
			}
			delete(kd.FS().Root().RemoteFiles, path)
		}
		kd.FS().Root().RemoteFilesLock.Unlock()

		if !hasOpenHandles {
			cachePath := filepath.Clean(filepath.Join(kd.FS().Root().LocalDownloadFolder, path))
			if rmErr := os.Remove(cachePath); rmErr != nil && !os.IsNotExist(rmErr) {
				logger.Warn("Failed to remove cache file", "path", cachePath, "error", rmErr)
			}
		}
	}

	if kd.SyncTracker != nil {
		kd.SyncTracker.RemoteFilesMu.Lock()
		delete(kd.SyncTracker.RemoteFiles, path)
		kd.SyncTracker.RemoteFilesMu.Unlock()
	}
}

// upsertRemoteInTracker mirrors a peer ADD_FILE/EDIT_FILE into
// SyncTracker.RemoteFiles. The desktop/mobile file lists read SyncTracker, so
// the FUSE path must update it too — otherwise files added over FUSE never
// appear in the list even though FUSE serves them (#164). Keyed by req.Path to
// match the RENAME_FILE and REMOVE_FILE handlers.
func (kd *KeibidropServiceImpl) upsertRemoteInTracker(req *bindings.NotifyRequest) {
	if kd.SyncTracker == nil || req.Attr == nil {
		return
	}
	name := req.Name
	if name == "" {
		name = filepath.Base(req.Path)
	}
	kd.SyncTracker.RemoteFilesMu.Lock()
	defer kd.SyncTracker.RemoteFilesMu.Unlock()
	if f, ok := kd.SyncTracker.RemoteFiles[req.Path]; ok {
		f.Name = name
		f.RelativePath = req.Path
		f.Size = uint64(req.Attr.Size)
		f.LastEditTime = req.Attr.ModificationTime
		f.CreatedTime = req.Attr.BirthTime
		f.Atime = req.Attr.AccessTime
		f.Mode = req.Attr.Mode
		return
	}
	kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
		Name:         name,
		RelativePath: req.Path,
		Size:         uint64(req.Attr.Size),
		LastEditTime: req.Attr.ModificationTime,
		CreatedTime:  req.Attr.BirthTime,
		Atime:        req.Attr.AccessTime,
		Mode:         req.Attr.Mode,
	}
}

// resolveFUSEPath returns the on-disk path for a peer-requested path in FUSE
// mode: AllFileMap first (keys carry a leading "/"), then SyncTracker.LocalFiles
// (drag-and-drop AddFile entries). Returns "" if not found. Each map lock is
// held only briefly and never across the returned path's use. Caller must have
// verified kd.FS() and kd.FS().Root() are non-nil.
func (kd *KeibidropServiceImpl) resolveFUSEPath(path string) string {
	fuseKey := path
	if !strings.HasPrefix(fuseKey, "/") {
		fuseKey = "/" + fuseKey
	}
	kd.FS().Root().AfmLock.RLock()
	f, ok := kd.FS().Root().AllFileMap[fuseKey]
	kd.FS().Root().AfmLock.RUnlock()
	if ok {
		return f.RealPathOfFile
	}
	if kd.SyncTracker == nil {
		return ""
	}
	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()
	if lf, ok := kd.SyncTracker.LocalFiles[strings.TrimPrefix(path, "/")]; ok {
		return lf.RealPathOfFile
	}
	if lf, ok := kd.SyncTracker.LocalFiles[path]; ok {
		return lf.RealPathOfFile
	}
	return ""
}

func (kd *KeibidropServiceImpl) Read(stream bindings.KeibiService_ReadServer) error {
	logger := kd.Logger.With("method", "server-read")

	if (kd.FS() == nil || kd.FS().Root() == nil) && kd.SyncTracker != nil {
		isOpen := false

		var fh *os.File
		var openedPath string
		buf := make([]byte, config.GRPCStreamBuffer)

		for {
			rec, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					if fh != nil {
						fh.Close()
					}
					logger.Info("Read stream finished")
					return nil
				}
				if fh != nil {
					fh.Close()
				}
				if err == context.Canceled || status.Code(err) == codes.Canceled {
					logger.Debug("Stream cancelled by peer", "error", err)
					return nil
				}
				logger.Error("Failed to receive from stream", "error", err)
				return status.Error(codes.Internal, "failed to receive read request")
			}

			if !isOpen {
				isOpen = true
				// Normalize: FUSE peers send "/filename", no-FUSE uses bare "filename".
				// LocalFiles keys use bare names (from AddFile's finfo.Name()).
				lookupPath := strings.TrimPrefix(rec.Path, "/")
				// Lock only the lookup; copy the path and stream lock-free.
				kd.SyncTracker.LocalFilesMu.RLock()
				f, ok := kd.SyncTracker.LocalFiles[lookupPath]
				if !ok {
					// Fallback: try original path (e.g. full path stored by PullFile).
					f, ok = kd.SyncTracker.LocalFiles[rec.Path]
				}
				var realPath string
				if ok {
					realPath = f.RealPathOfFile
				}
				kd.SyncTracker.LocalFilesMu.RUnlock()
				if !ok {
					logger.Warn("File not found", "rec", rec)
					return status.Error(codes.NotFound, "file not found")
				}

				fh, err = os.Open(realPath)
				if err != nil {
					logger.Error("Failed to open real file", "error", err)
					return status.Error(codes.Internal, "error accessing file")
				}
				openedPath = rec.Path
			} else if openedPath != rec.Path {
				logger.Error("Multiple paths in same stream not supported", "requested", rec.Path)
				return status.Error(codes.InvalidArgument, "stream can only read a single file")
			}

			size := int(rec.Size)
			offset := int64(rec.Offset)
			if size > len(buf) {
				logger.Warn("Requested size too large, truncating to buffer size", "requested", size, "buffer", len(buf))
				size = len(buf)
			}

			n, err := fh.ReadAt(buf[:size], offset)
			if err != nil && !errors.Is(err, io.EOF) {
				logger.Error("Failed to read file", "error", err)
				return status.Error(codes.Internal, "error reading file")
			}

			err = stream.Send(&bindings.ReadResponse{
				Data: buf[:n],
			})
			if err != nil {
				if errors.Is(err, io.EOF) {
					if fh != nil {
						fh.Close()
					}
					logger.Info("Read stream finished")
					return nil
				}
				if fh != nil {
					fh.Close()
				}
				logger.Error("Failed to send read data", "error", err)
				return status.Error(codes.Internal, "failed to send data")
			}
		}
	}

	if kd.FS() == nil || kd.FS().Root() == nil {
		logger.Error("FS or Root is nil")
		return ErrGRPCFailedPrecondition
	}

	isOpen := false
	// NOTE: We do NOT hold AfmLock for the entire stream anymore.
	// This was causing deadlocks with FUSE Open() which needs AfmLock.Lock().
	// Instead, we only lock briefly to look up the file path.

	var fh *os.File
	var openedPath string
	buf := make([]byte, config.GRPCStreamBuffer)

	for {
		rec, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if fh != nil {
					fh.Close()
				}
				logger.Info("Read stream finished")
				return nil
			}
			if fh != nil {
				fh.Close()
			}
			logger.Error("Failed to receive from stream", "error", err)
			return status.Error(codes.Internal, "failed to receive read request")
		}

		if !isOpen {
			isOpen = true

			realPath := kd.resolveFUSEPath(rec.Path)
			if realPath == "" {
				logger.Warn("File not found", "rec", rec)
				return status.Error(codes.NotFound, "file not found")
			}

			fh, err = os.Open(realPath) // #nosec G304
			if err != nil {
				logger.Error("Failed to open real file", "error", err)
				return status.Error(codes.Internal, "error accessing file")
			}
			openedPath = rec.Path
		} else if openedPath != rec.Path {
			logger.Error("Multiple paths in same stream not supported", "requested", rec.Path)
			return status.Error(codes.InvalidArgument, "stream can only read a single file")
		}

		size := int(rec.Size)
		offset := int64(rec.Offset)
		if size > len(buf) {
			logger.Warn("Requested size too large, truncating to buffer size", "requested", size, "buffer", len(buf))
			size = len(buf)
		}

		n, err := fh.ReadAt(buf[:size], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			logger.Error("Failed to read file", "error", err)
			return status.Error(codes.Internal, "error reading file")
		}

		err = stream.Send(&bindings.ReadResponse{
			Data: buf[:n],
		})
		if err != nil {
			if errors.Is(err, io.EOF) {
				if fh != nil {
					fh.Close()
				}
				logger.Info("Read stream finished")
				return nil
			}
			if fh != nil {
				fh.Close()
			}
			logger.Error("Failed to send read data", "error", err)
			return status.Error(codes.Internal, "failed to send data")
		}
	}
}

// StreamFile pushes an entire file's contents to the client in sequential
// chunks. The client sends one request; the server streams all data from
// start_offset to EOF with no per-chunk round-trip overhead.
func (kd *KeibidropServiceImpl) StreamFile(req *bindings.StreamFileRequest, stream bindings.KeibiService_StreamFileServer) error {
	logger := kd.Logger.With("method", "stream-file", "path", req.Path)

	// Look up the file — same logic as the Read handler for both modes.
	var realPath string

	if (kd.FS() == nil || kd.FS().Root() == nil) && kd.SyncTracker != nil {
		lookupPath := strings.TrimPrefix(req.Path, "/")
		kd.SyncTracker.LocalFilesMu.RLock()
		f, ok := kd.SyncTracker.LocalFiles[lookupPath]
		if !ok {
			f, ok = kd.SyncTracker.LocalFiles[req.Path]
		}
		kd.SyncTracker.LocalFilesMu.RUnlock()
		if ok {
			realPath = f.RealPathOfFile
		}
	} else if kd.FS() != nil && kd.FS().Root() != nil {
		realPath = kd.resolveFUSEPath(req.Path)
	}

	if realPath == "" {
		logger.Warn("File not found", "path", req.Path)
		return status.Error(codes.NotFound, "file not found")
	}

	fh, err := os.Open(realPath)
	if err != nil {
		logger.Error("Failed to open file", "error", err)
		return status.Error(codes.Internal, "error accessing file")
	}
	defer fh.Close()

	finfo, err := fh.Stat()
	if err != nil {
		logger.Error("Failed to stat file", "error", err)
		return status.Error(codes.Internal, "error stat file")
	}
	fileSize := uint64(finfo.Size())

	buf := make([]byte, config.GRPCStreamBuffer)
	chunkSize := uint64(config.BlockSize)
	offset := req.StartOffset

	logger.Info("StreamFile starting", "fileSize", fileSize, "startOffset", offset)

	for offset < fileSize {
		size := chunkSize
		if offset+size > fileSize {
			size = fileSize - offset
		}

		n, readErr := fh.ReadAt(buf[:size], int64(offset))
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			logger.Error("Failed to read file", "offset", offset, "error", readErr)
			return status.Error(codes.Internal, "error reading file")
		}
		if n == 0 {
			break
		}

		if sendErr := stream.Send(&bindings.StreamFileResponse{
			Data:      buf[:n],
			Offset:    offset,
			TotalSize: fileSize,
		}); sendErr != nil {
			// Client cancelled or disconnected — normal during file close.
			logger.Info("StreamFile send ended", "offset", offset, "error", sendErr)
			return sendErr
		}

		offset += uint64(n)
	}

	logger.Info("StreamFile complete", "bytesSent", offset-req.StartOffset)
	return nil
}

// GetChunkHashes streams per-chunk xxh3-64 fingerprints for a file. Path
// resolution and authorization mirror StreamFile exactly.
func (kd *KeibidropServiceImpl) GetChunkHashes(req *bindings.GetChunkHashesRequest, stream bindings.KeibiService_GetChunkHashesServer) error {
	logger := kd.Logger.With("method", "get-chunk-hashes", "path", req.Path)

	if req.ChunkSize == 0 {
		return status.Error(codes.InvalidArgument, "chunk_size must be non-zero")
	}
	// Reject an out-of-range chunk_size before allocating: a peer-supplied value
	// near MaxUint64 (or several GiB) would otherwise make([]byte, cs) OOM/panic
	// the whole process. The legitimate caller passes 512 KiB, far under the cap.
	if req.ChunkSize > config.GRPCStreamBuffer {
		return status.Error(codes.InvalidArgument, "chunk_size too large")
	}

	// Resolve path using the same logic as StreamFile.
	var realPath string

	if (kd.FS() == nil || kd.FS().Root() == nil) && kd.SyncTracker != nil {
		lookupPath := strings.TrimPrefix(req.Path, "/")
		kd.SyncTracker.LocalFilesMu.RLock()
		f, ok := kd.SyncTracker.LocalFiles[lookupPath]
		if !ok {
			f, ok = kd.SyncTracker.LocalFiles[req.Path]
		}
		kd.SyncTracker.LocalFilesMu.RUnlock()
		if ok {
			realPath = f.RealPathOfFile
		}
	} else if kd.FS() != nil && kd.FS().Root() != nil {
		realPath = kd.resolveFUSEPath(req.Path)
	}

	if realPath == "" {
		logger.Warn("File not found", "path", req.Path)
		return status.Error(codes.NotFound, "file not found")
	}

	fh, err := os.Open(realPath) // #nosec G304
	if err != nil {
		logger.Error("Failed to open file", "error", err)
		return status.Error(codes.Internal, "error accessing file")
	}
	defer fh.Close()

	finfo, err := fh.Stat()
	if err != nil {
		logger.Error("Failed to stat file", "error", err)
		return status.Error(codes.Internal, "error stat file")
	}
	fileSize := uint64(finfo.Size())

	cs := req.ChunkSize
	from := req.FromChunk
	count := req.Count
	buf := make([]byte, cs)

	logger.Info("GetChunkHashes starting", "fileSize", fileSize, "fromChunk", from, "count", count, "chunkSize", cs)

	for c := from; ; c++ {
		if count != 0 && c >= from+count {
			break
		}
		offset := c * cs
		if offset >= fileSize {
			break
		}
		size := cs
		if offset+size > fileSize {
			size = fileSize - offset
		}
		n, readErr := fh.ReadAt(buf[:size], int64(offset))
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			logger.Error("Failed to read chunk", "chunk", c, "error", readErr)
			return status.Error(codes.Internal, "error reading file")
		}
		if n == 0 {
			break
		}
		h := xxh3.Hash(buf[:n])
		if sendErr := stream.Send(&bindings.GetChunkHashesResponse{
			ChunkIndex: c,
			Hash:       h,
		}); sendErr != nil {
			logger.Info("GetChunkHashes send ended", "chunk", c, "error", sendErr)
			return sendErr
		}
	}

	logger.Info("GetChunkHashes complete")
	return nil
}

// Rekey drives the responder side of the entropy fold: an ephemeral hybrid-KEM whose shared
// secret mixes into the session ratchet for post-quantum forward secrecy. Self-synchronizing
// on the wire (no mid-stream key swap), so a stray or forged Rekey can't brick the connection.
func (kd *KeibidropServiceImpl) Rekey(_ context.Context, req *bindings.RekeyRequest) (*bindings.RekeyResponse, error) {
	if kd.Session == nil {
		return nil, status.Error(codes.FailedPrecondition, "no active session for rekey")
	}
	return kd.Session.RespondToFold(req)
}

// Heartbeat responds to connection health checks.
// Used by HealthMonitor to detect connection loss.
func (kd *KeibidropServiceImpl) Heartbeat(_ context.Context, req *bindings.HeartbeatRequest) (*bindings.HeartbeatResponse, error) {
	return &bindings.HeartbeatResponse{
		Timestamp:    uint64(time.Now().UnixNano()),
		ReqTimestamp: req.Timestamp,
		Seq:          req.Seq,
	}, nil
}
