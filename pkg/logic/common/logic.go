// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

const Timeout = 10*60 - 5

// Add a file to be tracked.
func (kd *KeibiDrop) AddFile(path string) error {
	logger := kd.logger.With("method", "add-file")
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Error("Invalid session", "error", ErrInvalidSession)
		return ErrInvalidSession
	}

	cleanPath := filepath.Clean(path)

	finfo, err := os.Stat(cleanPath)
	if err != nil {
		logger.Error("Failed to add file", "error", err)
		return err
	}

	name := finfo.Name()

	if finfo.IsDir() {
		logger.Warn("File is a directory", "error", syscall.EISDIR)
		return syscall.EISDIR
	}

	file := &synctracker.File{
		Name:           name,
		RelativePath:   name,
		RealPathOfFile: cleanPath,
		Size:           uint64(finfo.Size()),
		LastEditTime:   uint64(finfo.ModTime().UnixNano()),
		CreatedTime:    uint64(finfo.ModTime().UnixNano()),
	}

	kd.SyncTracker.LocalFilesMu.Lock()
	defer kd.SyncTracker.LocalFilesMu.Unlock()
	_, ok := kd.SyncTracker.LocalFiles[name]
	if ok {
		logger.Error("File already tracked", "name", name, "error", os.ErrExist)
		return os.ErrExist
	}
	kd.SyncTracker.LocalFiles[name] = file

	_, err = kd.session.GRPCClient.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.AddFile),
		Path: file.RelativePath,
		Attr: &bindings.Attr{
			Dev:              0,
			Ino:              0,
			Mode:             uint32(finfo.Mode().Perm()) | syscall.S_IFREG,
			Size:             finfo.Size(),
			AccessTime:       file.LastEditTime,
			ModificationTime: file.LastEditTime,
			ChangeTime:       file.LastEditTime,
			BirthTime:        file.LastEditTime,
			Flags:            0o444,
		},
	})
	if err != nil {
		logger.Error("Failed to notify peer", "error", err)
		return err
	}

	logger.Info("Success")

	return nil
}

func (kd *KeibiDrop) ListFiles() (remote []string, local []string) {
	remote = []string{}
	local = []string{}

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()

	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()

	for k, v := range kd.SyncTracker.LocalFiles {
		local = append(local, fmt.Sprintf("[Local] Path: %v Size: %v RealPath: %v\n", k, v.Size, v.RealPathOfFile))
	}

	for k, v := range kd.SyncTracker.RemoteFiles {
		remote = append(remote, fmt.Sprintf("[Remote] Path: %v Size: %v RealPath: %v\n", k, v.Size, v.RealPathOfFile))
	}

	return remote, local
}

func (kd *KeibiDrop) PullFile(remoteName, localPath string) error {
	logger := kd.logger.With("method", "pull-file")
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Error("Invalid session", "error", ErrInvalidSession)
		return ErrInvalidSession
	}

	localPath = filepath.Clean(localPath)

	// Snapshot remote file metadata under a short read lock.
	// Copy the struct so we don't alias the map entry pointer.
	kd.SyncTracker.RemoteFilesMu.RLock()
	remFilePtr, ok := kd.SyncTracker.RemoteFiles[remoteName]
	var fileCopy synctracker.File
	if ok {
		fileCopy = *remFilePtr
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	if !ok {
		logger.Error("Not found", "error", syscall.ENOENT)
		return syscall.ENOENT
	}

	fileSize := fileCopy.Size
	relPath := fileCopy.RelativePath

	// Ensure parent directories exist (for files in subdirectories).
	if dir := filepath.Dir(localPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Error("Failed to create parent directories", "error", err)
			return err
		}
	}

	f, err := os.Create(localPath)
	if err != nil {
		logger.Error("Failed to create local file", "error", err)
		return err
	}
	defer f.Close()

	// Pre-allocate the file to enable correct out-of-order pwrite.
	if fileSize > 0 {
		if err := f.Truncate(int64(fileSize)); err != nil {
			logger.Error("Failed to pre-allocate file", "error", err)
			os.Remove(localPath)
			return err
		}
	}

	totalChunks := int((fileSize + uint64(config.BlockSize) - 1) / uint64(config.BlockSize))

	if totalChunks > 0 {
		// Parallel download with N gRPC streams (chunk-index sharding).
		nWorkers := filesystem.StreamPoolSize
		if totalChunks < nWorkers {
			nWorkers = totalChunks
		}

		// Child context so we can cancel remaining workers on first error.
		dlCtx, dlCancel := context.WithCancel(kd.ctx)
		defer dlCancel()

		var wg sync.WaitGroup
		errCh := make(chan error, nWorkers)

		for w := 0; w < nWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				stream, err := kd.session.GRPCClient.Read(dlCtx)
				if err != nil {
					errCh <- fmt.Errorf("worker %d: open stream: %w", workerID, err)
					dlCancel()
					return
				}
				defer stream.CloseSend()

				for i := workerID; i < totalChunks; i += nWorkers {
					offset := uint64(i) * uint64(config.BlockSize)
					size := uint64(config.BlockSize)
					if offset+size > fileSize {
						size = fileSize - offset
					}

					if err := stream.Send(&bindings.ReadRequest{
						Handle: 0,
						Path:   relPath,
						Offset: offset,
						Size:   uint32(size),
					}); err != nil {
						errCh <- fmt.Errorf("worker %d: send chunk %d: %w", workerID, i, err)
						dlCancel()
						return
					}

					data, err := stream.Recv()
					if err != nil {
						errCh <- fmt.Errorf("worker %d: recv chunk %d: %w", workerID, i, err)
						dlCancel()
						return
					}

					if uint64(len(data.Data)) != size {
						errCh <- fmt.Errorf("worker %d: chunk %d: got %d bytes, expected %d", workerID, i, len(data.Data), size)
						dlCancel()
						return
					}

					if _, err := f.WriteAt(data.Data, int64(offset)); err != nil {
						errCh <- fmt.Errorf("worker %d: write chunk %d: %w", workerID, i, err)
						dlCancel()
						return
					}
				}
			}(w)
		}

		wg.Wait()
		close(errCh)
		if err := <-errCh; err != nil {
			logger.Error("Parallel download failed", "error", err)
			os.Remove(localPath)
			return err
		}
	}

	// Update tracker under short locks.
	fileCopy.RealPathOfFile = localPath

	kd.SyncTracker.RemoteFilesMu.Lock()
	if rf, ok := kd.SyncTracker.RemoteFiles[remoteName]; ok {
		rf.RealPathOfFile = localPath
	}
	kd.SyncTracker.RemoteFilesMu.Unlock()

	kd.SyncTracker.LocalFilesMu.Lock()
	kd.SyncTracker.LocalFiles[localPath] = &fileCopy
	kd.SyncTracker.LocalFilesMu.Unlock()

	logger.Info("Success")

	return nil
}

func (kd *KeibiDrop) ExportFingerprint() (string, error) {
	logger := kd.logger.With("method", "export-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	fp := kd.session.OwnFingerprint

	logger.Info("Success", "fingerprint", fp)

	return fp, nil
}

func (kd *KeibiDrop) AddPeerFingerprint(fp string) error {
	logger := kd.logger.With("method", "add-peer-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	fp = strings.TrimSpace(fp)

	err := ValidateFingerprint(fp)
	if err != nil {
		logger.Error("Failed to validate fingerprint", "error", err)
		return err
	}

	kd.session.ExpectedPeerFingerprint = fp

	return nil
}

func (kd *KeibiDrop) GetPeerFingerprint() (string, error) {
	logger := kd.logger.With("method", "get-peer-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	return kd.session.ExpectedPeerFingerprint, nil
}

func (kd *KeibiDrop) JoinRoom() error {
	logger := kd.logger.With("method", "join-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	if kd.running.Load() {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	// Wait for expected peer fingerprint
	elapsed := 0
	for {
		if elapsed >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			elapsed++
			time.Sleep(time.Second)
			continue
		}
		break
	}

	if err := kd.getRoomFromRelay(kd.session.ExpectedPeerFingerprint); err != nil {
		return err
	}

	if err := session.PerformOutboundHandshake(kd.session, net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(kd.session.PeerPort))); err != nil {
		logger.Error("Failed outbound handshake", "error", err)
		return err
	}

	conn, err := kd.listener.Accept()
	if err != nil {
		logger.Error("Failed to accept", "error", err)
		return err
	}

	if err := session.PerformInboundHandshake(kd.session, conn); err != nil {
		logger.Error("Failed inbound handshake", "error", err)
		return err
	}

	kd.filesystemReady = make(chan struct{})
	kd.filesystemReadyOnce = sync.Once{}
	kd.Start()

	// retry dialing until gRPC server is ready
	if err := kd.connectGRPCClientWithRetry(15 * time.Second); err != nil {
		logger.Error("Failed to connect to grpc server after retries", "error", err)
		return err
	}

	// Start health monitoring, reconnection, and relay keepalive.
	if err := kd.InitConnectionResilience(); err != nil {
		logger.Warn("Failed to init connection resilience", "error", err)
	}

	if !kd.IsFUSE {
		// Unblock Run()'s <-filesystemReady so it can process signals.
		kd.filesystemReadyOnce.Do(func() { close(kd.filesystemReady) })
		logger.Info("Success, starting without FUSE")
		return nil
	}

	err = kd.setupFilesystem(logger, kd.filesystemReady)
	if err != nil {
		return err
	}

	logger.Info("Success")
	return nil
}

func (kd *KeibiDrop) CreateRoom() error {
	logger := kd.logger.With("method", "create-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	if kd.running.Load() {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	if err := kd.registerRoomToRelay(); err != nil {
		return err
	}

	logger.Info("Waiting for peer to join...")

	// Wait for expected peer fingerprint
	elapsed := 0
	for {
		if elapsed >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			time.Sleep(time.Second)
			elapsed++
			continue
		}
		break
	}

	conn, err := kd.listener.Accept()
	if err != nil {
		logger.Error("Failed to accept", "error", err)
		return err
	}

	if err := session.PerformInboundHandshake(kd.session, conn); err != nil {
		return err
	}

	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		logger.Error("Failed to cast TCP address", "error", err)
		return err
	}

	kd.PeerIPv6IP = addr.IP.String()

	if err := session.PerformOutboundHandshake(kd.session, net.JoinHostPort(addr.IP.String(), strconv.Itoa(kd.session.PeerPort))); err != nil {
		return err
	}

	kd.filesystemReady = make(chan struct{})
	kd.filesystemReadyOnce = sync.Once{}
	kd.Start()

	if err := kd.connectGRPCClientWithRetry(15 * time.Second); err != nil {
		logger.Error("Failed to connect to grpc server after retries", "error", err)
		return err
	}

	// Start health monitoring, reconnection, and relay keepalive.
	if err := kd.InitConnectionResilience(); err != nil {
		logger.Warn("Failed to init connection resilience", "error", err)
	}

	if !kd.IsFUSE {
		// Unblock Run()'s <-filesystemReady so it can process signals.
		kd.filesystemReadyOnce.Do(func() { close(kd.filesystemReady) })
		logger.Info("Success, starting without FUSE")
		return nil
	}

	err = kd.setupFilesystem(logger, kd.filesystemReady)
	if err != nil {
		return err
	}

	logger.Info("Success")
	return nil
}

// NotifyDisconnect sends a best-effort DISCONNECT notification to the peer
// so they can clean up immediately instead of waiting for health monitor timeout.
func (kd *KeibiDrop) NotifyDisconnect() {
	logger := kd.logger.With("method", "notify-disconnect")
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Warn("Skipping disconnect notification: no session or gRPC client")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := kd.session.GRPCClient.Notify(ctx, &bindings.NotifyRequest{
		Type: bindings.NotifyType_DISCONNECT,
	})
	if err != nil {
		logger.Warn("Failed to send disconnect notification", "error", err)
	} else {
		logger.Info("Disconnect notification sent successfully")
	}
}

// This is blocking.
func (kd *KeibiDrop) MountFilesystem(toMount string, toSave string, isSecond bool) error {
	logger := kd.logger.With("method", "mount-filesystem")
	logger.Info("Mounting virtual filesystem", "virtual-folder", toMount, "passhtrough-folder", toSave, "isSecond", isSecond)
	if kd.session == nil || kd.KDSvc == nil {
		logger.Warn("Session not established", "error", ErrSessionNotEstablished)
		return ErrSessionNotEstablished
	}

	if kd.FS != nil {
		logger.Warn("Filesystem already mounted", "error", ErrFilesystemAlreadyMounted)
		return ErrFilesystemAlreadyMounted
	}

	fs := filesystem.NewFS(logger)
	kd.KDSvc.FS = fs

	fs.Mount(filepath.Clean(toMount), isSecond, filepath.Clean(toSave))

	return nil
}

func (kd *KeibiDrop) UnmountFilesystem() error {
	logger := kd.logger.With("method", "unmonut-filesystem")
	logger.Info("Unmounting virtual filesystem")
	if kd.FS == nil {
		logger.Warn("Nil filesystem", "error", ErrNilFilesystem)
		return ErrNilFilesystem
	}

	if kd.KDSvc != nil && kd.KDSvc.FS != nil {
		kd.KDSvc.FS = nil
	}

	kd.FS.Unmount()
	kd.FS = nil

	logger.Info("Success")
	return nil
}

/*
	func (kd *KeibiDrop) ResetSession() {
		kd.logger.Info("Resetting session state")

		// You probably want to close any existing net.Conn, etc.
	}

	func (kd *KeibiDrop) RegenerateKeys() error {
		kd.logger.Info("Regenerating ephemeral keys")

		return nil
	}
*/
