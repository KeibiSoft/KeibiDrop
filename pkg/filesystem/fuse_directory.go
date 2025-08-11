package filesystem

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/pkg/xattr"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// Info about methods:
// https://pkg.go.dev/github.com/winfsp/cgofuse/fuse#FileSystemInterface

func (d *Dir) Access(path string, _mask uint32) int {
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "access", "path", path)
	logger.Info("Access")
	stat := &syscall.Stat_t{}
	err := syscall.Stat(path, stat)
	if err != nil {
		logger.Error("Failed to stat", "error", err)
		return int(convertOsErrToSyscallErrno("stat", err))
	}

	return 0
}

func (d *Dir) Chmod(path string, mode uint32) int {
	d.logger.Info("Chmod", "path", path)
	return -winfuse.ENOSYS
}

func (d *Dir) Chown(path string, uid uint32, gid uint32) int {
	d.logger.Info("Chown", "path", path)
	return -winfuse.ENOSYS
}

func (d *Dir) Create(path string, flags int, mode uint32) (int, uint64) {
	d.logger.Info("Create", "path", path)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "open", "path", path, "flags", flags)
	fd, err := syscall.Open(path, flags, mode)
	if err != nil {
		logger.Error("Failed to open path", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	return 0, uint64(fd)
}

// Called on unmount.
func (d *Dir) Destroy() {
	d.logger.Info("Destroy")
}

func (d *Dir) Flush(path string, fh uint64) int {
	d.logger.Info("Flush", "path", path)
	return winfuse.ENOSYS
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) int {
	d.logger.Info("Fsync", "path", path)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "fsync", "path", path)
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
	logger := d.logger.New("method", "get-attr", "path", path, "fh", fh)
	logger.Info("Getattr")

	stgo := syscall.Stat_t{}
	cleanPath := filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := syscall.Lstat(cleanPath, &stgo)
	if err != nil {
		logger.Error("Failed to lstat path", "clean-path", cleanPath, "error-code", int(convertOsErrToSyscallErrno("lstat", err)), "error", err)
		cerr := convertOsErrToSyscallErrno("lstat", err)
		// Path not locally present, thus it is present on remote.
		if -cerr == syscall.ENOENT {
			d.adm.RLock()
			dir, ok := d.AllDirMap[path]
			if ok && dir.stat != nil {
				copyFusestatFromFusestat(stat, dir.stat)
				d.adm.RUnlock()
				return 0
			}
			d.adm.RUnlock()

			d.afm.RLock()
			f, ok := d.AllFileMap[path]
			if ok && f.stat != nil {
				copyFusestatFromFusestat(stat, f.stat)
				d.afm.RUnlock()
				return 0
			}
			d.afm.RUnlock()
		}
		return int(cerr)
	}

	// Note: We do not use Lampert timestamps, last edit wins.

	copyFusestatFromGostat(stat, &stgo)
	gtAtim := func(fst, snd winfuse.Timespec) bool {
		return fst.Time().After(snd.Time())
	}

	found := false

	d.adm.RLock()
	dir, ok := d.AllDirMap[path]
	if ok && dir.stat != nil && gtAtim(dir.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(stat, dir.stat)
	}
	if ok && dir.stat != nil && !gtAtim(dir.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(stat, dir.stat)
		dir.NotifyPeer()
	}
	if ok {
		found = ok
	}
	d.adm.RUnlock()

	d.afm.RLock()
	f, ok := d.AllFileMap[path]
	if ok && f.stat != nil && gtAtim(f.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(stat, f.stat)
	}
	if ok && f.stat != nil && !gtAtim(f.stat.Mtim, stat.Mtim) {
		copyFusestatFromFusestat(f.stat, stat)
		f.NotifyPeer()
	}
	if ok {
		found = ok
	}
	d.afm.RUnlock()

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
				fcl:                 sync.RWMutex{},
				dcl:                 sync.RWMutex{},
				adm:                 sync.RWMutex{},
				afm:                 sync.RWMutex{},
				Inode:               stat.Ino,
				RelativePath:        path,
				LocalDownloadFolder: cleanPath, // Maybe remove the last segment?
				IsLocalPresent:      true,
				Root:                d,
				OpenFileHandlers:    make(map[uint64]*File),
				OpenMapLock:         sync.RWMutex{},
				PeerLastEdit:        0,
				FileChildren:        make(map[uint64]*File),
				DirChildren:         make(map[uint64]*Dir),
				AllDirMap:           make(map[string]*Dir),
				AllFileMap:          make(map[string]*File),
				stat:                &winfuse.Stat_t{},
			}
			copyFusestatFromFusestat(dir.stat, stat)
			d.adm.Lock()
			d.AllDirMap[path] = dir
			d.adm.Unlock()

			dir.NotifyPeer()
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
				Name:            "TODO", // Maybe not needed.
				stat:            &winfuse.Stat_t{},
			}
			copyFusestatFromFusestat(f.stat, stat)

			d.afm.Lock()
			d.AllFileMap[path] = f
			d.afm.Unlock()

			f.NotifyPeer()
		}

	}

	return 0
}

func (d *Dir) Init() {
	d.logger.Debug("Init", "inode", d.Inode)
	// syscall.Chdir(d.LocalDownloadFolder)

}

func (d *Dir) Link(oldpath string, newpath string) int {
	d.logger.Debug("Link", "oldPath", oldpath, "newPath", newpath, "inode", d.Inode)

	return -winfuse.ENOSYS
}

func (d *Dir) Mkdir(path string, mode uint32) int {
	d.logger.Debug("Mkdir", "path", path, "inode", d.Inode)
	logger := d.logger.New("method", "mkdir", "path", path, "mode", mode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	err := syscall.Mkdir(path, mode)
	if err != nil {
		logger.Error("Failed to mkdir", "path", path, "error", err)
		return int(convertOsErrToSyscallErrno("mkdir", err))
	}
	return 0
}

func (d *Dir) Mknod(path string, mode uint32, dev uint64) int {
	d.logger.Debug("Mknod", "path", path, "inode", d.Inode)

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "mknod", "path", path, "mode", mode, "dev", dev)
	err := syscall.Mknod(path, mode, int(dev))
	if err != nil {
		logger.Error("Failed to mknor", "errro", err)
		return int(convertOsErrToSyscallErrno("mknod", err))
	}
	return 0
}

func (d *Dir) Open(path string, flags int) (int, uint64) {
	d.logger.Debug("Open", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "open", "path", path, "flags", flags)
	fd, err := syscall.Open(path, flags, 0)
	if err != nil {
		logger.Error("Failed to open path", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	return 0, uint64(fd)
}

func (d *Dir) Opendir(path string) (int, uint64) {
	d.logger.Debug("Opendir", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "opendir", "path", path)
	f, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		logger.Error("Failed to open dir", "error", err)
		return int(convertOsErrToSyscallErrno("open", err)), 0
	}

	return 0, uint64(f)
}

func (d *Dir) Readdir(path string, fill func(name string, stat *winfuse.Stat_t, offset int64) bool, offset int64, fh uint64) int {
	d.logger.Debug("Readdir", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "readdir", "path", path, "fh", fh)

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

	return 0
}

func (d *Dir) Readlink(path string) (int, string) {
	d.logger.Debug("Readlink", "path", path, "inode", d.Inode)

	return -winfuse.ENOSYS, ""
}

func (d *Dir) Release(path string, fh uint64) int {
	d.logger.Debug("Release", "path", path, "inode", d.Inode, "fh", fh)

	logger := d.logger.New("method", "release", "path", path, "fh", fh)
	err := syscall.Close(int(fh))
	if err != nil {
		logger.Error("Failed to release", "error", err)
		return int(convertOsErrToSyscallErrno("release", err))
	}

	return 0
}

func (d *Dir) Releasedir(path string, fh uint64) int {
	d.logger.Debug("Releasedir", "path", path, "inode", d.Inode, "fh", fh)
	logger := d.logger.New("method", "release-dir", "path", path, "fh", fh)
	err := syscall.Close(int(fh))
	if err != nil {
		logger.Error("Failed to release", "error", err)
		return int(convertOsErrToSyscallErrno("release", err))
	}

	return 0
}

// Mac OS High Level apps use Rename SWAP, which is really fun from my experience.
func (d *Dir) Rename(oldpath string, newpath string) int {
	d.logger.Debug("Rename", "oldpath", oldpath, "newpath", newpath, "inode", d.Inode)
	oldpath = filepath.Clean(filepath.Join(d.LocalDownloadFolder, oldpath))
	newpath = filepath.Clean(filepath.Join(d.LocalDownloadFolder, newpath))
	logger := d.logger.New("method", "rename", "old-path", oldpath, "new-path", newpath)
	err := syscall.Rename(oldpath, newpath)
	if err != nil {
		logger.Error("Failed to rename", "error", err)
		return int(convertOsErrToSyscallErrno("rename", err))
	}

	return 0
}

func (d *Dir) Rmdir(path string) int {
	d.logger.Debug("Rmdir", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "rmdir", "path", path)
	err := syscall.Rmdir(path)
	if err != nil {
		logger.Error("Failed to remove dir", "error", err)
		return int(convertOsErrToSyscallErrno("rmdir", err))
	}

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
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "statfs", "path", path)

	stgo := syscall.Statfs_t{}
	err := syscall_Statfs(path, &stgo)
	if err != nil {
		logger.Error("Faield to stat underlying folder", "error", err)
		return int(convertOsErrToSyscallErrno("statfs", err))
	}
	copyFusestatfsFromGostatfs(stat, &stgo)

	logger.Info("Statfs", "stat", stat, "inode", d.Inode)

	return 0
}

func (d *Dir) Symlink(target string, newpath string) int {
	d.logger.Debug("Symlink", "target", target, "newpath", newpath, "inode", d.Inode)

	return -winfuse.ENOSYS
}

// Note: On windows open does not have a truncate flag,
// thus Open is immediately followed by Truncate.
func (d *Dir) Truncate(path string, size int64, fh uint64) int {
	d.logger.Debug("Truncate", "path", path, "size", size, "inode", d.Inode, "fh", fh)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "truncate", "path", path, "size", size, "fh", fh)
	err := syscall.Truncate(path, size)
	if err != nil {
		logger.Error("Faile to truncate", "error", err)
		return int(convertOsErrToSyscallErrno("truncate", err))
	}

	return 0
}

// Unlink removes a file.
func (d *Dir) Unlink(path string) int {
	d.logger.Debug("Unlink", "path", path, "inode", d.Inode)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "unlink", "path", path)
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
	d.logger.Debug("Utimens", "path", path, "inode", d.Inode)

	return -winfuse.ENOSYS
}

// The method returns the number of bytes written.
func (d *Dir) Write(path string, buff []byte, offset int64, fh uint64) int {
	d.logger.Debug("Write", "path", path, "inode", d.Inode, "fh", fh)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "write", "path", path, "fh", fh, "offset", offset)

	n, err := syscall.Pwrite(int(fh), buff, offset)
	if err != nil {
		logger.Error("Failed to write", "error", err)
		return int(convertOsErrToSyscallErrno("pwrite", err))
	}

	return n
}

// The method returns the number of bytes read.
func (d *Dir) Read(path string, buff []byte, offset int64, fh uint64) int {
	d.logger.Debug("Read", "path", path, "inode", d.Inode, "fh", fh)
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "read", "path", path, "fh", fh, "offset", offset)

	n, err := syscall.Pread(int(fh), buff, offset)
	if err != nil {
		logger.Error("Failed to read", "error", err)
		return int(convertOsErrToSyscallErrno("pread", err))
	}

	return n
}

// Notes about extended attributes:
// Personally I have no care for them.
//
// But MacOS cares a bit too much about them (the only reason they are implemented here).
//
// Windows cares in the sense European Union cares about Romania:
// Meaning that EU (Windows) behaves like the rich grandmother
// who financially supports "that" cousin who sniffs dried wall paint (extended attributes)
// all for the sake of "regional security" and "greater values". But we all know
// people will just meme with ACLs and not bother with "download date" of files in the Xattr,
// and LARP some success metric of we "inreased security to 80%" because of this is "how you measure it".
//
// My decision is to just support them at the mounted filesystem level.
// If the underlying filesystem has xattrs, good for them, they wont be mapped to the mounted one,
// nor shared between peers.

func (d *Dir) Removexattr(path string, name string) int {
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "remove-xattr", "path", path, "name", name)

	err := xattr.Remove(path, name)
	if err != nil {
		logger.Error("Failed to remove xattr", "error", err)
		return int(convertOsErrToSyscallErrno("remove-xattr", err))
	}

	return 0
}

func (d *Dir) Listxattr(path string, fill func(name string) bool) int {
	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "list-xattr", "path", path)

	res, err := xattr.List(path)
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

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "get-xattr", "path", path, "name", name)

	res, err := xattr.Get(path, name)
	if err != nil {
		logger.Error("Failed to get xattr", "error", err)
		return int(convertOsErrToSyscallErrno("get-xattr", err)), nil
	}

	return 0, res
}

func (d *Dir) Setxattr(path string, name string, value []byte, flags int) int {
	// I do not support flags for this version.
	_ = flags

	path = filepath.Clean(filepath.Join(d.LocalDownloadFolder, path))
	logger := d.logger.New("method", "set-xattr", "path", path, "name", name, "val", string(value))

	err := xattr.Set(path, name, value)
	if err != nil {
		logger.Error("Failed to set xattr", "error", err)
		return int(convertOsErrToSyscallErrno("set-xattr", err))
	}

	return 0
}
