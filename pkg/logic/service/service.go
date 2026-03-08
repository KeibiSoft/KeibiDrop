// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/winfsp/cgofuse/fuse"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrGRPCNotMounted         = status.Error(codes.FailedPrecondition, "filesystem not mounted")
	ErrGRPCInvalidArgument    = status.Error(codes.InvalidArgument, "invalid argument")
	ErrGRPCFailedPrecondition = status.Error(codes.FailedPrecondition, "failed precondition")
	ErrGRPCAlreadyExists      = status.Error(codes.AlreadyExists, "already exists")
	ErrGRPCNotFound           = status.Error(codes.NotFound, "notFound")
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session     *session.Session
	Logger      *slog.Logger
	FS          *filesystem.FS
	SyncTracker *synctracker.SyncTracker
}

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("Debug called. Success!")

	return &bindings.DebugResponse{}, nil
}

func (kd *KeibidropServiceImpl) Notify(_ context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	logger := kd.Logger.With("method", "notify", "req-type", req.Type)

	if kd.FS == nil && kd.SyncTracker == nil {
		logger.Warn("Filesystem not mounted")
		return nil, ErrGRPCNotMounted
	}

	switch req.Type {
	case bindings.NotifyType_UNKNOWN:
		logger.Warn("Unknown notification")
		return nil, ErrGRPCInvalidArgument
	case bindings.NotifyType_ADD_DIR:
		logger.Info("Mkdir called")

		if kd.FS == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS.Root == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS.Root.MkdirFromPeer(req.Path, 0755) // Use FromPeer to avoid notification loop.
		if err != 0 {
			return nil, ErrGRPCFailedPrecondition
		}
	case bindings.NotifyType_EDIT_DIR:
		logger.Info("Edit dir is not implemented")
		// TODO: Modify the attr time as per the client payload.
	case bindings.NotifyType_REMOVE_DIR:
		// Peer stopped sharing this directory. Remove from our view and from local disk.
		logger.Info("Remove dir reference", "path", req.Path)

		if kd.FS != nil && kd.FS.Root != nil {
			kd.FS.Root.Adm.Lock()
			delete(kd.FS.Root.AllDirMap, req.Path)
			kd.FS.Root.Adm.Unlock()
			logger.Info("Removed directory reference from view", "path", req.Path)

			// Remove the local directory so Getattr doesn't find a stale disk entry.
			localDir := filepath.Clean(filepath.Join(kd.FS.Root.RealPathOfFile, req.Path))
			if err := os.RemoveAll(localDir); err != nil && !os.IsNotExist(err) {
				logger.Warn("Failed to remove local dir on peer remove", "path", localDir, "error", err)
			}
		}
	case bindings.NotifyType_ADD_FILE:
		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCInvalidArgument
		}

		logger.Info("<<< RECEIVED ADD_FILE FROM PEER",
			"path", req.Path,
			"size", req.Attr.Size)

		atim := time.Unix(0, int64(req.Attr.AccessTime))
		mtim := time.Unix(0, int64(req.Attr.ModificationTime))
		ctim := time.Unix(0, int64(req.Attr.ChangeTime))
		btim := time.Unix(0, int64(req.Attr.BirthTime))

		if (kd.FS == nil || kd.FS.Root == nil) && kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			defer kd.SyncTracker.RemoteFilesMu.Unlock()
			existing, ok := kd.SyncTracker.RemoteFiles[req.Path]
			if ok {
				// Update existing entry (peer overwrote the file).
				existing.Size = uint64(req.Attr.Size)
				existing.LastEditTime = req.Attr.ModificationTime
				logger.Info("Updated existing remote file", "path", req.Path, "newSize", req.Attr.Size)
				return &bindings.NotifyResponse{}, nil
			}

			kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
				Name:         req.Name,
				RelativePath: req.Path,
				Size:         uint64(req.Attr.Size),
				LastEditTime: req.Attr.ModificationTime,
				CreatedTime:  req.Attr.BirthTime,
			}

			logger.Info("Success")

			return &bindings.NotifyResponse{}, nil
		}

		if kd.FS == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS.Root == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS.Root.AddRemoteFile(logger, req.Path, req.Name, &fuse.Stat_t{
			Dev:      req.Attr.Dev,
			Ino:      req.Attr.Ino,
			Mode:     req.Attr.Mode,
			Nlink:    1,
			Uid:      uint32(os.Getuid()),
			Gid:      uint32(os.Getgid()),
			Size:     req.Attr.Size,
			Atim:     fuse.NewTimespec(atim),
			Mtim:     fuse.NewTimespec(mtim),
			Ctim:     fuse.NewTimespec(ctim),
			Birthtim: fuse.NewTimespec(btim),
			Flags:    req.Attr.Flags,
		})
		if err != nil {
			return nil, ErrGRPCInvalidArgument
		}
	case bindings.NotifyType_EDIT_FILE:
		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCFailedPrecondition
		}
		atim := time.Unix(0, int64(req.Attr.AccessTime))
		mtim := time.Unix(0, int64(req.Attr.ModificationTime))
		ctim := time.Unix(0, int64(req.Attr.ChangeTime))
		btim := time.Unix(0, int64(req.Attr.BirthTime))

		if (kd.FS == nil || kd.FS.Root == nil) && kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.RLock()
			defer kd.SyncTracker.RemoteFilesMu.RUnlock()
			f, ok := kd.SyncTracker.RemoteFiles[req.Path]
			if !ok {
				logger.Error("File does not exists", "error", ErrGRPCNotFound)
				return nil, ErrGRPCNotFound
			}

			f.Name = req.Name
			f.RelativePath = req.Path
			f.Size = uint64(req.Attr.Size)
			f.LastEditTime = req.Attr.ModificationTime
			f.CreatedTime = req.Attr.BirthTime

			logger.Info("Success")

			return &bindings.NotifyResponse{}, nil
		}

		if kd.FS == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS.Root == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS.Root.EditRemoteFile(logger, req.Path, req.Name, &fuse.Stat_t{
			Dev:      req.Attr.Dev,
			Ino:      req.Attr.Ino,
			Mode:     req.Attr.Mode,
			Nlink:    1,
			Uid:      uint32(os.Getuid()),
			Gid:      uint32(os.Getgid()),
			Size:     req.Attr.Size,
			Atim:     fuse.NewTimespec(atim),
			Mtim:     fuse.NewTimespec(mtim),
			Ctim:     fuse.NewTimespec(ctim),
			Birthtim: fuse.NewTimespec(btim),
			Flags:    req.Attr.Flags,
		})

		if err != nil {
			return nil, ErrGRPCFailedPrecondition
		}
	case bindings.NotifyType_REMOVE_FILE:
		// Peer stopped sharing this file. Remove from our view but keep any local/partial data.
		logger.Info("Remove file reference", "path", req.Path)

		if kd.FS != nil && kd.FS.Root != nil {
			// Get local path from RemoteFiles before removal (populated directly by AddRemoteFile,
			// unlike AllFileMap which is populated lazily by Getattr).
			var localPath string
			kd.FS.Root.RemoteFilesLock.Lock()
			if rf, ok := kd.FS.Root.RemoteFiles[req.Path]; ok {
				localPath = rf.RealPathOfFile
			}
			delete(kd.FS.Root.RemoteFiles, req.Path)
			kd.FS.Root.RemoteFilesLock.Unlock()

			kd.FS.Root.AfmLock.Lock()
			file, exists := kd.FS.Root.AllFileMap[req.Path]
			if exists && file != nil {
				if localPath == "" {
					localPath = file.RealPathOfFile
				}
				// Check if file has open handles (download in progress).
				openCount := file.CountOpenDescriptors()
				if openCount > 0 {
					// Download in progress - let it complete, then remove.
					file.PeerStoppedSharing = true
					logger.Info("File has open handles, marking for removal after download", "path", req.Path, "openHandles", openCount)
				} else {
					// No active downloads - remove immediately.
					delete(kd.FS.Root.AllFileMap, req.Path)
					logger.Info("Removed file reference from view", "path", req.Path)
				}
			}
			kd.FS.Root.AfmLock.Unlock()

			// Fallback: compute local path from root if still unknown.
			if localPath == "" {
				localPath = filepath.Clean(filepath.Join(kd.FS.Root.RealPathOfFile, req.Path))
			}

			// Remove the local placeholder file so Getattr doesn't find a stale disk entry.
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				logger.Warn("Failed to remove local file on peer remove", "path", localPath, "error", err)
			}
		}

		// Also handle non-FUSE mode.
		if kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			delete(kd.SyncTracker.RemoteFiles, req.Path)
			kd.SyncTracker.RemoteFilesMu.Unlock()
			logger.Info("Removed file from sync tracker", "path", req.Path)
		}
	case bindings.NotifyType_RENAME_FILE:
		// Peer renamed/moved a file. OldPath -> Path.
		logger.Info("Rename file", "oldPath", req.OldPath, "newPath", req.Path)

		if kd.FS != nil && kd.FS.Root != nil {
			var oldLocalPath, newLocalPath string
			// Remove old path from maps and add with new path.
			kd.FS.Root.RemoteFilesLock.Lock()
			file, exists := kd.FS.Root.RemoteFiles[req.OldPath]
			if exists {
				oldLocalPath = file.RealPathOfFile
				delete(kd.FS.Root.RemoteFiles, req.OldPath)
				file.RelativePath = req.Path
				file.Name = filepath.Base(req.Path)
				// Update disk path so prefetchFile can detect the rename and move
				// the downloaded content atomically after the goroutine completes.
				newLocalPath = filepath.Clean(filepath.Join(kd.FS.Root.RealPathOfFile, req.Path))
				file.RealPathOfFile = newLocalPath
				kd.FS.Root.RemoteFiles[req.Path] = file
				logger.Info("Renamed remote file reference", "oldPath", req.OldPath, "newPath", req.Path)
			}
			kd.FS.Root.RemoteFilesLock.Unlock()

			// Also update AllFileMap.
			kd.FS.Root.AfmLock.Lock()
			if f, ok := kd.FS.Root.AllFileMap[req.OldPath]; ok {
				delete(kd.FS.Root.AllFileMap, req.OldPath)
				f.RelativePath = req.Path
				f.Name = filepath.Base(req.Path)
				if newLocalPath == "" {
					newLocalPath = filepath.Clean(filepath.Join(kd.FS.Root.RealPathOfFile, req.Path))
				}
				f.RealPathOfFile = newLocalPath
				kd.FS.Root.AllFileMap[req.Path] = f
			}
			kd.FS.Root.AfmLock.Unlock()

			// Rename the local placeholder file so Getattr doesn't find a stale disk entry.
			if oldLocalPath != "" && newLocalPath != "" {
				if err := os.MkdirAll(filepath.Dir(newLocalPath), 0o755); err != nil {
					logger.Warn("Failed to create dir for renamed local file", "path", newLocalPath, "error", err)
				} else if err := os.Rename(oldLocalPath, newLocalPath); err != nil && !os.IsNotExist(err) {
					logger.Warn("Failed to rename local file on peer rename", "old", oldLocalPath, "new", newLocalPath, "error", err)
				}
			}
		}

		// Handle non-FUSE mode.
		if kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			if f, ok := kd.SyncTracker.RemoteFiles[req.OldPath]; ok {
				delete(kd.SyncTracker.RemoteFiles, req.OldPath)
				f.RelativePath = req.Path
				f.Name = filepath.Base(req.Path)
				kd.SyncTracker.RemoteFiles[req.Path] = f
			}
			kd.SyncTracker.RemoteFilesMu.Unlock()
			logger.Info("Renamed file in sync tracker", "oldPath", req.OldPath, "newPath", req.Path)
		}
	case bindings.NotifyType_RENAME_DIR:
		// Peer renamed/moved a directory. OldPath -> Path.
		logger.Info("Rename directory", "oldPath", req.OldPath, "newPath", req.Path)

		if kd.FS != nil && kd.FS.Root != nil {
			kd.FS.Root.Adm.Lock()
			if dir, ok := kd.FS.Root.AllDirMap[req.OldPath]; ok {
				delete(kd.FS.Root.AllDirMap, req.OldPath)
				dir.RelativePath = req.Path
				dir.Name = filepath.Base(req.Path)
				kd.FS.Root.AllDirMap[req.Path] = dir
				logger.Info("Renamed directory reference", "oldPath", req.OldPath, "newPath", req.Path)
			}
			kd.FS.Root.Adm.Unlock()
		}
	}

	logger.Info("Success")

	return &bindings.NotifyResponse{}, nil
}

func (kd *KeibidropServiceImpl) Read(stream bindings.KeibiService_ReadServer) error {
	logger := kd.Logger.With("method", "server-read")

	if (kd.FS == nil || kd.FS.Root == nil) && kd.SyncTracker != nil {
		// NOTE: We do NOT hold LocalFilesMu for the entire stream.
		// Long-lived read locks block PullFile's write lock, causing deadlocks
		// when background prefetch goroutines hold the read lock indefinitely.
		// Instead, we only hold the lock briefly to look up the real file path.
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
				logger.Error("Failed to receive from stream", "error", err)
				return status.Error(codes.Internal, "failed to receive read request")
			}

			if !isOpen {
				isOpen = true
				// Normalize: FUSE peers send "/filename", no-FUSE uses bare "filename".
				// LocalFiles keys use bare names (from AddFile's finfo.Name()).
				// Only hold the lock briefly to look up the real file path.
				lookupPath := strings.TrimPrefix(rec.Path, "/")
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

	if kd.FS == nil || kd.FS.Root == nil {
		logger.Error("FS or Root is nil")
		return ErrGRPCFailedPrecondition
	}

	isOpen := false
	// NOTE: We do NOT hold AfmLock for the entire stream anymore.
	// This was causing deadlocks with FUSE Open() which needs AfmLock.Lock().
	// Instead, we only lock briefly to look up the file path.

	var fh *os.File
	var openedPath string
	// hardcode buffer to 16 MiB (1<<24 is 16 MB; adjust if needed)
	buf := make([]byte, 1<<24)

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

			// Normalize to FUSE convention: AllFileMap keys have leading "/".
			fuseKey := rec.Path
			if !strings.HasPrefix(fuseKey, "/") {
				fuseKey = "/" + fuseKey
			}

			// Only hold the lock briefly to look up the file path
			kd.FS.Root.AfmLock.RLock()
			f, ok := kd.FS.Root.AllFileMap[fuseKey]
			kd.FS.Root.AfmLock.RUnlock()
			if !ok {
				logger.Warn("File not found", "rec", rec)
				return status.Error(codes.NotFound, "file not found")
			}

			fh, err = os.Open(f.RealPathOfFile)
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

// Rekey handles key rotation requests for forward secrecy.
func (kd *KeibidropServiceImpl) Rekey(_ context.Context, req *bindings.RekeyRequest) (*bindings.RekeyResponse, error) {
	logger := kd.Logger.With("method", "rekey", "epoch", req.Epoch)

	if kd.Session == nil {
		logger.Warn("Session not initialized")
		return nil, status.Error(codes.FailedPrecondition, "session not initialized")
	}

	resp, err := kd.Session.HandleRekeyRequest(req)
	if err != nil {
		logger.Error("Failed to process rekey request", "error", err)
		return nil, status.Error(codes.Internal, "rekey failed")
	}

	logger.Info("Rekey request processed successfully")
	return resp, nil
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
