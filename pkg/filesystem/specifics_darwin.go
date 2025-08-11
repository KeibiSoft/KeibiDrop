package filesystem

import (
	"path/filepath"
	"syscall"

	"github.com/winfsp/cgofuse/fuse"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

// My note is the following:
// You can increase it to 10MiB or 16MiB, and depending on the processor,
// it will be faster, but it might cap and downgrade at some point.
// I cannot provide a realistic best value as I have used different hardware specs
// for testing and finding this values, and at the end of the day it will be missleading
// to say: on intel i7 from 2018 Thinkpad 480T (windows + linux), I had 500 MB/s copy speed.
// But on Mac M3 had 1.2 GB/s sometimes up to 2GB/s
// And on Mac Intel I did not benchmark yet.

const FilesystemBlockSize = 2 << 18

func GetFreeDiskSpace(path string) (freeBytesAvail, totalNumberOfBytes, totalNumberFreeBytes uint64, err error) {
	stat := unix.Statfs_t{}

	err = unix.Statfs(filepath.Clean(path), &stat)

	freeBytesAvail = stat.Bavail * uint64(stat.Bsize)
	totalNumberOfBytes = stat.Blocks * uint64(stat.Bsize)
	totalNumberFreeBytes = stat.Bfree * uint64(stat.Bsize)

	return
}

func setuidgid() func() {
	euid := syscall.Geteuid()
	if 0 == euid {
		uid, gid, _ := fuse.Getcontext()
		egid := syscall.Getegid()
		syscall.Setegid(int(gid))
		syscall.Seteuid(int(uid))
		return func() {
			syscall.Seteuid(euid)
			syscall.Setegid(egid)
		}
	}
	return func() {
	}
}

func copyFusestatfsFromGostatfs(dst *fuse.Statfs_t, src *syscall.Statfs_t) {
	*dst = fuse.Statfs_t{}
	dst.Bsize = uint64(src.Bsize)
	dst.Frsize = 1
	dst.Blocks = uint64(src.Blocks)
	dst.Bfree = uint64(src.Bfree)
	dst.Bavail = uint64(src.Bavail)
	dst.Files = uint64(src.Files)
	dst.Ffree = uint64(src.Ffree)
	dst.Favail = uint64(src.Ffree)
	dst.Namemax = 255 //uint64(src.Namelen)
}

func copyFusestatFromGostat(dst *winfuse.Stat_t, src *syscall.Stat_t) {
	*dst = winfuse.Stat_t{}
	dst.Dev = uint64(src.Dev)
	dst.Ino = uint64(src.Ino)
	dst.Mode = uint32(src.Mode)
	dst.Nlink = uint32(src.Nlink)
	dst.Uid = uint32(src.Uid)
	dst.Gid = uint32(src.Gid)
	dst.Rdev = uint64(src.Rdev)
	dst.Size = int64(src.Size)
	dst.Atim.Sec, dst.Atim.Nsec = src.Atimespec.Sec, src.Atimespec.Nsec
	dst.Mtim.Sec, dst.Mtim.Nsec = src.Mtimespec.Sec, src.Mtimespec.Nsec
	dst.Ctim.Sec, dst.Ctim.Nsec = src.Ctimespec.Sec, src.Ctimespec.Nsec
	dst.Blksize = int64(src.Blksize)
	dst.Blocks = int64(src.Blocks)
	dst.Birthtim.Sec, dst.Birthtim.Nsec = src.Birthtimespec.Sec, src.Birthtimespec.Nsec
}

func syscall_Statfs(path string, stat *syscall.Statfs_t) error {
	return syscall.Statfs(path, stat)
}
