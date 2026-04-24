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

//go:build !android

package filesystem

import (
	"context"
	"errors"
	// "fmt"
	"io"
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
	// logger := d.logger.With("method", "access", "path", path, "mask", _mask)
	// logger.Debug("Access called")

	d.RemoteFilesLock.RLock()
	_, ok := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()
	if ok {
		// logger.Debug("Access OK (remote file)")
		return 0
	}

	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	_, err := platStat(realPath)
	if err != nil {
		// logger.Warn("Access FAILED", "error", err, "realPath", realPath)
		return int(convertOsErrToSyscallErrno("stat", err))
	}

	// logger.Debug("Access OK (local file)")
	return 0
}

func (d *Dir) Chmod(path string, mode uint32) (errCode int) {
	defer d.recoverPanic("Chmod", &errCode)

	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	if err := platChmod(cleanPath, mode); err != nil {
		if !os.IsNotExist(err) {
			return int(convertOsErrToSyscallErrno("chmod", err))
		}
	}

	now := winfuse.NewTimespec(time.Now())
	applyMode := func(st *winfuse.Stat_t) {
		st.Mode = (st.Mode & winfuse.S_IFMT) | (mode & ^uint32(winfuse.S_IFMT))
		st.Ctim = now
	}

	d.AfmLock.Lock()
	if f, ok := d.AllFileMap[path]; ok && f.stat != nil {
		applyMode(f.stat)
		stat := f.stat
		d.AfmLock.Unlock()
		if d.OnLocalChange != nil {
			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.EditFile,
				Attr:   types.StatToAttr(stat),
			})
		}
		return 0
	}
	d.AfmLock.Unlock()

	d.Adm.Lock()
	if dir, ok := d.AllDirMap[path]; ok && dir.stat != nil {
		applyMode(dir.stat)
		stat := dir.stat
		d.Adm.Unlock()
		if d.OnLocalChange != nil {
			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.EditDir,
				Attr:   types.StatToAttr(stat),
			})
		}
		return 0
	}
	d.Adm.Unlock()

	d.RemoteFilesLock.Lock()
	if rf, ok := d.RemoteFiles[path]; ok && rf.stat != nil {
		applyMode(rf.stat)
	}
	d.RemoteFilesLock.Unlock()

	return 0
}

func (d *Dir) Chown(path string, uid uint32, gid uint32) (errCode int) {
	defer d.recoverPanic("Chown", &errCode)

	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	chownUid, chownGid := int(uid), int(gid)
	if uid == ^uint32(0) {
		chownUid = -1
	}
	if gid == ^uint32(0) {
		chownGid = -1
	}
	_ = platChown(cleanPath, chownUid, chownGid)

	now := winfuse.NewTimespec(time.Now())
	applyOwner := func(st *winfuse.Stat_t) {
		if uid != ^uint32(0) {
			st.Uid = uid
			st.Mode &^= winfuse.S_ISUID | winfuse.S_ISGID
		}
		if gid != ^uint32(0) {
			st.Gid = gid
		}
		st.Ctim = now
	}

	d.AfmLock.Lock()
	if f, ok := d.AllFileMap[path]; ok && f.stat != nil {
		applyOwner(f.stat)
	}
	d.AfmLock.Unlock()

	d.Adm.Lock()
	if dir, ok := d.AllDirMap[path]; ok && dir.stat != nil {
		applyOwner(dir.stat)
	}
	d.Adm.Unlock()

	d.RemoteFilesLock.Lock()
	if rf, ok := d.RemoteFiles[path]; ok && rf.stat != nil {
		applyOwner(rf.stat)
	}
	d.RemoteFilesLock.Unlock()

	return 0
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
	// accessMode := flags & winfuse.O_ACCMODE
	// logger.Info("Create called", "flags", flags, "accessMode", accessMode, "mode", mode, "isRDWR", accessMode == winfuse.O_RDWR)

	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()
	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	relativePath := path
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	// Extract permission bits only (mode may include S_IFREG file type bits).
	// Ensure owner has write permission so file can be reopened after close.
	createMode := mode & 0o777
	if createMode&0o200 == 0 {
		createMode |= 0o200 // Add owner write permission
	}
	if createMode == 0 {
		createMode = 0o644 // Default if mode was 0
	}

	fd, err := platOpen(path, flags, createMode)
	if err != nil {
		logger.Error("Failed to create file", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	name := strings.Split(path, "/")

	f := &File{
		logger:          logger,
		openFileCounter: OpenFileCounter{mu: &sync.Mutex{}, counter: 1},
		Inode:           uint64(fd),
		Name:            name[len(name)-1],
		RelativePath:    relativePath,
		RealPathOfFile:  path,
		OnLocalChange:   d.OnLocalChange,
		StreamProvider:  d.OpenStreamProvider(),
		NotRemoteSynced: true,
		IsLocalPresent:  true, // File was just created locally
		LocalNewer:      true, // Local version is the only version
	}

	if stgo, statErr := platLstat(path); statErr == nil {
		st := new(winfuse.Stat_t)
		*st = stgo
		uid, gid, _ := winfuse.Getcontext()
		st.Uid = uid
		st.Gid = gid
		f.stat = st
	}

	handleID := allocHandleID()
	f.CurrentHandleID = handleID
	d.AllFileMap[relativePath] = f
	d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: f}

	// logger.Info("Created file", "fd", fd)
	return 0, handleID
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
	// d.logger.Info("FUSE create", "path", path, "mode", mode)
	logger := d.logger.With("method", "create-ex", "path", path)

	flags := fi.Flags
	// On Windows, WinFSP does not include O_CREAT in fi.Flags for CreateEx
	// because the "create" semantics are implicit in the call itself.
	// Ensure O_CREAT is set so platOpen actually creates the file.
	flags |= syscall.O_CREAT
	// accessMode := flags & winfuse.O_ACCMODE
	// logger.Info("CreateEx called", "flags", flags, "accessMode", accessMode, "mode", mode)

	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()
	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	relativePath := path
	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

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

	fd, err := platOpen(localPath, flags, createMode)
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

	if stgo, statErr := platLstat(localPath); statErr == nil {
		st := new(winfuse.Stat_t)
		*st = stgo
		uid, gid, _ := winfuse.Getcontext()
		st.Uid = uid
		st.Gid = gid
		f.stat = st
	}

	handleID := allocHandleID()
	f.CurrentHandleID = handleID
	d.AllFileMap[relativePath] = f
	d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: f}

	// Set per-file direct_io
	fi.Fh = handleID
	fi.DirectIo = shouldUseDirectIo(path, flags)
	// logger.Info("Created file", "fd", fd, "directIo", fi.DirectIo)
	return 0
}

// OpenEx implements FileSystemOpenEx interface for per-file direct_io control.
func (d *Dir) OpenEx(path string, fi *winfuse.FileInfo_t) (errCode int) {
	defer d.recoverPanic("OpenEx", &errCode)
	flags := fi.Flags
	// d.logger.Info("FUSE open", "path", path, "flags", flags)
	logger := d.logger.With("method", "open-ex", "path", path, "flags", flags)

	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

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
			// logger.Debug("File not found", "localPath", localPath)
			return -winfuse.ENOENT
		}
	}

	// File already opened - return existing handle
	if fh.openFileCounter.CountOpenDescriptors() != 0 {
		handleID := fh.CurrentHandleID
		d.AfmLock.Unlock()
		fi.Fh = handleID
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
		// logger.Info("=== REMOTE CHECK ===", "path", path, "hasRemote", hasRemote, "NotLocalSynced", remoteFile.NotLocalSynced)
		if remoteFile.stat != nil {
			remoteTotalSize = uint64(remoteFile.stat.Size)
		}
		if remoteFile.NotLocalSynced {
			// logger.Info("Remote has newer version, streaming from remote", "path", path)
			localNewer = false
			remoteHasUpdate = true
		} else if remoteTotalSize > 0 {
			// Even if NotLocalSynced=false, verify download is actually complete.
			// IMPORTANT: Can't use file size because pre-allocation makes file look complete.
			// Use Download.BytesDownloaded which tracks actual bytes received.
			bytesDownloaded := remoteFile.Download.BytesDownloaded.Load()
			if bytesDownloaded < remoteTotalSize {
				// logger.Info("Download incomplete, re-streaming from remote", "bytesDownloaded", bytesDownloaded, "remoteSize", remoteTotalSize)
				localNewer = false
				remoteHasUpdate = true
				needsRemark = true
			}
		}
	} else {
		// logger.Info("=== REMOTE CHECK ===", "path", path, "hasRemote", false)
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
		// logger.Info("Opening local file", "flags", flags, "accessMode", accessMode, "localPath", localPath)

		// Verify file actually exists before trying to open
		if _, statErr := os.Stat(localPath); statErr != nil {
			// logger.Warn("File marked as local but doesn't exist on disk", "error", statErr, "localPath", localPath)
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

			fd, err := platOpen(localPath, sysFlags, 0)
			if err != nil {
				logger.Error("Failed to open local file", "error", err, "sysFlags", sysFlags)
				return int(convertOsErrToSyscallErrno("open", err))
			}

			d.AfmLock.Lock()
			d.OpenMapLock.Lock()
			fh.Inode = uint64(fd)
			fh.openFileCounter.Open()
			handleID := allocHandleID()
			fh.CurrentHandleID = handleID
			d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: fh}
			d.OpenMapLock.Unlock()
			d.AfmLock.Unlock()

			fi.Fh = handleID
			fi.DirectIo = shouldUseDirectIo(path, flags)
			// logger.Info("Opened local file", "fh", fd, "directIo", fi.DirectIo)
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

		fd, err := platOpen(localPath, sysFlags, 0644)
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
		handleID := allocHandleID()
		fh.CurrentHandleID = handleID
		d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: fh}
		d.OpenMapLock.Unlock()
		d.AfmLock.Unlock()

		fi.Fh = handleID
		fi.DirectIo = shouldUseDirectIo(path, flags)
		// logger.Info("Created file via OpenEx", "fh", fd, "directIo", fi.DirectIo)
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
		// logger.Info("Found existing partial download", "existingSize", existingLocalSize)
	}

	// Create parent directories
	parentDir := filepath.Dir(localPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		logger.Error("Failed to create parent directories", "error", err)
		return -winfuse.EIO
	}

	fd, err := platOpen(localPath, syscall.O_RDWR|syscall.O_CREAT, 0644)
	if err != nil {
		logger.Error("Failed to create cache file", "error", err)
		return int(convertOsErrToSyscallErrno("open", err))
	}

	// Open stream pool + cache FD for reading (network call - no locks held)
	var pool *StreamPool
	var streamCancel context.CancelFunc
	var cacheFD *os.File
	if remoteHasUpdate {
		fsp := d.OpenStreamProvider()
		var streamCtx context.Context
		streamCtx, streamCancel = context.WithCancel(context.Background())
		pool, err = NewStreamPool(fsp, streamCtx, uint64(fd), path, StreamPoolSize) // on-demand jumps; prefetch uses StreamFile separately
		if err != nil {
			streamCancel()
			platClose(fd)
			logger.Error("Failed to open stream pool", "error", err)
			return -winfuse.EACCES
		}
		// Open persistent cache FD for on-demand writes (avoids open/close per chunk).
		cacheFD, err = os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			streamCancel()
			pool.Close()
			platClose(fd)
			logger.Error("Failed to open cache FD", "error", err)
			return -winfuse.EIO
		}
	}

	d.AfmLock.Lock()
	d.OpenMapLock.Lock()
	fh.Inode = uint64(fd)
	fh.openFileCounter.Open()
	fh.IsLocalPresent = true
	handleID := allocHandleID()
	fh.CurrentHandleID = handleID
	d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: fh}

	if remoteHasUpdate {
		fh.NotLocalSynced = true
		fh.StreamPool = pool
		fh.StreamCancel = streamCancel
		fh.CacheFD = cacheFD
		fh.Download.Reset(remoteTotalSize)

		// Pre-allocate local file to expected size for out-of-order writes.
		// Without this, non-sequential reads (e.g., video moov atom at end) can cause corruption.
		// macOS reads video files non-sequentially (header, then trailer at end, then content).
		if existingLocalSize == 0 && remoteTotalSize > 0 {
			if truncErr := os.Truncate(localPath, int64(remoteTotalSize)); truncErr != nil {
				// logger.Warn("Failed to pre-allocate local file", "size", remoteTotalSize, "error", truncErr)
				_ = truncErr
			} else {
				// logger.Info("Pre-allocated local file", "size", remoteTotalSize)
			}
		}

		// NOTE: We intentionally do NOT use local file size for resume tracking.
		// Pre-allocation creates a full-size file with zeros, which would falsely
		// indicate the download is complete. We only trust Download.BytesDownloaded
		// which is tracked explicitly by Read() operations.
	}
	d.OpenMapLock.Unlock()
	d.AfmLock.Unlock()



	fi.Fh = handleID
	fi.DirectIo = shouldUseDirectIo(path, flags)
	// logger.Info("Opened for remote streaming", "fh", fd, "directIo", fi.DirectIo, "remoteHasUpdate", remoteHasUpdate, "fhNotLocalSynced", fh.NotLocalSynced, "poolNil", fh.StreamPool == nil)
	return 0
}

// Called on unmount.
func (d *Dir) Destroy() {
	// d.logger.Info("Destroy")
}

func (d *Dir) Flush(path string, fh uint64) (errCode int) {
	defer d.recoverPanic("Flush", &errCode)
	// d.logger.Debug("FUSE Flush (stub)", "path", path, "fh", fh)
	return 0 // Return success - actual sync happens in Fsync/Release.
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) (errCode int) {
	defer d.recoverPanic("Fsync", &errCode)
	// d.logger.Info("FUSE fsync", "path", path, "fh", fh)
	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "fsync", "path", localPath)

	d.OpenMapLock.RLock()
	entry, ok := d.OpenFileHandlers[fh]
	d.OpenMapLock.RUnlock()

	if ok {
		err := platFsync(entry.FD)
		if err == nil {
			return 0
		}
		if !errors.Is(err, syscall.EBADF) {
			logger.Error("Fsync failed", "error", err)
			return int(convertOsErrToSyscallErrno("fsync", err))
		}
	}

	// Handle not found or EBADF: fd was already closed (Release called before Fsync).
	// This is a FUSE race condition. Workaround: open, fsync, close.
	{
		// d.logger.Warn("FUSE Fsync EBADF - attempting fallback open/fsync/close", "path", localPath, "fh", fh)

		fd, openErr := platOpen(localPath, syscall.O_RDONLY, 0)
		if openErr != nil {
			// File might have been renamed/deleted - that's OK, data was already written
			// d.logger.Warn("FUSE Fsync fallback open failed (file may have been renamed)", "path", localPath, "error", openErr)
			return 0 // Return success - the data was committed before close
		}

		fsyncErr := platFsync(fd)
		platClose(fd)

		if fsyncErr != nil {
			// d.logger.Warn("FUSE Fsync fallback fsync failed", "path", localPath, "error", fsyncErr)
			return int(convertOsErrToSyscallErrno("fsync", fsyncErr))
		}

		// d.logger.Warn("FUSE Fsync fallback SUCCESS", "path", localPath)
		return 0
	}
}

func (d *Dir) Fsyncdir(path string, datasync bool, fh uint64) (errCode int) {
	defer d.recoverPanic("Fsyncdir", &errCode)
	// d.logger.Debug("FUSE Fsyncdir (stub)", "path", path, "datasync", datasync, "fh", fh)
	return 0 // Return success - directory syncs are no-ops for our use case.
}

func (d *Dir) Getattr(path string, stat *winfuse.Stat_t, fh uint64) (errCode int) {
	defer d.recoverPanic("Getattr", &errCode)
	// NOTE: Do NOT log getattr — called 200,000+ times during large clones.
	// Synchronous logging on this hot path causes 30-second hangs.
	logger := d.logger.With("method", "get-attr", "path", path, "fh", fh)

	// Hide .kdbitmap sidecar files from FUSE — they're internal download state.
	if strings.HasSuffix(path, ".kdbitmap") {
		return -winfuse.ENOENT
	}

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

	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	// Check if the file is on remote, and add it to local tree.
	if isRemote {
		remFile, okRemote := d.RemoteFiles[path]
		if okRemote {
			// File is on remote, let's see if it is also locally.
			stgo, lstatErr := platLstat(cleanPath)
			if lstatErr != nil {
				// Ok file not locally. Just add it, and download it on Open.
				copyFusestatFromFusestat(stat, remFile.stat)
				d.AllFileMap[path] = remFile
				// All good.
				return 0
			}

			// Ok, file is also locally present, but we already got the pointer to it.
			// Let's see if the stats are ok.

			auxStat := &stgo

			if isModificationTimeNewer(auxStat, remFile.stat) {
				if auxStat.Size > 0 {
					savedUid := remFile.stat.Uid
					savedGid := remFile.stat.Gid
					savedMode := remFile.stat.Mode
					copyFusestatFromFusestat(remFile.stat, auxStat)
					remFile.stat.Uid = savedUid
					remFile.stat.Gid = savedGid
					if !platDiskModeIsAuthoritative {
						remFile.stat.Mode = savedMode
					}
				}
				copyFusestatFromFusestat(stat, remFile.stat)
				return 0
			}

			copyFusestatFromFusestat(stat, remFile.stat)
			return 0
		}
	}

	stgo, lstatErr := platLstat(cleanPath)
	if lstatErr != nil {
		// ENOENT is normal — macOS probes hundreds of paths (Spotlight, fsevents,
		// .DS_Store, etc.). Only log unexpected errors.
		if !os.IsNotExist(lstatErr) {
			logger.Error("Failed to lstat path", "clean-path", cleanPath, "error", lstatErr)
		}
		cerr := convertOsErrToSyscallErrno("lstat", lstatErr)
		return int(cerr)
	}

	// Note: We do not use Lampert timestamps, last edit wins.

	*stat = stgo
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
		savedUid := f.stat.Uid
		savedGid := f.stat.Gid
		savedMode := f.stat.Mode
		copyFusestatFromFusestat(f.stat, stat)
		f.stat.Uid = savedUid
		f.stat.Gid = savedGid
		if !platDiskModeIsAuthoritative {
			f.stat.Mode = savedMode
		}
		copyFusestatFromFusestat(stat, f.stat)
	}
	if ok {
		found = ok
	}

	// TODO: Sigh, refactor later.

	// File not found in tree.

	// In an ideal world: do not stat again :<.
	finfo, err := os.Stat(cleanPath)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error("Failed to determine if dir or file", "error", err)
		}
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
				OpenFileHandlers:    make(map[uint64]*HandleEntry),
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
	// d.logger.Info("Init", "inode", d.Inode)
	// syscall.Chdir(d.LocalDownloadFolder)

}

func (d *Dir) Link(oldpath string, newpath string) (errCode int) {
	defer d.recoverPanic("Link", &errCode)
	// d.logger.Debug("FUSE Link (stub - not supported)", "oldPath", oldpath, "newPath", newpath, "inode", d.Inode)
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
	// d.logger.Info("FUSE mkdir", "path", path, "mode", mode, "notifyPeer", notifyPeer)
	logger := d.logger.With("method", "mkdir", "path", path, "mode", mode)
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := platMkdir(cleanPath, mode)
	if err != nil {
		logger.Error("Failed to mkdir", "path", cleanPath, "error", err)
		return int(convertOsErrToSyscallErrno("mkdir", err))
	}

	if stgo, statErr := platLstat(cleanPath); statErr == nil {
		st := new(winfuse.Stat_t)
		*st = stgo
		if notifyPeer {
			uid, gid, _ := winfuse.Getcontext()
			st.Uid = uid
			st.Gid = gid
		}

		d.Adm.Lock()
		dir := &Dir{
			logger:              logger,
			Adm:                 sync.RWMutex{},
			AfmLock:             sync.RWMutex{},
			Inode:               st.Ino,
			RelativePath:        path,
			LocalDownloadFolder: cleanPath,
			IsLocalPresent:      true,
			Root:                d,
			OpenFileHandlers:    make(map[uint64]*HandleEntry),
			OpenMapLock:         sync.RWMutex{},
			AllDirMap:           make(map[string]*Dir),
			AllFileMap:          make(map[string]*File),
			stat:                st,
			OnLocalChange:       d.OnLocalChange,
			OpenStreamProvider:  d.OpenStreamProvider,
			RemoteFilesLock:     sync.RWMutex{},
			RemoteFiles:         make(map[string]*File),
		}
		d.AllDirMap[path] = dir
		d.Adm.Unlock()

		if notifyPeer && d.OnLocalChange != nil {
			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.AddDir,
				Attr:   types.StatToAttr(st),
			})
		}
	}

	return 0
}

func (d *Dir) Mknod(path string, mode uint32, dev uint64) (errCode int) {
	defer d.recoverPanic("Mknod", &errCode)
	// d.logger.Info("Mknod", "path", path, "inode", d.Inode)

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "mknod", "path", path, "mode", mode, "dev", dev)
	err := platMknod(path, mode, int(dev))
	if err != nil {
		logger.Error("Failed to mknor", "errro", err)
		return int(convertOsErrToSyscallErrno("mknod", err))
	}
	return 0
}

func (d *Dir) Open(path string, flags int) (errCode int, retFh uint64) {
	defer d.recoverPanic("Open", &errCode)
	// d.logger.Info("FUSE open(legacy)", "path", path, "flags", flags)
	logger := d.logger.With("method", "open", "path", path, "flags", flags)

	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

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
		handleID := fh.CurrentHandleID
		d.AfmLock.Unlock()
		return 0, handleID
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
			// logger.Info("Remote has newer version, streaming from remote", "path", path)
			localNewer = false
			remoteHasUpdate = true
		} else if remoteTotalSize > 0 {
			// Even if NotLocalSynced=false, verify download is actually complete.
			// Use bitmap (preferred) or BytesDownloaded as fallback.
			if remoteBitmap != nil && !remoteBitmap.IsComplete() {
				// logger.Info("Download incomplete (bitmap), re-streaming from remote", "progress", remoteBitmap.Progress(), "remoteSize", remoteTotalSize)
				localNewer = false
				remoteHasUpdate = true
				needsRemark = true
			} else if remoteBitmap == nil {
				bytesDownloaded := remoteFile.Download.BytesDownloaded.Load()
				if bytesDownloaded < remoteTotalSize {
					// logger.Info("Download incomplete, re-streaming from remote", "bytesDownloaded", bytesDownloaded, "remoteSize", remoteTotalSize)
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
		// accessMode := flags & winfuse.O_ACCMODE
		// logger.Info("Opening local file", "flags", flags, "accessMode", accessMode, "isReadOnly", accessMode == winfuse.O_RDONLY)
		fd, err := platOpen(localPath, flags, 0)
		if err != nil {
			logger.Error("Failed to open local file", "error", err)
			return int(convertOsErrToSyscallErrno("open", err)), 0
		}

		d.AfmLock.Lock()
		d.OpenMapLock.Lock()
		fh.Inode = uint64(fd)
		fh.openFileCounter.Open()
		handleID := allocHandleID()
		fh.CurrentHandleID = handleID
		d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: fh}
		d.OpenMapLock.Unlock()
		d.AfmLock.Unlock()
		// logger.Info("Opened local file", "fh", fd)
		return 0, handleID
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
			// logger.Info("Found partial download, will resume", "localSize", existingLocalSize, "remoteSize", remoteTotalSize)
		}
	}

	// accessMode := flags & winfuse.O_ACCMODE
	// logger.Info("Opening remote cache file", "flags", flags, "accessMode", accessMode, "isReadOnly", accessMode == winfuse.O_RDONLY)
	fd, err := platOpen(localPath, flags, 0)
	if err != nil {
		logger.Error("Failed to open path", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	// Open stream pool + cache FD (network call - no locks held)
	fsp := d.OpenStreamProvider()
	streamCtx, streamCancel := context.WithCancel(context.Background())
	pool, poolErr := NewStreamPool(fsp, streamCtx, uint64(fd), path, StreamPoolSize) // on-demand jumps; prefetch uses StreamFile separately
	if poolErr != nil {
		streamCancel()
		platClose(fd)
		logger.Error("Failed to open stream pool", "error", poolErr)
		return -winfuse.EACCES, 0
	}
	// Open persistent cache FD for on-demand writes (avoids open/close per chunk).
	cacheFD, cacheErr := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY, 0644)
	if cacheErr != nil {
		streamCancel()
		pool.Close()
		platClose(fd)
		logger.Error("Failed to open cache FD", "error", cacheErr)
		return -winfuse.EIO, 0
	}

	d.AfmLock.Lock()
	d.OpenMapLock.Lock()
	fh.Inode = uint64(fd)
	fh.StreamProvider = fsp
	fh.StreamPool = pool
	fh.StreamCancel = streamCancel
	fh.CacheFD = cacheFD
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
				// logger.Warn("Failed to pre-allocate local file", "size", remoteTotalSize, "error", truncErr)
				_ = truncErr
			} else {
				// logger.Info("Pre-allocated local file", "size", remoteTotalSize)
			}
		}
	}
	handleID := allocHandleID()
	fh.CurrentHandleID = handleID
	d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: fh}
	fh.openFileCounter.Open()
	d.OpenMapLock.Unlock()
	d.AfmLock.Unlock()

	// logger.Info("Opened remote file", "fh", handleID, "notLocalSynced", fh.NotLocalSynced, "poolNil", fh.StreamPool == nil, "totalSize", remoteTotalSize)
	return 0, handleID
}

func (d *Dir) Opendir(path string) (errCode int, retFh uint64) {
	defer d.recoverPanic("Opendir", &errCode)
	// d.logger.Info("FUSE opendir", "path", path)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "opendir", "path", path)
	f, err := platOpen(path, syscall.O_RDONLY|platO_DIRECTORY, 0)
	if err != nil {
		logger.Error("Failed to open dir", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	return 0, uint64(f)
}

func (d *Dir) Readdir(path string, fill func(name string, stat *winfuse.Stat_t, offset int64) bool, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Readdir", &errCode)
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
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
		name := dir.Name()
		if strings.HasSuffix(name, ".kdbitmap") {
			continue // Hide download progress sidecar files from FUSE readdir.
		}
		localFiles[name] = struct{}{}
		if !fill(name, nil, 0) {
			break
		}
	}

	// Add remote files/dirs that don't exist locally, filtered to direct children of this path.
	d.RemoteFilesLock.RLock()
	defer d.RemoteFilesLock.RUnlock()
	remoteFiles, remoteDirs := remoteChildrenForDir(d.RemoteFiles, path)
	for name := range remoteFiles {
		if _, exists := localFiles[name]; !exists {
			fill(name, nil, 0)
		}
	}
	for name := range remoteDirs {
		if _, exists := localFiles[name]; !exists {
			fill(name, nil, 0)
		}
	}

	// d.logger.Info("FUSE readdir", "path", path, "local", len(localFiles), "remoteFiles", len(remoteFiles), "remoteDirs", len(remoteDirs))
	return 0
}

func (d *Dir) Readlink(path string) (errCode int, target string) {
	defer d.recoverPanic("Readlink", &errCode)
	// d.logger.Debug("FUSE Readlink (stub)", "path", path, "inode", d.Inode)
	// No symlinks in our filesystem - return EINVAL (not a symlink).
	return -winfuse.EINVAL, ""
}

func (d *Dir) Release(path string, fh uint64) (errCode int) {
	defer d.recoverPanic("Release", &errCode)
	// d.logger.Info("FUSE release", "path", path, "fh", fh)
	logger := d.logger.With("method", "release", "path", path, "fh", fh)

	d.OpenMapLock.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			d.OpenMapLock.Unlock()
		}
	}()

	entry, ok := d.OpenFileHandlers[fh]
	if !ok {
		// handle not in map - either already released, or was a late fcopyfile handle
		// Don't try to close - just return success
		// logger.Warn("Release called for unknown fh (already released or fcopyfile race)")
		return 0
	}
	f := entry.File

	v := f.openFileCounter.Release()

	// Debug: log ALL relevant state at Release entry
	// logger.Info("=== RELEASE ===",
	// 	"path", path,
	// 	"openCount", v,
	// 	"NotRemoteSynced", f.NotRemoteSynced,
	// 	"HadEdits", f.HadEdits,
	// 	"IsLocalPresent", f.IsLocalPresent,
	// 	"LocalNewer", f.LocalNewer)

	if v == 0 {
		err := platClose(entry.FD)
		if err != nil {
			logger.Error("Failed to close fd", "error", err)
			return int(convertOsErrToSyscallErrno("release", err))
		}

		delete(d.OpenFileHandlers, fh)

		// SINGLE notification path: notify peer if file was created OR edited locally
		needsNotify := (f.NotRemoteSynced || f.HadEdits) && d.OnLocalChange != nil

		if needsNotify {
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			stgo, lstatErr := platLstat(cleanPath)
			if lstatErr != nil {
				// File was deleted between close and notification — skip notification
				return 0
			}

			// logger.Info("Release lstat result", "path", path, "size", stgo.Size)

			if !f.HadEdits {
				// No FUSE Write calls seen. Data arrived via fcopyfile (Finder
				// drag-and-drop) which bypasses FUSE Write entirely. The lstat
				// size may be partial because fcopyfile is still flushing.
				// Defer notification with exponential back-off until the file
				// size stabilizes (two consecutive lstats return the same size).
				go func() {
					// Wait for fcopyfile to finish. Require 3 consecutive lstats
					// with the same size, starting after a 500ms initial delay.
					// fcopyfile on large files can pause between writes, so 2
					// matches isn't enough (469 MB -> 469 MB -> then 611 MB).
					time.Sleep(500 * time.Millisecond)
					prevSize := int64(-1)
					stableCount := 0
					for attempt := 0; attempt < 15; attempt++ {
						delay := time.Duration(300+attempt*100) * time.Millisecond
						time.Sleep(delay)
						if f.openFileCounter.CountOpenDescriptors() > 0 {
							return
						}
						recheckStat, recheckErr := platLstat(cleanPath)
						if recheckErr != nil {
							return
						}
						if recheckStat.Size == prevSize {
							stableCount++
							if stableCount >= 2 {
								d.OnLocalChange(types.FileEvent{
									Path:   path,
									Action: types.AddFile,
									Attr:   types.StatToAttr(&recheckStat),
								})
								f.NotRemoteSynced = false
								f.HadEdits = false
								return
							}
						} else {
							stableCount = 0
						}
						prevSize = recheckStat.Size
					}
					// Timed out waiting for stable size. Send what we have.
					finalStat, finalErr := platLstat(cleanPath)
					if finalErr != nil {
						return
					}
					d.OnLocalChange(types.FileEvent{
						Path:   path,
						Action: types.AddFile,
						Attr:   types.StatToAttr(&finalStat),
					})
					f.NotRemoteSynced = false
					f.HadEdits = false
				}()
			} else {
				d.OnLocalChange(types.FileEvent{
					Path:   path,
					Action: types.AddFile,
					Attr:   types.StatToAttr(&stgo),
				})

				f.NotRemoteSynced = false
				f.HadEdits = false
			}
		}

		// Reset sync state for future opens
		f.IsLocalPresent = true
		// Only mark as synced if download is actually COMPLETE.
		// Use ChunkBitmap (preferred) or BytesDownloaded as fallback.
		if f.Bitmap != nil {
			if f.Bitmap.IsComplete() {
				f.NotLocalSynced = false
				// logger.Info("Download complete (all chunks verified)", "progress", f.Bitmap.Progress())
			} else {
				// logger.Info("Download incomplete, keeping NotLocalSynced=true", "progress", f.Bitmap.Progress())
			}
		} else {
			// Fallback for files without bitmap (local-origin or legacy).
			expectedSize := f.Download.TotalSize.Load()
			bytesDownloaded := f.Download.BytesDownloaded.Load()
			if expectedSize > 0 && bytesDownloaded >= expectedSize {
				f.NotLocalSynced = false
				// logger.Info("Download complete (bytes check)", "bytesDownloaded", bytesDownloaded, "expectedSize", expectedSize)
			} else if expectedSize > 0 {
				// logger.Info("Download incomplete, keeping NotLocalSynced=true", "bytesDownloaded", bytesDownloaded, "expectedSize", expectedSize)
			} else {
				// No expected size tracked (local file, not from remote) - mark as synced
				cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
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
			// logger.Info("Removed file reference after download completed (peer stopped sharing)", "path", path)
		}

		// Get pool/cacheFD/cancel references, clear under lock, then close OUTSIDE lock
		// to avoid holding OpenMapLock during network I/O.
		pool := f.StreamPool
		streamCancel := f.StreamCancel
		cacheFD := f.CacheFD
		f.StreamPool = nil
		f.StreamCancel = nil
		f.CacheFD = nil
		if pool != nil || cacheFD != nil {
			d.OpenMapLock.Unlock()
			unlocked = true
			if cacheFD != nil {
				f.CacheWg.Wait() // Wait for in-flight async cache writes to finish.
				if closeErr := cacheFD.Close(); closeErr != nil {
					logger.Error("Failed to close cache FD", "error", closeErr)
				}
			}
			if pool != nil {
				if closeErr := pool.Close(); closeErr != nil {
					logger.Error("Failed to close stream pool", "error", closeErr)
				}
			}
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
	// d.logger.Info("Releasedir", "path", path, "inode", d.Inode, "fh", fh)
	logger := d.logger.With("method", "release-dir", "path", path, "fh", fh)
	err := platClose(int(fh))
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
	// d.logger.Info("FUSE rename", "old", oldpath, "new", newpath)
	// d.logger.Warn("FUSE Rename called",
	// 	"oldpath", oldpath,
	// 	"newpath", newpath,
	// 	"note", "macOS apps may use RENAME_SWAP - not supported by cgofuse")

	cleanOldPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, oldpath))
	cleanNewPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, newpath))
	logger := d.logger.With("method", "rename", "old-path", cleanOldPath, "new-path", cleanNewPath)

	// d.logger.Warn("FUSE Rename resolved paths", "cleanOldPath", cleanOldPath, "cleanNewPath", cleanNewPath)

	// Check if this is a remote-only file (don't notify peer about their own files).
	d.RemoteFilesLock.RLock()
	_, isRemote := d.RemoteFiles[oldpath]
	d.RemoteFilesLock.RUnlock()

	err := syscall.Rename(cleanOldPath, cleanNewPath)
	if err != nil {
		// d.logger.Warn("FUSE Rename FAILED", "oldpath", oldpath, "newpath", newpath, "error", err)
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
		// logger.Info("Updated AllFileMap for rename", "oldpath", oldpath, "newpath", newpath)
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
		// logger.Info("Updated RemoteFiles for rename", "oldpath", oldpath, "newpath", newpath)
	}
	d.RemoteFilesLock.Unlock()

	// Notify peer about the rename (only for local files, not remote-only).
	if d.OnLocalChange != nil && !isRemote {
		// Get stat of the renamed file.
		var attr *keibidrop.Attr
		if stgo, statErr := platLstat(cleanNewPath); statErr == nil {
			attr = types.StatToAttr(&stgo)
		}

		d.OnLocalChange(types.FileEvent{
			Path:    newpath,
			OldPath: oldpath,
			Action:  types.RenameFile,
			Attr:    attr,
		})
		// logger.Info("Notified peer about rename", "oldpath", oldpath, "newpath", newpath)
	}

	// d.logger.Warn("FUSE Rename SUCCESS", "oldpath", oldpath, "newpath", newpath)
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
	// d.logger.Info("FUSE rmdir", "path", path, "notifyPeer", notifyPeer)
	logger := d.logger.With("method", "rmdir", "path", path)

	// Check if this is a remote-only directory (track if we removed it from map).
	d.Adm.Lock()
	_, isRemoteDir := d.AllDirMap[path]
	if isRemoteDir {
		delete(d.AllDirMap, path)
		// logger.Info("Removed directory from AllDirMap", "path", path)
	}
	d.Adm.Unlock()

	// Try to rmdir local directory (may not exist if remote-only).
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := syscall.Rmdir(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Directory doesn't exist locally.
			if isRemoteDir {
				// Remote-only dir - we already cleaned up the map, success.
				// logger.Info("Remote-only directory removed (no local copy)", "path", path)
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
		// logger.Info("Local directory removed", "path", path)
	}

	// Notify peer about the removed directory (only for local changes).
	if notifyPeer && d.OnLocalChange != nil && !isRemoteDir {
		d.OnLocalChange(types.FileEvent{
			Path:   path,
			Action: types.RemoveDir,
			Attr:   nil, // No attributes needed for removal
		})
		// logger.Info("Notified peer about removed directory", "path", path)
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
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "statfs", "path", path, "rea-path", cleanPath)

	stfs, sfsErr := platStatfs(cleanPath)
	if sfsErr != nil {
		logger.Error("Failed to stat underlying folder", "error", sfsErr)
		return int(convertOsErrToSyscallErrno("statfs", sfsErr))
	}
	*stat = stfs

	// logger.Info("Statfs", "stat", stat, "inode", d.Inode)

	return 0
}

func (d *Dir) Symlink(target string, newpath string) (errCode int) {
	defer d.recoverPanic("Symlink", &errCode)
	// d.logger.Debug("FUSE Symlink (stub - not supported)", "target", target, "newpath", newpath, "inode", d.Inode)
	// Symlinks not supported - return EPERM.
	return -winfuse.EPERM
}

// Note: On windows open does not have a truncate flag,
// thus Open is immediately followed by Truncate.
func (d *Dir) Truncate(path string, size int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Truncate", &errCode)
	// d.logger.Info("FUSE truncate", "path", path, "size", size)

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "truncate", "path", path, "size", size, "fh", fh)
	err := platTruncate(path, size)
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
	// d.logger.Info("FUSE unlink", "path", path, "notifyPeer", notifyPeer)
	logger := d.logger.With("method", "unlink", "path", path)

	// Check if this is a remote-only file (not downloaded locally).
	d.RemoteFilesLock.Lock()
	_, isRemote := d.RemoteFiles[path]
	if isRemote {
		delete(d.RemoteFiles, path)
		// logger.Info("Removed remote file from map", "path", path)
	}
	d.RemoteFilesLock.Unlock()

	// Also clean up AllFileMap.
	d.AfmLock.Lock()
	delete(d.AllFileMap, path)
	d.AfmLock.Unlock()

	// Try to unlink local file (may not exist if remote-only).
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := platUnlink(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist locally.
			if isRemote {
				// Remote-only file - we already cleaned up the maps, success.
				// logger.Info("Remote-only file removed (no local copy)", "path", path)
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
		// logger.Info("Local file unlinked", "path", path)
	}

	// Notify peer about the removed file.
	// Notify if: (a) it was our local file, OR (b) it was a remote file that we
	// successfully deleted from disk (meaning we had a local copy and something
	// like git checkout removed it from the working tree).
	if notifyPeer && d.OnLocalChange != nil && (!isRemote || err == nil) {
		d.OnLocalChange(types.FileEvent{
			Path:   path,
			Action: types.RemoveFile,
			Attr:   nil,
		})
	}

	return 0
}

// Utimens sets file access and modification times.
// We return success but don't persist the changes (timestamps come from underlying storage).
func (d *Dir) Utimens(path string, tmsp []winfuse.Timespec) (errCode int) {
	defer d.recoverPanic("Utimens", &errCode)
	// d.logger.Debug("FUSE Utimens (stub)", "path", path, "inode", d.Inode)
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
		// totalTime := ws.totalLockTime + ws.totalPwriteTime + ws.totalRemoteTime
		// mbWritten := float64(ws.totalBytes) / 1024 / 1024
		// slog.Warn("WRITE STATS",
		// 	"calls", ws.totalCalls,
		// 	"MB", fmt.Sprintf("%.2f", mbWritten),
		// 	"lock_ms", ws.totalLockTime.Milliseconds(),
		// 	"pwrite_ms", ws.totalPwriteTime.Milliseconds(),
		// 	"remote_ms", ws.totalRemoteTime.Milliseconds(),
		// 	"total_ms", totalTime.Milliseconds(),
		// 	"MB/s", fmt.Sprintf("%.2f", mbWritten/(totalTime.Seconds()+0.001)),
		// )
	}
}

// The method returns the number of bytes written.
func (d *Dir) Write(path string, buff []byte, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Write", &errCode)
	// d.logger.Info("FUSE write", "path", path, "offset", offset, "len", len(buff))
	logger := d.logger.With("method", "write", "path", path, "fh", fh, "offset", offset)

	// startTotal := time.Now()

	// Hold lock during write to prevent Release from closing fd mid-write
	startLock := time.Now()
	d.OpenMapLock.RLock()
	lockTime := time.Since(startLock)

	entry, ok := d.OpenFileHandlers[fh]
	if !ok {
		d.OpenMapLock.RUnlock()
		// macOS fcopyfile() can call Write after Release - try to reopen and write
		// slog.Warn("FCOPYFILE WORKAROUND", "path", path, "fh", fh, "offset", offset, "len", len(buff))
		cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
		startOpen := time.Now()
		fd, err := platOpen(cleanPath, syscall.O_RDWR, 0)
		openTime := time.Since(startOpen)
		if err != nil {
			logger.Warn("fcopyfile fallback open failed", "error", err)
			return int(convertOsErrToSyscallErrno("open", err))
		}
		defer platClose(fd)
		startPw := time.Now()
		n, err := platPwrite(fd, buff, offset)
		pwTime := time.Since(startPw)
		if err != nil {
			logger.Warn("fcopyfile fallback pwrite failed", "error", err)
			return int(convertOsErrToSyscallErrno("pwrite", err))
		}
		writeStats.record(openTime, pwTime, 0, n)
		// slog.Info("FCOPYFILE OK", "bytes", n, "open_ms", openTime.Milliseconds(), "pwrite_ms", pwTime.Milliseconds())
		return n
	}
	f := entry.File
	f.HadEdits = true
	f.NotLocalSynced = false  // Local write makes us authoritative - don't read from remote
	f.NotRemoteSynced = true  // File content changed - notify peer on Release with new size
	f.LocalNewer = true

	startPwrite := time.Now()
	n, err := platPwrite(entry.FD, buff, offset)
	pwriteTime := time.Since(startPwrite)

	d.OpenMapLock.RUnlock() // Release AFTER Pwrite to prevent race with Release

	if err != nil {
		// fd reuse race: kernel reused fd number, old FUSE handle matched new map entry
		// but the actual fd was closed. Fallback to fcopyfile workaround.
		// Use errors.Is for robust comparison (handles wrapped errors)
		if errors.Is(err, syscall.EBADF) {
			// slog.Warn("EBADF on mapped fd, falling back to fcopyfile workaround", "path", path, "fh", fh)
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			fd, err2 := platOpen(cleanPath, syscall.O_RDWR, 0)
			if err2 != nil {
				logger.Warn("EBADF fallback open failed", "error", err2)
				return int(convertOsErrToSyscallErrno("open", err2))
			}
			defer platClose(fd)
			n2, err2 := platPwrite(fd, buff, offset)
			if err2 != nil {
				logger.Warn("EBADF fallback pwrite failed", "error", err2)
				return int(convertOsErrToSyscallErrno("pwrite", err2))
			}
			// slog.Info("Fallback write OK", "bytes", n2)
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
	// totalTime := time.Since(startTotal)
	// if totalTime > 10*time.Millisecond {
	// 	logger.Warn("SLOW WRITE",
	// 		"total_ms", totalTime.Milliseconds(),
	// 		"lock_ms", lockTime.Milliseconds(),
	// 		"pwrite_ms", pwriteTime.Milliseconds(),
	// 		"remote_ms", remoteTime.Milliseconds(),
	// 		"bytes", n,
	// 	)
	// }

	return n
}

func (d *Dir) Read(path string, buff []byte, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Read", &errCode)
	logger := d.logger.With("method", "read", "path", path, "fh", fh, "offset", offset)

	// Get file info briefly, release lock before I/O (RWMutex is write-preferring)
	d.OpenMapLock.RLock()
	entry, ok := d.OpenFileHandlers[fh]
	var f *File
	var fd int
	var pool *StreamPool
	var cacheFD *os.File
	var notLocalSynced bool
	var bitmap *ChunkBitmap
	var remoteFileSize int64 // snapshot of f.stat.Size under lock
	if ok {
		f = entry.File
		fd = entry.FD
		pool = f.StreamPool
		cacheFD = f.CacheFD
		notLocalSynced = f.NotLocalSynced
		bitmap = f.Bitmap
		if f.stat != nil {
			remoteFileSize = f.stat.Size
		}
	}
	d.OpenMapLock.RUnlock()

	// If the file is remote and has no stream pool but has incomplete chunks,
	// try to create a pool on-demand so we can fetch the missing data.
	if ok && notLocalSynced && pool == nil && bitmap != nil && !bitmap.IsComplete() && f.StreamProvider != nil {
		// logger.Info("Creating on-demand stream pool for incomplete remote file")
		streamCtx, streamCancel := context.WithCancel(context.Background())
		newPool, openErr := NewStreamPool(f.StreamProvider, streamCtx, fh, path, StreamPoolSize)
		if openErr != nil {
			streamCancel()
			logger.Warn("Failed to create on-demand stream pool", "error", openErr)
		} else {
			d.OpenMapLock.Lock()
			f.StreamPool = newPool
			f.StreamCancel = streamCancel
			d.OpenMapLock.Unlock()
			pool = newPool
		}
	}

	if ok && notLocalSynced && pool != nil {
		// HYBRID READ: Check bitmap to decide local vs remote.
		// If all chunks for this range are already downloaded (by prefetch or prior read),
		// serve from local cache. Otherwise fetch on-demand from remote.
		if bitmap != nil && bitmap.HasRange(offset, len(buff)) {
			// Fast path: all chunks available locally.
			// d.logger.Info("FUSE read", "path", path, "offset", offset, "len", len(buff), "src", "bitmap")
			n, preadErr := platPread(fd, buff, offset)
			if preadErr != nil {
				logger.Error("Local pread failed after bitmap hit", "error", preadErr)
				return int(convertOsErrToSyscallErrno("pread", preadErr))
			}
			// Clamp to remote file size — pre-allocated files may contain
			// garbage bytes past the actual content boundary.
			if remoteFileSize > 0 && offset+int64(n) > remoteFileSize {
				n = int(remoteFileSize - offset)
				if n < 0 {
					n = 0
				}
			}
			return n
		}

		// d.logger.Info("FUSE read", "path", path, "offset", offset, "len", len(buff), "src", "remote")

		// Clamp the request size to the file's actual size to prevent
		// reading garbage past EOF on pre-allocated cache files.
		readSize := int64(len(buff))
		if remoteFileSize > 0 && offset+readSize > remoteFileSize {
			readSize = remoteFileSize - offset
			if readSize <= 0 {
				return 0
			}
		}

		// Retry loop for resilience against transient failures.
		var data []byte
		var readErr error
		for attempt := 0; attempt < 3; attempt++ {
			// Read from stream pool with timeout to prevent system freeze.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			data, readErr = pool.ReadAt(ctx, offset, readSize)
			cancel()

			if readErr == nil {
				break // Success.
			}

			// logger.Warn("Remote read failed, attempting retry", "attempt", attempt+1, "error", readErr)
			f.Download.RecordAttempt()

			if !f.Download.CanRetry() {
				logger.Error("Max retries exceeded for remote read", "path", path)
				break
			}

			// Try to re-establish the stream pool.
			if f.StreamProvider != nil {
				// logger.Info("Attempting to re-establish stream pool", "path", path, "attempt", attempt+1)
				streamCtx, streamCancel := context.WithCancel(context.Background())
				newPool, openErr := NewStreamPool(f.StreamProvider, streamCtx, fh, path, StreamPoolSize)
				if openErr != nil {
					streamCancel()
					logger.Error("Failed to re-establish stream pool", "error", openErr)
					time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond) // Backoff.
					continue
				}

				// Close old pool (best effort).
				if pool != nil {
					_ = pool.Close()
				}
				if f.StreamCancel != nil {
					f.StreamCancel()
				}

				// Update file with new pool (need lock).
				d.OpenMapLock.Lock()
				f.StreamPool = newPool
				f.StreamCancel = streamCancel
				d.OpenMapLock.Unlock()

				pool = newPool
				// logger.Info("Stream pool re-established successfully", "path", path)
			}

			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond) // Backoff.
		}

		if readErr != nil {
			logger.Warn("Remote read failed, returning EOF", "error", readErr, "path", path)
			// The file may have been deleted on the remote peer (e.g. git's
			// temporary .keep files). Return 0 (EOF) so callers see an
			// empty file instead of aborting.
			return 0
		}

		// Copy remote data into buffer for FUSE.
		n := copy(buff, data)

		// Clamp to remote file size — the server may return more bytes
		// than the actual file content on pre-allocated cache files.
		if f.stat != nil {
			if fileSize := f.stat.Size; fileSize > 0 && offset+int64(n) > fileSize {
				n = int(fileSize - offset)
				if n < 0 {
					n = 0
				}
			}
		}

		// Track download progress and update checksum.
		f.Download.UpdateProgress(offset, n)
		f.Download.UpdateChecksum(data[:n])

		// Write to local cache asynchronously — data is already in the FUSE
		// buffer so we can return immediately. The cache write and bitmap
		// update happen in the background. CacheWg is waited on in Release()
		// before closing the FD.
		if cacheFD != nil {
			cacheData := data[:n]
			cacheOffset := offset
			bm := bitmap
			f.CacheWg.Add(1)
			go func() {
				defer f.CacheWg.Done()
				if _, err := cacheFD.WriteAt(cacheData, cacheOffset); err != nil {
					logger.Error("Async cache write failed", "error", err)
					return
				}
				if bm != nil {
					bm.SetRange(cacheOffset, len(cacheData))
				}
			}()
		}

		// logger.Debug("Read completed", "bytes", n, "progress", f.Download.Progress())
		return n
	}

	// Fallback: read directly from local file
	// d.logger.Info("FUSE read", "path", path, "offset", offset, "len", len(buff), "src", "local")
	n, err := platPread(fd, buff, offset)
	if err != nil {
		// EBADF fallback: fd may have been closed by fcopyfile race or fd reuse.
		// Reopen the file and read from it.
		if errors.Is(err, syscall.EBADF) {
			// d.logger.Warn("EBADF on pread, falling back to reopen", "path", path, "fh", fh)
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			reopenFD, err2 := platOpen(cleanPath, syscall.O_RDONLY, 0)
			if err2 != nil {
				logger.Error("Fallback open failed", "error", err2, "cleanPath", cleanPath)
				return int(convertOsErrToSyscallErrno("open", err2))
			}
			defer platClose(reopenFD)
			n2, err2 := platPread(reopenFD, buff, offset)
			if err2 != nil {
				logger.Error("Fallback pread failed", "error", err2)
				return int(convertOsErrToSyscallErrno("pread", err2))
			}
			// d.logger.Info("Fallback read OK", "bytes", n2)
			return n2
		}
		// d.logger.Warn("FUSE Read LOCAL FAILED", "path", path, "error", err)
		logger.Error("Failed to read local file", "error", err)
		return int(convertOsErrToSyscallErrno("pread", err))
	}

	// d.logger.Warn("FUSE Read SUCCESS", "path", path, "bytesRead", n)
	return n
}

func (d *Dir) Removexattr(path string, name string) (errCode int) {
	defer d.recoverPanic("Removexattr", &errCode)
	// Don't log xattr operations - too frequent
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	err := xattr.Remove(cleanPath, name)
	if err != nil {
		return int(convertOsErrToSyscallErrno("remove-xattr", err))
	}

	return 0
}

func (d *Dir) Listxattr(path string, fill func(name string) bool) (errCode int) {
	defer d.recoverPanic("Listxattr", &errCode)
	// Don't log xattr operations - too frequent
	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

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
	// logger := d.logger.With("method", "getxattr", "path", path, "name", name)
	// logger.Debug("Getxattr called")

	// Block quarantine xattr - macOS Gatekeeper checks this and refuses to open
	// files on FUSE mounts if quarantine is present. Return ENODATA to make
	// macOS think the file isn't quarantined.
	if name == "com.apple.quarantine" {
		// logger.Debug("Getxattr blocked quarantine xattr")
		return -int(platENODATA), nil
	}

	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	res, err := xattr.Get(realPath, name)
	if err != nil {
		// Only log as warning for unexpected errors (not ENOATTR which is normal)
		// logger.Debug("Getxattr failed", "error", err, "realPath", realPath)
		return int(convertOsErrToSyscallErrno("get-xattr", err)), nil
	}

	// logger.Debug("Getxattr OK", "dataLen", len(res))
	return 0, res
}

func (d *Dir) Setxattr(path string, name string, value []byte, flags int) (errCode int) {
	defer d.recoverPanic("Setxattr", &errCode)
	// Don't log xattr operations - too frequent
	// I do not support flags for this version.
	_ = flags

	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	err := xattr.Set(realPath, name, value)
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
	// d.logger.Info("FUSE remote-add", "path", path, "size", stat.Size)

	d.RemoteFilesLock.Lock()

	if existing, ok := d.RemoteFiles[path]; ok {
		oldSize := existing.stat.Size
		sizeChanged := oldSize != stat.Size
		existing.LocalNewer = false // Remote has newer content.

		// Reject stale ADD_FILE with smaller size — this happens when git's
		// debounced notification for a temp file arrives AFTER a RENAME already
		// set the correct (larger) size. Example: git index-pack appends a
		// 20-byte SHA-1 checksum between the temp write and the rename.
		if sizeChanged && stat.Size < oldSize && existing.Bitmap != nil && existing.Bitmap.Have() > 0 {
			d.RemoteFilesLock.Unlock()
			// Stale ADD_FILE with smaller size — rename already set correct size
			return nil
		}

		existing.stat = stat

		if sizeChanged {
			// Size changed: cancel old prefetch, reset bitmap, re-download.
			existing.NotLocalSynced = true
			existing.Download.Reset(uint64(stat.Size))
			if existing.PrefetchCancel != nil {
				existing.PrefetchCancel()
				existing.PrefetchCancel = nil
			}
			existing.Bitmap = NewChunkBitmap(stat.Size)
			d.RemoteFilesLock.Unlock()

			// Only truncate if size actually changed — truncating to the same size
			// zeros out content that a previous prefetch already wrote.
			if existing.RealPathOfFile != "" {
				if truncErr := os.Truncate(existing.RealPathOfFile, stat.Size); truncErr != nil && !os.IsNotExist(truncErr) {
					logger.Warn("Failed to truncate cache file to new size", "path", existing.RealPathOfFile, "size", stat.Size, "error", truncErr)
				}
			}

			d.AfmLock.Lock()
			d.AllFileMap[path] = existing
			d.AfmLock.Unlock()
			d.startPrefetch(logger, existing, path)
		} else {
			// Same size: update metadata but don't reset bitmap or truncate.
			// The previous prefetch may have already completed — don't destroy its work.
			d.RemoteFilesLock.Unlock()
			d.AfmLock.Lock()
			d.AllFileMap[path] = existing
			d.AfmLock.Unlock()
			// If prefetch isn't running and bitmap isn't complete, restart it.
			if existing.PrefetchCancel == nil && existing.Bitmap != nil && !existing.Bitmap.IsComplete() {
				existing.NotLocalSynced = true
				d.startPrefetch(logger, existing, path)
			}
		}
		return nil
	}

	// Auto-create parent directories in the FUSE tree if they don't exist.
	// This handles files from no-FUSE peers (mobile AddFileAs) that send
	// ADD_FILE for "go-fp/examples/client/client.go" without prior ADD_DIR.
	parentDir := filepath.Dir(path)
	if parentDir != "/" && parentDir != "." {
		d.Adm.Lock()
		if _, exists := d.AllDirMap[parentDir]; !exists {
			d.Adm.Unlock()
			d.MkdirFromPeer(parentDir, 0755)
		} else {
			d.Adm.Unlock()
		}
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
		RealPathOfFile:  filepath.Clean(filepath.Join(d.RealPathOfFile, path)),
		Bitmap:          NewChunkBitmap(stat.Size),
	}
	d.RemoteFiles[path] = f
	d.RemoteFilesLock.Unlock()
	d.startPrefetch(logger, f, path)
	return nil
}

// startPrefetch pre-allocates the local cache file and launches a background
// goroutine that downloads the file using push-based StreamFile RPC.
func (d *Dir) startPrefetch(logger *slog.Logger, f *File, path string) {
	if f.Bitmap == nil {
		return
	}

	realPath := f.RealPathOfFile
	fileSize := f.Bitmap.FileSize()

	// Try resuming from a persisted bitmap.
	bmPath := BitmapPath(realPath)
	if info, err := os.Stat(realPath); err == nil && info.Size() == fileSize {
		if bm, loadErr := LoadChunkBitmap(bmPath, fileSize); loadErr == nil {
			f.Bitmap = bm
			// logger.Info("Prefetch: resuming from bitmap", "progress", bm.Progress(), "have", bm.Have(), "total", bm.Total())
		}
	}

	if f.Bitmap.IsComplete() {
		os.Remove(bmPath)
		f.NotLocalSynced = false
		return
	}

	// Pre-allocate local cache file to full size so pwrite at any offset works.
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		logger.Warn("Prefetch: failed to create dirs", "path", realPath, "error", err)
	}
	if _, statErr := os.Stat(realPath); os.IsNotExist(statErr) {
		// Only create/truncate if file doesn't exist (resume keeps existing data).
		if err := os.Truncate(realPath, fileSize); err != nil {
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
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.PrefetchCancel = cancel

	go d.prefetchFile(ctx, logger, f, path, realPath)
}

// prefetchFile uses the push-based StreamFile RPC to download the entire file.
// The server pushes all chunks sequentially without per-chunk round-trips.
// Bitmap coordination with on-demand FUSE Read is unchanged.
func (d *Dir) prefetchFile(ctx context.Context, logger *slog.Logger, f *File, path string, realPath string) {
	bitmap := f.Bitmap
	if bitmap == nil {
		return
	}

	// Acquire prefetch semaphore — limits concurrent downloads to prevent
	// overwhelming the gRPC connection during large clones (600+ files).
	sem := d.Root.PrefetchSem
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
	}

	logger = logger.With("prefetch", path)
	// logger.Info("Push-based prefetch starting", "chunks", bitmap.Total(), "fileSize", bitmap.FileSize())

	fsp := d.OpenStreamProvider()
	if fsp == nil {
		logger.Warn("Prefetch: no stream provider available")
		return
	}

	// Resume from the first missing chunk instead of offset 0.
	startChunk := bitmap.NextMissing(0)
	if startChunk < 0 {
		return
	}
	startOffset := uint64(startChunk) * uint64(bitmap.ChunkSizeBytes())

	receiver, err := fsp.StreamFile(ctx, path, startOffset)
	if err != nil {
		logger.Warn("Prefetch: failed to start StreamFile", "error", err)
		_ = bitmap.Save(BitmapPath(realPath))
		return
	}

	// Open local file for writing.
	lf, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logger.Warn("Prefetch: failed to open local file", "error", err)
		return
	}
	// Close and handle rename-on-complete (git .lock -> final path race fix).
	defer func() {
		lf.Close()
		if !bitmap.IsComplete() {
			return
		}
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
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// logger.Info("Prefetch cancelled", "progress", bitmap.Progress())
			_ = bitmap.Save(BitmapPath(realPath))
			return
		default:
		}

		data, offset, _, recvErr := receiver.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				break // Stream complete.
			}
			logger.Warn("Prefetch: recv failed", "error", recvErr)
			_ = bitmap.Save(BitmapPath(realPath))
			break
		}

		chunkIdx := int(offset / uint64(bitmap.ChunkSizeBytes()))
		if bitmap.Has(chunkIdx) {
			continue // On-demand FUSE Read already got this chunk.
		}

		_, writeErr := lf.WriteAt(data, int64(offset))
		if writeErr != nil {
			logger.Warn("Prefetch: write failed", "chunk", chunkIdx, "error", writeErr)
			continue
		}

		bitmap.Set(chunkIdx)
		f.Download.UpdateProgress(int64(offset), len(data))
	}

	if bitmap.IsComplete() {
		// logger.Info("Prefetch complete — all chunks downloaded", "fileSize", bitmap.FileSize())
		f.NotLocalSynced = false
		os.Remove(BitmapPath(realPath))
	} else {
		// logger.Info("Prefetch finished with gaps", "progress", bitmap.Progress())
		_ = bitmap.Save(BitmapPath(realPath))
	}
}

func (d *Dir) EditRemoteFile(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t) error {
	// Normalize to FUSE convention: paths must have leading "/".
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// d.logger.Info("FUSE remote-edit", "path", path, "size", stat.Size)

	d.RemoteFilesLock.Lock()

	f, ok := d.RemoteFiles[path]
	if !ok {
		// LOCAL file edited by remote peer - add to RemoteFiles so we fetch updated version
		// logger.Info("Local file edited by remote, marking for sync", "path", path, "mtime", stat.Mtim.Time())
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
			RealPathOfFile:  filepath.Clean(filepath.Join(d.RealPathOfFile, path)),
			Bitmap:          NewChunkBitmap(stat.Size),
		}
		d.RemoteFiles[path] = newFile
		d.RemoteFilesLock.Unlock()
		// Ensure AllFileMap points to the same object so OpenEx sees correct state.
		d.AfmLock.Lock()
		d.AllFileMap[path] = newFile
		d.AfmLock.Unlock()
		d.startPrefetch(logger, newFile, path)
		return nil
	}

	if stat.Mtim.Time().Before(f.stat.Mtim.Time()) {
		d.RemoteFilesLock.Unlock()
		// logger.Warn("Remote edit rejected - local is newer", "path", path, "remoteMtime", stat.Mtim.Time(), "localMtime", f.stat.Mtim.Time())
		return syscall.ECANCELED
	}

	// logger.Info("Remote file edited", "path", path, "mtime", stat.Mtim.Time())
	oldSize := f.stat.Size
	sizeChanged := oldSize != stat.Size
	f.stat = stat
	f.LocalNewer = false // Remote has newer content.

	if sizeChanged {
		// Size changed: full re-download required.
		f.NotLocalSynced = true
		f.Download.Reset(uint64(stat.Size))
		if f.PrefetchCancel != nil {
			f.PrefetchCancel()
		}
		f.Bitmap = NewChunkBitmap(stat.Size)
		d.RemoteFilesLock.Unlock()
		d.AfmLock.Lock()
		d.AllFileMap[path] = f
		d.AfmLock.Unlock()
		d.startPrefetch(logger, f, path)
	} else {
		// Same size: metadata update only. Don't reset bitmap or cancel prefetch.
		d.RemoteFilesLock.Unlock()
		d.AfmLock.Lock()
		d.AllFileMap[path] = f
		d.AfmLock.Unlock()
		if f.PrefetchCancel == nil && f.Bitmap != nil && !f.Bitmap.IsComplete() {
			f.NotLocalSynced = true
			d.startPrefetch(logger, f, path)
		}
	}
	return nil
}
