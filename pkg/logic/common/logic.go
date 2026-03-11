// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

	f, err := os.Create(localPath)
	if err != nil {
		logger.Error("Failed to create local file", "error", err)
		return err
	}

	kd.SyncTracker.RemoteFilesMu.Lock()
	remFile, ok := kd.SyncTracker.RemoteFiles[remoteName]
	defer kd.SyncTracker.RemoteFilesMu.Unlock()
	if !ok {
		logger.Error("Not found", "error", syscall.ENOENT)
		return syscall.ENOENT
	}

	stream, err := kd.session.GRPCClient.Read(kd.ctx)
	if err != nil {
		logger.Error("Failed to open stream", "error", err)
		return err
	}

	blocks := remFile.Size / config.BlockSize
	leftoverBlockSize := remFile.Size % config.BlockSize

	i := 0
	for {
		offset := i * config.BlockSize
		size := uint32(config.BlockSize)
		if i >= int(blocks) {
			if leftoverBlockSize != 0 {
				size = uint32(leftoverBlockSize)
			} else {
				break
			}
		}
		err := stream.Send(&bindings.ReadRequest{
			Handle: 0,
			Path:   remFile.RelativePath,
			Offset: uint64(offset),
			Size:   size,
		})
		if err != nil {
			logger.Error("Failed to send stream", "error", err)
			return err
		}

		data, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			logger.Error("Failed to receive data", "error", err)
			return err
		}

		n, err := f.WriteAt(data.Data, int64(offset))
		if err != nil {
			logger.Error("Failed to write data to disk", "error", err)

			return err
		}

		if n != int(size) {
			logger.Warn("Wrote less than the requested size")
			break
		}

		i++
	}

	remFile.RealPathOfFile = localPath
	kd.SyncTracker.LocalFilesMu.Lock()
	kd.SyncTracker.LocalFiles[localPath] = remFile
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
	if kd.running {
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
	if kd.running {
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
