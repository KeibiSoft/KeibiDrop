//go:build !android

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
	// "fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	keibidrop "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/pkg/xattr"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/zeebo/xxh3"
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

const maxPathLen = 1023
const maxNameLen = 255

func checkPath(path string) int {
	if len(path) > maxPathLen {
		return -int(syscall.ENAMETOOLONG)
	}
	for _, component := range strings.Split(path, "/") {
		if len(component) > maxNameLen {
			return -int(syscall.ENAMETOOLONG)
		}
	}
	return 0
}

func (d *Dir) refreshDirStat(dirPath string) {
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, dirPath))
	if st, err := platLstat(cleanPath); err == nil {
		d.Adm.Lock()
		if dir, ok := d.AllDirMap[dirPath]; ok && dir.stat != nil {
			dir.stat.Ctim = st.Ctim
			dir.stat.Mtim = st.Mtim
		}
		d.Adm.Unlock()
	}
}

func (d *Dir) refreshFileStat(filePath string) {
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, filePath))
	if st, err := platLstat(cleanPath); err == nil {
		d.AfmLock.Lock()
		if f, ok := d.AllFileMap[filePath]; ok {
			f.metaMu.Lock()
			if f.stat != nil {
				f.stat.Ctim = st.Ctim
				f.stat.Mtim = st.Mtim
			}
			f.metaMu.Unlock()
		}
		d.AfmLock.Unlock()
	}
}

func (d *Dir) Access(path string, _mask uint32) (errCode int) {
	defer d.recoverPanic("Access", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
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
	if e := checkPath(path); e != 0 {
		return e
	}

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
		// Guard the stat write under metaMu: OpenEx/prefetch read f.stat.Mode on the
		// same *File via metaMu (a different lock than AfmLock), so a bare write here
		// races their reads. metaMu is innermost; AfmLock stays held. Take a value
		// snapshot of the stat under metaMu so the notify below builds its Attr from a
		// stable copy, without holding either lock across StatToAttr's allocation.
		f.metaMu.Lock()
		applyMode(f.stat)
		statSnap := *f.stat
		f.metaMu.Unlock()
		d.AfmLock.Unlock()
		if d.OnLocalChange != nil {
			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.EditFile,
				Attr:   types.StatToAttr(&statSnap),
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
		rf.metaMu.Lock()
		applyMode(rf.stat)
		rf.metaMu.Unlock()
	}
	d.RemoteFilesLock.Unlock()

	return 0
}

func (d *Dir) Chown(path string, uid uint32, gid uint32) (errCode int) {
	defer d.recoverPanic("Chown", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}

	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	chownUid, chownGid := int(uid), int(gid)
	if uid == ^uint32(0) {
		chownUid = -1
	}
	if gid == ^uint32(0) {
		chownGid = -1
	}
	if err := platChown(cleanPath, chownUid, chownGid); err != nil {
		// EPERM: caller isn't root / lacks CAP_CHOWN — expected for non-privileged flows.
		// ENOENT: file vanished between lookup and chown — race, not our bug.
		// ENOSYS: platform has no chown (Windows) — in-memory update below still applies.
		if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOSYS) {
			d.logger.Warn("Chown: disk chown failed, in-memory stat will diverge",
				"path", cleanPath, "uid", chownUid, "gid", chownGid, "error", err)
		}
	}

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
		// metaMu guards f.stat against the OpenEx/prefetch metaMu readers (see Chmod).
		f.metaMu.Lock()
		applyOwner(f.stat)
		f.metaMu.Unlock()
	}
	d.AfmLock.Unlock()

	d.Adm.Lock()
	if dir, ok := d.AllDirMap[path]; ok && dir.stat != nil {
		applyOwner(dir.stat)
	}
	d.Adm.Unlock()

	d.RemoteFilesLock.Lock()
	if rf, ok := d.RemoteFiles[path]; ok && rf.stat != nil {
		rf.metaMu.Lock()
		applyOwner(rf.stat)
		rf.metaMu.Unlock()
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
// Create is dead on all platforms: cgofuse routes creates through CreateEx
// (which we implement; see host.go create dispatch). It is kept only to satisfy
// fuse.FileSystemInterface. Delegate to CreateEx so there is ONE implementation
// — no duplicated logic to drift, and it stays correct if it were ever called.
func (d *Dir) Create(path string, flags int, mode uint32) (errCode int, retFh uint64) {
	fi := winfuse.FileInfo_t{Flags: flags}
	errc := d.CreateEx(path, mode, &fi)
	return errc, fi.Fh
}

// shouldPrefetchOnOpen decides whether to background-prefetch a file of the given
// size on Open: forced when prefetchOnOpen is set, otherwise auto for files
// >= autoMB megabytes (autoMB <= 0 disables auto). Smaller files stay pure
// on-demand (instant open, fetch only what's read). Prefetch always cooperates
// with on-demand jumps, so a seek into a not-yet-filled region never lags.
func shouldPrefetchOnOpen(prefetchOnOpen bool, autoMB int, size uint64) bool {
	if prefetchOnOpen {
		return true
	}
	if autoMB <= 0 {
		return false
	}
	return size >= uint64(autoMB)*1024*1024
}

// vcsMetaDirs are the repository metadata directories whose stores are mmap'd, so they must
// keep the page cache. Match by directory, not extension: the store has no telling suffix
// (git's "index", pijul's "pristine/db").
var vcsMetaDirs = []string{".git/", ".pijul/"}

// fuseOpLog enables per-op experiment logging (KD_FUSE_OPLOG=1); high volume, off by default.
var fuseOpLog = os.Getenv("KD_FUSE_OPLOG") == "1"

// shouldUseDirectIo determines if a file should bypass kernel page cache.
// Returns true for files that need real-time sync (write access, not VCS metadata).
// Returns false for VCS metadata (to allow mmap).
// Returns false for mmap-dependent files (PDF, images) that apps like Preview need.
func shouldUseDirectIo(path string, flags int) bool {
	// VCS metadata: keep the page cache so the store can be mmap'd. A direct_io file cannot
	// be mapped, so the first page touch raises SIGBUS and kills the client. git mmaps its
	// pack files and index; pijul's pristine is one mmap'd Sanakirja B-tree.
	for _, dir := range vcsMetaDirs {
		if strings.Contains(path, "/"+dir) || strings.HasPrefix(path, dir) {
			return false
		}
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
	if e := checkPath(path); e != 0 {
		return e
	}
	// d.logger.Info("FUSE create", "path", path, "mode", mode)
	logger := d.logger.With("method", "create-ex", "path", path)

	flags := fi.Flags
	// On Windows, WinFSP does not include O_CREAT in fi.Flags for CreateEx
	// because the "create" semantics are implicit in the call itself.
	// Ensure O_CREAT is set so platOpen actually creates the file.
	flags |= syscall.O_CREAT
	// The fd is the backing cache handle, not the client's handle: open it
	// RDWR regardless of client access mode (the OpenEx localize path does
	// the same). A read-only fd fails Write on Windows with EACCES, which
	// the EBADF fallback does not catch; on unix it costs a reopen per write.
	flags = (flags &^ winfuse.O_ACCMODE) | syscall.O_RDWR
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
		// A fresh create has no prior peer version. -1 is a REAL base (unlike
		// 0 = unknown): a concurrent same-name create on the peer is provable.
		EditBaseMtimeNs: -1,
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
	if fuseOpLog {
		d.logger.Debug("oplog OpenEx", "path", path, "flags", fi.Flags)
	}
	defer d.recoverPanic("OpenEx", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
	flags := fi.Flags
	// d.logger.Info("FUSE open", "path", path, "flags", flags)
	logger := d.logger.With("method", "open-ex", "path", path, "flags", flags)

	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	hasCreate := flags&syscall.O_CREAT != 0
	hasExcl := flags&syscall.O_EXCL != 0

	// Snapshot whether this path is a known remote file BEFORE taking AfmLock.
	// This check used to run in the !ok branch below while AfmLock was held, which
	// inverts the codebase lock order (RemoteFilesLock -> Adm -> AfmLock, used by
	// Getattr and AddRemoteFile) and deadlocks under Go's RWMutex writer-preference
	// when an AddRemoteFile writer is pending. The value only seeds a new File's
	// LocalNewer below; a microsecond-stale read self-corrects on the next announce.
	d.RemoteFilesLock.RLock()
	_, isRemoteFile := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()

	d.AfmLock.Lock()
	fh, ok := d.AllFileMap[path]

	if hasCreate && hasExcl && ok {
		d.AfmLock.Unlock()
		return -winfuse.EEXIST
	}

	if !ok {
		fileExists := false
		if _, statErr := os.Stat(localPath); statErr == nil {
			fileExists = true
		}

		if hasCreate && hasExcl && fileExists {
			d.AfmLock.Unlock()
			return -winfuse.EEXIST
		}

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

	// File already opened - reuse the existing shared handle, but ONLY while it is
	// still active. OpenIfActive atomically bumps the open count iff it is >0; the
	// kernel calls Release once per open(), so each open must add a count or a
	// racing async Release of an earlier handle drops the count to 0 and tears
	// down the shared fd/pool — leaving this open's Read with ok=false, which then
	// blind-preads the sparse cache file and returns zeros. If the count just hit
	// 0 (last handle being torn down), fall through to a fresh open.
	if fh.openFileCounter.OpenIfActive() {
		handleID := fh.CurrentHandleID
		d.AfmLock.Unlock()
		fi.Fh = handleID
		fi.DirectIo = shouldUseDirectIo(path, flags)
		return 0
	}

	fh.metaMu.RLock()
	isLocalPresent := fh.IsLocalPresent
	localNewer := fh.LocalNewer
	fh.metaMu.RUnlock()
	d.AfmLock.Unlock()

	// Check if remote has newer version
	remoteHasUpdate := false
	var remoteTotalSize uint64
	needsRemark := false
	d.RemoteFilesLock.RLock()
	remoteFile, hasRemote := d.RemoteFiles[path]
	if hasRemote {
		// Read this File's metadata (stat/NotLocalSynced) under metaMu — Release and
		// the notify handlers write them under metaMu (reached via a different map
		// lock). Logic unchanged; this only makes the reads race-free.
		remoteFile.metaMu.RLock()
		// logger.Info("=== REMOTE CHECK ===", "path", path, "hasRemote", hasRemote, "NotLocalSynced", remoteFile.NotLocalSynced)
		if remoteFile.stat != nil {
			remoteTotalSize = uint64(remoteFile.stat.Size)
		}
		// Only consider streaming from the peer when our local copy is NOT
		// authoritative. A local write or rename sets LocalNewer=true (e.g. git's
		// atomic index.lock→index on commit); the local content then wins. Without
		// this guard, the incomplete-download check below fires whenever
		// Download.BytesDownloaded — which only counts REMOTE fetches — lags the
		// stat.Size that refreshFileStat updated from the local write, so we
		// re-stream the peer's stale bytes over our just-written file. That is the
		// "git index file corrupt" we lose on Linux (and usually win on macOS).
		// Peer edits set LocalNewer=false, so live-collab still streams here.
		if !localNewer {
			if remoteFile.NotLocalSynced {
				// logger.Info("Remote has newer version, streaming from remote", "path", path)
				remoteHasUpdate = true
			} else if remoteTotalSize > 0 {
				// Even if NotLocalSynced=false, verify download is actually complete.
				// IMPORTANT: Can't use file size because pre-allocation makes file look complete.
				// Use Download.BytesDownloaded which tracks actual bytes received.
				bytesDownloaded := remoteFile.Download.BytesDownloaded.Load()
				if bytesDownloaded < remoteTotalSize {
					// logger.Info("Download incomplete, re-streaming from remote", "bytesDownloaded", bytesDownloaded, "remoteSize", remoteTotalSize)
					remoteHasUpdate = true
					needsRemark = true
				}
			}
		}
		remoteFile.metaMu.RUnlock()
	}
	d.RemoteFilesLock.RUnlock()

	// Re-mark for streaming outside of read lock
	if needsRemark && hasRemote {
		d.RemoteFilesLock.Lock()
		if rf, ok := d.RemoteFiles[path]; ok {
			rf.metaMu.Lock()
			rf.NotLocalSynced = true
			rf.metaMu.Unlock()
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
		if err := os.MkdirAll(parentDir, 0750); err != nil {
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
		fh.metaMu.Lock()
		fh.IsLocalPresent = true
		if !fh.NotRemoteSynced {
			fh.EditBaseMtimeNs = max(fh.RemoteMtimeNs, fh.LastAnnouncedMtimeNs)
		}
		fh.NotRemoteSynced = true
		fh.metaMu.Unlock()
		handleID := allocHandleID()
		fh.CurrentHandleID = handleID
		d.OpenFileHandlers[handleID] = &HandleEntry{FD: fd, File: fh}
		d.OpenMapLock.Unlock()
		d.AfmLock.Unlock()

		fi.Fh = handleID
		fi.DirectIo = shouldUseDirectIo(path, flags)
		if flags&syscall.O_TRUNC != 0 {
			d.refreshFileStat(path)
		}
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

	cacheMode := uint32(0644)
	if fh != nil {
		fh.metaMu.RLock()
		if fh.stat != nil {
			cacheMode = fh.stat.Mode & 0o777
		}
		fh.metaMu.RUnlock()
		if cacheMode == 0 {
			cacheMode = 0644
		}
	}
	fd, err := platOpen(localPath, syscall.O_RDWR|syscall.O_CREAT, cacheMode)
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
		streamCtx, cancel := context.WithCancel(d.FsCtx)
		streamCancel = cancel
		pool, err = NewStreamPool(fsp, streamCtx, uint64(fd), path, StreamPoolSize) // on-demand jumps; prefetch uses StreamFile separately
		if err != nil {
			cancel()
			_ = platClose(fd)
			logger.Error("Failed to open stream pool", "error", err)
			return -winfuse.EACCES
		}
		// Open persistent cache FD for on-demand writes (avoids open/close per chunk).
		cacheFD, err = os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			cancel()
			pool.Close()
			_ = platClose(fd)
			logger.Error("Failed to open cache FD", "error", err)
			return -winfuse.EIO
		}
	}

	d.AfmLock.Lock()
	d.OpenMapLock.Lock()
	fh.Inode = uint64(fd)
	fh.openFileCounter.Open()
	fh.metaMu.Lock()
	fh.IsLocalPresent = true
	fh.metaMu.Unlock()
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
			_ = os.Truncate(localPath, int64(remoteTotalSize))
		}

		// NOTE: We intentionally do NOT use local file size for resume tracking.
		// Pre-allocation creates a full-size file with zeros, which would falsely
		// indicate the download is complete. We only trust Download.BytesDownloaded
		// which is tracked explicitly by Read() operations.
	}
	d.OpenMapLock.Unlock()
	d.AfmLock.Unlock()

	// Prefetch-on-open (Netflix-style): fill the rest of the file in the background
	// while on-demand reads serve immediate seeks; the two cooperate via the chunk
	// bitmap (prefetch skips chunks reads already got). Strategy: force when
	// PrefetchOnOpen, otherwise auto for files >= PrefetchAutoMB — large files get
	// wire-speed sequential fill + lag-free jumps, while small files stay pure
	// on-demand (instant open, fetch only what's read). Guarded so a second handle
	// on the same file doesn't start a duplicate.
	if remoteHasUpdate && shouldPrefetchOnOpen(d.PrefetchOnOpen, d.PrefetchAutoMB, remoteTotalSize) && fh.PrefetchCancel == nil {
		d.startPrefetch(logger, fh, path)
	}

	fi.Fh = handleID
	fi.DirectIo = shouldUseDirectIo(path, flags)
	// Remote file: don't let the kernel keep the page cache across opens. A peer
	// can edit the file underneath us (live collab); macFUSE only drops its data
	// cache on a size change, so a same-size in-place edit would keep serving
	// stale pages. KeepCache=false makes each open re-read through our Read
	// handler, which the reset bitmap then re-fetches. This is per-file, so
	// local mmap writes (git's index) keep their cache and stay consistent —
	// unlike the global auto_cache mount option, which corrupts them.
	fi.KeepCache = false
	// logger.Info("Opened for remote streaming", "fh", fd, "directIo", fi.DirectIo, "remoteHasUpdate", remoteHasUpdate, "fhNotLocalSynced", fh.NotLocalSynced, "poolNil", fh.StreamPool == nil)
	return 0
}

// Called on unmount.
func (d *Dir) Destroy() {
	// d.logger.Info("Destroy")
}

func (d *Dir) Flush(path string, fh uint64) (errCode int) {
	defer d.recoverPanic("Flush", &errCode)
	if fuseOpLog {
		d.logger.Debug("oplog Flush", "path", path, "fh", fh)
	}
	return 0 // Return success - actual sync happens in Fsync/Release.
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) (errCode int) {
	defer d.recoverPanic("Fsync", &errCode)
	if fuseOpLog {
		d.logger.Debug("oplog Fsync", "path", path, "fh", fh, "datasync", datasync)
	}
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
		// On Windows, "bad file descriptor" is ERROR_INVALID_HANDLE (6), not POSIX EBADF.
		// Fall through to the fallback for any handle-related error.
		if !errors.Is(err, syscall.EBADF) && !errors.Is(err, os.ErrClosed) {
			logger.Error("Fsync failed", "error", err)
			return int(convertOsErrToSyscallErrno("fsync", err))
		}
	}

	// Handle not found or EBADF: fd was already closed (Release called before Fsync).
	// This is a FUSE race condition. Workaround: open, fsync, close.
	{
		// Open with write access — FlushFileBuffers on Windows requires it.
		fd, openErr := platOpen(localPath, syscall.O_RDWR, 0)
		if openErr != nil {
			// File might have been renamed/deleted - that's OK, data was already written
			return 0
		}

		fsyncErr := platFsync(fd)
		_ = platClose(fd)

		if fsyncErr != nil {
			return int(convertOsErrToSyscallErrno("fsync", fsyncErr))
		}

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
	if e := checkPath(path); e != 0 {
		return e
	}
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
			// See if the file is also present locally. Do the lstat BEFORE taking
			// metaMu: the syscall needs nothing from remFile (cleanPath derives from
			// path only), and holding metaMu across it would stall every other op on
			// this File (Read/OpenEx/Write/Release) behind a slow lstat.
			stgo, lstatErr := platLstat(cleanPath)
			// Guard remFile.stat under its metaMu for the rest of the branch: it is
			// mutated in place below while Read/OpenEx read it via a DIFFERENT lock
			// (OpenMapLock). Every path here returns, so the defer releases metaMu
			// (innermost) before the map locks, so there is no deadlock.
			remFile.metaMu.Lock()
			defer remFile.metaMu.Unlock()
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
	// Guard the shared *stat under the File's metaMu: Getattr mutates f.stat in
	// place here while Read/OpenEx read it through OpenMapLock — a different lock,
	// hence the data race. metaMu is innermost (no map lock taken under it).
	if ok && f != nil {
		f.metaMu.Lock()
		if f.stat != nil && gtAtim(f.stat.Mtim, stat.Mtim) {
			copyFusestatFromFusestat(stat, f.stat)
		}
		if f.stat != nil && !gtAtim(f.stat.Mtim, stat.Mtim) {
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
		f.metaMu.Unlock()
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
				FsCtx:               d.FsCtx,

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
	if e := checkPath(oldpath); e != 0 {
		return e
	}
	if e := checkPath(newpath); e != 0 {
		return e
	}
	// d.logger.Debug("FUSE Link (stub - not supported)", "oldPath", oldpath, "newPath", newpath, "inode", d.Inode)
	// Hard links not supported - return EPERM (more accurate than ENOSYS).
	return -winfuse.EPERM
}

func (d *Dir) Mkdir(path string, mode uint32) (errCode int) {
	defer d.recoverPanic("Mkdir", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
	return d.mkdirInternal(path, mode, true)
}

// MkdirFromPeer creates a directory without notifying the peer (to avoid loops).
func (d *Dir) MkdirFromPeer(path string, mode uint32) (errCode int) {
	return d.mkdirInternal(path, mode, false)
}

func (d *Dir) mkdirInternal(path string, mode uint32, notifyPeer bool) (errCode int) {
	// d.logger.Info("FUSE mkdir", "path", path, "mode", mode, "notifyPeer", notifyPeer)

	// Root directory always exists. WinFSP calls Mkdir("\") on mount and on
	// every file notification — return success immediately.
	cp := filepath.Clean(path)
	if cp == "." || cp == string(filepath.Separator) || cp == "/" || cp == "\\" {
		return 0
	}

	logger := d.logger.With("method", "mkdir", "path", path, "mode", mode)
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := platMkdir(cleanPath, mode)
	if err != nil {
		// On Windows, FUSE may call Mkdir for directories that already exist
		// (e.g. during git clone when peer notifications race with local ops).
		// Treat EEXIST as success — the directory exists, which is what we want.
		if errors.Is(err, os.ErrExist) {
			// Not an error — proceed to update internal state below.
		} else {
			logger.Error("Failed to mkdir", "path", cleanPath, "error", err)
			return int(convertOsErrToSyscallErrno("mkdir", err))
		}
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
			FsCtx:               d.FsCtx,
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

	d.refreshDirStat(filepath.Dir(path))
	return 0
}

func (d *Dir) Mknod(path string, mode uint32, dev uint64) (errCode int) {
	defer d.recoverPanic("Mknod", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
	// d.logger.Info("Mknod", "path", path, "inode", d.Inode)

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "mknod", "path", path, "mode", mode, "dev", dev)
	err := platMknod(path, mode, int(dev)) // #nosec G115
	if err != nil {
		logger.Error("Failed to mknor", "errro", err)
		return int(convertOsErrToSyscallErrno("mknod", err))
	}
	return 0
}

// Open is dead on all platforms: cgofuse routes opens through OpenEx (which we
// implement — see host.go's open dispatch), proven by the suite passing with this
// returning ENOSYS on Linux+macOS. It exists only to satisfy the base
// fuse.FileSystemInterface; OpenEx is the real (default) open path. Delegate to
// it so there is no duplicated open logic to drift — that duplication is what hid
// the handle-counter bug, now fixed once in OpenEx.
func (d *Dir) Open(path string, flags int) (errCode int, retFh uint64) {
	fi := winfuse.FileInfo_t{Flags: flags}
	errc := d.OpenEx(path, &fi)
	return errc, fi.Fh
}

func (d *Dir) Opendir(path string) (errCode int, retFh uint64) {
	defer d.recoverPanic("Opendir", &errCode)
	if e := checkPath(path); e != 0 {
		return e, 0
	}
	// d.logger.Info("FUSE opendir", "path", path)
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	// Ensure the directory exists (it may not yet on a fresh mount).
	_ = os.MkdirAll(cleanPath, 0755)

	f, err := platOpendir(cleanPath)
	if err != nil {
		logger := d.logger.With("method", "opendir", "path", cleanPath)
		logger.Error("Failed to open dir", "error", err)
		return int(convertOsErrToSyscallErrno("opendir", err)), 0
	}

	return 0, uint64(f)
}

func (d *Dir) Readdir(path string, fill func(name string, stat *winfuse.Stat_t, offset int64) bool, offset int64, fh uint64) (errCode int) {
	defer d.recoverPanic("Readdir", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
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
	if e := checkPath(path); e != 0 {
		return e, ""
	}
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

	if fuseOpLog {
		f.metaMu.RLock()
		d.logger.Debug("oplog Release", "path", path, "fh", fh, "openCount", v,
			"hadEdits", f.HadEdits, "notRemoteSynced", f.NotRemoteSynced)
		f.metaMu.RUnlock()
	}

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
		f.metaMu.RLock()
		notRemoteSynced := f.NotRemoteSynced
		hadEdits := f.HadEdits
		localNewer := f.LocalNewer
		f.metaMu.RUnlock()
		// Do not announce an open that wrote nothing (pijul locks source repos
		// RDWR). Such an announce makes the owner drop its authority.
		needsNotify := (hadEdits || (notRemoteSynced && localNewer)) && d.OnLocalChange != nil

		if needsNotify {
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			stgo, lstatErr := platLstat(cleanPath)
			if lstatErr != nil {
				// File was deleted between close and notification — skip notification
				return 0
			}

			// logger.Info("Release lstat result", "path", path, "size", stgo.Size)

			if !hadEdits {
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
								f.metaMu.Lock()
								base := f.EditBaseMtimeNs
								f.NotRemoteSynced = false
								f.HadEdits = false
								f.LastAnnouncedMtimeNs = recheckStat.Mtim.Sec*1e9 + recheckStat.Mtim.Nsec
								f.metaMu.Unlock()
								d.OnLocalChange(types.FileEvent{
									Path:        path,
									Action:      types.AddFile,
									Attr:        types.StatToAttr(&recheckStat),
									BaseMtimeNs: base,
								})
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
					f.metaMu.Lock()
					base := f.EditBaseMtimeNs
					f.NotRemoteSynced = false
					f.HadEdits = false
					f.LastAnnouncedMtimeNs = finalStat.Mtim.Sec*1e9 + finalStat.Mtim.Nsec
					f.metaMu.Unlock()
					d.OnLocalChange(types.FileEvent{
						Path:        path,
						Action:      types.AddFile,
						Attr:        types.StatToAttr(&finalStat),
						BaseMtimeNs: base,
					})
				}()
			} else {
				f.metaMu.Lock()
				base := f.EditBaseMtimeNs
				f.NotRemoteSynced = false
				f.HadEdits = false
				f.LastAnnouncedMtimeNs = stgo.Mtim.Sec*1e9 + stgo.Mtim.Nsec
				f.metaMu.Unlock()
				d.OnLocalChange(types.FileEvent{
					Path:        path,
					Action:      types.AddFile,
					Attr:        types.StatToAttr(&stgo),
					BaseMtimeNs: base,
				})
			}
		}

		// Reset sync state for future opens. Guard under metaMu (OpenEx reads
		// IsLocalPresent/NotLocalSynced via AfmLock — a different lock). Released
		// BEFORE the AfmLock acquisition below to avoid a metaMu->AfmLock cycle.
		f.metaMu.Lock()
		f.IsLocalPresent = true
		// Only mark as synced if download is actually COMPLETE.
		// Use ChunkBitmap (preferred) or BytesDownloaded as fallback.
		if f.Bitmap != nil {
			if f.Bitmap.IsComplete() {
				f.NotLocalSynced = false
				// logger.Info("Download complete (all chunks verified)", "progress", f.Bitmap.Progress())
			}
		} else {
			// Fallback for files without bitmap (local-origin or legacy).
			expectedSize := f.Download.TotalSize.Load()
			bytesDownloaded := f.Download.BytesDownloaded.Load()
			switch {
			case expectedSize > 0 && bytesDownloaded >= expectedSize:
				f.NotLocalSynced = false
			case expectedSize > 0:
				// Download incomplete, keeping NotLocalSynced=true
			default:
				cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
				if localInfo, statErr := os.Stat(cleanPath); statErr == nil && localInfo.Size() > 0 {
					f.NotLocalSynced = false
				}
			}
		}
		f.metaMu.Unlock()

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
			// Abort in-flight network reads FIRST so CacheWg.Wait() below cannot
			// block on a read-ahead goroutine stuck in a 16 MiB fetch. Cancelling
			// the stream context and closing the pool make any in-flight
			// pool.ReadAt return promptly (mirrors drainInFlightOperations); the
			// read-ahead goroutine then bails and calls CacheWg.Done(). The async
			// cache writer touches only cacheFD, not the pool, so closing the pool
			// before waiting is safe for it.
			if streamCancel != nil {
				streamCancel()
			}
			if pool != nil {
				if closeErr := pool.Close(); closeErr != nil {
					logger.Error("Failed to close stream pool", "error", closeErr)
				}
			}
			if cacheFD != nil {
				f.CacheWg.Wait() // Wait for in-flight async cache writes to finish.
				if closeErr := cacheFD.Close(); closeErr != nil {
					logger.Error("Failed to close cache FD", "error", closeErr)
				}
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
	err := platClose(int(fh)) // #nosec G115
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
	if e := checkPath(oldpath); e != 0 {
		return e
	}
	if e := checkPath(newpath); e != 0 {
		return e
	}
	// d.logger.Info("FUSE rename", "old", oldpath, "new", newpath)
	// d.logger.Warn("FUSE Rename called",
	// 	"oldpath", oldpath,
	// 	"newpath", newpath,
	// 	"note", "macOS apps may use RENAME_SWAP - not supported by cgofuse")

	oldBase := filepath.Base(oldpath)
	newBase := filepath.Base(newpath)
	if oldBase == "." || oldBase == ".." || newBase == "." || newBase == ".." {
		return -winfuse.EINVAL
	}

	cleanOldPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, oldpath))
	cleanNewPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, newpath))
	logger := d.logger.With("method", "rename", "old-path", cleanOldPath, "new-path", cleanNewPath)

	// A rename ONTO a tracked file is an app swap-save. Declare the target's
	// identity at swap time as the announce base: the receiver then preserves
	// its bytes when they are provably newer. Identity at swap time can only
	// overstate the true session base, so this never fires a false copy.
	swapBase := d.targetIdentity(newpath)

	// On Windows, rename fails if source OR target has an open handle.
	// Close any handles we hold on either path before attempting the rename.
	if runtime.GOOS == "windows" {
		d.OpenMapLock.Lock()
		for fh, entry := range d.OpenFileHandlers {
			entryPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, entry.File.RelativePath))
			if entryPath == cleanOldPath || entryPath == cleanNewPath {
				_ = platClose(entry.FD)
				delete(d.OpenFileHandlers, fh)
			}
		}
		d.OpenMapLock.Unlock()
	}

	err := os.Rename(cleanOldPath, cleanNewPath)
	if err != nil {
		logger.Error("Failed to rename", "error", err)
		return int(convertOsErrToSyscallErrno("rename", err))
	}

	// Remove destination entry from AllFileMap if it existed (rename-over-existing).
	// Do NOT remove from RemoteFiles — the peer still tracks this file.
	d.AfmLock.Lock()
	delete(d.AllFileMap, newpath)
	d.AfmLock.Unlock()

	// Update internal maps to reflect the rename
	var movedLocal *File
	d.AfmLock.Lock()
	if f, ok := d.AllFileMap[oldpath]; ok {
		delete(d.AllFileMap, oldpath)
		f.RelativePath = newpath
		f.Name = getNameFromPath(newpath)
		f.RealPathOfFile = cleanNewPath
		d.AllFileMap[newpath] = f
		movedLocal = f
	}
	d.AfmLock.Unlock()

	// Update AllDirMap if a directory was renamed
	d.Adm.Lock()
	if dir, ok := d.AllDirMap[oldpath]; ok {
		delete(d.AllDirMap, oldpath)
		dir.RelativePath = newpath
		dir.LocalDownloadFolder = cleanNewPath
		d.AllDirMap[newpath] = dir
	}
	d.Adm.Unlock()

	// Refresh stats after map update
	oldParent := filepath.Dir(oldpath)
	newParent := filepath.Dir(newpath)
	d.refreshDirStat(oldParent)
	if oldParent != newParent {
		d.refreshDirStat(newParent)
	}
	d.refreshFileStat(newpath)
	d.refreshDirStat(newpath)

	// Also update RemoteFiles if the file was remote
	d.RemoteFilesLock.Lock()
	if f, ok := d.RemoteFiles[oldpath]; ok {
		delete(d.RemoteFiles, oldpath)
		f.RelativePath = newpath
		f.Name = getNameFromPath(newpath)
		f.RealPathOfFile = cleanNewPath
		d.RemoteFiles[newpath] = f
	} else if movedLocal != nil {
		// A LOCAL file swapped over a tracked target: the moved object is the
		// content now, so it must also be the tracked object. Leaving the old
		// target entry made later acceptances run on a stale parallel object
		// (split-brain) while this object silently lost the swap.
		if old, ok := d.RemoteFiles[newpath]; ok && old != movedLocal {
			old.metaMu.Lock()
			watermark := old.RemoteMtimeNs
			old.metaMu.Unlock()
			movedLocal.metaMu.Lock()
			if watermark > movedLocal.RemoteMtimeNs {
				movedLocal.RemoteMtimeNs = watermark
			}
			movedLocal.metaMu.Unlock()
			d.RemoteFiles[newpath] = movedLocal
		}
	}
	d.RemoteFilesLock.Unlock()

	// Announce every kernel-originated rename: it is always a local user action,
	// also when the source is a peer file (reader moves the owner's file) or an
	// adopted own file. Peer-driven renames do not pass through this op, so
	// there is no echo.
	if d.OnLocalChange != nil {
		// Get stat of the renamed file.
		var attr *keibidrop.Attr
		if stgo, statErr := platLstat(cleanNewPath); statErr == nil {
			attr = types.StatToAttr(&stgo)
			if movedLocal != nil {
				movedLocal.metaMu.Lock()
				movedLocal.LastAnnouncedMtimeNs = stgo.Mtim.Sec*1e9 + stgo.Mtim.Nsec
				movedLocal.metaMu.Unlock()
			}
		}

		d.OnLocalChange(types.FileEvent{
			Path:        newpath,
			OldPath:     oldpath,
			Action:      types.RenameFile,
			Attr:        attr,
			BaseMtimeNs: swapBase,
		})
	}

	// d.logger.Warn("FUSE Rename SUCCESS", "oldpath", oldpath, "newpath", newpath)
	return 0
}

func (d *Dir) Rmdir(path string) (errCode int) {
	defer d.recoverPanic("Rmdir", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
	return d.rmdirInternal(path, true)
}

// RmdirFromPeer removes a directory without notifying the peer (to avoid loops).
func (d *Dir) RmdirFromPeer(path string) (errCode int) {
	return d.rmdirInternal(path, false)
}

func (d *Dir) rmdirInternal(path string, notifyPeer bool) (errCode int) {
	// d.logger.Info("FUSE rmdir", "path", path, "notifyPeer", notifyPeer)
	logger := d.logger.With("method", "rmdir", "path", path)

	d.Adm.RLock()
	_, isRemoteDir := d.AllDirMap[path]
	d.Adm.RUnlock()

	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := syscall.Rmdir(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			if isRemoteDir {
				d.Adm.Lock()
				delete(d.AllDirMap, path)
				d.Adm.Unlock()
			} else {
				logger.Error("Failed to remove dir - not found", "error", err)
				return int(convertOsErrToSyscallErrno("rmdir", err))
			}
		} else {
			// Real error (not empty, permission denied, etc.) - fail regardless.
			logger.Error("Failed to remove dir", "error", err)
			return int(convertOsErrToSyscallErrno("rmdir", err))
		}
	}

	// Rmdir succeeded on disk, clean up the map entry.
	d.Adm.Lock()
	delete(d.AllDirMap, path)
	d.Adm.Unlock()

	if notifyPeer && d.OnLocalChange != nil && !isRemoteDir {
		d.OnLocalChange(types.FileEvent{
			Path:   path,
			Action: types.RemoveDir,
			Attr:   nil, // No attributes needed for removal
		})
		// logger.Info("Notified peer about removed directory", "path", path)
	}

	d.refreshDirStat(filepath.Dir(path))
	return 0
}

func (d *Dir) Statfs(path string, stat *winfuse.Statfs_t) (errCode int) {
	defer d.recoverPanic("Statfs", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
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
	if e := checkPath(newpath); e != 0 {
		return e
	}
	// d.logger.Debug("FUSE Symlink (stub - not supported)", "target", target, "newpath", newpath, "inode", d.Inode)
	// Symlinks not supported - return EPERM.
	return -winfuse.EPERM
}

// Note: On windows open does not have a truncate flag,
// thus Open is immediately followed by Truncate.
func (d *Dir) Truncate(path string, size int64, fh uint64) (errCode int) {
	if fuseOpLog {
		d.logger.Debug("oplog Truncate", "path", path, "size", size, "fh", fh)
	}
	defer d.recoverPanic("Truncate", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "truncate", "path", path, "size", size, "fh", fh)

	if info, statErr := os.Stat(cleanPath); statErr == nil && info.IsDir() {
		return -winfuse.EISDIR
	}

	err := platTruncate(cleanPath, size)
	if err != nil {
		logger.Error("Failed to truncate", "error", err)
		return int(convertOsErrToSyscallErrno("truncate", err))
	}

	// Re-stat and propagate the new SIZE to the peer. A truncate has no Write/Release to ride,
	// so without this a shrink never reaches the peer: it keeps the old (larger) size and serves
	// stale bytes past the new EOF (integrity violation). refreshFileStat only refreshes
	// timestamps, so update Size here too, snapshot under metaMu, then notify EDIT_FILE outside
	// the locks (same discipline as Chmod).
	st, statErr := platLstat(cleanPath)
	var statSnap winfuse.Stat_t
	var baseSnap int64
	haveSnap, sizeChanged := false, false
	d.AfmLock.Lock()
	if f, ok := d.AllFileMap[path]; ok && f.stat != nil && statErr == nil {
		f.metaMu.Lock()
		sizeChanged = st.Size != f.stat.Size
		f.stat.Size = st.Size
		f.stat.Ctim = st.Ctim
		f.stat.Mtim = st.Mtim
		statSnap = *f.stat
		haveSnap = true
		if f.NotRemoteSynced {
			baseSnap = f.EditBaseMtimeNs
		} else {
			baseSnap = max(f.RemoteMtimeNs, f.LastAnnouncedMtimeNs)
		}
		// Do NOT stamp LastAnnouncedMtimeNs here: a truncate is usually the
		// first act of an O_TRUNC save whose open skipped the session flip
		// (remote-open path). Stamping made the follow-up Write adopt the
		// truncate's own time as its base, hiding real conflicts.
		f.metaMu.Unlock()
	}
	d.AfmLock.Unlock()
	// Only emit on an actual size change. Windows re-opens an O_TRUNC file by calling Truncate
	// even when the size is unchanged; a no-op truncate must not churn the peer with a redundant
	// EDIT_FILE. A genuine grow/shrink/overwrite always changes the size.
	if haveSnap && sizeChanged && d.OnLocalChange != nil {
		d.OnLocalChange(types.FileEvent{Path: path, Action: types.EditFile, Attr: types.StatToAttr(&statSnap), BaseMtimeNs: baseSnap})
	}
	return 0
}

// Unlink removes a file.
func (d *Dir) Unlink(path string) (errCode int) {
	defer d.recoverPanic("Unlink", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
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

	// On Windows, close any open handle we hold on this file before removing.
	// Windows does not allow deleting files with open handles.
	if runtime.GOOS == "windows" {
		cleanTarget := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
		d.OpenMapLock.Lock()
		for fh, entry := range d.OpenFileHandlers {
			entryPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, entry.File.RelativePath))
			if entryPath == cleanTarget {
				_ = platClose(entry.FD)
				delete(d.OpenFileHandlers, fh)
				break
			}
		}
		d.OpenMapLock.Unlock()
	}

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
	}

	// Refresh parent directory stat (ctime/mtime change on unlink)
	d.refreshDirStat(filepath.Dir(path))

	// Notify peer about the removed file.
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
	if fuseOpLog {
		d.logger.Debug("oplog Utimens", "path", path)
	}
	defer d.recoverPanic("Utimens", &errCode)
	if e := checkPath(path); e != 0 {
		return e
	}
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
		if fuseOpLog {
			d.logger.Debug("oplog Write MISS", "path", path, "fh", fh, "offset", offset, "len", len(buff))
		}
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
		defer func() { _ = platClose(fd) }()
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
	if fuseOpLog {
		d.logger.Debug("oplog Write hit", "path", path, "fh", fh, "offset", offset, "len", len(buff))
	}
	f.metaMu.Lock()
	f.HadEdits = true
	if !f.NotRemoteSynced {
		// First dirtying op of this edit session: record which version the
		// edit starts from — the newest this file has shown the world.
		// The announce carries it (conflict proof).
		f.EditBaseMtimeNs = max(f.RemoteMtimeNs, f.LastAnnouncedMtimeNs)
	}
	f.NotLocalSynced = false // Local write makes us authoritative - don't read from remote
	f.NotRemoteSynced = true // File content changed - notify peer on Release with new size
	f.LocalNewer = true
	f.metaMu.Unlock()

	startPwrite := time.Now()
	n, err := platPwrite(entry.FD, buff, offset)
	pwriteTime := time.Since(startPwrite)

	// The write is the newest truth for size and time. Coarse disk-mtime ticks
	// make a same-tick truncate+write look not-newer to Getattr, which then
	// serves the stale size (vim in-place save: reads see an empty file).
	if err == nil && n > 0 {
		f.metaMu.Lock()
		if f.stat != nil {
			if end := offset + int64(n); end > f.stat.Size {
				f.stat.Size = end
			}
			f.stat.Mtim = winfuse.NewTimespec(time.Now())
		}
		f.metaMu.Unlock()
	}

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
			defer func() { _ = platClose(fd) }()
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
		// Guard under metaMu: Read/OpenEx read these on the same *File via a
		// different lock (metaMu). metaMu is innermost; RemoteFilesLock stays held.
		rf.metaMu.Lock()
		rf.NotLocalSynced = false
		rf.LocalNewer = true
		rf.metaMu.Unlock()
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

// errBlockFetchIncomplete settles a read-ahead block whose leader exited without
// caching it (panic, early return, or write failure), so waiters re-fetch.
var errBlockFetchIncomplete = errors.New("read-ahead block fetch did not complete")

// blockFetch coalesces concurrent on-demand fetches of the same read-ahead
// block. The leader fetches the block and writes it to cache, then settles;
// waiters read the cached block instead of fetching it over the network again.
type blockFetch struct {
	done chan struct{}
	err  error // Published by settle(); read only via wait(), after done.
	once sync.Once
}

// settle records the result and wakes waiters, exactly once. Idempotent, so the
// normal finish and a panic backstop can both call it without double-closing done.
func (bf *blockFetch) settle(err error) {
	bf.once.Do(func() {
		bf.err = err
		close(bf.done)
	})
}

// wait blocks until the block is settled or ctx is cancelled. err is read only
// after done is observed (closed by settle), so it needs no lock; settled is
// false when ctx cancelled first.
func (bf *blockFetch) wait(ctx context.Context) (settled bool, err error) {
	select {
	case <-bf.done:
		return true, bf.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// beginBlockFetch registers the caller as the leader for the block at start, or
// returns the existing in-flight fetch to wait on. Caller must hold no other lock.
func (f *File) beginBlockFetch(start int64) (bf *blockFetch, leader bool) {
	f.fetchMu.Lock()
	defer f.fetchMu.Unlock()
	if f.inflight == nil {
		f.inflight = make(map[int64]*blockFetch)
	}
	if bf = f.inflight[start]; bf != nil {
		return bf, false
	}
	bf = &blockFetch{done: make(chan struct{})}
	f.inflight[start] = bf
	return bf, true
}

// finishBlockFetch settles the block and drops its in-flight entry. The delete is
// conditional so a late or backstop call cannot evict a newer leader's entry, and
// settle is idempotent, so calling this more than once for the same block is safe.
func (f *File) finishBlockFetch(start int64, bf *blockFetch, err error) {
	f.fetchMu.Lock()
	if f.inflight[start] == bf {
		delete(f.inflight, start)
	}
	f.fetchMu.Unlock()
	bf.settle(err)
}

// prefetchRange fetches blocks [fromBlock, toBlock) ahead of the read head and
// caches them, in ONE goroutine, SEQUENTIALLY and in order — the nearest block
// first, each at full link bandwidth. It deliberately does NOT fetch the blocks
// in parallel: on a single shared wire whose bandwidth-delay product is smaller
// than one 16 MiB block, parallel fetches of consecutive blocks just split the
// bandwidth and DELAY the block the reader needs next; in-order fetching delivers
// that block soonest. The goroutine holds one prefetch-semaphore slot for the
// whole range (one prefetch stream per file -> the shared bridge wire stays
// balanced) and is cancellable via ctx (a seek cancels the now-stale range).
// Each block is single-flighted (a read that catches up coalesces instead of
// re-fetching) and only fully-written chunks are marked present (the chunk-
// completeness invariant: never mark a partial/EOF chunk, or a later read there
// would serve sparse-hole zeros).
func (d *Dir) prefetchRange(f *File, pool *StreamPool, cacheFD *os.File, bitmap *ChunkBitmap, fromBlock, toBlock, remoteFileSize int64, ctx context.Context) {
	defer func() {
		f.raMu.Lock()
		f.raPrefetching = false
		f.raMu.Unlock()
		f.CacheWg.Done() // Release waits on this before closing cacheFD/pool.
	}()
	if pool == nil || cacheFD == nil || bitmap == nil {
		return
	}
	sem := d.Root.PrefetchSem
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
	}
	ra := int64(ReadAheadBlock)
	cs := int64(ChunkSize)
	for b := fromBlock; b < toBlock; b++ {
		if ctx.Err() != nil {
			return // shutdown or a seek cancelled this now-stale range
		}
		blockStart := b * ra
		if blockStart < 0 || (remoteFileSize > 0 && blockStart >= remoteFileSize) {
			return // past EOF
		}
		end := blockStart + ra
		if remoteFileSize > 0 && end > remoteFileSize {
			end = remoteFileSize
		}
		if end-blockStart <= 0 {
			return
		}
		if bitmap.HasRange(blockStart, int(end-blockStart)) {
			continue // already cached
		}
		bf, leader := f.beginBlockFetch(blockStart)
		if !leader {
			continue // a read or earlier prefetch already owns this block
		}
		d.raPrefetchCalls.Add(1) // observability: blocks actually fetched by read-ahead
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		data, err := pool.ReadAt(rctx, blockStart, end-blockStart)
		cancel()
		if err != nil || len(data) == 0 {
			// Best-effort: release this block's waiters and stop the range (a real
			// read will fetch on demand). Bailing also makes teardown prompt when
			// Release closes the pool out from under us.
			f.finishBlockFetch(blockStart, bf, errBlockFetchIncomplete)
			return
		}
		if _, werr := cacheFD.WriteAt(data, blockStart); werr != nil {
			f.finishBlockFetch(blockStart, bf, werr)
			return
		}
		dataEnd := blockStart + int64(len(data))
		for c := int(blockStart / cs); ; c++ {
			chunkBegin := int64(c) * cs
			chunkEnd := chunkBegin + cs
			if remoteFileSize > 0 && chunkEnd > remoteFileSize {
				chunkEnd = remoteFileSize
			}
			if chunkBegin >= dataEnd || chunkEnd > dataEnd {
				break
			}
			bitmap.Set(c)
			bitmap.SetHash(c, xxh3.Hash(data[chunkBegin-blockStart:chunkEnd-blockStart]))
		}
		f.finishBlockFetch(blockStart, bf, nil) // release waiters after the bytes are in cache
	}
}

// reconcileMinSize is the file-size floor below which a remote edit is not worth
// reconciling: re-pulling a small file on demand is cheaper than a GetChunkHashes
// round trip. 16 MiB (one ReadAheadBlock).
const reconcileMinSize = int64(ReadAheadBlock)

// maybeReconcileEdit is the eligibility gate the edit reset sites call. The site has
// ALREADY done today's full reset (newBitmap is the fresh, valid "re-fetch everything"
// state). This only ADDS a background optimization: when the pre-edit bitmap carried
// per-chunk fingerprints and the file is large enough, spawn reconcileEditAsync to
// re-mark the unchanged chunks present so reads serve them from cache instead of
// re-fetching. On any failure or ineligibility the full reset simply stands.
//
// oldBitmap/oldSize are captured at the call site BEFORE the reset (oldBitmap before
// f.Bitmap is reassigned, oldSize before f.stat is overwritten). newBitmap is the live
// post-reset f.Bitmap; newSize is the incoming notify size. Takes no RemoteFilesLock.
func (d *Dir) maybeReconcileEdit(path string, f *File, oldBitmap, newBitmap *ChunkBitmap, oldSize, newSize int64) {
	if oldBitmap == nil || newBitmap == nil || !oldBitmap.HasHashes() || newSize < reconcileMinSize {
		return
	}
	hasher, ok := d.OpenStreamProvider().(types.ChunkHasher)
	if !ok || hasher == nil {
		return // older peer / no RPC: full reset stands.
	}
	go d.reconcileEditAsync(path, f, oldBitmap, newBitmap, oldSize, newSize)
}

// reconcileEditAsync runs in its own goroutine after a remote-edit full reset and marks
// the chunks proven unchanged (our cached hash == the peer's new hash) present again on
// the LIVE bitmap captured at spawn. It is lock-free with respect to RemoteFilesLock,
// never waits on CacheWg, never writes the cache file, and never swaps f.Bitmap: the
// hash-matched chunks' bytes are already in cache (in-place edits leave the cache; the
// size-change site truncates, preserving the common prefix), so marking them present is
// correct. On ANY error or non-match it does nothing extra and the full reset stands.
//
// newBitmap is a specific object: if a later edit swaps f.Bitmap again, this job's Set
// calls land on an orphaned bitmap (harmless) and the newer edit spawns its own job.
// This job never re-reads f.Bitmap.
func (d *Dir) reconcileEditAsync(path string, f *File, oldBitmap, newBitmap *ChunkBitmap, oldSize, newSize int64) {
	if oldBitmap == nil || newBitmap == nil {
		return
	}
	// Bound concurrency with the same semaphore prefetch uses; abort on unmount/disconnect.
	ctx := d.FsCtx
	sem := d.Root.PrefetchSem
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
	}

	cs := int64(newBitmap.ChunkSizeBytes())
	if cs <= 0 {
		return
	}
	commonEnd := min(oldSize, newSize)
	count := uint64(commonEnd / cs) // only fully-covered common-prefix chunks are eligible
	if count == 0 {
		return
	}

	hasher, ok := d.OpenStreamProvider().(types.ChunkHasher)
	if !ok || hasher == nil {
		return
	}
	receiver, err := hasher.GetChunkHashes(ctx, path, uint64(cs), 0, count)
	if err != nil || receiver == nil {
		return // includes codes.Unimplemented: full reset stands.
	}

	peerHashes := make(map[int]uint64, count)
	for {
		idx, h, recvErr := receiver.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				break
			}
			return // transient failure: full reset stands.
		}
		peerHashes[int(idx)] = h
	}

	decided, ok := reconcileBitmap(oldBitmap, oldSize, newSize, peerHashes)
	if !ok {
		return
	}

	for c := 0; c < int(count); c++ {
		if !decided.Has(c) {
			continue
		}
		if newBitmap.Has(c) {
			continue // a concurrent read already fetched (new content) and marked it.
		}
		h, _ := decided.Hash(c)
		newBitmap.Set(c)
		newBitmap.SetHash(c, h)
	}
}

// maybeReadAhead is the predictive read-ahead detector, called on every read of a
// remote file but designed to ISSUE prefetches sparsely — at most once per
// ReadAheadBlock the read head crosses, never on every small read. It keeps a
// frontier, raPrefetchedTo (the next block index not yet prefetched), and extends
// it only when the head advances into a new block, so a stream of 512 KiB reads
// inside one 16 MiB block triggers ZERO work (the bug that defeats the purpose).
//
// The lead scales with sequential CONFIDENCE: each new block the head crosses
// sequentially doubles the window (16 -> 32 -> 64 ... blocks ahead), capped at
// ReadAheadWindowBlocks. The longer the confirmed sequential run, the further
// ahead it fetches; a slow consumer (video) and a fast bulk reader both converge
// to keeping ~window blocks resident. The design is deliberately optimistic:
//
//   - A non-sequential jump (seek) resets the window to 1 and restarts the
//     frontier at the new position; the jump read itself prefetches NOTHING, so a
//     mispredict wastes nothing. Worst case after a long run is one capped window
//     of blocks fetched that a sudden jump won't use — bounded by the cap.
//   - Read-ahead is withheld until the head is past the midpoint of its block, so
//     a header-only probe (a thumbnailer) never triggers a fetch.
func (d *Dir) maybeReadAhead(f *File, pool *StreamPool, cacheFD *os.File, bitmap *ChunkBitmap, offset int64, n int, remoteFileSize int64) {
	maxWin := d.ReadAheadWindowBlocks
	if maxWin <= 0 || pool == nil || cacheFD == nil || bitmap == nil {
		return
	}
	ra := int64(ReadAheadBlock)
	curBlock := offset / ra
	readEnd := offset + int64(n)

	f.raMu.Lock()
	headBlock := f.raHeadOffset / ra
	// FUSE delivers a sequential scan as PARALLEL, out-of-order reads (one read per
	// worker thread), so a stride test against the previous read sees constant
	// backward jumps and never fires. Classify by DISTANCE from the read head's
	// high-water mark instead: a read within +/- maxWin blocks of the head belongs to
	// this stream (parallel laggards and kernel look-ahead included); farther away is
	// a real seek. Order-independent.
	inStream := f.raStreamActive &&
		curBlock <= headBlock+int64(maxWin) &&
		curBlock >= headBlock-int64(maxWin)

	if !inStream {
		// Fresh file, or a seek: (re)start the stream here and prefetch NOTHING — a
		// mispredicted jump wastes no bandwidth, and a thumbnailer's scattered probes
		// each land here and never fetch. Cancel any now-stale in-flight prefetch.
		f.raStreamActive = true
		f.raStreamBytes = int64(n)
		f.raHeadOffset = readEnd
		f.raPrefetchedTo = curBlock + 1
		f.raWindowBlocks = 1
		if f.raCancel != nil {
			f.raCancel()
			f.raCancel = nil
		}
		f.raMu.Unlock()
		return
	}
	f.raStreamBytes += int64(n)
	if readEnd > f.raHeadOffset {
		f.raHeadOffset = readEnd
	}
	headBlock = f.raHeadOffset / ra
	// Thumbnail gate: fire read-ahead only after the stream has actually CONSUMED at
	// least half a block of bytes. A scattered probe (thumbnailer) advances the head
	// but reads almost nothing, so raStreamBytes stays tiny and it never prefetches;
	// a real player streaming through the file crosses the threshold quickly.
	if f.raStreamBytes < ra/2 {
		f.raMu.Unlock()
		return
	}
	// The frontier can never lag the read head (reset on miss).
	if f.raPrefetchedTo < headBlock+1 {
		f.raPrefetchedTo = headBlock + 1
	}
	// Refill only when the buffered lead ahead of the head has drained to half the
	// window; otherwise do nothing — reads inside the buffered region issue ZERO work.
	blocksAhead := f.raPrefetchedTo - (headBlock + 1)
	half := int64(f.raWindowBlocks) / 2
	if half < 1 {
		half = 1
	}
	if blocksAhead > half {
		f.raMu.Unlock()
		return
	}
	// One sequential prefetch in flight per file: a previous refill still running is
	// already fetching this forward region in order, so let it finish.
	if f.raPrefetching {
		f.raMu.Unlock()
		return
	}
	// Prime straight to the full window; prefetchRange fetches the blocks SEQUENTIALLY
	// in order at full bandwidth (nearest first), never a parallel burst that would
	// split the shared wire and delay the next-needed block.
	f.raWindowBlocks = maxWin
	from := f.raPrefetchedTo
	target := headBlock + 1 + int64(maxWin)
	f.raPrefetchedTo = target
	f.raPrefetching = true
	if f.raCancel != nil {
		f.raCancel() // release the previous (finished) refill's context
	}
	pctx, pcancel := context.WithCancel(d.FsCtx)
	f.raCancel = pcancel
	f.raMu.Unlock()

	f.CacheWg.Add(1) // paired with prefetchRange's deferred CacheWg.Done()
	go d.prefetchRange(f, pool, cacheFD, bitmap, from, target, remoteFileSize, pctx)
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
		bitmap = f.Bitmap
		// Read f.NotLocalSynced and f.stat under metaMu: Getattr mutates stat in
		// place and the notify handlers write NotLocalSynced, both via a different
		// lock than this one (OpenMapLock).
		f.metaMu.RLock()
		notLocalSynced = f.NotLocalSynced
		if f.stat != nil {
			remoteFileSize = f.stat.Size
		}
		f.metaMu.RUnlock()
	}
	d.OpenMapLock.RUnlock()

	// If the file is remote and has no stream pool but has incomplete chunks,
	// try to create a pool on-demand so we can fetch the missing data.
	if ok && notLocalSynced && pool == nil && bitmap != nil && !bitmap.IsComplete() && f.StreamProvider != nil {
		// logger.Info("Creating on-demand stream pool for incomplete remote file")
		streamCtx, streamCancel := context.WithCancel(d.FsCtx)
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
		hit := bitmap != nil && bitmap.HasRange(offset, len(buff))

		// Predictive read-ahead: on sequential access, fetch upcoming blocks
		// before the read head reaches them so a high-RTT link does not stall at
		// each block boundary. Bounded, single-flighted, self-tuning, and a no-op
		// when disabled (ReadAheadWindowBlocks == 0). Runs on both hit and miss so
		// the frontier keeps advancing while reads are served from cache.
		d.maybeReadAhead(f, pool, cacheFD, bitmap, offset, len(buff), remoteFileSize)

		if hit {
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

		// On-demand READ-AHEAD. A miss fetches a whole ReadAheadBlock (16 MiB),
		// aligned to a fixed grid, in one round trip and caches it; the next reads
		// in that block are then served locally. Block bounds are multiples of
		// ChunkSize, so the cache holds, and the bitmap only ever marks, COMPLETE
		// chunks. Marking a partially-written chunk "present" was the random-seek
		// corruption bug: a later read in that chunk took the HasRange fast path
		// and served sparse-hole zeros from the pre-allocated cache file.
		cs := int64(ChunkSize)
		ra := int64(ReadAheadBlock)
		reqEnd := offset + int64(len(buff))

		// Fetch the block containing this read. A read that straddles the block
		// boundary (rare: only within one read of a 16 MiB edge) falls back to a
		// chunk-aligned fetch of just the request, so we never short-read mid-file
		// and never split the response across two gRPC messages.
		blockStart := (offset / ra) * ra
		fetchStart := blockStart
		fetchEnd := blockStart + ra
		coalesce := true
		if reqEnd > fetchEnd {
			fetchStart = (offset / cs) * cs
			fetchEnd = ((reqEnd + cs - 1) / cs) * cs
			coalesce = false
		}
		if remoteFileSize > 0 && fetchEnd > remoteFileSize {
			fetchEnd = remoteFileSize
		}
		fetchLen := fetchEnd - fetchStart
		if fetchLen <= 0 {
			return 0
		}

		// Singleflight: concurrent and follow-on reads of the same block wait for
		// the leader's fetch to land in cache instead of each re-fetching 16 MiB.
		var bf *blockFetch
		isLeader := false
		if coalesce && cacheFD != nil {
			var leader bool
			bf, leader = f.beginBlockFetch(blockStart)
			if !leader {
				settled, err := bf.wait(d.FsCtx)
				// Serve from cache only if the leader actually cached our exact
				// span. A short block (remote truncation / EOF race) leaves the
				// tail unmarked, so HasRange catches it and we fetch it ourselves
				// rather than read a sparse hole.
				if settled && err == nil && bitmap != nil && bitmap.HasRange(offset, len(buff)) {
					n, preadErr := platPread(fd, buff, offset)
					if preadErr != nil {
						logger.Error("Local pread failed after block-fetch wait", "error", preadErr)
						return int(convertOsErrToSyscallErrno("pread", preadErr))
					}
					if remoteFileSize > 0 && offset+int64(n) > remoteFileSize {
						n = int(remoteFileSize - offset)
						if n < 0 {
							n = 0
						}
					}
					return n
				}
				if !settled {
					return 0 // Context cancelled while waiting.
				}
				// Leader failed or returned a short block; fetch it ourselves.
			} else {
				isLeader = true
			}
		}

		// A leader must release its waiters on every exit. settle() is idempotent,
		// so the normal finish (below, or in the async writer) wins; this backstop
		// only fires on a panic or an unforeseen early return, so waiters re-fetch
		// instead of hanging until unmount.
		asyncWriterScheduled := false
		if isLeader {
			defer func() {
				if !asyncWriterScheduled {
					f.finishBlockFetch(blockStart, bf, errBlockFetchIncomplete)
				}
			}()
		}

		// Retry loop for resilience against transient failures.
		var data []byte
		var readErr error
		waitBegin := time.Now() // The reader is blocked here until the fetch lands.
		for attempt := 0; attempt < 3; attempt++ {
			if d.FsCtx.Err() != nil {
				return 0 // The deferred backstop releases any waiters.
			}
			// Read the aligned block from the stream pool with a timeout.
			ctx, cancel := context.WithTimeout(d.FsCtx, 10*time.Second)
			data, readErr = pool.ReadAt(ctx, fetchStart, fetchLen)
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
				streamCtx, streamCancel := context.WithCancel(d.FsCtx)
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
			// Shutting down (unmount/disconnect cancelled FsCtx): stop quietly,
			// this is not a read error. The deferred backstop releases any waiters.
			if d.FsCtx.Err() != nil {
				return 0
			}
			// The peer genuinely no longer has the file (e.g. git's transient
			// .keep files): surface EOF so callers see an empty/absent file, as
			// before. The proxy maps a gRPC NotFound status to this sentinel.
			if errors.Is(readErr, types.ErrRemoteFileNotFound) {
				logger.Warn("Remote file not found, returning EOF", "error", readErr, "path", path)
				return 0
			}
			// Transient fetch failure after exhausting retries (timeout, connection
			// loss, or a server-side I/O error). Return EIO, NOT a short read: a
			// false EOF makes a media player believe the file ended mid-stream. No
			// portable errno asks the kernel or a blocking reader to retry (EAGAIN
			// is for non-blocking fds, EINTR for signals — read(2)), so EIO is the
			// correct cross-platform signal. The deferred backstop releases waiters.
			logger.Warn("Remote read stalled, returning EIO", "error", readErr, "path", path)
			return -winfuse.EIO
		}

		// The reader blocked on this synchronous fetch (a cache miss). At
		// intercontinental RTT this is a user-visible stall; log slow ones at
		// debug so a benchmark can count reader stalls without spamming normal
		// operation. Read-ahead should make these rare on sequential access.
		if waited := time.Since(waitBegin); waited > 50*time.Millisecond {
			logger.Debug("on-demand read blocked on fetch", "offset", offset, "block", blockStart, "waited", waited)
		}

		// Serve the requested [offset, offset+len(buff)) slice out of the block.
		sliceOff := offset - fetchStart
		var n int
		if sliceOff >= 0 && sliceOff < int64(len(data)) {
			n = copy(buff, data[sliceOff:])
		}

		// Track download progress and update checksum for the served bytes.
		f.Download.UpdateProgress(offset, n)
		if n > 0 {
			f.Download.UpdateChecksum(buff[:n])
		}

		// Persist the WHOLE block and mark only the chunks fully present in it
		// (handles a short read and the final EOF chunk). Done async, off the read
		// path; CacheWg is waited on in Release() before closing the FD. On a
		// leader this write also releases the block's waiters, after the bytes are
		// in cache, so a waiter never reads a sparse hole.
		if cacheFD != nil && len(data) > 0 {
			block := data
			base := fetchStart
			fsz := remoteFileSize
			bm := bitmap
			leaderBf := bf
			leader := isLeader
			asyncWriterScheduled = true // The writer now owns releasing waiters.
			f.CacheWg.Add(1)
			go func() {
				defer f.CacheWg.Done()
				if leader {
					// Backstop: release waiters even on panic (settle is idempotent).
					defer f.finishBlockFetch(blockStart, leaderBf, errBlockFetchIncomplete)
				}
				if _, werr := cacheFD.WriteAt(block, base); werr != nil {
					logger.Error("Async cache write failed", "error", werr)
					if leader {
						f.finishBlockFetch(blockStart, leaderBf, werr)
					}
					return
				}
				if bm != nil {
					end := base + int64(len(block))
					for c := int(base / cs); ; c++ {
						chunkBegin := int64(c) * cs
						chunkEnd := chunkBegin + cs
						if fsz > 0 && chunkEnd > fsz {
							chunkEnd = fsz
						}
						// Stop once a chunk isn't fully covered by the written block.
						if chunkBegin >= end || chunkEnd > end {
							break
						}
						bm.Set(c)
						bm.SetHash(c, xxh3.Hash(block[chunkBegin-base:chunkEnd-base]))
					}
				}
				if leader {
					f.finishBlockFetch(blockStart, leaderBf, nil)
				}
			}()
		}
		// An empty fetch (len(data)==0) caches nothing; the deferred backstop
		// releases waiters, and the bitmap gate makes them re-fetch.

		// logger.Debug("Read completed", "bytes", n, "progress", f.Download.Progress())
		return n
	}

	// Fallback: read directly from local file.
	// If handle was not in the map (ok=false), fd is 0 which is invalid.
	// Go straight to the reopen-by-path fallback in that case.
	n, err := 0, error(nil)
	if ok {
		n, err = platPread(fd, buff, offset)
	} else {
		err = syscall.EBADF
	}
	if err != nil {
		if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ENXIO) {
			// d.logger.Warn("EBADF on pread, falling back to reopen", "path", path, "fh", fh)
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			reopenFD, err2 := platOpen(cleanPath, syscall.O_RDONLY, 0)
			if err2 != nil {
				logger.Error("Fallback open failed", "error", err2, "cleanPath", cleanPath)
				return int(convertOsErrToSyscallErrno("open", err2))
			}
			defer func() { _ = platClose(reopenFD) }()
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
	if e := checkPath(path); e != 0 {
		return e
	}
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
	if e := checkPath(path); e != 0 {
		return e
	}
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
	if e := checkPath(path); e != 0 {
		return e, nil
	}
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
	if e := checkPath(path); e != 0 {
		return e
	}
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

// targetIdentity returns the announce-visible version of the file at path:
// the newest accepted announcement, raised by a local write when we hold
// authority. 0 = untracked (no identity to conflict with).
func (d *Dir) targetIdentity(path string) int64 {
	d.RemoteFilesLock.RLock()
	f := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()
	if f == nil {
		d.AfmLock.Lock()
		f = d.AllFileMap[path]
		d.AfmLock.Unlock()
	}
	if f == nil {
		return 0
	}
	f.metaMu.Lock()
	id := f.RemoteMtimeNs
	if f.LocalNewer && f.stat != nil {
		if m := f.stat.Mtim.Sec*1e9 + f.stat.Mtim.Nsec; m > id {
			id = m
		}
	}
	f.metaMu.Unlock()
	return id
}

// SwapWouldConflict reports whether a swap arriving for path with the given
// declared base would hit LOCAL authority it provably never saw. The rename
// handler must then not clobber the disk before the acceptance verdict runs.
func (d *Dir) SwapWouldConflict(path string, baseMtimeNs int64) bool {
	if baseMtimeNs == 0 {
		return false
	}
	d.RemoteFilesLock.RLock()
	f := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()
	if f == nil {
		d.AfmLock.Lock()
		f = d.AllFileMap[path]
		d.AfmLock.Unlock()
	}
	if f == nil {
		return false
	}
	f.metaMu.Lock()
	localNewer := f.LocalNewer
	id := f.RemoteMtimeNs
	if localNewer && f.stat != nil {
		if m := f.stat.Mtim.Sec*1e9 + f.stat.Mtim.Nsec; m > id {
			id = m
		}
	}
	f.metaMu.Unlock()
	return localNewer && baseMtimeNs < id
}

// conflictNames builds the sibling names for a preserved conflict version:
// report.docx -> report.conflict-20260801-104512-123456789.docx (UTC). The
// nanosecond part keeps the two peers' simultaneous preserves from colliding
// on one name and LWW-fighting each other. Bumps a numeric suffix until the
// real path is free.
func conflictNames(relPath, realPath string) (string, string) {
	now := time.Now()
	stamp := now.UTC().Format("20060102-150405") + "-" + strconv.Itoa(now.Nanosecond())
	ext := filepath.Ext(relPath)
	stem := strings.TrimSuffix(relPath, ext)
	rel := stem + ".conflict-" + stamp + ext
	for i := 2; ; i++ {
		real := filepath.Join(filepath.Dir(realPath), filepath.Base(rel))
		if _, err := os.Lstat(real); os.IsNotExist(err) {
			return rel, real
		}
		rel = stem + ".conflict-" + stamp + "-" + strconv.Itoa(i) + ext
	}
}

// registerConflictCopy makes a preserved version a normal local file: visible
// in the tree and announced, so both peers end with both versions.
func (d *Dir) registerConflictCopy(logger *slog.Logger, relPath, realPath string) {
	stgo, err := platLstat(realPath)
	if err != nil {
		logger.Warn("Conflict copy vanished before registration", "path", relPath, "error", err)
		return
	}
	st := new(winfuse.Stat_t)
	*st = stgo
	f := &File{
		logger:          logger,
		openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
		Name:            getNameFromPath(relPath),
		RelativePath:    relPath,
		RealPathOfFile:  realPath,
		OnLocalChange:   d.OnLocalChange,
		StreamProvider:  d.OpenStreamProvider(),
		IsLocalPresent:  true,
		LocalNewer:      true,
		stat:            st,
	}
	d.AfmLock.Lock()
	d.AllFileMap[relPath] = f
	d.AfmLock.Unlock()
	d.refreshDirStat(filepath.Dir(relPath))
	if d.OnLocalChange != nil {
		d.OnLocalChange(types.FileEvent{Path: relPath, Action: types.AddFile, Attr: types.StatToAttr(&stgo)})
	}
}

func (d *Dir) AddRemoteFile(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t) error {
	return d.AddRemoteFileWithBase(logger, path, name, stat, 0)
}

// AddRemoteFileWithBase is AddRemoteFile plus the announcement's declared base
// (see NotifyRequest.base_mtime_ns). Base 0 means unknown: plain LWW, never a
// conflict copy.
func (d *Dir) AddRemoteFileWithBase(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t, baseMtimeNs int64) error {
	// Normalize to FUSE convention: paths must have leading "/".
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// d.logger.Info("FUSE remote-add", "path", path, "size", stat.Size)

	// A peer edit of a local file must update the object local reads use.
	// A parallel entry makes reads serve the stale local copy forever.
	d.AfmLock.RLock()
	local, hasLocal := d.AllFileMap[path]
	d.AfmLock.RUnlock()

	d.RemoteFilesLock.Lock()
	if _, ok := d.RemoteFiles[path]; !ok && hasLocal {
		d.RemoteFiles[path] = local
	}

	if existing, ok := d.RemoteFiles[path]; ok {
		// Snapshot stat fields under metaMu: Getattr mutates existing.stat in
		// place under the same lock (map lock first, metaMu innermost).
		existing.metaMu.Lock()
		oldSize := existing.stat.Size
		existingStatMtime := existing.stat.Mtim.Sec*1e9 + existing.stat.Mtim.Nsec
		wasLocalNewer := existing.LocalNewer
		existing.metaMu.Unlock()
		sizeChanged := oldSize != stat.Size
		// Snapshot the incoming size now, BEFORE `existing.stat = stat` aliases this
		// struct into existing.stat: the size-changed branch reads it for os.Truncate
		// AFTER releasing RemoteFilesLock, where Getattr can mutate existing.stat
		// (the same struct) in place. The local copy keeps that read race-free.
		newSize := stat.Size

		// Reject stale ADD_FILE only when the incoming mtime is older than
		// what we already have. A debounced notification for a temp file can
		// arrive AFTER a RENAME set the correct size (git index-pack appends
		// 20 bytes between write and rename). But we cannot reject based on
		// size alone: git's HEAD goes from 25 bytes (.invalid placeholder)
		// to 21 bytes (ref: refs/heads/main), so smaller size can be correct.
		incomingMtime := stat.Mtim.Sec*1e9 + stat.Mtim.Nsec
		// Compare with the announced watermark, not stat.Mtim. Local Truncate
		// and Getattr write local time into stat.Mtim and make remote updates
		// look stale (pijul opens the source pristine RDWR+ftruncate).
		existingMtime := existing.RemoteMtimeNs
		if existingMtime == 0 {
			existingMtime = existingStatMtime
		}
		// A local write outranks an older announcement in flight. A stale ADD
		// must not overwrite newer local content (model checker finding).
		// Only real writes set LocalNewer; reader-side stat changes do not.
		if wasLocalNewer && existingStatMtime > existingMtime {
			existingMtime = existingStatMtime
		}
		accepted := incomingMtime > existingMtime
		// The edit is concurrent when its declared base is older than the
		// version we hold: it provably never saw our bytes (model v2, T3b).
		// Turn-taking edits carry base == our version and never trip this.
		conflict := accepted && wasLocalNewer && baseMtimeNs != 0 && baseMtimeNs < existingMtime
		if fuseOpLog {
			d.logger.Debug("oplog conflict predicate", "path", path, "accepted", accepted,
				"wasLocalNewer", wasLocalNewer, "base", baseMtimeNs, "identity", existingMtime, "conflict", conflict)
		}
		if accepted && d.OnLocalChange != nil {
			// A queued announce for this path describes the replaced content.
			// Its flush-time attr refresh would stamp the PEER'S bytes with a
			// fresh local mtime and bounce them back as a newer edit.
			d.OnLocalChange(types.FileEvent{Path: path, Action: types.CancelPendingNotify})
		}
		if accepted {
			existing.metaMu.Lock()
			existing.LocalNewer = false // Remote has newer content.
			existing.metaMu.Unlock()
		}
		if incomingMtime > existing.RemoteMtimeNs {
			existing.metaMu.Lock()
			existing.RemoteMtimeNs = incomingMtime
			existing.metaMu.Unlock()
		}
		if fuseOpLog {
			d.logger.Debug("oplog AddRemoteFile existing", "path", path, "sizeChanged", sizeChanged,
				"incomingMtime", incomingMtime, "existingMtime", existingMtime)
		}
		existing.metaMu.Lock()
		bmSnap := existing.Bitmap
		existing.metaMu.Unlock()
		if sizeChanged && stat.Size < oldSize && bmSnap != nil && bmSnap.Have() > 0 && incomingMtime < existingMtime {
			d.RemoteFilesLock.Unlock()
			return nil
		}
		// The announcement is older than a local write: keep local authority.
		if !accepted && wasLocalNewer {
			d.RemoteFilesLock.Unlock()
			return nil
		}

		// Preserve the losing local version before the acceptance clobbers it.
		// A rename is O(1); the canonical name re-fetches the winner in full.
		var conflictRel, conflictReal string
		if conflict && existing.RealPathOfFile != "" {
			conflictRel, conflictReal = conflictNames(path, existing.RealPathOfFile)
			if renErr := os.Rename(existing.RealPathOfFile, conflictReal); renErr != nil {
				logger.Warn("Conflict preserve failed, last-writer-wins", "path", path, "error", renErr)
				conflictRel, conflictReal = "", ""
			} else {
				_ = os.Remove(BitmapPath(existing.RealPathOfFile))
				if cf, cErr := os.OpenFile(existing.RealPathOfFile, os.O_CREATE|os.O_WRONLY, 0o644); cErr == nil { // #nosec G304
					_ = cf.Truncate(newSize)
					_ = cf.Close()
				}
				logger.Info("Concurrent edit: local version preserved", "path", path, "conflict", conflictRel)
			}
		}

		existing.metaMu.Lock()
		existing.stat = stat
		existing.metaMu.Unlock()

		if sizeChanged {
			// Size changed: cancel old prefetch, reset bitmap, re-download.
			existing.metaMu.Lock()
			existing.NotLocalSynced = true
			existing.metaMu.Unlock()
			existing.Download.Reset(uint64(stat.Size))
			if existing.PrefetchCancel != nil {
				existing.PrefetchCancel()
				existing.PrefetchCancel = nil
			}
			// Capture the pre-reset bitmap (hashes to compare) before swapping it,
			// and the fresh live bitmap reads will now use, for async reconcile.
			// Swap under metaMu: Release reads the pointer under the same lock.
			existing.metaMu.Lock()
			oldBitmap := existing.Bitmap
			existing.Bitmap = NewChunkBitmap(stat.Size)
			newBitmap := existing.Bitmap
			existing.metaMu.Unlock()
			if conflictRel != "" {
				// The local bytes moved to the conflict name: nothing cached
				// remains, so reconcile must not claim proven chunks.
				oldBitmap = nil
			}
			d.RemoteFilesLock.Unlock()

			// Only truncate if size actually changed — truncating to the same size
			// zeros out content that a previous prefetch already wrote. Use the
			// snapshot, not stat.Size: stat is now aliased to existing.stat, which
			// Getattr may be mutating now that RemoteFilesLock is released.
			if existing.RealPathOfFile != "" {
				if truncErr := os.Truncate(existing.RealPathOfFile, newSize); truncErr != nil && !os.IsNotExist(truncErr) {
					logger.Warn("Failed to truncate cache file to new size", "path", existing.RealPathOfFile, "size", newSize, "error", truncErr)
				}
			}

			// Truncate preserved the common prefix, so cached prefix bytes still match
			// their stored hashes: re-mark the proven-unchanged chunks present async.
			d.maybeReconcileEdit(path, existing, oldBitmap, newBitmap, oldSize, newSize)

			d.AfmLock.Lock()
			d.AllFileMap[path] = existing
			d.AfmLock.Unlock()
			// Metadata-only on announce. Read streams chunks on-demand; prefetch
			// (if enabled) starts when the file is Opened.
		} else {
			// Same size. By default keep the cache: a duplicate or stale
			// ADD_FILE re-announce must not destroy a completed prefetch. But a
			// genuine in-place edit keeps the byte length while advancing mtime
			// (live collab: "version-one" -> "version-two-x"). When the incoming
			// mtime is newer, the content changed under us — reset the bitmap +
			// cancel any in-flight prefetch so reads re-fetch the new bytes
			// on-demand, mirroring EditRemoteFile. Without this, a peer that
			// already fully cached the file keeps serving stale content.
			var oldBitmap, newBitmap *ChunkBitmap
			if fuseOpLog {
				d.logger.Debug("oplog same-size verdict", "path", path, "reset", incomingMtime > existingMtime)
			}
			if incomingMtime > existingMtime {
				existing.metaMu.Lock()
				existing.NotLocalSynced = true
				existing.metaMu.Unlock()
				existing.Download.Reset(uint64(stat.Size))
				if existing.PrefetchCancel != nil {
					existing.PrefetchCancel()
					existing.PrefetchCancel = nil
				}
				// In-place edit: same byte length, cache left intact, so prefix bytes
				// still match their stored hashes. Capture for async reconcile.
				existing.metaMu.Lock()
				oldBitmap = existing.Bitmap
				existing.Bitmap = NewChunkBitmap(stat.Size)
				newBitmap = existing.Bitmap
				existing.metaMu.Unlock()
				if conflictRel != "" {
					oldBitmap = nil
				}
			} else if existing.Bitmap != nil && !existing.Bitmap.IsComplete() {
				// Same mtime, still incomplete — keep fetching the rest on-demand.
				existing.metaMu.Lock()
				existing.NotLocalSynced = true
				existing.metaMu.Unlock()
			}
			d.RemoteFilesLock.Unlock()
			// No-op unless the in-place reset branch above ran (oldBitmap stays nil for
			// the same-mtime keep-cache path); then re-mark unchanged chunks present.
			// Same size here, so old==new==newSize (the unaliased snapshot, not
			// existing.stat which Getattr may now be mutating).
			d.maybeReconcileEdit(path, existing, oldBitmap, newBitmap, newSize, newSize)
			d.AfmLock.Lock()
			d.AllFileMap[path] = existing
			d.AfmLock.Unlock()
		}
		if conflictRel != "" {
			d.registerConflictCopy(logger, conflictRel, conflictReal)
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
		RemoteMtimeNs:   stat.Mtim.Sec*1e9 + stat.Mtim.Nsec,
	}
	d.RemoteFiles[path] = f
	d.RemoteFilesLock.Unlock()
	// Metadata-only on announce: registered with metadata + bitmap +
	// NotLocalSynced=true above. Read streams chunks on-demand; prefetch (if
	// enabled) starts when the file is Opened.
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
		f.metaMu.Lock()
		f.NotLocalSynced = false
		f.metaMu.Unlock()
		return
	}

	// Create cache file (empty). Pre-allocation to full size is deferred to
	// prefetchFile or OpenEx to avoid leaving zero-filled files when the
	// prefetch queue is full and the session closes before download starts.
	if err := os.MkdirAll(filepath.Dir(realPath), 0o750); err != nil {
		logger.Warn("Prefetch: failed to create dirs", "path", realPath, "error", err)
	}
	if _, statErr := os.Stat(realPath); os.IsNotExist(statErr) { // #nosec G304
		// Snapshot the mode off f.stat under metaMu (Getattr mutates the struct in
		// place); do NOT hold metaMu across the os.OpenFile below.
		fileMode := os.FileMode(0644)
		f.metaMu.RLock()
		if f.stat != nil {
			fileMode = os.FileMode(f.stat.Mode & 0o777)
		}
		f.metaMu.RUnlock()
		if fileMode == 0 {
			fileMode = 0644
		}
		lf, createErr := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, fileMode)
		if createErr != nil {
			logger.Warn("Prefetch: failed to create cache file", "path", realPath, "error", createErr)
			return
		}
		lf.Close()
	}

	ctx, cancel := context.WithCancel(d.FsCtx)
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

	// Pre-allocate now that we have a semaphore slot and will actually download.
	fileSize := bitmap.FileSize()
	if info, err := os.Stat(realPath); err == nil && info.Size() < fileSize {
		_ = os.Truncate(realPath, fileSize)
	}

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

	// Snapshot the mode off f.stat under metaMu (Getattr mutates the struct in
	// place); do NOT hold metaMu across the os.OpenFile below.
	fileMode := os.FileMode(0644)
	f.metaMu.RLock()
	if f.stat != nil {
		fileMode = os.FileMode(f.stat.Mode & 0o777)
	}
	f.metaMu.RUnlock()
	if fileMode == 0 {
		fileMode = 0644
	}
	lf, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, fileMode)
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

		// StreamFile sends in config.BlockSize (4 MiB) units while the bitmap
		// tracks 512 KiB chunks, so one Recv covers several chunks. Mark and hash
		// each chunk over this block (same span convention as the read-ahead path)
		// so a later SetHash never leaves a prefetch-only chunk reporting Hash==0.
		cs := int64(bitmap.ChunkSizeBytes())
		blockStart := int64(offset)
		dataEnd := blockStart + int64(len(data))
		fileSize := bitmap.FileSize()
		for c := int(blockStart / cs); ; c++ {
			chunkBegin := int64(c) * cs
			chunkEnd := chunkBegin + cs
			if fileSize > 0 && chunkEnd > fileSize {
				chunkEnd = fileSize
			}
			if chunkBegin >= dataEnd || chunkEnd > dataEnd {
				break
			}
			bitmap.Set(c)
			bitmap.SetHash(c, xxh3.Hash(data[chunkBegin-blockStart:chunkEnd-blockStart]))
		}
		f.Download.UpdateProgress(int64(offset), len(data))
	}

	if bitmap.IsComplete() {
		// logger.Info("Prefetch complete — all chunks downloaded", "fileSize", bitmap.FileSize())
		f.metaMu.Lock()
		f.NotLocalSynced = false
		f.metaMu.Unlock()
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
			RemoteMtimeNs:   stat.Mtim.Sec*1e9 + stat.Mtim.Nsec,
		}
		d.RemoteFiles[path] = newFile
		d.RemoteFilesLock.Unlock()
		// Ensure AllFileMap points to the same object so OpenEx sees correct state.
		d.AfmLock.Lock()
		d.AllFileMap[path] = newFile
		d.AfmLock.Unlock()
		// Metadata-only — the edited content is fetched on-demand on the next
		// read (or prefetched on Open). Keeps live collab lightweight.
		return nil
	}

	// Same watermark discipline as AddRemoteFile: stat.Mtim is local-clock-tainted.
	// Snapshot under metaMu: Getattr mutates f.stat in place under the same lock.
	f.metaMu.Lock()
	editStatMtime := f.stat.Mtim.Sec*1e9 + f.stat.Mtim.Nsec
	editLocalNewer := f.LocalNewer
	f.metaMu.Unlock()
	editRef := f.RemoteMtimeNs
	if editRef == 0 {
		editRef = editStatMtime
	}
	if editLocalNewer && editStatMtime > editRef {
		editRef = editStatMtime
	}
	incomingEditMtime := stat.Mtim.Sec*1e9 + stat.Mtim.Nsec
	if incomingEditMtime < editRef {
		d.RemoteFilesLock.Unlock()
		return syscall.ECANCELED
	}
	if incomingEditMtime > f.RemoteMtimeNs {
		f.RemoteMtimeNs = incomingEditMtime
	}

	// logger.Info("Remote file edited", "path", path, "mtime", stat.Mtim.Time())
	// Content changed: mark unsynced and reset bitmap + cancel prefetch so reads
	// re-fetch on-demand. The field stores go under metaMu (innermost); the
	// PrefetchCancel/Download/Bitmap resets stay outside it.
	// Snapshot sizes as unaliased locals before f.stat = stat: afterwards f.stat is
	// the same struct Getattr may mutate, so reading it post-unlock would race.
	oldSize := f.stat.Size
	newSize := stat.Size
	f.metaMu.Lock()
	f.stat = stat
	f.LocalNewer = false    // Remote has newer content.
	f.NotLocalSynced = true // Re-fetch the edited bytes on the next read.
	f.metaMu.Unlock()

	f.Download.Reset(uint64(stat.Size))
	if f.PrefetchCancel != nil {
		f.PrefetchCancel()
		f.PrefetchCancel = nil
	}
	// Capture the pre-reset bitmap (hashes to compare) and the fresh live bitmap;
	// the cache is left intact here, so common-prefix bytes still match their hashes.
	f.metaMu.Lock()
	oldBitmap := f.Bitmap
	f.Bitmap = NewChunkBitmap(stat.Size)
	newBitmap := f.Bitmap
	f.metaMu.Unlock()
	d.RemoteFilesLock.Unlock()
	// Re-mark the proven-unchanged chunks present in the background (full reset stands
	// on any failure or ineligibility).
	d.maybeReconcileEdit(path, f, oldBitmap, newBitmap, oldSize, newSize)
	d.AfmLock.Lock()
	d.AllFileMap[path] = f
	d.AfmLock.Unlock()
	return nil
}
