// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
//
// Portions of this file are derived from the cgofuse project,
// which is licensed under the MIT License.
// Copyright (c) 2018–2023, Bill Zissimopoulos and cgofuse contributors.
// See https://github.com/billziss-gh/cgofuse for details.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	keibidrop "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/pkg/xattr"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// recoverPanic recovers from panics in FUSE handlers and logs the error.
// Pass a pointer to the error return value to set it to EIO on panic.
func (d *Dir) recoverPanic(funcName string, errCode *int) {
	if r := recover(); r != nil {
		d.logger.Error("PANIC in FUSE handler", "function", funcName, "error", r)
		if errCode != nil {
			*errCode = -winfuse.EIO
		}
	}
}

// Info about methods:
// https://pkg.go.dev/github.com/winfsp/cgofuse/fuse#FileSystemInterface

func (d *Dir) Access(path string, _mask uint32) (errCode int) {
	defer d.recoverPanic("Access", &errCode)
	logger := d.logger.With("method", "access", "path", path, "mask", _mask)
	logger.Debug("Access called")

	d.RemoteFilesLock.RLock()
	_, ok := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()
	if ok {
		logger.Debug("Access OK (remote file)")
		return 0
	}

	realPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in Access", "path", path, "error", err)
		return -int(syscall.EACCES)
	}

	stat := &syscall.Stat_t{}
	err = syscall.Stat(realPath, stat)
	if err != nil {
		logger.Warn("Access FAILED", "error", err, "realPath", realPath)
		return int(convertOsErrToSyscallErrno("stat", err))
	}

	logger.Debug("Access OK (local file)")
	return 0
}

func (d *Dir) Chmod(path string, mode uint32) (errCode int) {
	defer d.recoverPanic("Chmod", &errCode)
	// Return success. But we do not implement it.
	d.logger.Info("Chmod", "path", path)
	return 0
	// return -winfuse.ENOSYS
}

func (d *Dir) Chown(path string, uid uint32, gid uint32) (errCode int) {
	defer d.recoverPanic("Chown", &errCode)
	d.logger.Info("Chown", "path", path)
	// Return success but we do not implement it.
	return 0
	// return -winfuse.ENOSYS
}

// Create creates a new file.
//
// From open(2) man page on Intel macOS:
//
//	"The flags specified for the oflag argument must include exactly one of
//	 the following file access modes:
//	   O_RDONLY    open for reading only
//	   O_WRONLY    open for writing only
//	   O_RDWR      open for reading and writing
//
//	 In addition any combination of the following values can be or'ed in oflag:
//	   O_APPEND    append on each write
//	   O_CREAT     create file if it does not exist
//	   O_TRUNC     truncate size to 0
//	   O_EXCL      error if O_CREAT and the file exists"
//
// Use winfuse.O_ACCMODE to extract access mode (portable across macOS/Linux/Windows).
func (d *Dir) Create(path string, flags int, mode uint32) (errCode int, fh uint64) {
	defer d.recoverPanic("Create", &errCode)
	logger := d.logger.With("method", "create", "path", path)
	accessMode := flags & winfuse.O_ACCMODE
	logger.Info("Create called", "flags", flags, "accessMode", accessMode, "mode", mode, "isRDWR", accessMode == winfuse.O_RDWR)

	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()
	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	relativePath := path
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in Create", "path", path, "error", err)
		return -int(syscall.EACCES), 0
	}

	// Extract permission bits only (mode may include S_IFREG file type bits).
	// Ensure owner has write permission so file can be reopened after close.
	createMode := mode & 0o777
	if createMode&0o200 == 0 {
		createMode |= 0o200 // Add owner write permission
	}
	if createMode == 0 {
		createMode = 0o644 // Default if mode was 0
	}

	fd, err := syscall.Open(cleanPath, flags, createMode)
	if err != nil {
		logger.Error("Failed to create file", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	name := strings.Split(cleanPath, "/")

	f := &File{
		logger:          logger,
		openFileCounter: OpenFileCounter{mu: &sync.Mutex{}, counter: 1},
		Inode:           uint64(fd),
		Name:            name[len(name)-1],
		RelativePath:    relativePath,
		RealPathOfFile:  cleanPath,
		OnLocalChange:   d.OnLocalChange,
		StreamProvider:  d.OpenStreamProvider(),
		NotRemoteSynced: true,
		IsLocalPresent:  true, // File was just created locally
		LocalNewer:      true, // Local version is the only version
	}

	d.AllFileMap[relativePath] = f
	d.OpenFileHandlers[uint64(fd)] = f

	logger.Info("Created file", "fd", fd)
	return 0, uint64(fd)
}

// shouldUseDirectIo determines if a file should bypass kernel page cache.
// Returns true for files that need real-time sync (write access, not in .git/).
// Returns false for .git/ files (to allow mmap for git operations).
// Returns false for mmap-dependent files (PDF, images) that apps like Preview need.
func shouldUseDirectIo(path string, flags int) bool {
	// .git/ files: allow page cache for mmap (git uses mmap for pack files)
	if strings.Contains(path, "/.git/") || strings.HasPrefix(path, ".git/") {
		return false
	}

	// PDF, images, and videos need mmap for Preview.app/QuickTime
	// These apps open files with O_RDWR but need mmap to read content.
	lowerPath := strings.ToLower(path)
	mmapExtensions := []string{".pdf", ".jpg", ".jpeg", ".png", ".gif", ".tiff", ".heic", ".webp", ".mov", ".mp4"}
	for _, ext := range mmapExtensions {
		if strings.HasSuffix(lowerPath, ext) {
			return false
		}
	}

	// Write access: use direct_io for real-time sync
	accessMode := flags & winfuse.O_ACCMODE
	if accessMode == winfuse.O_WRONLY || accessMode == winfuse.O_RDWR {
		return true
	}

	// Read-only: allow page cache
	return false
}

// CreateEx implements FileSystemOpenEx interface for per-file direct_io control.
func (d *Dir) CreateEx(path string, mode uint32, fi *winfuse.FileInfo_t) (errCode int) {
	defer d.recoverPanic("CreateEx", &errCode)
	logger := d.logger.With("method", "create-ex", "path", path)

	flags := fi.Flags
	accessMode := flags & winfuse.O_ACCMODE
	logger.Info("CreateEx called", "flags", flags, "accessMode", accessMode, "mode", mode)

	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()
	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	relativePath := path
	localPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in CreateEx", "path", path, "error", err)
		return -int(syscall.EACCES)
	}

	// Extract permission bits only (mode may include S_IFREG file type bits).
	// Ensure owner has write permission so file can be reopened after close.
	// Without this, files created with mode 0444 (read-only) cannot be reopened for write.
	createMode := mode & 0o777
	if createMode&0o200 == 0 {
		createMode |= 0o200 // Add owner write permission
	}
	if createMode == 0 {
		createMode = 0o644 // Default if mode was 0
	}

	fd, err := syscall.Open(localPath, flags, createMode)
	if err != nil {
		logger.Error("Failed to create file", "error", err)
		return int(convertOsErrToSyscallErrno("open", err))
	}

	name := strings.Split(localPath, "/")

	f := &File{
		logger:          logger,
		openFileCounter: OpenFileCounter{mu: &sync.Mutex{}, counter: 1},
		Inode:           uint64(fd),
		Name:            name[len(name)-1],
		RelativePath:    relativePath,
		RealPathOfFile:  localPath,
		OnLocalChange:   d.OnLocalChange,
		StreamProvider:  d.OpenStreamProvider(),
		NotRemoteSynced: true,
		IsLocalPresent:  true,
		LocalNewer:      true,
	}

	d.AllFileMap[relativePath] = f
	d.OpenFileHandlers[uint64(fd)] = f

	// Set per-file direct_io
	fi.Fh = uint64(fd)
	fi.DirectIo = shouldUseDirectIo(path, flags)
	logger.Info("Created file", "fd", fd, "directIo", fi.DirectIo)
	return 0
}

// OpenEx implements FileSystemOpenEx interface for per-file direct_io control.
func (d *Dir) OpenEx(path string, fi *winfuse.FileInfo_t) (errCode int) {
	defer d.recoverPanic("OpenEx", &errCode)
	flags := fi.Flags
	logger := d.logger.With("method", "open-ex", "path", path, "flags", flags)

	localPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in OpenEx", "path", path, "error", err)
		return -int(syscall.EACCES)
	}

	// Check if O_CREAT is in flags - if so, we might need to create the file
	hasCreate := flags&syscall.O_CREAT != 0

	d.AfmLock.Lock()
	fh, ok := d.AllFileMap[path]
	if !ok {
		// File not in map - check if it exists on disk
		fileExists := false
		if _, statErr := os.Stat(localPath); statErr == nil {
			fileExists = true
		}

		// Check if this is a remote file (don't mark as LocalNewer if so)
		d.RemoteFilesLock.RLock()
		_, isRemoteFile := d.RemoteFiles[path]
		d.RemoteFilesLock.RUnlock()

		if fileExists || hasCreate {
			fh = &File{
				logger:          d.logger,
				openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
				Name:            getNameFromPath(path),
				RelativePath:    path,
				RealPathOfFile:  localPath,
				IsLocalPresent:  fileExists,
				LocalNewer:      !isRemoteFile, // Remote files should not be marked as local newer
				OnLocalChange:   d.OnLocalChange,
				StreamProvider:  d.OpenStreamProvider(),
				stat:            &winfuse.Stat_t{},
			}
			d.AllFileMap[path] = fh
		} else {
			d.AfmLock.Unlock()
			logger.Debug("File not found", "localPath", localPath)
			return -winfuse.ENOENT
		}
	}

	// File already opened - return existing handle
	if fh.openFileCounter.CountOpenDescriptors() != 0 {
		inode := fh.Inode
		d.AfmLock.Unlock()
		fi.Fh = inode
		fi.DirectIo = shouldUseDirectIo(path, flags)
		return 0
	}

	isLocalPresent := fh.IsLocalPresent
	localNewer := fh.LocalNewer
	d.AfmLock.Unlock()

	// Check if remote has newer version
	remoteHasUpdate := false
	var remoteTotalSize uint64
	needsRemark := false
	d.RemoteFilesLock.RLock()
	remoteFile, hasRemote := d.RemoteFiles[path]
	if hasRemote {
		logger.Info("=== REMOTE CHECK ===", "path", path, "hasRemote", hasRemote, "NotLocalSynced", remoteFile.NotLocalSynced)
		if remoteFile.stat != nil {
			remoteTotalSize = uint64(remoteFile.stat.Size)
		}
		if remoteFile.NotLocalSynced {
			logger.Info("Remote has newer version, streaming from remote", "path", path)
			localNewer = false
			remoteHasUpdate = true
		} else if remoteTotalSize > 0 {
			// Even if NotLocalSynced=false, verify download is actually complete.
			// IMPORTANT: Can't use file size because pre-allocation makes file look complete.
			// Use Download.BytesDownloaded which tracks actual bytes received.
			bytesDownloaded := remoteFile.Download.BytesDownloaded.Load()
			if bytesDownloaded < remoteTotalSize {
				logger.Info("Download incomplete, re-streaming from remote", "bytesDownloaded", bytesDownloaded, "remoteSize", remoteTotalSize)
				localNewer = false
				remoteHasUpdate = true
				needsRemark = true
			}
		}
	} else {
		logger.Info("=== REMOTE CHECK ===", "path", path, "hasRemote", false)
	}
	d.RemoteFilesLock.RUnlock()

	// Re-mark for streaming outside of read lock
	if needsRemark && hasRemote {
		d.RemoteFilesLock.Lock()
		if rf, ok := d.RemoteFiles[path]; ok {
			rf.NotLocalSynced = true
		}
		d.RemoteFilesLock.Unlock()
	}

	// Open locally if we have newer local version
	if isLocalPresent && localNewer {
		accessMode := flags & winfuse.O_ACCMODE
		logger.Info("Opening local file", "flags", flags, "accessMode", accessMode, "localPath", localPath)

		// Verify file actually exists before trying to open
		if _, statErr := os.Stat(localPath); statErr != nil {
			logger.Warn("File marked as local but doesn't exist on disk", "error", statErr, "localPath", localPath)
			// File doesn't exist - might need to create it
			// Fall through to remote/creation path by setting isLocalPresent=false
			isLocalPresent = false
		} else {
			// Convert FUSE flags to syscall-compatible flags for opening existing file
			var sysFlags int
			switch accessMode {
			case winfuse.O_RDONLY:
				sysFlags = syscall.O_RDONLY
			case winfuse.O_WRONLY:
				sysFlags = syscall.O_WRONLY
			case winfuse.O_RDWR:
				sysFlags = syscall.O_RDWR
			}
			// Add append flag if present
			if flags&syscall.O_APPEND != 0 {
				sysFlags |= syscall.O_APPEND
			}

			fd, err := syscall.Open(localPath, sysFlags, 0)
			if err != nil {
				logger.Error("Failed to open local file", "error", err, "sysFlags", sysFlags)
				return int(convertOsErrToSyscallErrno("open", err))
			}

			d.AfmLock.Lock()
			d.OpenMapLock.Lock()
			fh.Inode = uint64(fd)
			fh.openFileCounter.Open()
			d.OpenFileHandlers[fh.Inode] = fh
			d.OpenMapLock.Unlock()
			d.AfmLock.Unlock()

			fi.Fh = uint64(fd)
			fi.DirectIo = shouldUseDirectIo(path, flags)
			logger.Info("Opened local file", "fh", fd, "directIo", fi.DirectIo)
			return 0
		}
	}

	// Handle case where file needs to be created (O_CREAT or file doesn't exist)
	if hasCreate && !isLocalPresent {
		// Create parent directories if needed
		parentDir := filepath.Dir(localPath)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			logger.Error("Failed to create parent directories", "error", err)
			return -winfuse.EIO
		}

		// Determine access mode for creation
		accessMode := flags & winfuse.O_ACCMODE
		var sysFlags int
		switch accessMode {
		case winfuse.O_RDONLY:
			sysFlags = syscall.O_RDONLY | syscall.O_CREAT
		case winfuse.O_WRONLY:
			sysFlags = syscall.O_WRONLY | syscall.O_CREAT
		case winfuse.O_RDWR:
			sysFlags = syscall.O_RDWR | syscall.O_CREAT
		default:
			sysFlags = syscall.O_RDWR | syscall.O_CREAT
		}
		// Add truncate if present
		if flags&syscall.O_TRUNC != 0 {
			sysFlags |= syscall.O_TRUNC
		}

		fd, err := syscall.Open(localPath, sysFlags, 0644)
		if err != nil {
			logger.Error("Failed to create file via OpenEx", "error", err)
			return int(convertOsErrToSyscallErrno("open", err))
		}

		d.AfmLock.Lock()
		d.OpenMapLock.Lock()
		fh.Inode = uint64(fd)
		fh.openFileCounter.Open()
		fh.IsLocalPresent = true
		fh.NotRemoteSynced = true
		d.OpenFileHandlers[fh.Inode] = fh
		d.OpenMapLock.Unlock()
		d.AfmLock.Unlock()

		fi.Fh = uint64(fd)
		fi.DirectIo = shouldUseDirectIo(path, flags)
		logger.Info("Created file via OpenEx", "fh", fd, "directIo", fi.DirectIo)
		return 0
	}

	// If we get here with isLocalPresent=false and no hasCreate, this is a remote file
	if !isLocalPresent && !hasCreate {
		// Continue to remote file handling below
	} else if isLocalPresent && localNewer {
		// This case was already handled above, but guard against logic errors
		logger.Error("Logic error: isLocalPresent && localNewer should have been handled")
		return -winfuse.EIO
	}

	// Remote file - check for partial download or create cache file
	var existingLocalSize int64
	localStat, err := os.Stat(localPath)
	if err == nil {
		existingLocalSize = localStat.Size()
		logger.Info("Found existing partial download", "existingSize", existingLocalSize)
	}

	// Create parent directories
	parentDir := filepath.Dir(localPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		logger.Error("Failed to create parent directories", "error", err)
		return -winfuse.EIO
	}

	fd, err := syscall.Open(localPath, syscall.O_RDWR|syscall.O_CREAT, 0644)
	if err != nil {
		logger.Error("Failed to create cache file", "error", err)
		return int(convertOsErrToSyscallErrno("open", err))
	}

	// Open remote stream for reading (network call - no locks held)
	var stream types.RemoteFileStream
	var streamCancel context.CancelFunc
	if remoteHasUpdate {
		fsp := d.OpenStreamProvider()
		var streamCtx context.Context
		streamCtx, streamCancel = context.WithCancel(context.Background())
		stream, err = fsp.OpenRemoteFile(streamCtx, uint64(fd), path)
		if err != nil {
			streamCancel()
			syscall.Close(fd)
			logger.Error("Failed to open remote stream", "error", err)
			return -winfuse.EACCES
		}
	}

	d.AfmLock.Lock()
	d.OpenMapLock.Lock()
	fh.Inode = uint64(fd)
	fh.openFileCounter.Open()
	fh.IsLocalPresent = true
	d.OpenFileHandlers[fh.Inode] = fh

	if remoteHasUpdate {
		fh.NotLocalSynced = true
		fh.RemoteFileStream = stream
		fh.StreamCancel = streamCancel
		fh.Download.Reset(remoteTotalSize)

		// Pre-allocate local file to expected size for out-of-order writes.
		// Without this, non-sequential reads (e.g., video moov atom at end) can cause corruption.
		// macOS reads video files non-sequentially (header, then trailer at end, then content).
		if existingLocalSize == 0 && remoteTotalSize > 0 {
			if truncErr := os.Truncate(localPath, int64(remoteTotalSize)); truncErr != nil {
				logger.Warn("Failed to pre-allocate local file", "size", remoteTotalSize, "error", truncErr)
			} else {
				logger.Info("Pre-allocated local file", "size", remoteTotalSize)
			}
		}

		// NOTE: We intentionally do NOT use local file size for resume tracking.
		// Pre-allocation creates a full-size file with zeros, which would falsely
		// indicate the download is complete. We only trust Download.BytesDownloaded
		// which is tracked explicitly by Read() operations.
	}
	d.OpenMapLock.Unlock()
	d.AfmLock.Unlock()

	fi.Fh = uint64(fd)
	fi.DirectIo = shouldUseDirectIo(path, flags)
	logger.Info("Opened for remote streaming", "fh", fd, "directIo", fi.DirectIo, "remoteHasUpdate", remoteHasUpdate, "fhNotLocalSynced", fh.NotLocalSynced, "fhStreamNil", fh.RemoteFileStream == nil)
	return 0
}

// Called on unmount.
func (d *Dir) Destroy() {
	d.logger.Info("Destroy")
}

func (d *Dir) Flush(path string, fh uint64) (errCode int) {
	defer d.recoverPanic("Flush", &errCode)
	d.logger.Debug("FUSE Flush (stub)", "path", path, "fh", fh)
	return 0 // Return success - actual sync happens in Fsync/Release.
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) (errCode int) {
	defer d.recoverPanic("Fsync", &errCode)
	d.logger.Warn("FUSE Fsync", "path", path, "datasync", datasync, "fh", fh)
	localPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		d.logger.Error("Path traversal blocked in Fsync", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	logger := d.logger.With("method", "fsync", "path", localPath)

	err = syscall.Fsync(int(fh))
	if err == nil {
		d.logger.Warn("FUSE Fsync SUCCESS", "path", localPath, "fh", fh)
		return 0
	}

	// If EBADF, the fd was already closed (Release called before Fsync).
	// This is a FUSE race condition. Workaround: open, fsync, close.
	if err == syscall.EBADF {
		d.logger.Warn("FUSE Fsync EBADF - attempting fallback open/fsync/close", "path", localPath, "fh", fh)

		fd, openErr := syscall.Open(localPath, syscall.O_RDONLY, 0)
		if openErr != nil {
			// File might have been renamed/deleted - that's OK, data was already written
			d.logger.Warn("FUSE Fsync fallback open failed (file may have been renamed)", "path", localPath, "error", openErr)
			return 0 // Return success - the data was committed before close
		}

		fsyncErr := syscall.Fsync(fd)
		syscall.Close(fd)

		if fsyncErr != nil {
			d.logger.Warn("FUSE Fsync fallback fsync failed", "path", localPath, "error", fsyncErr)
			return int(convertOsErrToSyscallErrno("fsync", fsyncErr))
		}

		d.logger.Warn("FUSE Fsync fallback SUCCESS", "path", localPath)
		return 0
	}

	d.logger.Warn("FUSE Fsync FAILED", "path", localPath, "fh", fh, "error", err)
	logger.Error("Failed to fsync", "error", err)
	return int(convertOsErrToSyscallErrno("fsync", err))
}

func (d *Dir) Fsyncdir(path string, datasync bool, fh uint64) (errCode int) {
	defer d.recoverPanic("Fsyncdir", &errCode)
	d.logger.Debug("FUSE Fsyncdir (stub)", "path", path, "datasync", datasync, "fh", fh)
	return 0 // Return success - directory syncs are no-ops for our use case.
}

func (d *Dir) Getattr(path string, stat *winfuse.Stat_t, fh uint64) (errCode int) {
	defer d.recoverPanic("Getattr", &errCode)
	logger := d.logger.With("method", "get-attr", "path", path, "fh", fh)

	// CRITICAL: Lock order is RemoteFilesLock → Adm → AfmLock (prevents deadlock with AddRemoteFile)
	d.RemoteFilesLock.RLock()
	isRemote := len(d.RemoteFiles) != 0
	d.RemoteFilesLock.RUnlock()

	if isRemote {
		d.RemoteFilesLock.RLock()
		defer d.RemoteFilesLock.RUnlock()
	}

	d.Adm.Lock()
	defer d.Adm.Unlock()
	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()

	stgo := syscall.Stat_t{}
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in Getattr", "path", path, "error", err)
		return -int(syscall.EACCES)
	}

	// Check if the file is on remote, and add it to local tree.
	if isRemote {
		remFile, okRemote := d.RemoteFiles[path]
		if okRemote {
			// File is on remote, let's see if it is also locally.
			err := syscall.Lstat(cleanPath, &stgo)
			if err != nil {
				// Ok file not locally. Just add it, and download it on Open.
				copyFusestatFromFusestat(stat, remFile.stat)
				d.AllFileMap[path] = remFile
				// All good.
				return 0
			}

			// Ok, file is also locally present, but we already got the pointer to it.
			// Let's see if the stats are ok.

			auxStat := &winfuse.Stat_t{}
			copyFusestatFromGostat(auxStat, &stgo)

			if isModificationTimeNewer(auxStat, remFile.stat) {
				// Only mark as LocalNewer if local file has actual content.
				// Empty files (size=0) are just placeholders created for streaming - not real local edits.
				if auxStat.Size > 0 {
					remFile.LocalNewer = true
					copyFusestatFromFusestat(remFile.stat, auxStat)
				}
				copyFusestatFromFusestat(stat, remFile.stat)
				return 0
			}

			copyFusestatFromFusestat(stat, remFile.stat)
			remFile.LocalNewer = false

			return 0
		}
	}

	err = syscall.Lstat(cleanPath, &stgo)
	if err != nil {
		logger.Error("Failed to lstat path", "clean-path", cleanPath, "error-code", int(convertOsErrToSyscallErrno("lstat", err)), "error", err)
		cerr := convertOsErrToSyscallErrno("lstat", err)
		return int(cerr)
	}

	// Note: We do not use Lampert timestamps, last edit wins.

	copyFusestatFromGostat(stat, &stgo)
	gtAtim := func(fst, snd winfuse.Timespec) bool {
		return fst.Time().After(snd.Time())
	}

	found := false

	dir, ok := d.AllDirMap[path]
	if ok && dir.stat != nil && gtAtim(dir.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(stat, dir.stat)
	}
	if ok && dir.stat != nil && !gtAtim(dir.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(stat, dir.stat)
	}
	if ok {
		found = ok
	}

	f, ok := d.AllFileMap[path]
	if ok && f.stat != nil && gtAtim(f.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(stat, f.stat)
	}
	if ok && f.stat != nil && !gtAtim(f.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(f.stat, stat)
	}
	if ok {
		found = ok
	}

	// TODO: Sigh, refactor later.

	// File not found in tree.

	// In an ideal world: do not stat again :<.
	finfo, err := os.Stat(cleanPath)
	if err != nil {
		logger.Error("Failed to determine if dir or file", "error", "error")
		return int(convertOsErrToSyscallErrno("stat", err))
	}

	if !found {
		if finfo.IsDir() {
			dir := &Dir{
				logger:              logger,
				Adm:                 sync.RWMutex{},
				AfmLock:             sync.RWMutex{},
				Inode:               stat.Ino,
				RelativePath:        path,
				LocalDownloadFolder: cleanPath, // Maybe remove the last segment?
				IsLocalPresent:      true,
				Root:                d,
				OpenFileHandlers:    make(map[uint64]*File),
				OpenMapLock:         sync.RWMutex{},
				PeerLastEdit:        0,
				AllDirMap:           make(map[string]*Dir),
				AllFileMap:          make(map[string]*File),
				stat:                &winfuse.Stat_t{},
				OnLocalChange:       d.OnLocalChange,
				OpenStreamProvider:  d.OpenStreamProvider,

				RemoteFilesLock: sync.RWMutex{},
				RemoteFiles:     make(map[string]*File),
			}
			copyFusestatFromFusestat(dir.stat, stat)
			d.AllDirMap[path] = dir
			// d.OnLocalChange(types.FileEvent{
			// 	Action: types.AddDir,
			// 	Path:   path,
			// 	Attr:   types.StatToAttr(dir.stat),
			// })
		} else {
			f := &File{
				logger:          logger,
				Inode:           stat.Ino,
				RelativePath:    path,
				RealPathOfFile:  cleanPath,
				IsLocalPresent:  true,
				Root:            d,
				PeerLastEdit:    0,
				openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
				Name:            getNameFromPath(path),
				stat:            &winfuse.Stat_t{},
				StreamProvider:  d.OpenStreamProvider(),
				OnLocalChange:   d.OnLocalChange,
			}
			copyFusestatFromFusestat(f.stat, stat)

			d.AllFileMap[path] = f
			// d.OnLocalChange(types.FileEvent{
			// 	Action: types.AddFile,
			// 	Path:   path,
			// 	Attr:   types.StatToAttr(f.stat),
			// })
		}

	}

	return 0
}

func (d *Dir) Init() {
	d.logger.Info("Init", "inode", d.Inode)
	// syscall.Chdir(d.LocalDownloadFolder)

}

func (d *Dir) Link(oldpath string, newpath string) (errCode int) {
	defer d.recoverPanic("Link", &errCode)
	d.logger.Debug("FUSE Link (stub - not supported)", "oldPath", oldpath, "newPath", newpath, "inode", d.Inode)
	// Hard links not supported - return EPERM (more accurate than ENOSYS).
	return -winfuse.EPERM
}

func (d *Dir) Mkdir(path string, mode uint32) (errCode int) {
	defer d.recoverPanic("Mkdir", &errCode)
	return d.mkdirInternal(path, mode, true)
}

// MkdirFromPeer creates a directory without notifying the peer (to avoid loops).
func (d *Dir) MkdirFromPeer(path string, mode uint32) (errCode int) {
	return d.mkdirInternal(path, mode, false)
}

func (d *Dir) mkdirInternal(path string, mode uint32, notifyPeer bool) (errCode int) {
	logger := d.logger.With("method", "mkdir", "path", path, "mode", mode)
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal attempt blocked", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	err = syscall.Mkdir(cleanPath, mode)
	if err != nil {
		logger.Error("Failed to mkdir", "path", cleanPath, "error", err)
		return int(convertOsErrToSyscallErrno("mkdir", err))
	}

	// Notify peer about the new directory (only for local changes).
	if notifyPeer && d.OnLocalChange != nil {
		stgo := syscall.Stat_t{}
		if statErr := syscall.Lstat(cleanPath, &stgo); statErr == nil {
			aux := &winfuse.Stat_t{}
			copyFusestatFromGostat(aux, &stgo)
			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.AddDir,
				Attr:   types.StatToAttr(aux),
			})
			logger.Info("Notified peer about new directory", "path", path)
		} else {
			logger.Warn("Failed to stat new directory for notification", "error", statErr)
		}
	}

	return 0
}

func (d *Dir) Mknod(path string, mode uint32, dev uint64) (errCode int) {
	defer d.recoverPanic("Mknod", &errCode)
	d.logger.Info("Mknod", "path", path, "inode", d.Inode)

	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		d.logger.Error("Path traversal blocked in Mknod", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	logger := d.logger.With("method", "mknod", "path", path, "mode", mode, "dev", dev)
	err = syscall.Mknod(cleanPath, mode, int(dev))
	if err != nil {
		logger.Error("Failed to mknor", "errro", err)
		return int(convertOsErrToSyscallErrno("mknod", err))
	}
	return 0
}

func (d *Dir) Open(path string, flags int) (errCode int, retFh uint64) {
	defer d.recoverPanic("Open", &errCode)
	logger := d.logger.With("method", "open", "path", path, "flags", flags)

	localPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in Open", "path", path, "error", err)
		return -int(syscall.EACCES), 0
	}

	d.AfmLock.Lock()
	fh, ok := d.AllFileMap[path]
	if !ok {
		// File not in map - check if it exists on disk (pre-existing local file)
		if _, statErr := os.Stat(localPath); statErr == nil {
			// Check if this is a remote file (placeholder might have been created)
			d.RemoteFilesLock.RLock()
			_, isRemoteFile := d.RemoteFiles[path]
			d.RemoteFilesLock.RUnlock()

			fh = &File{
				logger:          d.logger,
				openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
				Name:            getNameFromPath(path),
				RelativePath:    path,
				RealPathOfFile:  localPath,
				IsLocalPresent:  true,
				LocalNewer:      !isRemoteFile, // Remote files should not be marked as local newer
				OnLocalChange:   d.OnLocalChange,
				StreamProvider:  d.OpenStreamProvider(),
				stat:            &winfuse.Stat_t{},
			}
			d.AllFileMap[path] = fh
		} else {
			d.AfmLock.Unlock()
			return -winfuse.ENOENT, 0
		}
	}

	// File already opened - return existing handle (FUSE calls Release once per fh)
	if fh.openFileCounter.CountOpenDescriptors() != 0 {
		inode := fh.Inode
		d.AfmLock.Unlock()
		return 0, uint64(inode)
	}

	// CRITICAL: Release AfmLock BEFORE RemoteFilesLock to maintain lock order
	isLocalPresent := fh.IsLocalPresent
	localNewer := fh.LocalNewer
	d.AfmLock.Unlock()

	// Check if remote has newer version
	remoteHasUpdate := false
	var remoteTotalSize uint64
	var remoteMtime time.Time
	var remoteBitmap *ChunkBitmap
	needsRemark := false
	d.RemoteFilesLock.RLock()
	if remoteFile, hasRemote := d.RemoteFiles[path]; hasRemote {
		if remoteFile.stat != nil {
			remoteTotalSize = uint64(remoteFile.stat.Size)
			remoteMtime = remoteFile.stat.Mtim.Time()
		}
		remoteBitmap = remoteFile.Bitmap
		if remoteFile.NotLocalSynced {
			logger.Info("Remote has newer version, streaming from remote", "path", path)
			localNewer = false
			remoteHasUpdate = true
		} else if remoteTotalSize > 0 {
			// Even if NotLocalSynced=false, verify download is actually complete.
			// Use bitmap (preferred) or BytesDownloaded as fallback.
			if remoteBitmap != nil && !remoteBitmap.IsComplete() {
				logger.Info("Download incomplete (bitmap), re-streaming from remote", "progress", remoteBitmap.Progress(), "remoteSize", remoteTotalSize)
				localNewer = false
				remoteHasUpdate = true
				needsRemark = true
			} else if remoteBitmap == nil {
				bytesDownloaded := remoteFile.Download.BytesDownloaded.Load()
				if bytesDownloaded < remoteTotalSize {
					logger.Info("Download incomplete, re-streaming from remote", "bytesDownloaded", bytesDownloaded, "remoteSize", remoteTotalSize)
					localNewer = false
					remoteHasUpdate = true
					needsRemark = true
				}
			}
		}
	}
	d.RemoteFilesLock.RUnlock()

	// Re-mark for streaming outside of read lock
	if needsRemark {
		d.RemoteFilesLock.Lock()
		if rf, ok := d.RemoteFiles[path]; ok {
			rf.NotLocalSynced = true
		}
		d.RemoteFilesLock.Unlock()
	}

	// Open locally if we have newer local version
	if isLocalPresent && localNewer {
		accessMode := flags & winfuse.O_ACCMODE
		logger.Info("Opening local file", "flags", flags, "accessMode", accessMode, "isReadOnly", accessMode == winfuse.O_RDONLY)
		fd, err := syscall.Open(localPath, flags, 0)
		if err != nil {
			logger.Error("Failed to open local file", "error", err)
			return int(convertOsErrToSyscallErrno("open", err)), 0
		}

		d.AfmLock.Lock()
		d.OpenMapLock.Lock()
		fh.Inode = uint64(fd)
		fh.openFileCounter.Open()
		d.OpenFileHandlers[fh.Inode] = fh
		d.OpenMapLock.Unlock()
		d.AfmLock.Unlock()
		logger.Info("Opened local file", "fh", fd)
		return 0, uint64(fd)
	}

	// Remote file path - check for partial download or create cache file.
	var existingLocalSize int64
	localStat, err := os.Stat(localPath)
	if err != nil {
		if err2 := os.MkdirAll(getPathWithoutName(localPath), 0o755); err2 != nil {
			logger.Error("Failed to create folders", "error", err2)
			return int(convertOsErrToSyscallErrno("open", err2)), 0
		}
		f, err2 := os.Create(localPath)
		if err2 != nil {
			logger.Error("Failed to create cache file", "error", err2)
			return int(convertOsErrToSyscallErrno("open", err2)), 0
		}
		_ = f.Close()
		// Set placeholder mtime to match remote file's mtime.
		// This ensures Getattr's mtime comparison favors remote stat (with correct size).
		if !remoteMtime.IsZero() {
			_ = os.Chtimes(localPath, remoteMtime, remoteMtime)
		}
	} else {
		// Local file exists - this may be a partial download from a previous session.
		existingLocalSize = localStat.Size()
		if existingLocalSize > 0 && remoteTotalSize > 0 && uint64(existingLocalSize) < remoteTotalSize {
			logger.Info("Found partial download, will resume", "localSize", existingLocalSize, "remoteSize", remoteTotalSize)
		}
	}

	accessMode := flags & winfuse.O_ACCMODE
	logger.Info("Opening remote cache file", "flags", flags, "accessMode", accessMode, "isReadOnly", accessMode == winfuse.O_RDONLY)
	fd, err := syscall.Open(localPath, flags, 0)
	if err != nil {
		logger.Error("Failed to open path", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	// Open remote stream (network call - no locks held)
	fsp := d.OpenStreamProvider()
	streamCtx, streamCancel := context.WithCancel(context.Background())
	stream, err := fsp.OpenRemoteFile(streamCtx, uint64(fd), path)
	if err != nil {
		streamCancel()
		syscall.Close(fd)
		logger.Error("Failed to open remote stream", "error", err)
		return -winfuse.EACCES, 0
	}

	d.AfmLock.Lock()
	d.OpenMapLock.Lock()
	fh.Inode = uint64(fd)
	fh.StreamProvider = fsp
	fh.RemoteFileStream = stream
	fh.StreamCancel = streamCancel
	if remoteHasUpdate {
		fh.NotLocalSynced = true // Ensure Read uses stream, not stale local cache
		// Initialize download state for resume capability.
		fh.Download.Reset(remoteTotalSize)
		// Share the bitmap from RemoteFiles so Read() can check which chunks are cached.
		fh.Bitmap = remoteBitmap

		// Pre-allocate local file to expected size for out-of-order writes.
		// Without this, non-sequential reads (e.g., video moov atom at end) can cause corruption.
		if existingLocalSize == 0 && remoteTotalSize > 0 {
			if truncErr := os.Truncate(localPath, int64(remoteTotalSize)); truncErr != nil {
				logger.Warn("Failed to pre-allocate local file", "size", remoteTotalSize, "error", truncErr)
			} else {
				logger.Info("Pre-allocated local file", "size", remoteTotalSize)
			}
		}
	}
	d.OpenFileHandlers[fh.Inode] = fh
	fh.openFileCounter.Open()
	d.OpenMapLock.Unlock()
	d.AfmLock.Unlock()

	logger.Info("Opened remote file", "fh", fh.Inode, "notLocalSynced", fh.NotLocalSynced, "hasStream", fh.RemoteFileStream != nil, "totalSize", remoteTotalSize)
	return 0, fh.Inode
}

func (d *Dir) Opendir(path string) (errCode int, retFh uint64) {
	defer d.recoverPanic("Opendir", &errCode)
	d.logger.Info("Opendir", "path", path, "inode", d.Inode)

	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		d.logger.Error("Path traversal blocked in Opendir", "path", path, "error", err)
		return -int(syscall.EACCES), 0
	}

	logger := d.logger.With("method", "opendir", "path", cleanPath)
	f, err := syscall.Open(cleanPath, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)

	if err != nil {
		logger.Error("Failed to open dir", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	return 0, uint64(f)
}

func (d *Dir) Readdir(path string, fill func(name string, stat *winfuse.Stat_t, offset int64) bool, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Readdir", &errCode)
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		return -int(syscall.EACCES)
	}
	logger := d.logger.With("method", "readdir", "path", cleanPath)

	dirEn, err := os.ReadDir(cleanPath)
	if err != nil {
		logger.Error("Failed to read dir", "error", err)
		return int(convertOsErrToSyscallErrno("readdir", err))
	}

	localFiles := make(map[string]struct{})
	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, dir := range dirEn {
		localFiles[dir.Name()] = struct{}{}
		if !fill(dir.Name(), nil, 0) {
			break
		}
	}

	// Add remote files that don't exist locally
	d.RemoteFilesLock.RLock()
	defer d.RemoteFilesLock.RUnlock()
	logger.Info("=== READDIR RemoteFiles ===", "count", len(d.RemoteFiles), "path", path)
	for k := range d.RemoteFiles {
		name := getNameFromPath(k)
		logger.Info("=== READDIR checking remote ===", "key", k, "name", name, "existsLocal", localFiles[name] != struct{}{})
		if _, exists := localFiles[name]; !exists {
			fill(name, nil, 0)
		}
	}

	return 0
}

func (d *Dir) Readlink(path string) (errCode int, target string) {
	defer d.recoverPanic("Readlink", &errCode)
	d.logger.Debug("FUSE Readlink (stub)", "path", path, "inode", d.Inode)
	// No symlinks in our filesystem - return EINVAL (not a symlink).
	return -winfuse.EINVAL, ""
}

func (d *Dir) Release(path string, fh uint64) (errCode int) {
	defer d.recoverPanic("Release", &errCode)
	logger := d.logger.With("method", "release", "path", path, "fh", fh)

	d.OpenMapLock.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			d.OpenMapLock.Unlock()
		}
	}()

	f, ok := d.OpenFileHandlers[fh]
	if !ok {
		// fd not in map - either already released, or was a late fcopyfile handle
		// Don't try to close - just return success
		logger.Warn("Release called for unknown fh (already released or fcopyfile race)")
		return 0
	}

	v := f.openFileCounter.Release()

	// Debug: log ALL relevant state at Release entry
	logger.Info("=== RELEASE ===",
		"path", path,
		"openCount", v,
		"NotRemoteSynced", f.NotRemoteSynced,
		"HadEdits", f.HadEdits,
		"IsLocalPresent", f.IsLocalPresent,
		"LocalNewer", f.LocalNewer)

	if v == 0 {
		err := syscall.Close(int(fh))
		if err != nil {
			logger.Error("Failed to close fd", "error", err)
			return int(convertOsErrToSyscallErrno("release", err))
		}

		delete(d.OpenFileHandlers, fh)

		// SINGLE notification path: notify peer if file was created OR edited locally
		needsNotify := (f.NotRemoteSynced || f.HadEdits) && d.OnLocalChange != nil

		if needsNotify {
			stgo := syscall.Stat_t{}
			cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
			if err != nil {
				return -int(syscall.EACCES)
			}
			err = syscall.Lstat(cleanPath, &stgo)
			if err != nil {
				logger.Error("lstat failed", "cleanPath", cleanPath, "error", err)
				return int(convertOsErrToSyscallErrno("lstat", err))
			}

			logger.Info("Release lstat result", "path", path, "size", stgo.Size)

			if stgo.Size == 0 && !f.HadEdits {
				// Size=0 with no writes: could be either:
				// 1. macOS Finder drag: uses fcopyfile which bypasses FUSE Write.
				//    Data lands on disk but we never see it. No second Release comes.
				// 2. Genuinely empty file: touch, .gitkeep, etc.
				//
				// Defer notification by 300ms. After the delay:
				// - If file was reopened (open→write→release cycle), skip (that Release handles it)
				// - If file grew on disk (fcopyfile completed), notify with real size
				// - If still size=0 (genuine empty file), notify with size=0
				logger.Info("Deferring size=0 notification (fcopyfile or empty)", "path", path)
				go func() {
					time.Sleep(300 * time.Millisecond)
					// If file was reopened for writing, that Release will handle it
					if f.openFileCounter.CountOpenDescriptors() > 0 {
						logger.Info("File reopened, cancelling deferred notify", "path", path)
						return
					}
					// Re-lstat: fcopyfile may have written data by now
					var recheckStat syscall.Stat_t
					if err := syscall.Lstat(cleanPath, &recheckStat); err != nil {
						logger.Error("Deferred lstat failed", "path", path, "error", err)
						return
					}
					// Notify with whatever size we find (0 for empty, >0 for fcopyfile)
					aux := &winfuse.Stat_t{}
					copyFusestatFromGostat(aux, &recheckStat)
					d.OnLocalChange(types.FileEvent{
						Path:   path,
						Action: types.AddFile,
						Attr:   types.StatToAttr(aux),
					})
					f.NotRemoteSynced = false
					f.HadEdits = false
					logger.Info(">>> DEFERRED NOTIFIED PEER", "path", path, "size", recheckStat.Size)
				}()
			} else {
				aux := &winfuse.Stat_t{}
				copyFusestatFromGostat(aux, &stgo)

				d.OnLocalChange(types.FileEvent{
					Path:   path,
					Action: types.AddFile, // Always AddFile - peer handles updates
					Attr:   types.StatToAttr(aux),
				})

				f.NotRemoteSynced = false
				f.HadEdits = false
				logger.Info(">>> NOTIFIED PEER", "path", path, "size", stgo.Size)
			}
		}

		// Reset sync state for future opens
		f.IsLocalPresent = true
		// Only mark as synced if download is actually COMPLETE.
		// Use ChunkBitmap (preferred) or BytesDownloaded as fallback.
		if f.Bitmap != nil {
			if f.Bitmap.IsComplete() {
				f.NotLocalSynced = false
				logger.Info("Download complete (all chunks verified)", "progress", f.Bitmap.Progress())
			} else {
				logger.Info("Download incomplete, keeping NotLocalSynced=true", "progress", f.Bitmap.Progress())
			}
		} else {
			// Fallback for files without bitmap (local-origin or legacy).
			expectedSize := f.Download.TotalSize.Load()
			bytesDownloaded := f.Download.BytesDownloaded.Load()
			if expectedSize > 0 && bytesDownloaded >= expectedSize {
				f.NotLocalSynced = false
				logger.Info("Download complete (bytes check)", "bytesDownloaded", bytesDownloaded, "expectedSize", expectedSize)
			} else if expectedSize > 0 {
				logger.Info("Download incomplete, keeping NotLocalSynced=true", "bytesDownloaded", bytesDownloaded, "expectedSize", expectedSize)
			} else {
				// No expected size tracked (local file, not from remote) - mark as synced
				cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
				if err != nil {
					return -int(syscall.EACCES)
				}
				if localInfo, statErr := os.Stat(cleanPath); statErr == nil && localInfo.Size() > 0 {
					f.NotLocalSynced = false
				}
			}
		}

		// If peer stopped sharing and download is now complete, remove from AllFileMap.
		if f.PeerStoppedSharing {
			d.AfmLock.Lock()
			delete(d.AllFileMap, path)
			d.AfmLock.Unlock()
			logger.Info("Removed file reference after download completed (peer stopped sharing)", "path", path)
		}

		// Get stream reference and cancel func, clear under lock, then close OUTSIDE lock
		// to avoid holding OpenMapLock during network I/O
		stream := f.RemoteFileStream
		streamCancel := f.StreamCancel
		f.RemoteFileStream = nil
		f.StreamCancel = nil
		if stream != nil {
			d.OpenMapLock.Unlock()
			unlocked = true
			err = stream.Close()
			if err != nil {
				logger.Error("Failed to close remote file stream", "error", err)
			}
			// Cancel the stream context after closing
			if streamCancel != nil {
				streamCancel()
			}
			return 0 // Already unlocked, just return
		}
	}

	return 0
}

func (d *Dir) Releasedir(path string, fh uint64) (errCode int) {
	defer d.recoverPanic("Releasedir", &errCode)
	d.logger.Info("Releasedir", "path", path, "inode", d.Inode, "fh", fh)
	logger := d.logger.With("method", "release-dir", "path", path, "fh", fh)
	err := syscall.Close(int(fh))
	if err != nil {
		logger.Error("Failed to release", "error", err)
		return int(convertOsErrToSyscallErrno("release", err))
	}

	return 0
}

// Mac OS High Level apps use Rename SWAP, which is really fun from my experience.
// Note: cgofuse does not expose renamex_np with RENAME_SWAP flag.
// When apps try atomic rename-swap, we fall back to basic rename.
func (d *Dir) Rename(oldpath string, newpath string) (errCode int) {
	defer d.recoverPanic("Rename", &errCode)
	d.logger.Warn("FUSE Rename called",
		"oldpath", oldpath,
		"newpath", newpath,
		"note", "macOS apps may use RENAME_SWAP - not supported by cgofuse")

	cleanOldPath, err := SecureJoin(d.LocalDownloadFolder, oldpath)
	if err != nil {
		d.logger.Error("Path traversal blocked in Rename (oldpath)", "path", oldpath, "error", err)
		return -int(syscall.EACCES)
	}
	cleanNewPath, err := SecureJoin(d.LocalDownloadFolder, newpath)
	if err != nil {
		d.logger.Error("Path traversal blocked in Rename (newpath)", "path", newpath, "error", err)
		return -int(syscall.EACCES)
	}
	logger := d.logger.With("method", "rename", "old-path", cleanOldPath, "new-path", cleanNewPath)

	d.logger.Warn("FUSE Rename resolved paths", "cleanOldPath", cleanOldPath, "cleanNewPath", cleanNewPath)

	// Check if this is a remote-only file (don't notify peer about their own files).
	d.RemoteFilesLock.RLock()
	_, isRemote := d.RemoteFiles[oldpath]
	d.RemoteFilesLock.RUnlock()

	err = syscall.Rename(cleanOldPath, cleanNewPath)
	if err != nil {
		d.logger.Warn("FUSE Rename FAILED", "oldpath", oldpath, "newpath", newpath, "error", err)
		logger.Error("Failed to rename", "error", err)
		return int(convertOsErrToSyscallErrno("rename", err))
	}

	// Update internal maps to reflect the rename
	d.AfmLock.Lock()
	if f, ok := d.AllFileMap[oldpath]; ok {
		delete(d.AllFileMap, oldpath)
		f.RelativePath = newpath
		f.Name = getNameFromPath(newpath)
		f.RealPathOfFile = cleanNewPath
		d.AllFileMap[newpath] = f
		logger.Info("Updated AllFileMap for rename", "oldpath", oldpath, "newpath", newpath)
	}
	d.AfmLock.Unlock()

	// Also update RemoteFiles if the file was remote
	d.RemoteFilesLock.Lock()
	if f, ok := d.RemoteFiles[oldpath]; ok {
		delete(d.RemoteFiles, oldpath)
		f.RelativePath = newpath
		f.Name = getNameFromPath(newpath)
		f.RealPathOfFile = cleanNewPath
		d.RemoteFiles[newpath] = f
		logger.Info("Updated RemoteFiles for rename", "oldpath", oldpath, "newpath", newpath)
	}
	d.RemoteFilesLock.Unlock()

	// Notify peer about the rename (only for local files, not remote-only).
	if d.OnLocalChange != nil && !isRemote {
		// Get stat of the renamed file.
		stgo := syscall.Stat_t{}
		var attr *keibidrop.Attr
		if statErr := syscall.Lstat(cleanNewPath, &stgo); statErr == nil {
			fuseStat := &winfuse.Stat_t{}
			copyFusestatFromGostat(fuseStat, &stgo)
			attr = types.StatToAttr(fuseStat)
		}

		d.OnLocalChange(types.FileEvent{
			Path:    newpath,
			OldPath: oldpath,
			Action:  types.RenameFile,
			Attr:    attr,
		})
		logger.Info("Notified peer about rename", "oldpath", oldpath, "newpath", newpath)
	}

	d.logger.Warn("FUSE Rename SUCCESS", "oldpath", oldpath, "newpath", newpath)
	return 0
}

func (d *Dir) Rmdir(path string) (errCode int) {
	defer d.recoverPanic("Rmdir", &errCode)
	return d.rmdirInternal(path, true)
}

// RmdirFromPeer removes a directory without notifying the peer (to avoid loops).
func (d *Dir) RmdirFromPeer(path string) (errCode int) {
	return d.rmdirInternal(path, false)
}

func (d *Dir) rmdirInternal(path string, notifyPeer bool) (errCode int) {
	d.logger.Info("Rmdir", "path", path, "inode", d.Inode)
	logger := d.logger.With("method", "rmdir", "path", path)

	// Check if this is a remote-only directory (track if we removed it from map).
	d.Adm.Lock()
	_, isRemoteDir := d.AllDirMap[path]
	if isRemoteDir {
		delete(d.AllDirMap, path)
		logger.Info("Removed directory from AllDirMap", "path", path)
	}
	d.Adm.Unlock()

	// Try to rmdir local directory (may not exist if remote-only).
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in Rmdir", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	err = syscall.Rmdir(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Directory doesn't exist locally.
			if isRemoteDir {
				// Remote-only dir - we already cleaned up the map, success.
				logger.Info("Remote-only directory removed (no local copy)", "path", path)
			} else {
				// No remote entry and no local dir - doesn't exist.
				logger.Error("Failed to remove dir - not found", "error", err)
				return int(convertOsErrToSyscallErrno("rmdir", err))
			}
		} else {
			// Real error (not empty, permission denied, etc.) - fail regardless.
			logger.Error("Failed to remove dir", "error", err)
			return int(convertOsErrToSyscallErrno("rmdir", err))
		}
	} else {
		logger.Info("Local directory removed", "path", path)
	}

	// Notify peer about the removed directory (only for local changes, i.e. notifyPeer=true).
	// Do not use isRemoteDir to gate this — AllDirMap contains both local and peer-received dirs
	// (Getattr lazily adds local dirs), so membership is not a reliable peer-origin indicator.
	// Loop prevention is handled by RmdirFromPeer (notifyPeer=false).
	if notifyPeer && d.OnLocalChange != nil {
		d.OnLocalChange(types.FileEvent{
			Path:   path,
			Action: types.RemoveDir,
			Attr:   nil, // No attributes needed for removal
		})
		logger.Info("Notified peer about removed directory", "path", path)
	}

	// TODO: Remove also sub-files and sub dirs from maps.

	return 0
}

func (d *Dir) Statfs(path string, stat *winfuse.Statfs_t) (errCode int) {
	defer d.recoverPanic("Statfs", &errCode)
	/*
		var freeBytesAvailable uint64
		var totalNumberOfBytes uint64
		var totalNumberOfFreeBytes uint64

		freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes, err := GetFreeDiskSpace(d.LocalDownloadFolder)
		if err != nil {
			logger.Error("Failed to get disk free space", "error", err)
			return winfuse.EIO
		}
	*/
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		d.logger.Error("Path traversal blocked in Statfs", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	logger := d.logger.With("method", "statfs", "path", path, "rea-path", cleanPath)

	stgo := syscall.Statfs_t{}
	err = syscall_Statfs(cleanPath, &stgo)
	if err != nil {
		logger.Error("Failed to stat underlying folder", "error", err)
		return int(convertOsErrToSyscallErrno("statfs", err))
	}
	copyFusestatfsFromGostatfs(stat, &stgo)

	logger.Info("Statfs", "stat", stat, "inode", d.Inode)

	return 0
}

func (d *Dir) Symlink(target string, newpath string) (errCode int) {
	defer d.recoverPanic("Symlink", &errCode)
	d.logger.Debug("FUSE Symlink (stub - not supported)", "target", target, "newpath", newpath, "inode", d.Inode)
	// Symlinks not supported - return EPERM.
	return -winfuse.EPERM
}

// Note: On windows open does not have a truncate flag,
// thus Open is immediately followed by Truncate.
func (d *Dir) Truncate(path string, size int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Truncate", &errCode)
	d.logger.Info("Truncate", "path", path, "size", size, "inode", d.Inode, "fh", fh)

	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		d.logger.Error("Path traversal blocked in Truncate", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	logger := d.logger.With("method", "truncate", "path", path, "size", size, "fh", fh)
	err = syscall.Truncate(cleanPath, size)
	if err != nil {
		logger.Error("Faile to truncate", "error", err)
		return int(convertOsErrToSyscallErrno("truncate", err))
	}

	return 0
}

// Unlink removes a file.
func (d *Dir) Unlink(path string) (errCode int) {
	defer d.recoverPanic("Unlink", &errCode)
	return d.unlinkInternal(path, true)
}

// UnlinkFromPeer removes a file without notifying the peer (to avoid loops).
func (d *Dir) UnlinkFromPeer(path string) (errCode int) {
	return d.unlinkInternal(path, false)
}

func (d *Dir) unlinkInternal(path string, notifyPeer bool) (errCode int) {
	d.logger.Info("Unlink", "path", path, "inode", d.Inode)
	logger := d.logger.With("method", "unlink", "path", path)

	// Check if this is a remote-only file (not downloaded locally).
	d.RemoteFilesLock.Lock()
	remoteFile, isRemote := d.RemoteFiles[path]
	if isRemote {
		delete(d.RemoteFiles, path)
		logger.Info("Removed remote file from map", "path", path)
	}
	d.RemoteFilesLock.Unlock()

	// Also clean up AllFileMap.
	d.AfmLock.Lock()
	delete(d.AllFileMap, path)
	d.AfmLock.Unlock()

	// Try to unlink local file (may not exist if remote-only).
	cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		logger.Error("Path traversal blocked in Unlink", "path", path, "error", err)
		return -int(syscall.EACCES)
	}
	err = syscall.Unlink(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist locally.
			if isRemote {
				// Remote-only file - we already cleaned up the maps, success.
				logger.Info("Remote-only file removed (no local copy)", "path", path)
			} else {
				// No remote entry and no local file - file doesn't exist.
				logger.Error("Failed to unlink - file not found", "error", err)
				return int(convertOsErrToSyscallErrno("unlink", err))
			}
		} else {
			// Real error (permission denied, etc.) - fail regardless.
			logger.Error("Failed to unlink", "error", err)
			return int(convertOsErrToSyscallErrno("unlink", err))
		}
	} else {
		logger.Info("Local file unlinked", "path", path)
	}

	// Notify peer about the removed file (only for local changes).
	// Only notify if this was OUR file (not a remote file we're just hiding locally).
	if notifyPeer && d.OnLocalChange != nil && !isRemote && remoteFile == nil {
		d.OnLocalChange(types.FileEvent{
			Path:   path,
			Action: types.RemoveFile,
			Attr:   nil, // No attributes needed for removal
		})
		logger.Info("Notified peer about removed file", "path", path)
	}

	return 0
}

// Utimens sets file access and modification times.
// We return success but don't persist the changes (timestamps come from underlying storage).
func (d *Dir) Utimens(path string, tmsp []winfuse.Timespec) (errCode int) {
	defer d.recoverPanic("Utimens", &errCode)
	d.logger.Debug("FUSE Utimens (stub)", "path", path, "inode", d.Inode)
	return 0 // Return success - git and other tools call this frequently.
}

// WriteStats tracks timing for Write operations (for profiling)
type WriteStats struct {
	mu              sync.Mutex
	totalCalls      int64
	totalBytes      int64
	totalLockTime   time.Duration
	totalPwriteTime time.Duration
	totalRemoteTime time.Duration
	lastReport      time.Time
}

var writeStats = &WriteStats{lastReport: time.Now()}

func (ws *WriteStats) record(lockTime, pwriteTime, remoteTime time.Duration, bytes int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.totalCalls++
	ws.totalBytes += int64(bytes)
	ws.totalLockTime += lockTime
	ws.totalPwriteTime += pwriteTime
	ws.totalRemoteTime += remoteTime

	// Report every 100 calls or every 5 seconds
	if ws.totalCalls%100 == 0 || time.Since(ws.lastReport) > 5*time.Second {
		ws.lastReport = time.Now()
		totalTime := ws.totalLockTime + ws.totalPwriteTime + ws.totalRemoteTime
		mbWritten := float64(ws.totalBytes) / 1024 / 1024
		slog.Warn("WRITE STATS",
			"calls", ws.totalCalls,
			"MB", fmt.Sprintf("%.2f", mbWritten),
			"lock_ms", ws.totalLockTime.Milliseconds(),
			"pwrite_ms", ws.totalPwriteTime.Milliseconds(),
			"remote_ms", ws.totalRemoteTime.Milliseconds(),
			"total_ms", totalTime.Milliseconds(),
			"MB/s", fmt.Sprintf("%.2f", mbWritten/(totalTime.Seconds()+0.001)),
		)
	}
}

// The method returns the number of bytes written.
func (d *Dir) Write(path string, buff []byte, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Write", &errCode)
	logger := d.logger.With("method", "write", "path", path, "fh", fh, "offset", offset)

	startTotal := time.Now()

	// Hold lock during write to prevent Release from closing fd mid-write
	startLock := time.Now()
	d.OpenMapLock.RLock()
	lockTime := time.Since(startLock)

	f, ok := d.OpenFileHandlers[fh]
	if !ok {
		d.OpenMapLock.RUnlock()
		// macOS fcopyfile() can call Write after Release - try to reopen and write
		slog.Warn("FCOPYFILE WORKAROUND", "path", path, "fh", fh, "offset", offset, "len", len(buff))
		cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
		if err != nil {
			return -int(syscall.EACCES)
		}
		startOpen := time.Now()
		fd, err := syscall.Open(cleanPath, syscall.O_RDWR, 0)
		openTime := time.Since(startOpen)
		if err != nil {
			slog.Error("FCOPYFILE OPEN FAILED", "error", err, "cleanPath", cleanPath)
			return int(convertOsErrToSyscallErrno("open", err))
		}
		defer syscall.Close(fd)
		startPw := time.Now()
		n, err := syscall.Pwrite(fd, buff, offset)
		pwTime := time.Since(startPw)
		if err != nil {
			slog.Error("FCOPYFILE PWRITE FAILED", "error", err, "fd", fd, "offset", offset, "len", len(buff))
			return int(convertOsErrToSyscallErrno("pwrite", err))
		}
		writeStats.record(openTime, pwTime, 0, n)
		slog.Info("FCOPYFILE OK", "bytes", n, "open_ms", openTime.Milliseconds(), "pwrite_ms", pwTime.Milliseconds())
		return n
	}
	f.HadEdits = true
	f.NotLocalSynced = false  // Local write makes us authoritative - don't read from remote
	f.NotRemoteSynced = true  // File content changed - notify peer on Release with new size
	f.LocalNewer = true

	startPwrite := time.Now()
	n, err := syscall.Pwrite(int(fh), buff, offset)
	pwriteTime := time.Since(startPwrite)

	d.OpenMapLock.RUnlock() // Release AFTER Pwrite to prevent race with Release

	if err != nil {
		// fd reuse race: kernel reused fd number, old FUSE handle matched new map entry
		// but the actual fd was closed. Fallback to fcopyfile workaround.
		// Use errors.Is for robust comparison (handles wrapped errors)
		if errors.Is(err, syscall.EBADF) {
			slog.Warn("EBADF on mapped fd, falling back to fcopyfile workaround", "path", path, "fh", fh)
			cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
			if err != nil {
				return -int(syscall.EACCES)
			}
			fd, err2 := syscall.Open(cleanPath, syscall.O_RDWR, 0)
			if err2 != nil {
				slog.Error("Fallback open failed", "error", err2, "cleanPath", cleanPath)
				return int(convertOsErrToSyscallErrno("open", err2))
			}
			defer syscall.Close(fd)
			n2, err2 := syscall.Pwrite(fd, buff, offset)
			if err2 != nil {
				slog.Error("Fallback pwrite failed", "error", err2)
				return int(convertOsErrToSyscallErrno("pwrite", err2))
			}
			slog.Info("Fallback write OK", "bytes", n2)
			return n2
		}
		logger.Error("Failed to write", "error", err, "fh", fh)
		return int(convertOsErrToSyscallErrno("pwrite", err))
	}

	// Also update RemoteFiles to prevent new handles from reading stale remote data
	startRemote := time.Now()
	d.RemoteFilesLock.Lock()
	if rf, exists := d.RemoteFiles[path]; exists {
		rf.NotLocalSynced = false
		rf.LocalNewer = true
	}
	d.RemoteFilesLock.Unlock()
	remoteTime := time.Since(startRemote)

	// Record stats
	writeStats.record(lockTime, pwriteTime, remoteTime, n)

	// Log slow writes (>10ms)
	totalTime := time.Since(startTotal)
	if totalTime > 10*time.Millisecond {
		logger.Warn("SLOW WRITE",
			"total_ms", totalTime.Milliseconds(),
			"lock_ms", lockTime.Milliseconds(),
			"pwrite_ms", pwriteTime.Milliseconds(),
			"remote_ms", remoteTime.Milliseconds(),
			"bytes", n,
		)
	}

	return n
}

func (d *Dir) Read(path string, buff []byte, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Read", &errCode)
	logger := d.logger.With("method", "read", "path", path, "fh", fh, "offset", offset)

	// Get file info briefly, release lock before I/O (RWMutex is write-preferring)
	d.OpenMapLock.RLock()
	f, ok := d.OpenFileHandlers[fh]
	var stream types.RemoteFileStream
	var realPath string
	var notLocalSynced bool
	var bitmap *ChunkBitmap
	if ok {
		stream = f.RemoteFileStream
		realPath = f.RealPathOfFile
		notLocalSynced = f.NotLocalSynced
		bitmap = f.Bitmap
	}
	d.OpenMapLock.RUnlock()

	logger.Debug("=== READ ===",
		"path", path,
		"ok", ok,
		"notLocalSynced", notLocalSynced,
		"streamNil", stream == nil,
		"hasBitmap", bitmap != nil,
		"fh", fh)

	if ok && notLocalSynced && stream != nil {
		// HYBRID READ: Check bitmap to decide local vs remote.
		// If all chunks for this range are already downloaded (by prefetch or prior read),
		// serve from local cache. Otherwise fetch on-demand from remote.
		if bitmap != nil && bitmap.HasRange(offset, len(buff)) {
			// Fast path: all chunks available locally.
			logger.Debug("Bitmap hit — reading from local cache", "offset", offset, "len", len(buff))
			n, preadErr := syscall.Pread(int(fh), buff, offset)
			if preadErr != nil {
				logger.Error("Local pread failed after bitmap hit", "error", preadErr)
				return int(convertOsErrToSyscallErrno("pread", preadErr))
			}
			return n
		}

		logger.Debug("Reading from remote (on-demand)", "bufLen", len(buff), "progress", f.Download.Progress())

		// Retry loop for resilience against transient failures.
		var data []byte
		var readErr error
		for attempt := 0; attempt < 3; attempt++ {
			// Read from remote stream with timeout to prevent system freeze.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			data, readErr = stream.ReadAt(ctx, offset, int64(len(buff)))
			cancel()

			if readErr == nil {
				break // Success.
			}

			logger.Warn("Remote read failed, attempting retry", "attempt", attempt+1, "error", readErr)
			f.Download.RecordAttempt()

			if !f.Download.CanRetry() {
				logger.Error("Max retries exceeded for remote read", "path", path)
				break
			}

			// Try to re-establish the stream.
			if f.StreamProvider != nil {
				logger.Info("Attempting to re-establish stream", "path", path, "attempt", attempt+1)
				streamCtx, streamCancel := context.WithCancel(context.Background())
				newStream, openErr := f.StreamProvider.OpenRemoteFile(streamCtx, fh, path)
				if openErr != nil {
					streamCancel()
					logger.Error("Failed to re-establish stream", "error", openErr)
					time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond) // Backoff.
					continue
				}

				// Close old stream (best effort).
				if stream != nil {
					_ = stream.Close()
				}
				if f.StreamCancel != nil {
					f.StreamCancel()
				}

				// Update file with new stream (need lock).
				d.OpenMapLock.Lock()
				f.RemoteFileStream = newStream
				f.StreamCancel = streamCancel
				d.OpenMapLock.Unlock()

				stream = newStream
				logger.Info("Stream re-established successfully", "path", path)
			}

			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond) // Backoff.
		}

		if readErr != nil {
			logger.Error("Failed to read data from remote after retries", "error", readErr)
			return -winfuse.EBADF
		}

		// Copy remote data into buffer for FUSE.
		n := copy(buff, data)

		// Track download progress and update checksum.
		f.Download.UpdateProgress(offset, n)
		f.Download.UpdateChecksum(data[:n])

		// Write data into local file for caching (no lock needed - pwrite is atomic).
		lf, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logger.Error("Failed to open local file for writing", "error", err)
			return -winfuse.EIO
		}
		defer lf.Close()

		_, err = lf.WriteAt(data, offset)
		if err != nil {
			logger.Error("Failed to write remote data to local file", "error", err)
			return -winfuse.EIO
		}

		// Mark bitmap for the chunks we just downloaded on-demand.
		if bitmap != nil {
			bitmap.SetRange(offset, n)
		}

		logger.Debug("Read completed", "bytes", n, "progress", f.Download.Progress())
		return n
	}

	// Fallback: read directly from local file
	d.logger.Warn("FUSE Read from LOCAL", "path", path, "offset", offset, "bufLen", len(buff))
	n, err := syscall.Pread(int(fh), buff, offset)
	if err != nil {
		// EBADF fallback: fd may have been closed by fcopyfile race or fd reuse.
		// Reopen the file and read from it.
		if errors.Is(err, syscall.EBADF) {
			d.logger.Warn("EBADF on pread, falling back to reopen", "path", path, "fh", fh)
			cleanPath, err := SecureJoin(d.LocalDownloadFolder, path)
			if err != nil {
				return -int(syscall.EACCES)
			}
			fd, err2 := syscall.Open(cleanPath, syscall.O_RDONLY, 0)
			if err2 != nil {
				logger.Error("Fallback open failed", "error", err2, "cleanPath", cleanPath)
				return int(convertOsErrToSyscallErrno("open", err2))
			}
			defer syscall.Close(fd)
			n2, err2 := syscall.Pread(fd, buff, offset)
			if err2 != nil {
				logger.Error("Fallback pread failed", "error", err2)
				return int(convertOsErrToSyscallErrno("pread", err2))
			}
			d.logger.Info("Fallback read OK", "bytes", n2)
			return n2
		}
		d.logger.Warn("FUSE Read LOCAL FAILED", "path", path, "error", err)
		logger.Error("Failed to read local file", "error", err)
		return int(convertOsErrToSyscallErrno("pread", err))
	}

	d.logger.Warn("FUSE Read SUCCESS", "path", path, "bytesRead", n)
	return n
}

func (d *Dir) Removexattr(path string, name string) (errCode int) {
	defer d.recoverPanic("Removexattr", &errCode)
	// Don't log xattr operations - too frequent
	realPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		return -int(syscall.EACCES)
	}

	err = xattr.Remove(realPath, name)

	if err != nil {
		return int(convertOsErrToSyscallErrno("remove-xattr", err))
	}

	return 0
}

func (d *Dir) Listxattr(path string, fill func(name string) bool) (errCode int) {
	defer d.recoverPanic("Listxattr", &errCode)
	// Don't log xattr operations - too frequent
	realPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		return -int(syscall.EACCES)
	}

	res, err := xattr.List(realPath)
	if err != nil {
		return int(convertOsErrToSyscallErrno("list-xattr", err))
	}

	for _, s := range res {
		// Hide quarantine xattr - macOS Gatekeeper blocks FUSE files with this
		if s == "com.apple.quarantine" {
			continue
		}
		fill(s)
	}

	return 0
}

func (d *Dir) Getxattr(path string, name string) (errCode int, data []byte) {
	defer d.recoverPanic("Getxattr", &errCode)
	logger := d.logger.With("method", "getxattr", "path", path, "name", name)
	logger.Debug("Getxattr called")

	// Block quarantine xattr - macOS Gatekeeper checks this and refuses to open
	// files on FUSE mounts if quarantine is present. Return ENODATA to make
	// macOS think the file isn't quarantined.
	if name == "com.apple.quarantine" {
		logger.Debug("Getxattr blocked quarantine xattr")
		return -int(syscall.ENODATA), nil
	}

	realPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		return -int(syscall.EACCES), nil
	}

	res, err := xattr.Get(realPath, name)
	if err != nil {
		// Only log as warning for unexpected errors (not ENOATTR which is normal)
		logger.Debug("Getxattr failed", "error", err, "realPath", realPath)
		return int(convertOsErrToSyscallErrno("get-xattr", err)), nil
	}

	logger.Debug("Getxattr OK", "dataLen", len(res))
	return 0, res
}

func (d *Dir) Setxattr(path string, name string, value []byte, flags int) (errCode int) {
	defer d.recoverPanic("Setxattr", &errCode)
	// Don't log xattr operations - too frequent
	// I do not support flags for this version.
	_ = flags

	realPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		return -int(syscall.EACCES)
	}

	err = xattr.Set(realPath, name, value)
	if err != nil {
		return int(convertOsErrToSyscallErrno("set-xattr", err))
	}

	return 0
}

// Non-FUSE helper methods, used for keeping track of sync.

// Notes: I am confident that it is not a good idea to use syscall errors for GRPC called methods.

func (d *Dir) AddRemoteFile(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t) error {
	// Normalize to FUSE convention: paths must have leading "/".
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	d.RemoteFilesLock.Lock()

	if existing, ok := d.RemoteFiles[path]; ok {
		logger.Info("Remote file exists, updating", "path", path, "size", stat.Size, "mtime", stat.Mtim.Time())
		existing.stat = stat
		existing.NotLocalSynced = true
		// CRITICAL: Reset download state so BytesDownloaded doesn't carry over from previous attempts
		existing.Download.Reset(uint64(stat.Size))
		// Cancel old prefetch, create new bitmap, start new prefetch.
		if existing.PrefetchCancel != nil {
			existing.PrefetchCancel()
		}
		existing.Bitmap = NewChunkBitmap(stat.Size)
		d.RemoteFilesLock.Unlock()
		d.startPrefetch(logger, existing, path)
		return nil
	}

	logger.Info("Adding remote file", "path", path, "size", stat.Size, "mtime", stat.Mtim.Time())
	cleanDiskPath, err := SecureJoin(d.LocalDownloadFolder, path)
	if err != nil {
		d.RemoteFilesLock.Unlock()
		return err
	}
	f := &File{
		logger:          d.logger,
		openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
		stat:            stat,
		RelativePath:    path,
		IsLocalPresent:  false,
		Name:            name,
		NotLocalSynced:  true,
		StreamProvider:  d.OpenStreamProvider(),
		OnLocalChange:   d.OnLocalChange,
		RealPathOfFile:  cleanDiskPath,
		Bitmap:          NewChunkBitmap(stat.Size),
	}
	d.RemoteFiles[path] = f
	d.RemoteFilesLock.Unlock()
	d.startPrefetch(logger, f, path)
	return nil
}

// startPrefetch pre-allocates the local cache file and launches a background
// goroutine that sequentially downloads all missing chunks from the remote peer.
func (d *Dir) startPrefetch(logger *slog.Logger, f *File, path string) {
	if f.Bitmap == nil {
		// Empty file or size=0 — nothing to prefetch.
		return
	}

	realPath := f.RealPathOfFile
	fileSize := f.Bitmap.FileSize()

	// Pre-allocate local cache file to full size so pwrite at any offset works.
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		logger.Warn("Prefetch: failed to create dirs", "path", realPath, "error", err)
	}
	if err := os.Truncate(realPath, fileSize); err != nil {
		// File may not exist yet — create it.
		lf, createErr := os.Create(realPath)
		if createErr != nil {
			logger.Warn("Prefetch: failed to create cache file", "path", realPath, "error", createErr)
			return
		}
		if truncErr := lf.Truncate(fileSize); truncErr != nil {
			logger.Warn("Prefetch: failed to truncate cache file", "path", realPath, "error", truncErr)
		}
		lf.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.PrefetchCancel = cancel

	go d.prefetchFile(ctx, logger, f, path, realPath)
}

// prefetchFile sequentially downloads all missing chunks from the remote peer.
// It opens its own gRPC stream so it doesn't interfere with on-demand FUSE reads.
func (d *Dir) prefetchFile(ctx context.Context, logger *slog.Logger, f *File, path string, realPath string) {
	bitmap := f.Bitmap
	if bitmap == nil {
		return
	}

	logger = logger.With("prefetch", path)
	logger.Info("Background prefetch starting", "chunks", bitmap.Total(), "fileSize", bitmap.FileSize())

	// Open a dedicated stream for prefetch (separate from FUSE Read streams).
	fsp := d.OpenStreamProvider()
	if fsp == nil {
		logger.Warn("Prefetch: no stream provider available")
		return
	}

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	stream, err := fsp.OpenRemoteFile(streamCtx, 0, path)
	if err != nil {
		logger.Warn("Prefetch: failed to open remote stream", "error", err)
		return
	}
	defer stream.Close()

	// Open local file for writing.
	lf, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logger.Warn("Prefetch: failed to open local file", "error", err)
		return
	}
	// Close the local file and, if the file was renamed while the download was
	// in flight, atomically move the content to the new disk path (Option C fix
	// for the prefetch-rename race: git writes HEAD.lock then renames to HEAD).
	defer func() {
		lf.Close()
		// Only rename if the download completed fully. Partial content must
		// never be placed at the final path.
		if !bitmap.IsComplete() {
			return
		}
		// Read f.RealPathOfFile under RLock — the RENAME_FILE handler writes
		// this field under RemoteFilesLock.Lock(). Release the lock before
		// calling os.Rename so we never hold a lock across a blocking syscall.
		d.RemoteFilesLock.RLock()
		currentDiskPath := f.RealPathOfFile
		d.RemoteFilesLock.RUnlock()
		if currentDiskPath != realPath {
			if mkErr := os.MkdirAll(filepath.Dir(currentDiskPath), 0o755); mkErr != nil {
				logger.Warn("Prefetch: failed to create dirs for renamed path",
					"path", currentDiskPath, "error", mkErr)
			} else if rnErr := os.Rename(realPath, currentDiskPath); rnErr != nil {
				logger.Warn("Prefetch: atomic rename failed",
					"from", realPath, "to", currentDiskPath, "error", rnErr)
			} else {
				logger.Info("Prefetch: atomically moved content to renamed path",
					"from", realPath, "to", currentDiskPath)
			}
		}
	}()

	fileSize := bitmap.FileSize()
	for idx := 0; idx < bitmap.Total(); idx++ {
		select {
		case <-ctx.Done():
			logger.Info("Prefetch cancelled", "progress", bitmap.Progress())
			return
		default:
		}

		if bitmap.Has(idx) {
			continue // Already fetched by on-demand FUSE Read.
		}

		offset := int64(idx) * ChunkSize
		size := int64(ChunkSize)
		if offset+size > fileSize {
			size = fileSize - offset
		}

		readCtx, readCancel := context.WithTimeout(ctx, 15*time.Second)
		data, readErr := stream.ReadAt(readCtx, offset, size)
		readCancel()

		if readErr != nil {
			logger.Warn("Prefetch: read failed", "chunk", idx, "error", readErr)
			// Try to continue with next chunk rather than aborting entirely.
			continue
		}

		_, writeErr := lf.WriteAt(data, offset)
		if writeErr != nil {
			logger.Warn("Prefetch: write failed", "chunk", idx, "error", writeErr)
			continue
		}

		bitmap.Set(idx)
		f.Download.UpdateProgress(offset, len(data))
	}

	if bitmap.IsComplete() {
		logger.Info("Prefetch complete — all chunks downloaded", "fileSize", fileSize)
		f.NotLocalSynced = false
	} else {
		logger.Info("Prefetch finished with gaps", "progress", bitmap.Progress())
	}
}

func (d *Dir) EditRemoteFile(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t) error {
	// Normalize to FUSE convention: paths must have leading "/".
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	d.RemoteFilesLock.Lock()

	f, ok := d.RemoteFiles[path]
	if !ok {
		// LOCAL file edited by remote peer - add to RemoteFiles so we fetch updated version
		logger.Info("Local file edited by remote, marking for sync", "path", path, "mtime", stat.Mtim.Time())
		cleanDiskPath, err := SecureJoin(d.LocalDownloadFolder, path)
		if err != nil {
			d.RemoteFilesLock.Unlock()
			return err
		}
		newFile := &File{
			logger:          d.logger,
			openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
			stat:            stat,
			RelativePath:    path,
			IsLocalPresent:  true,
			Name:            name,
			NotLocalSynced:  true,
			StreamProvider:  d.OpenStreamProvider(),
			OnLocalChange:   d.OnLocalChange,
			RealPathOfFile:  cleanDiskPath,
			Bitmap:          NewChunkBitmap(stat.Size),
		}
		d.RemoteFiles[path] = newFile
		d.RemoteFilesLock.Unlock()
		d.startPrefetch(logger, newFile, path)
		return nil
	}

	if stat.Mtim.Time().Before(f.stat.Mtim.Time()) {
		d.RemoteFilesLock.Unlock()
		logger.Warn("Remote edit rejected - local is newer", "path", path, "remoteMtime", stat.Mtim.Time(), "localMtime", f.stat.Mtim.Time())
		return syscall.ECANCELED
	}

	logger.Info("Remote file edited", "path", path, "mtime", stat.Mtim.Time())
	f.stat = stat
	f.NotLocalSynced = true
	// CRITICAL: Reset download state for re-download
	f.Download.Reset(uint64(stat.Size))
	// Cancel old prefetch, reset bitmap, start new prefetch.
	if f.PrefetchCancel != nil {
		f.PrefetchCancel()
	}
	f.Bitmap = NewChunkBitmap(stat.Size)
	d.RemoteFilesLock.Unlock()
	d.startPrefetch(logger, f, path)
	return nil
}
