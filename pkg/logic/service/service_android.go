// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Android-only service implementation. Uses SyncTracker paths only.
// ABOUTME: FUSE/cgofuse paths are excluded — FUSE is not available on Android.

//go:build android

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
	ErrGRPCUnsafePath         = status.Error(codes.InvalidArgument, "path escapes the save root")
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session      *session.Session
	Logger       *slog.Logger
	SyncTracker  *synctracker.SyncTracker
	OnEvent      func(string)
	OnDisconnect func()

	// ShareReadOnly rejects every inbound mutation before it touches the
	// tracker. Mirrors the desktop struct; DISCONNECT still passes.
	ShareReadOnly bool

	// readOnlyRefusalLogged keeps the refusal log to one line per session.
	readOnlyRefusalLogged atomic.Bool

	// fs mirrors the desktop struct so the common package compiles unchanged.
	// Android has no FUSE, so it stays nil.
	fs atomic.Pointer[filesystem.FS]

	// pendingRemoves buffers REMOVE_FILE for 1000ms; an ADD/EDIT/RENAME for the
	// same path within that window cancels it (git's atomic .lock->rename dance).
	pendingRemovesMu sync.Mutex
	pendingRemoves   map[string]*time.Timer
}

// FS returns the current FUSE tree. Always nil on Android.
func (kd *KeibidropServiceImpl) FS() *filesystem.FS { return kd.fs.Load() }

// SetFS publishes the FUSE tree. A no-op target on Android; kept for parity.
func (kd *KeibidropServiceImpl) SetFS(f *filesystem.FS) { kd.fs.Store(f) }

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("Debug called. Success!")
	return &bindings.DebugResponse{}, nil
}

func (kd *KeibidropServiceImpl) Notify(_ context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	logger := kd.Logger.With("method", "notify", "req-type", req.Type)

	if req.Type == bindings.NotifyType_DISCONNECT {
		logger.Info("Peer requested graceful disconnect")
		kd.cancelAllPendingRemoves()
		if kd.OnEvent != nil {
			kd.OnEvent("peer_disconnected:")
		}
		if kd.OnDisconnect != nil {
			go kd.OnDisconnect()
		}
		return &bindings.NotifyResponse{Status: "ok"}, nil
	}

	// Read-only share: refuse every peer mutation before it reaches the tracker.
	if kd.ShareReadOnly {
		if kd.readOnlyRefusalLogged.CompareAndSwap(false, true) {
			logger.Warn("Refusing peer mutation: share is read-only", "first-path", req.Path)
		}
		return nil, ErrGRPCReadOnlyShare
	}
	// Reject a peer path that could escape the save root, before it reaches the tracker.
	if isUnsafeNotify(req.Type, req.Path, req.OldPath) {
		logger.Warn("Refusing peer path outside the save root", "path", req.Path, "oldPath", req.OldPath)
		return nil, ErrGRPCUnsafePath
	}

	if kd.SyncTracker == nil {
		logger.Warn("SyncTracker not initialized")
		return nil, ErrGRPCNotMounted
	}

	switch req.Type {
	case bindings.NotifyType_UNKNOWN:
		logger.Warn("Unknown notification")
		return nil, ErrGRPCInvalidArgument

	case bindings.NotifyType_ADD_DIR, bindings.NotifyType_EDIT_DIR,
		bindings.NotifyType_REMOVE_DIR, bindings.NotifyType_RENAME_DIR:
		// Directories are not tracked in SyncTracker mode.
		logger.Info("Directory notification ignored in SyncTracker mode", "type", req.Type)

	case bindings.NotifyType_ADD_FILE:
		kd.CancelPendingRemove(req.Path)
		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCInvalidArgument
		}

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
		logger.Info("Success")

	case bindings.NotifyType_EDIT_FILE:
		kd.CancelPendingRemove(req.Path)
		if req.Attr == nil {
			logger.Error("Failed to edit file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCFailedPrecondition
		}

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
		logger.Info("Success")

	case bindings.NotifyType_REMOVE_FILE:
		// Buffer the remove; a quick ADD/RENAME for the same path cancels it.
		logger.Info("Buffering remove (1000ms)", "path", req.Path)
		kd.bufferRemove(req.Path, logger)

	case bindings.NotifyType_RENAME_FILE:
		kd.CancelPendingRemove(req.Path)
		kd.CancelPendingRemove(req.OldPath)
		logger.Info("Rename file", "oldPath", req.OldPath, "newPath", req.Path)
		kd.SyncTracker.RemoteFilesMu.Lock()
		if f, ok := kd.SyncTracker.RemoteFiles[req.OldPath]; ok {
			delete(kd.SyncTracker.RemoteFiles, req.OldPath)
			f.RelativePath = req.Path
			f.Name = filepath.Base(req.Path)
			if req.Attr != nil && req.Attr.Size > 0 {
				f.Size = uint64(req.Attr.Size)
			}
			kd.SyncTracker.RemoteFiles[req.Path] = f
		} else if req.Attr != nil && req.Attr.Size > 0 {
			kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
				Name:         filepath.Base(req.Path),
				RelativePath: req.Path,
				Size:         uint64(req.Attr.Size),
				LastEditTime: req.Attr.ModificationTime,
			}
			logger.Info("Created file from RENAME (old path not tracked)", "path", req.Path, "size", req.Attr.Size)
		}
		kd.SyncTracker.RemoteFilesMu.Unlock()
		logger.Info("Renamed file in sync tracker", "oldPath", req.OldPath, "newPath", req.Path)
	}

	logger.Info("Success")
	return &bindings.NotifyResponse{}, nil
}

// bufferRemove delays a REMOVE_FILE by 1000ms; CancelPendingRemove for the same
// path before it fires discards it.
func (kd *KeibidropServiceImpl) bufferRemove(path string, logger *slog.Logger) {
	kd.pendingRemovesMu.Lock()
	defer kd.pendingRemovesMu.Unlock()

	if kd.pendingRemoves == nil {
		kd.pendingRemoves = make(map[string]*time.Timer)
	}
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

// CancelPendingRemove discards a buffered REMOVE when an ADD/EDIT/RENAME for the
// same path arrives (the remove was part of git's atomic .lock->rename dance).
func (kd *KeibidropServiceImpl) CancelPendingRemove(path string) {
	kd.pendingRemovesMu.Lock()
	defer kd.pendingRemovesMu.Unlock()
	if t, ok := kd.pendingRemoves[path]; ok {
		t.Stop()
		delete(kd.pendingRemoves, path)
		kd.Logger.Info("Cancelled buffered remove (ADD/RENAME arrived)", "path", path)
	}
}

// cancelAllPendingRemoves stops every buffered remove (on disconnect).
func (kd *KeibidropServiceImpl) cancelAllPendingRemoves() {
	kd.pendingRemovesMu.Lock()
	defer kd.pendingRemovesMu.Unlock()
	for _, t := range kd.pendingRemoves {
		t.Stop()
	}
	kd.pendingRemoves = nil
}

// executeRemove drops the file from the sync tracker (android has no FUSE).
func (kd *KeibidropServiceImpl) executeRemove(path string, logger *slog.Logger) {
	if kd.SyncTracker == nil {
		return
	}
	kd.SyncTracker.RemoteFilesMu.Lock()
	delete(kd.SyncTracker.RemoteFiles, path)
	kd.SyncTracker.RemoteFilesMu.Unlock()
	logger.Info("Removed file from sync tracker (buffered)", "path", path)
}

func (kd *KeibidropServiceImpl) BatchNotify(ctx context.Context, req *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
	logger := kd.Logger.With("method", "batch-notify", "count", len(req.Notifications), "seq", req.Seq)
	logger.Info("Processing batch")

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

func (kd *KeibidropServiceImpl) Read(stream bindings.KeibiService_ReadServer) error {
	logger := kd.Logger.With("method", "server-read")

	if kd.SyncTracker == nil {
		logger.Error("SyncTracker not initialized")
		return ErrGRPCFailedPrecondition
	}

	var fh *os.File
	var openedPath string
	// Allocated on the first request. A stream that never reads costs no buffer.
	var buf []byte

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

		if fh == nil {
			lookupPath := strings.TrimPrefix(rec.Path, "/")
			kd.SyncTracker.LocalFilesMu.RLock()
			f, ok := kd.SyncTracker.LocalFiles[lookupPath]
			if !ok {
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

			fh, err = kd.openServeRead(realPath)
			if err != nil {
				logger.Error("Failed to open real file", "error", err)
				return status.Error(codes.Internal, "error accessing file")
			}
			openedPath = rec.Path
		} else if openedPath != rec.Path {
			fh.Close()
			logger.Error("Multiple paths in same stream not supported", "requested", rec.Path)
			return status.Error(codes.InvalidArgument, "stream can only read a single file")
		}

		if buf == nil {
			buf = make([]byte, config.GRPCStreamBuffer)
		}
		requested := int(rec.Size)
		size := clampReadSize(requested, len(buf))
		offset, offsetOK := safeReadOffset(rec.Offset)
		if !offsetOK {
			fh.Close()
			logger.Warn("Peer requested an out-of-range read offset", "offset", rec.Offset)
			return status.Error(codes.InvalidArgument, "read offset out of range")
		}
		if size != requested {
			logger.Warn("Requested size out of range, clamped to buffer", "requested", requested, "buffer", len(buf))
		}

		n, err := fh.ReadAt(buf[:size], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			fh.Close()
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

// readBatchMaxPaths bounds one ReadBatch request.
const readBatchMaxPaths = 512

// readBatchFrameSize is the payload cap per ReadBatch frame.
const readBatchFrameSize = 512 * 1024

// ReadBatch streams a set of files in requested order. Android serves from
// the SyncTracker only. The budget is soft: the current file finishes, no
// new file starts past it. A per-file failure sends an err_code frame and
// the batch continues.
func (kd *KeibidropServiceImpl) ReadBatch(req *bindings.ReadBatchRequest, stream bindings.KeibiService_ReadBatchServer) error {
	logger := kd.Logger.With("method", "read-batch", "files", len(req.Paths))
	if kd.SyncTracker == nil {
		return ErrGRPCFailedPrecondition
	}
	if len(req.Paths) == 0 {
		return nil
	}
	if len(req.Paths) > readBatchMaxPaths {
		return status.Error(codes.InvalidArgument, "too many paths in one batch")
	}
	budget := req.MaxBytes
	if budget == 0 || budget > config.GRPCStreamBuffer {
		budget = config.GRPCStreamBuffer
	}
	buf := make([]byte, readBatchFrameSize)
	var sent uint64
	served := 0
	for _, p := range req.Paths {
		if sent >= budget {
			break
		}
		lookupPath := strings.TrimPrefix(p, "/")
		kd.SyncTracker.LocalFilesMu.RLock()
		f, ok := kd.SyncTracker.LocalFiles[lookupPath]
		if !ok {
			f, ok = kd.SyncTracker.LocalFiles[p]
		}
		var realPath string
		if ok {
			realPath = f.RealPathOfFile
		}
		kd.SyncTracker.LocalFilesMu.RUnlock()
		if realPath == "" {
			if err := stream.Send(&bindings.ReadBatchResponse{Path: p, ErrCode: 1, FileDone: true}); err != nil {
				return err
			}
			continue
		}
		if err := kd.streamBatchFile(stream, p, realPath, buf, &sent); err != nil {
			return err
		}
		served++
	}
	logger.Info("Batch served", "served", served, "bytes", sent)
	return nil
}

// streamBatchFile sends one file as frames. A read failure sends an err_code
// frame and returns nil so the batch continues; a Send failure returns the
// error and kills the batch.
func (kd *KeibidropServiceImpl) streamBatchFile(stream bindings.KeibiService_ReadBatchServer, path, realPath string, buf []byte, sent *uint64) error {
	fh, err := kd.openServeRead(realPath)
	if err != nil {
		return stream.Send(&bindings.ReadBatchResponse{Path: path, ErrCode: 2, FileDone: true})
	}
	defer fh.Close()
	finfo, err := fh.Stat()
	if err != nil {
		return stream.Send(&bindings.ReadBatchResponse{Path: path, ErrCode: 2, FileDone: true})
	}
	total := uint64(finfo.Size())
	if total == 0 {
		return stream.Send(&bindings.ReadBatchResponse{Path: path, FileDone: true})
	}
	var offset uint64
	for offset < total {
		want := min(total-offset, uint64(len(buf)))
		n, rerr := fh.ReadAt(buf[:want], int64(offset))
		if n > 0 {
			end := offset + uint64(n)
			if serr := stream.Send(&bindings.ReadBatchResponse{
				Path:      path,
				Offset:    offset,
				Data:      buf[:n],
				TotalSize: total,
				FileDone:  end >= total,
			}); serr != nil {
				return serr
			}
			offset = end
			*sent += uint64(n)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) && offset >= total {
				return nil
			}
			return stream.Send(&bindings.ReadBatchResponse{Path: path, ErrCode: 2, FileDone: true})
		}
	}
	return nil
}

// StreamFile pushes an entire file's contents to the client in sequential
// chunks. The client sends one request; the server streams all data from
// start_offset to EOF with no per-chunk round-trip overhead.
func (kd *KeibidropServiceImpl) StreamFile(req *bindings.StreamFileRequest, stream bindings.KeibiService_StreamFileServer) error {
	logger := kd.Logger.With("method", "stream-file", "path", req.Path)

	if kd.SyncTracker == nil {
		logger.Warn("SyncTracker not initialized")
		return status.Error(codes.FailedPrecondition, "sync tracker not initialized")
	}

	lookupPath := strings.TrimPrefix(req.Path, "/")
	kd.SyncTracker.LocalFilesMu.RLock()
	f, ok := kd.SyncTracker.LocalFiles[lookupPath]
	if !ok {
		f, ok = kd.SyncTracker.LocalFiles[req.Path]
	}
	kd.SyncTracker.LocalFilesMu.RUnlock()

	if !ok {
		logger.Warn("File not found", "path", req.Path)
		return status.Error(codes.NotFound, "file not found")
	}

	fh, err := kd.openServeRead(f.RealPathOfFile)
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

	// StreamFile sends at most BlockSize per frame; a larger buffer is waste.
	buf := make([]byte, config.BlockSize)
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
			logger.Info("StreamFile send ended", "offset", offset, "error", sendErr)
			return sendErr
		}

		offset += uint64(n)
	}

	logger.Info("StreamFile complete", "bytesSent", offset-req.StartOffset)
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
func (kd *KeibidropServiceImpl) Heartbeat(_ context.Context, req *bindings.HeartbeatRequest) (*bindings.HeartbeatResponse, error) {
	return &bindings.HeartbeatResponse{
		Timestamp:    uint64(time.Now().UnixNano()),
		ReqTimestamp: req.Timestamp,
		Seq:          req.Seq,
	}, nil
}
