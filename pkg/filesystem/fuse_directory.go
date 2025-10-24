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
	d.logger.Info("Create", "path", path)
	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()

	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	relativePath := path
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "open", "path", path, "flags", flags)
	fd, err := syscall.Open(path, flags, mode)
	if err != nil {
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

	return 0, uint64(fd)
}

// Called on unmount.
func (d *Dir) Destroy() {
	d.logger.Info("Destroy")
}

func (d *Dir) Flush(path string, fh uint64) int {
	d.logger.Info("Flush", "path", path)
	return -winfuse.ENOSYS
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) int {
	d.logger.Info("Fsync", "path", path)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "fsync", "path", path)
	err := syscall.Fsync(int(fh))
	if err != nil {
		logger.Error("Failed to fsync", "error", err)
		return int(convertOsErrToSyscallErrno("fsync", err))
	}

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
	d.logger.Info("Open", "path", path, "inode", d.Inode)
	logger := d.logger.With("method", "open", "path", path, "flags", flags)

	// TODO: Check flags. O_RW, O_RDONLY, O_WRITE, O_TRUNCATE.

	d.AfmLock.Lock()
	defer d.AfmLock.Unlock()
	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()

	fh, ok := d.AllFileMap[path]
	if !ok {
		return -winfuse.ENOENT, 0
	}

	// File already opened. It exists. All good.
	if fh.openFileCounter.CountOpenDescriptors() != 0 {
		fh.openFileCounter.Open()

		logger.Info("We already opened it", "fh", fh.Inode)

		return 0, uint64(fh.Inode)
	}

	// We do not have the file open.

	localPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))

	// Check if file is locally present.
	if fh.IsLocalPresent && fh.LocalNewer {
		fd, err := syscall.Open(localPath, flags, 0)
		if err != nil {
			logger.Error("Failed to open path", "error", err)
			return int(convertOsErrToSyscallErrno("open", err)), 0
		}

		fh.Inode = uint64(fd)
		fh.openFileCounter.Open()

		d.OpenFileHandlers[fh.Inode] = fh

		logger.Info("We just opened local", "fh", fh.Inode)

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

	fh.Inode = uint64(fd)

	fsp := d.OpenStreamProvider()
	fh.StreamProvider = fsp
	// TODO: need context with cancel.. on file close.
	stream, err := fsp.OpenRemoteFile(context.Background(), fh.Inode, path)
	if err != nil {
		d.logger.Error("Failed to open remote stream", "error", err)
		return -winfuse.EACCES, 0
	}
	fh.RemoteFileStream = stream
	d.OpenFileHandlers[fh.Inode] = fh

	fh.openFileCounter.Open()

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
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "readdir", "path", path, "fh", fh)

	dirEn, err := os.ReadDir(path)
	if err != nil {
		logger.Error("Failed to read dir", "error", err)
		return int(convertOsErrToSyscallErrno("readdir", err))
	}

	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, dir := range dirEn {
		if !fill(dir.Name(), nil, 0) {
			break
		}
	}

	if len(d.RemoteFiles) == 0 {
		return 0
	}

	d.RemoteFilesLock.RLock()
	defer d.RemoteFilesLock.RUnlock()
	for k := range d.RemoteFiles {
		fill(getNameFromPath(k), nil, 0)
	}

	return 0
}

func (d *Dir) Readlink(path string) (int, string) {
	d.logger.Info("Readlink", "path", path, "inode", d.Inode)

	return -winfuse.ENOSYS, ""
}

func (d *Dir) Release(path string, fh uint64) int {
	d.logger.Info("Release", "path", path, "inode", d.Inode, "fh", fh)

	logger := d.logger.With("method", "release", "path", path, "fh", fh)
	err := syscall.Close(int(fh))
	if err != nil {
		logger.Error("Failed to release", "error", err)
		return int(convertOsErrToSyscallErrno("release", err))
	}

	d.OpenMapLock.Lock()
	defer d.OpenMapLock.Unlock()
	f, ok := d.OpenFileHandlers[fh]
	if ok {
		v := f.openFileCounter.Release()
		if v == 0 {
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
func (d *Dir) Rename(oldpath string, newpath string) int {
	d.logger.Info("Rename", "oldpath", oldpath, "newpath", newpath, "inode", d.Inode)
	oldpath = filepath.Clean(filepath.Join(d.LocalDownloadFolder, oldpath))
	newpath = filepath.Clean(filepath.Join(d.LocalDownloadFolder, newpath))
	logger := d.logger.With("method", "rename", "old-path", oldpath, "new-path", newpath)
	err := syscall.Rename(oldpath, newpath)
	if err != nil {
		logger.Error("Failed to rename", "error", err)
		return int(convertOsErrToSyscallErrno("rename", err))
	}

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
	d.logger.Info("Write", "path", path, "inode", d.Inode, "fh", fh)
	logger := d.logger.With("method", "write", "path", path, "fh", fh, "offset", offset)
	d.OpenMapLock.RLock()
	defer d.OpenMapLock.RUnlock()

	f, ok := d.OpenFileHandlers[fh]
	if !ok {
		logger.Error("Failed to find open FD", "error", syscall.EBADF)
		return -winfuse.EBADF
	}

	f.HadEdits = true

	n, err := syscall.Pwrite(int(fh), buff, offset)
	if err != nil {
		logger.Error("Failed to write", "error", err)
		return -int(convertOsErrToSyscallErrno("pwrite", err))
	}

	return n
}

func (d *Dir) Read(path string, buff []byte, offset int64, fh uint64) int {
	logger := d.logger.With("method", "read", "path", path, "fh", fh, "offset", offset)
	logger.Info("Read")

	// Check if this file has a remote stream
	d.OpenMapLock.RLock()
	defer d.OpenMapLock.RUnlock()

	f, ok := d.OpenFileHandlers[fh]
	if ok && f.NotLocalSynced && f.RemoteFileStream != nil {
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
	n, err := syscall.Pread(int(fh), buff, offset)
	if err != nil {
		logger.Error("Failed to read local file", "error", err)
		return int(convertOsErrToSyscallErrno("pread", err))
	}

	return n
}

func (d *Dir) Removexattr(path string, name string) int {
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "remove-xattr", "path", path, "name", name)

	err := xattr.Remove(path, name)
	if err != nil {
		logger.Error("Failed to remove xattr", "error", err)
		return int(convertOsErrToSyscallErrno("remove-xattr", err))
	}

	return 0
}

func (d *Dir) Listxattr(path string, fill func(name string) bool) int {
	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "list-xattr", "path", path, "real-path", realPath)

	res, err := xattr.List(realPath)
	if err != nil {
		logger.Error("Failed to list xattr", "error", err)
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

	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "get-xattr", "path", path, "real-path", realPath, "xattr-name", name)

	res, err := xattr.Get(realPath, name)
	if err != nil {
		logger.Error("Failed to get xattr", "error", err)
		return int(convertOsErrToSyscallErrno("get-xattr", err)), nil
	}

	return 0, res
}

func (d *Dir) Setxattr(path string, name string, value []byte, flags int) int {
	// I do not support flags for this version.
	_ = flags

	realPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.With("method", "set-xattr", "path", path, "real-path", realPath, "name", name, "val", string(value), "flags", flags)

	err := xattr.Set(realPath, name, value)
	if err != nil {
		logger.Error("Failed to set xattr", "error", err)
		return int(convertOsErrToSyscallErrno("set-xattr", err))
	}

	return 0
}

// Non-FUSE helper methods, used for keeping track of sync.

// Notes: I am confident that it is not a good idea to use syscall errors for GRPC called methods.

func (d *Dir) AddRemoteFile(logger *slog.Logger, path string, name string, stat *winfuse.Stat_t) error {
	d.AfmLock.RLock()
	defer d.AfmLock.RUnlock()

	d.RemoteFilesLock.Lock()
	defer d.RemoteFilesLock.Unlock()
	_, ok := d.AllFileMap[path]
	if ok {
		logger.Error("Failed to create file, it already exists", "error", syscall.EEXIST)
		return syscall.EEXIST
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
