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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/pkg/xattr"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// Info about methods:
// https://pkg.go.dev/github.com/winfsp/cgofuse/fuse#FileSystemInterface

func (d *Dir) Access(path string, _mask uint32) int {
	logger := d.logger.With("method", "access", "path", path)
	logger.Info("Access", "path", path)

	d.RemoteFilesLock.RLock()
	_, ok := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()
	if ok {
		return 0
	}

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	stat := &syscall.Stat_t{}
	err := syscall.Stat(path, stat)
	if err != nil {
		logger.Error("Failed to stat", "error", err)
		return int(convertOsErrToSyscallErrno("stat", err))
	}

	return 0
}

func (d *Dir) Chmod(path string, mode uint32) int {
	// Return success. But we do not implement it.
	d.logger.Info("Chmod", "path", path)
	return 0
	// return -winfuse.ENOSYS
}

func (d *Dir) Chown(path string, uid uint32, gid uint32) int {
	d.logger.Info("Chown", "path", path)
	// Return success but we do not implement it.
	return 0
	// return -winfuse.ENOSYS
}

func (d *Dir) Create(path string, flags int, mode uint32) (int, uint64) {
	d.logger.Warn("FUSE Create", "path", path, "flags", flags, "mode", mode)
	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()

	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	relativePath := path
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "open", "path", path, "flags", flags)
	fd, err := syscall.Open(path, flags, mode)
	if err != nil {
		d.logger.Warn("FUSE Create FAILED", "path", path, "flags", flags, "mode", mode, "error", err)
		logger.Error("Failed to open path", "error", err)
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
	}

	d.AllFileMap[relativePath] = f
	d.OpenFileHandlers[uint64(fd)] = f

	d.logger.Warn("FUSE Create SUCCESS", "path", relativePath, "fd", fd)
	return 0, uint64(fd)
}

// Called on unmount.
func (d *Dir) Destroy() {
	d.logger.Info("Destroy")
}

func (d *Dir) Flush(path string, fh uint64) int {
	d.logger.Warn("FUSE Flush", "path", path, "fh", fh, "note", "returning ENOSYS - macOS apps may need this!")
	return -winfuse.ENOSYS
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) int {
	d.logger.Warn("FUSE Fsync", "path", path, "datasync", datasync, "fh", fh)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "fsync", "path", path)
	err := syscall.Fsync(int(fh))
	if err != nil {
		d.logger.Warn("FUSE Fsync FAILED", "path", path, "fh", fh, "error", err)
		logger.Error("Failed to fsync", "error", err)
		return int(convertOsErrToSyscallErrno("fsync", err))
	}

	d.logger.Warn("FUSE Fsync SUCCESS", "path", path, "fh", fh)
	return 0
}

func (d *Dir) Fsyncdir(path string, datasync bool, fh uint64) int {
	d.logger.Info("Fsyncdir", "path", path)
	return -winfuse.ENOSYS
}

func (d *Dir) Getattr(path string, stat *winfuse.Stat_t, fh uint64) int {
	logger := d.logger.With("method", "get-attr", "path", path, "fh", fh)
	logger.Info("Getattr")
	d.Adm.Lock()
	defer d.Adm.Unlock()
	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()

	stgo := syscall.Stat_t{}
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	isRemote := len(d.RemoteFiles) != 0
	if isRemote {
		d.RemoteFilesLock.RLock()
		defer d.RemoteFilesLock.RUnlock()
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
				remFile.LocalNewer = true
				copyFusestatFromFusestat(remFile.stat, auxStat)
				copyFusestatFromFusestat(stat, remFile.stat)
				return 0
			}

			copyFusestatFromFusestat(stat, remFile.stat)
			remFile.LocalNewer = false

			return 0
		}
	}

	err := syscall.Lstat(cleanPath, &stgo)
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
				openFileCounter: OpenFileCounter{},
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

func (d *Dir) Link(oldpath string, newpath string) int {
	d.logger.Info("Link", "oldPath", oldpath, "newPath", newpath, "inode", d.Inode)

	return -winfuse.ENOSYS
}

func (d *Dir) Mkdir(path string, mode uint32) int {
	d.logger.Info("Mkdir", "path", path, "inode", d.Inode)
	logger := d.logger.With("method", "mkdir", "path", path, "mode", mode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := syscall.Mkdir(path, mode)
	if err != nil {
		logger.Error("Failed to mkdir", "path", path, "error", err)
		return int(convertOsErrToSyscallErrno("mkdir", err))
	}
	return 0
}

func (d *Dir) Mknod(path string, mode uint32, dev uint64) int {
	d.logger.Info("Mknod", "path", path, "inode", d.Inode)

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "mknod", "path", path, "mode", mode, "dev", dev)
	err := syscall.Mknod(path, mode, int(dev))
	if err != nil {
		logger.Error("Failed to mknor", "errro", err)
		return int(convertOsErrToSyscallErrno("mknod", err))
	}
	return 0
}

func (d *Dir) Open(path string, flags int) (int, uint64) {
	d.logger.Warn("FUSE Open", "path", path, "flags", flags)
	logger := d.logger.With("method", "open", "path", path, "flags", flags)

	// TODO: Check flags. O_RW, O_RDONLY, O_WRITE, O_TRUNCATE.

	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	// First, check if file exists and if already open (brief lock)
	d.AfmLock.Lock()
	fh, ok := d.AllFileMap[path]
	if !ok {
		// File not in map - check if it exists on disk (pre-existing local file)
		if _, statErr := os.Stat(localPath); statErr == nil {
			// File exists on disk but not in map - create a File struct for it
			logger.Info("Pre-existing local file found, adding to map", "path", path)
			fh = &File{
				logger:          d.logger,
				openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
				Name:            getNameFromPath(path),
				RelativePath:    path,
				RealPathOfFile:  localPath,
				IsLocalPresent:  true,
				LocalNewer:      true,
				OnLocalChange:   d.OnLocalChange,
				StreamProvider:  d.OpenStreamProvider(),
				stat:            &winfuse.Stat_t{},
			}
			d.AllFileMap[path] = fh
		} else {
			d.AfmLock.Unlock()
			d.logger.Warn("FUSE Open FAILED - file not in map and not on disk", "path", path)
			return -winfuse.ENOENT, 0
		}
	}

	// File already opened. It exists. All good.
	// NOTE: We do NOT increment counter here because we're returning the same fh.
	// FUSE only calls Release once per unique fh returned from Open.
	// If we increment counter on every Open but Release is only called once,
	// the counter never reaches 0 and sync never happens.
	if fh.openFileCounter.CountOpenDescriptors() != 0 {
		inode := fh.Inode
		d.AfmLock.Unlock()

		d.logger.Warn("FUSE Open SUCCESS - already open (no counter increment)", "path", path, "fh", inode)
		logger.Info("We already opened it", "fh", inode)

		return 0, uint64(inode)
	}

	// Get info we need, then release lock before I/O operations
	isLocalPresent := fh.IsLocalPresent
	localNewer := fh.LocalNewer
	d.AfmLock.Unlock()

	// We do not have the file open.

	// Check if file is locally present.
	if isLocalPresent && localNewer {
		fd, err := syscall.Open(localPath, flags, 0)
		if err != nil {
			logger.Error("Failed to open path", "error", err)
			return int(convertOsErrToSyscallErrno("open", err)), 0
		}

		// Re-acquire locks to update state
		d.AfmLock.Lock()
		d.OpenMapLock.Lock()
		fh.Inode = uint64(fd)
		fh.openFileCounter.Open()
		d.OpenFileHandlers[fh.Inode] = fh
		d.OpenMapLock.Unlock()
		d.AfmLock.Unlock()

		logger.Info("We just opened local", "fh", fd)
		return 0, uint64(fd)
	}

	_, err := os.Stat(localPath)
	if err != nil {
		// Create the directory, and file.
		logger.Debug("The path we create dir at", "path", getPathWithoutName(localPath))
		err2 := os.MkdirAll(getPathWithoutName(localPath), 0o755)
		if err2 != nil {
			logger.Error("Failed to create folders along the path", "error", err2)
			return int(convertOsErrToSyscallErrno("open", err2)), 0
		}
		f, err2 := os.Create(localPath)
		if err2 != nil {
			logger.Error("Failed to create or truncate the file", "error", err2)
			return int(convertOsErrToSyscallErrno("open", err2)), 0
		}
		_ = f.Close()
	}

	fd, err := syscall.Open(localPath, flags, 0)
	if err != nil {
		logger.Error("Failed to open path", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	logger.Info("File inode before open", "inode", fh.Inode)

	// Open remote stream WITHOUT holding locks (this is a network call!)
	fsp := d.OpenStreamProvider()
	// TODO: need context with cancel.. on file close.
	stream, err := fsp.OpenRemoteFile(context.Background(), uint64(fd), path)
	if err != nil {
		syscall.Close(fd) // Clean up on failure
		d.logger.Error("Failed to open remote stream", "error", err)
		return -winfuse.EACCES, 0
	}

	// Re-acquire locks to update state
	d.AfmLock.Lock()
	d.OpenMapLock.Lock()
	fh.Inode = uint64(fd)
	fh.StreamProvider = fsp
	fh.RemoteFileStream = stream
	d.OpenFileHandlers[fh.Inode] = fh
	fh.openFileCounter.Open()
	d.OpenMapLock.Unlock()
	d.AfmLock.Unlock()

	d.logger.Warn("FUSE Open SUCCESS - new open",
		"path", path,
		"fh", fh.Inode,
		"isLocalPresent", fh.IsLocalPresent,
		"notLocalSynced", fh.NotLocalSynced,
		"hasRemoteStream", fh.RemoteFileStream != nil)
	logger.Info("Success with inode", "inode", fh.Inode)

	return 0, fh.Inode
}

func (d *Dir) Opendir(path string) (int, uint64) {
	d.logger.Info("Opendir", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "opendir", "path", path)
	f, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		logger.Error("Failed to open dir", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	return 0, uint64(f)
}

func (d *Dir) Readdir(path string, fill func(name string, stat *winfuse.Stat_t, offset int64) bool, offset int64, fh uint64) int {
	d.logger.Info("Readdir", "path", path, "inode", d.Inode)
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "readdir", "path", cleanPath, "fh", fh)

	dirEn, err := os.ReadDir(cleanPath)
	if err != nil {
		logger.Error("Failed to read dir", "error", err)
		return int(convertOsErrToSyscallErrno("readdir", err))
	}

	// Track local files to avoid duplicates with remote files
	localFiles := make(map[string]struct{})

	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, dir := range dirEn {
		localFiles[dir.Name()] = struct{}{}
		if !fill(dir.Name(), nil, 0) {
			break
		}
	}

	if len(d.RemoteFiles) == 0 {
		return 0
	}

	// Only list remote files that don't exist locally
	d.RemoteFilesLock.RLock()
	defer d.RemoteFilesLock.RUnlock()
	for k := range d.RemoteFiles {
		name := getNameFromPath(k)
		if _, exists := localFiles[name]; !exists {
			fill(name, nil, 0)
		}
	}

	return 0
}

func (d *Dir) Readlink(path string) (int, string) {
	d.logger.Info("Readlink", "path", path, "inode", d.Inode)

	return -winfuse.ENOSYS, ""
}

func (d *Dir) Release(path string, fh uint64) int {
	d.logger.Warn("FUSE Release", "path", path, "fh", fh)

	logger := d.logger.With("method", "release", "path", path, "fh", fh)

	// Lock FIRST to prevent race conditions with multiple releases
	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	f, ok := d.OpenFileHandlers[fh]
	if !ok {
		// File handle not in our map - close it anyway to be safe
		d.logger.Warn("FUSE Release - fh not in map, closing anyway", "path", path, "fh", fh)
		err := syscall.Close(int(fh))
		if err != nil {
			d.logger.Warn("FUSE Release FAILED - close error", "path", path, "fh", fh, "error", err)
			return int(convertOsErrToSyscallErrno("release", err))
		}
		return 0
	}

	d.logger.Warn("FUSE Release sync state",
		"path", path,
		"fh", fh,
		"counter", f.openFileCounter.CountOpenDescriptors(),
		"NotLocalSynced", f.NotLocalSynced,
		"NotRemoteSynced", f.NotRemoteSynced,
		"HadEdits", f.HadEdits,
		"IsLocalPresent", f.IsLocalPresent,
		"LocalNewer", f.LocalNewer)

	// Decrement counter - only close fd when all openers have released
	v := f.openFileCounter.Release()
	if v == 0 {
		// Last opener - actually close the fd
		err := syscall.Close(int(fh))
		if err != nil {
			d.logger.Warn("FUSE Release FAILED - close error", "path", path, "fh", fh, "error", err)
			logger.Error("Failed to release", "error", err)
			return int(convertOsErrToSyscallErrno("release", err))
		}

		delete(d.OpenFileHandlers, fh)

		if f.NotLocalSynced {
			f.NotLocalSynced = false
		}

		if f.NotRemoteSynced && d.OnLocalChange != nil {
			stgo := syscall.Stat_t{}
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			err := syscall.Lstat(cleanPath, &stgo)
			if err != nil {
				logger.Error("Failed to lstat path", "clean-path", cleanPath, "error-code", int(convertOsErrToSyscallErrno("lstat", err)), "error", err)
				cerr := convertOsErrToSyscallErrno("lstat", err)
				return int(cerr)
			}

			aux := &winfuse.Stat_t{}
			copyFusestatFromGostat(aux, &stgo)

			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.AddFile,
				Attr:   types.StatToAttr(aux),
			})

			f.NotLocalSynced = false
			// It was just created. Clear the edits.
			f.HadEdits = false
		}

		// It is remote synced. Add the edits.
		if f.HadEdits && d.OnLocalChange != nil {
			stgo := syscall.Stat_t{}
			cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
			err := syscall.Lstat(cleanPath, &stgo)
			if err != nil {
				logger.Error("Failed to lstat path", "clean-path", cleanPath, "error-code", int(convertOsErrToSyscallErrno("lstat", err)), "error", err)
				cerr := convertOsErrToSyscallErrno("lstat", err)
				return -int(cerr)
			}

			aux := &winfuse.Stat_t{}
			copyFusestatFromGostat(aux, &stgo)

			d.OnLocalChange(types.FileEvent{
				Path:   path,
				Action: types.EditFile,
				Attr:   types.StatToAttr(aux),
			})

			f.HadEdits = false
		}

		if !f.IsLocalPresent || !f.LocalNewer || f.NotLocalSynced {
			f.IsLocalPresent = true
			f.LocalNewer = false
			f.NotLocalSynced = false
		}

		if f.RemoteFileStream != nil {
			err = f.RemoteFileStream.Close()
			if err != nil {
				logger.Error("Failed to close remote file stream", "error", err)
			}
			f.RemoteFileStream = nil
		}
	}

	return 0
}

func (d *Dir) Releasedir(path string, fh uint64) int {
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
func (d *Dir) Rename(oldpath string, newpath string) int {
	d.logger.Warn("FUSE Rename called",
		"oldpath", oldpath,
		"newpath", newpath,
		"note", "macOS apps may use RENAME_SWAP - not supported by cgofuse")

	cleanOldPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, oldpath))
	cleanNewPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, newpath))
	logger := d.logger.With("method", "rename", "old-path", cleanOldPath, "new-path", cleanNewPath)

	d.logger.Warn("FUSE Rename resolved paths", "cleanOldPath", cleanOldPath, "cleanNewPath", cleanNewPath)

	err := syscall.Rename(cleanOldPath, cleanNewPath)
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

	d.logger.Warn("FUSE Rename SUCCESS", "oldpath", oldpath, "newpath", newpath)
	return 0
}

func (d *Dir) Rmdir(path string) int {
	d.logger.Info("Rmdir", "path", path, "inode", d.Inode)
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "rmdir", "path", path)
	err := syscall.Rmdir(cleanPath)
	if err != nil {
		logger.Error("Failed to remove dir", "error", err)
		return int(convertOsErrToSyscallErrno("rmdir", err))
	}

	// TODO: Remove also sub-files and sub dirs.

	return 0
}

func (d *Dir) Statfs(path string, stat *winfuse.Statfs_t) int {
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

	stgo := syscall.Statfs_t{}
	err := syscall_Statfs(cleanPath, &stgo)
	if err != nil {
		logger.Error("Failed to stat underlying folder", "error", err)
		return int(convertOsErrToSyscallErrno("statfs", err))
	}
	copyFusestatfsFromGostatfs(stat, &stgo)

	logger.Info("Statfs", "stat", stat, "inode", d.Inode)

	return 0
}

func (d *Dir) Symlink(target string, newpath string) int {
	d.logger.Info("Symlink", "target", target, "newpath", newpath, "inode", d.Inode)

	return -winfuse.ENOSYS
}

// Note: On windows open does not have a truncate flag,
// thus Open is immediately followed by Truncate.
func (d *Dir) Truncate(path string, size int64, fh uint64) int {
	d.logger.Info("Truncate", "path", path, "size", size, "inode", d.Inode, "fh", fh)

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "truncate", "path", path, "size", size, "fh", fh)
	err := syscall.Truncate(path, size)
	if err != nil {
		logger.Error("Faile to truncate", "error", err)
		return int(convertOsErrToSyscallErrno("truncate", err))
	}

	return 0
}

// Unlink removes a file.
func (d *Dir) Unlink(path string) int {
	d.logger.Info("Unlink", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "unlink", "path", path)
	err := syscall.Unlink(path)
	if err != nil {
		logger.Error("Failed to unlink", "error", err)
		return int(convertOsErrToSyscallErrno("unlink", err))
	}

	return 0
}

// Utimens changes the access and modification times of a file.
// Note: I do not care about it :^D for this version.
func (d *Dir) Utimens(path string, tmsp []winfuse.Timespec) int {
	d.logger.Info("Utimens", "path", path, "inode", d.Inode)

	return -winfuse.ENOSYS
}

// The method returns the number of bytes written.
func (d *Dir) Write(path string, buff []byte, offset int64, fh uint64) int {
	d.logger.Warn("FUSE Write", "path", path, "len", len(buff), "offset", offset, "fh", fh)
	logger := d.logger.With("method", "write", "path", path, "fh", fh, "offset", offset)
	d.OpenMapLock.RLock()
	defer d.OpenMapLock.RUnlock()

	f, ok := d.OpenFileHandlers[fh]
	if !ok {
		d.logger.Warn("FUSE Write FAILED - fd not found", "path", path, "fh", fh)
		logger.Error("Failed to find open FD", "error", syscall.EBADF)
		return -winfuse.EBADF
	}

	f.HadEdits = true

	n, err := syscall.Pwrite(int(fh), buff, offset)
	if err != nil {
		d.logger.Warn("FUSE Write FAILED", "path", path, "fh", fh, "error", err)
		logger.Error("Failed to write", "error", err)
		return -int(convertOsErrToSyscallErrno("pwrite", err))
	}

	d.logger.Warn("FUSE Write SUCCESS", "path", path, "bytesWritten", n, "fh", fh)
	return n
}

func (d *Dir) Read(path string, buff []byte, offset int64, fh uint64) int {
	logger := d.logger.With("method", "read", "path", path, "fh", fh, "offset", offset)
	d.logger.Warn("FUSE Read", "path", path, "bufLen", len(buff), "offset", offset, "fh", fh)

	// Check if this file has a remote stream
	d.OpenMapLock.RLock()
	defer d.OpenMapLock.RUnlock()

	f, ok := d.OpenFileHandlers[fh]
	if ok && f.NotLocalSynced && f.RemoteFileStream != nil {
		d.logger.Warn("FUSE Read from REMOTE", "path", path, "offset", offset, "bufLen", len(buff))
		// Read from remote stream
		data, err := f.RemoteFileStream.ReadAt(context.Background(), offset, int64(len(buff)))
		if err != nil {
			logger.Error("Failed to read data from remote", "error", err)
			return -winfuse.EBADF
		}

		// Copy remote data into buffer for FUSE
		n := copy(buff, data)

		// Write data into local file for caching
		lf, err := os.OpenFile(f.RealPathOfFile, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logger.Error("Failed to open local file for writing", "error", err)
			return -winfuse.EIO
		}
		defer lf.Close()

		// Write at offset (overwrite existing bytes)
		_, err = lf.WriteAt(data, offset)
		if err != nil {
			logger.Error("Failed to write remote data to local file", "error", err)
			return -winfuse.EIO
		}

		return n
	}

	// Fallback: read directly from local file
	d.logger.Warn("FUSE Read from LOCAL", "path", path, "offset", offset, "bufLen", len(buff))
	n, err := syscall.Pread(int(fh), buff, offset)
	if err != nil {
		d.logger.Warn("FUSE Read LOCAL FAILED", "path", path, "error", err)
		logger.Error("Failed to read local file", "error", err)
		return int(convertOsErrToSyscallErrno("pread", err))
	}

	d.logger.Warn("FUSE Read SUCCESS", "path", path, "bytesRead", n)
	return n
}

func (d *Dir) Removexattr(path string, name string) int {
	// Don't log xattr operations - too frequent
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	err := xattr.Remove(cleanPath, name)
	if err != nil {
		return int(convertOsErrToSyscallErrno("remove-xattr", err))
	}

	return 0
}

func (d *Dir) Listxattr(path string, fill func(name string) bool) int {
	// Don't log xattr operations - too frequent
	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	res, err := xattr.List(realPath)
	if err != nil {
		return int(convertOsErrToSyscallErrno("list-xattr", err))
	}

	for _, s := range res {
		fill(s)
	}

	return 0
}

func (d *Dir) Getxattr(path string, name string) (int, []byte) {
	// Note for the reader:
	// If the reader has a need for xattr, use the filesystem path instead of the
	// method signature path.
	// d.RealPathOfFile is the real path of d on the system.
	// but the catch is that the file/dir name in the method input path:
	// is the last segment, this implies that you need to
	// xattr.Get(d.RealPathOfFile+"/"+ name)

	// Don't log xattr lookups - too frequent and mostly expected failures
	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	res, err := xattr.Get(realPath, name)
	if err != nil {
		return int(convertOsErrToSyscallErrno("get-xattr", err)), nil
	}

	return 0, res
}

func (d *Dir) Setxattr(path string, name string, value []byte, flags int) int {
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
	d.RemoteFilesLock.Lock()
	defer d.RemoteFilesLock.Unlock()

	// Check if already in RemoteFiles - if so, update instead of failing
	if existing, ok := d.RemoteFiles[path]; ok {
		logger.Info("Remote file already exists, updating", "path", path)
		existing.stat = stat
		existing.NotLocalSynced = true
		return nil
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
	}

	d.RemoteFiles[path] = f

	return nil
}

func (d *Dir) EditRemoteFile(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t) error {
	d.RemoteFilesLock.RLock()
	defer d.RemoteFilesLock.RUnlock()

	f, ok := d.RemoteFiles[path]
	if !ok {
		logger.Error("Failed to edit file, it doesn't exists", "error", syscall.ENOENT)
		return syscall.ENOENT
	}

	if stat.Mtim.Time().Before(f.stat.Mtim.Time()) {
		logger.Error("Remote has older modifications than us, edit rejected", "error", syscall.ECANCELED)
		return syscall.ECANCELED
	}

	f.stat = stat
	f.NotLocalSynced = true

	return nil
}
