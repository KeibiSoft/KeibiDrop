package filesystem

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/xattr"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// Info about methods:
// https://pkg.go.dev/github.com/winfsp/cgofuse/fuse#FileSystemInterface

func (d *Dir) Access(path string, _mask uint32) int {
	return 0
}

func (d *Dir) Chmod(path string, mode uint32) int {
	return 0
}

func (d *Dir) Chown(path string, uid uint32, gid uint32) int {
	return 0
}

func (d *Dir) Create(path string, flags int, mode uint32) (int, uint64) {
	return 0, 0
}

// Called on unmount.
func (d *Dir) Destroy() {
}

func (d *Dir) Flush(path string, fh uint64) int {
	return 0
}

func (d *Dir) Fsync(path string, datasync bool, fh uint64) int {
	return 0
}

func (d *Dir) Fsyncdir(path string, datasync bool, fh uint64) int {
	return winfuse.ENOTSUP
}

func (d *Dir) Getattr(path string, stat *winfuse.Stat_t, fh uint64) int {
	logger := d.logger.New("method", "get-attr", "path", path, "fh", fh)

	// No worries about slasher vs backslasher generes of movies, cgofuse is our saviour.
	// splitPath := filepath.SplitList(path)
	// name := splitPath[len(splitPath)-1]

	uid, gid, _ := winfuse.Getcontext()
	stat.Uid = uid
	stat.Gid = gid

	if path == "." {
		logger.Info("Get attr for current dir or parent. Fix later.")
		info, err := os.Stat(d.LocalDownloadFolder)
		if err != nil {
			logger.Error("Failed to stat local download path", "download-path", d.LocalDownloadFolder, "error", err)
			return int(convertOsErrToSyscallErrno("stat", err))
		}

		blks := info.Size() / FilesystemBlockSize
		now := winfuse.NewTimespec(info.ModTime())
		stat.Atim = now
		stat.Birthtim = now
		stat.Blksize = FilesystemBlockSize
		stat.Blocks = blks
		stat.Ctim = now
		stat.Ino = d.Inode
		stat.Mode = 0600
		stat.Mtim = now
		stat.Nlink = 0 // TODO
		stat.Rdev = 0  // TODO
		stat.Flags = 0
	}

	if path == "/" || path == d.Name || fh == d.Inode {
		logger.Debug("Ching")
		info, err := os.Stat(d.LocalDownloadFolder)
		if err != nil {
			logger.Error("Failed to stat local download path", "download-path", d.LocalDownloadFolder, "error", err)
			return int(convertOsErrToSyscallErrno("stat", err))
		}

		blks := info.Size() / FilesystemBlockSize
		now := winfuse.NewTimespec(info.ModTime())
		stat.Atim = now
		stat.Birthtim = now
		stat.Blksize = FilesystemBlockSize
		stat.Blocks = blks
		stat.Ctim = now
		stat.Ino = d.Inode
		stat.Mode = 0600
		stat.Mtim = now
		stat.Nlink = 0 // TODO
		stat.Rdev = 0  // TODO
		stat.Flags = 0

		return 0
	}

	// Find file.

	d.fcl.RLock()
	f, ok := d.FileChildren[fh]
	d.fcl.RUnlock()
	if ok {
		// TODO: Add logic for locally present or remote edits or w/e.

		logger.Info("Real path of file", "path", f.RealPathOfFile)
		finfo, err := os.Stat(f.RealPathOfFile)
		if err != nil {
			logger.Error("Failed to us stat the file", "real-path", f.RealPathOfFile, "error", err)
			return int(convertOsErrToSyscallErrno("stat", err))
		}

		stat.Atim = winfuse.NewTimespec(finfo.ModTime())
		stat.Birthtim = winfuse.NewTimespec(finfo.ModTime())
		stat.Blksize = FilesystemBlockSize
		stat.Blocks = 1000 // TODO
		stat.Ctim = winfuse.NewTimespec(finfo.ModTime())
		stat.Ino = fh
		stat.Mode = 0700
		stat.Mtim = winfuse.NewTimespec(finfo.ModTime())
		stat.Nlink = 0
		stat.Rdev = 0 // TODO
		stat.Flags = 0
		stat.Size = finfo.Size()

		return 0
	}

	// Find directory.

	d.dcl.RLock()
	dir, ok := d.DirChildren[fh]
	d.dcl.RUnlock()
	if ok {
		logger.Info("Real path of dir", "path", dir.RealPathOfFile)
		finfo, err := os.Stat(dir.RealPathOfFile)
		if err != nil {
			logger.Error("Failed to us stat the file", "real-path", dir.RealPathOfFile, "error", err)
			return int(convertOsErrToSyscallErrno("stat", err))
		}

		stat.Atim = winfuse.NewTimespec(finfo.ModTime())
		stat.Birthtim = winfuse.NewTimespec(finfo.ModTime())
		stat.Blksize = FilesystemBlockSize
		stat.Blocks = 1000 // TODO
		stat.Ctim = winfuse.NewTimespec(finfo.ModTime())
		stat.Ino = fh
		stat.Mode = 0700
		stat.Mtim = winfuse.NewTimespec(finfo.ModTime())
		stat.Nlink = 0
		stat.Rdev = 0 // TODO
		stat.Flags = 0
		stat.Size = finfo.Size()

		return 0
	}

	dir, f = d.lookup(path, fh)
	if dir != nil {
		return dir.Getattr(path, stat, fh)
	}
	if f != nil {
		logger.Info("Real path of file", "path", f.RealPathOfFile)
		finfo, err := os.Stat(f.RealPathOfFile)
		if err != nil {
			logger.Error("Failed to us stat the file", "real-path", f.RealPathOfFile, "error", err)
			return int(convertOsErrToSyscallErrno("stat", err))
		}

		stat.Atim = winfuse.NewTimespec(finfo.ModTime())
		stat.Birthtim = winfuse.NewTimespec(finfo.ModTime())
		stat.Blksize = FilesystemBlockSize
		stat.Blocks = 1000 // TODO
		stat.Ctim = winfuse.NewTimespec(finfo.ModTime())
		stat.Ino = fh
		stat.Mode = 0700
		stat.Mtim = winfuse.NewTimespec(finfo.ModTime())
		stat.Nlink = 0
		stat.Rdev = 0 // TODO
		stat.Flags = 0
		stat.Size = finfo.Size()

		return 0
	}

	logger.Info("Very enoent, but this one is correct I guess")

	return winfuse.ENOENT
}

func (d *Dir) lookup(path string, fh uint64) (*Dir, *File) {
	if path == "/" {
		return d.Root, nil
	}
	splitPath := filepath.SplitList(path)
	return d.findNode(splitPath, fh)
}

func (d *Dir) findNode(pathSegments []string, fh uint64) (*Dir, *File) {
	useFh := fh != 0

	if len(pathSegments) == 0 {
		return nil, nil
	}

	if len(pathSegments) == 1 {
		if d.Name == pathSegments[0] {
			if useFh {
				if fh == d.Inode {
					return d, nil
				}
			} else {
				return d, nil
			}
		}
	}

	var ok bool
	var f *File
	var dir *Dir

	if !useFh {
		goto SkipFhUse
	}

	d.fcl.RLock()
	f, ok = d.FileChildren[fh]
	d.fcl.RUnlock()
	if ok {
		return nil, f
	}

	d.dcl.RLock()
	dir, ok = d.DirChildren[fh]
	d.dcl.RUnlock()
	if ok {
		return dir, nil
	}

SkipFhUse:
	// This recursion will hurt you all!
	// The monster of bloatware is real!
	d.dcl.RLock()
	// I could just defer here the dcl.RUnlock()
	// But my mind doesn't want to think about it now.
	// No need to keep the lock until the recursion finishes
	// With the risk that it might get deleted.
	// However the call stack is first Lookup then Delete.
	// Always.
	for _, dir := range d.DirChildren {
		if dir.Name == pathSegments[0] {
			d.dcl.RUnlock()
			return dir.findNode(pathSegments[1:], fh)
		}
	}
	d.dcl.RUnlock()

	return nil, nil
}

func (d *Dir) Init() {
	d.logger.Debug("Init", "inode", d.Inode)

}

func (d *Dir) Link(oldpath string, newpath string) int {
	d.logger.Debug("Link", "oldPath", oldpath, "newPath", newpath, "inode", d.Inode)

	return winfuse.ENOTSUP
}

func (d *Dir) Mkdir(path string, mode uint32) int {
	d.logger.Debug("Mkdir", "path", path, "inode", d.Inode)
	logger := d.logger.New("method", "mkdir", "path", path, "mode", mode)
	logger.Info("MKDIR CALL")

	splitPath := filepath.SplitList(path)
	if len(path) == 0 {
		logger.Warn("Invalid path")
		return winfuse.EIO
	}

	name := splitPath[len(splitPath)-1]

	if len(name) > 255 {
		logger.Warn("Name too long")
		return winfuse.E2BIG
	}

	splitPath = splitPath[:len(splitPath)-1]
	dir, _ := d.findNode(splitPath, 0)
	if dir == nil && len(splitPath) != 0 {
		logger.Warn("Failed to mkdir, intermediary path link is missing")
		return winfuse.EIO
	}

	dir.dcl.Lock()
	inode := dir.inodeGen.Generate()
	dir.DirChildren[inode] = &Dir{
		logger:              logger,
		inodeGen:            dir.inodeGen,
		fcl:                 sync.RWMutex{},
		dcl:                 sync.RWMutex{},
		Name:                name,
		RelativePath:        path,
		Inode:               inode,
		RealPathOfFile:      filepath.Join(dir.LocalDownloadFolder, name),
		PeerLastEdit:        0,
		IsLocalPresent:      true,
		LocalDownloadFolder: dir.LocalDownloadFolder,
		Parent:              dir,
		Root:                dir.Root,
		OpenFileHandlers:    make(map[uint64]*File),
		OpenMapLock:         sync.RWMutex{},
		FileChildren:        make(map[uint64]*File),
		DirChildren:         make(map[uint64]*Dir),
	}
	dir.dcl.Unlock()

	logger.Info("Success")

	return 0
}

func (d *Dir) Mknod(path string, mode uint32, dev uint64) int {
	d.logger.Debug("Mknod", "path", path, "inode", d.Inode)

	return winfuse.ENOTSUP
}

func (d *Dir) Open(path string, flags int) (int, uint64) {
	d.logger.Debug("Open", "path", path, "inode", d.Inode)

	return 0, 0
}

func (d *Dir) Opendir(path string) (int, uint64) {
	d.logger.Debug("Opendir", "path", path, "inode", d.Inode)

	return 0, 0
}

func (d *Dir) Readdir(path string, fill func(name string, stat *winfuse.Stat_t, offset int64) bool, offset int64, fh uint64) int {
	d.logger.Debug("Readdir", "path", path, "inode", d.Inode)

	logger := d.logger.New("method", "readdir", "path", path, "fh", fh)
	logger.Info("ok")

	dir, _ := d.lookup(path, fh)
	if dir == nil {
		logger.Info("Enoent")
		return winfuse.ENOENT
	}

	curAttr := &winfuse.Stat_t{}
	logger.Info("We get attr for .")
	errNo := dir.Getattr(dir.RelativePath, curAttr, dir.Inode)
	if errNo == 0 {
		logger.Info("for . it worked")
		fill(".", curAttr, 0)
	}

	if dir.Parent != nil {
		errNo := dir.Getattr(dir.Parent.RelativePath, curAttr, dir.Parent.Inode)
		if errNo == 0 {
			fill("..", curAttr, 0)
		}
	} else {
		fill("..", nil, 0)
	}

	logger.Info("Children dir")

	dir.dcl.RLock()
	for _, cdir := range dir.DirChildren {
		errNo = dir.Getattr(cdir.RelativePath, curAttr, cdir.Inode)
		if errNo == 0 {
			logger.Info("Dir errno 0")
			fill(cdir.Name, curAttr, int64(cdir.Inode)) // The cast makes me :'|  I promise I will fix it in the future.
		} else {
			logger.Warn("Dir eernno", "errno", errNo)
		}
	}
	dir.dcl.RUnlock()

	logger.Info("Children file")
	dir.fcl.RLock()
	for _, f := range dir.FileChildren {
		errNo = dir.Getattr(f.RelativePath, curAttr, f.Inode)
		if errNo == 0 {
			logger.Info("File errno 0")
			fill(f.Name, curAttr, int64(f.Inode)) // Another one, but realistically, who is gonna use 1<<31 Inodes to overflow.
		} else {
			logger.Warn("File eernno", "errno", errNo)
		}
	}
	dir.fcl.RUnlock()

	logger.Info("ok")

	return 0
}

func (d *Dir) Readlink(path string) (int, string) {
	d.logger.Debug("Readlink", "path", path, "inode", d.Inode)

	return winfuse.ENOTSUP, ""
}

func (d *Dir) Release(path string, fh uint64) int {
	d.logger.Debug("Release", "path", path, "inode", d.Inode, "fh", fh)

	return 0
}

func (d *Dir) Releasedir(path string, fh uint64) int {
	d.logger.Debug("Releasedir", "path", path, "inode", d.Inode, "fh", fh)

	return 0
}

// Mac OS High Level apps use Rename SWAP, which is really fun from my experience.
func (d *Dir) Rename(oldpath string, newpath string) int {
	d.logger.Debug("Rename", "oldpath", oldpath, "newpath", newpath, "inode", d.Inode)

	return 0
}

func (d *Dir) Rmdir(path string) int {
	d.logger.Debug("Rmdir", "path", path, "inode", d.Inode)

	return 0
}

func (d *Dir) Statfs(path string, stat *winfuse.Statfs_t) int {
	logger := d.logger.New("method", "statfs", "path", path)
	var freeBytesAvailable uint64
	var totalNumberOfBytes uint64
	var totalNumberOfFreeBytes uint64

	freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes, err := GetFreeDiskSpace(path)
	if err != nil {
		logger.Error("Failed to get disk free space", "error", err)
		return winfuse.EIO
	}

	// I've split Inode numbers between two peers.
	// a uint64 value into two uint32 values.
	stat.Files = 1 << 32
	stat.Ffree = 1 << 32
	stat.Favail = 1 << 32

	stat.Bavail = freeBytesAvailable / FilesystemBlockSize
	stat.Bfree = totalNumberOfFreeBytes / FilesystemBlockSize
	stat.Blocks = totalNumberOfBytes / FilesystemBlockSize
	stat.Bsize = FilesystemBlockSize
	stat.Frsize = FilesystemBlockSize

	// Note (from my memory) for this value:
	// On windows I think the full path is capped at 255 :< (might be 1024?)
	// But for Mac/Linux it is per path segment at 255.

	stat.Namemax = uint64(255) // We allow only 256 characters in the name.

	logger.Info("Statfs", "stat", stat, "inode", d.Inode)

	return 0
}

func (d *Dir) Symlink(target string, newpath string) int {
	d.logger.Debug("Symlink", "target", target, "newpath", newpath, "inode", d.Inode)

	return 0
}

// Note: On windows open does not have a truncate flag,
// thus Open is immediately followed by Truncate.
func (d *Dir) Truncate(path string, size int64, fh uint64) int {
	d.logger.Debug("Truncate", "path", path, "size", size, "inode", d.Inode, "fh", fh)
	return 0
}

// Unlink removes a file.
func (d *Dir) Unlink(path string) int {
	d.logger.Debug("Unlink", "path", path, "inode", d.Inode)

	return 0
}

// Utimens changes the access and modification times of a file.
// Note: I do not care about it :^D for this version.
func (d *Dir) Utimens(path string, tmsp []winfuse.Timespec) int {
	d.logger.Debug("Utimens", "path", path, "inode", d.Inode)

	return winfuse.ENOTSUP
}

// The method returns the number of bytes written.
func (d *Dir) Write(path string, buff []byte, offset int64, fh uint64) int {
	d.logger.Debug("Write", "path", path, "inode", d.Inode, "fh", fh)

	return 0
}

// The method returns the number of bytes read.
func (d *Dir) Read(path string, buff []byte, offset int64, fh uint64) int {
	d.logger.Debug("Read", "path", path, "inode", d.Inode, "fh", fh)

	return 0
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
	logger := d.logger.New("method", "remove-xattr")
	err := xattr.Remove(path, name)
	if err != nil {
		logger.Error("Failed to remove xattr", "path", path, "name", name, "error", err)
		return int(convertOsErrToSyscallErrno("remove-xattr", err))
	}

	return 0
}

func (d *Dir) Listxattr(path string, fill func(name string) bool) int {
	logger := d.logger.New("method", "list-xattr")
	res, err := xattr.List(path)
	if err != nil {
		logger.Error("Failed to list xattr", "path", path, "error", err)
		return int(convertOsErrToSyscallErrno("list-xattr", err))
	}

	for _, s := range res {
		fill(s)
	}

	return 0
}

func (d *Dir) Getxattr(path string, name string) (int, []byte) {
	logger := d.logger.New("method", "get-xattr")
	// Note for the reader:
	// If the reader has a need for xattr, use the filesystem path instead of the
	// method signature path.
	// d.RealPathOfFile is the real path of d on the system.
	// but the catch is that the file/dir name in the method input path:
	// is the last segment, this implies that you need to
	// xattr.Get(d.RealPathOfFile+"/"+ name)

	res, err := xattr.Get(path, name)
	if err != nil {
		logger.Error("Failed to get xattr", "path", path, "name", name, "error", err)
		return int(convertOsErrToSyscallErrno("get-xattr", err)), nil
	}

	return 0, res
}

func (d *Dir) Setxattr(path string, name string, value []byte, flags int) int {
	logger := d.logger.New("methid", "set-xattr")
	// I do not support flags for this version.
	_ = flags

	err := xattr.Set(path, name, value)
	if err != nil {
		logger.Error("Failed to set xattr", "path", path, "name", name, "error", err)
		return int(convertOsErrToSyscallErrno("set-xattr", err))
	}

	return 0
}
