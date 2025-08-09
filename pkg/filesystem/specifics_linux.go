package filesystem

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// On linux cp (1) works with 126 KiB file block size.
// Setting this value for the StatFS ensures optimal speed.

const FilesystemBlockSize = 2 << 16

func GetFreeDiskSpace(path string) (freeBytesAvail, totalNumberOfBytes, totalNumberFreeBytes uint64, err error) {
	stat := unix.Statfs_t{}

	err = unix.Statfs(filepath.Clean(path), &stat)

	freeBytesAvail = stat.Bavail * uint64(stat.Bsize)
	totalNumberFreeBytes = stat.Blocks * uint64(stat.Bsize)
	totalNumberFreeBytes = stat.Bfree * uint64(stat.Bsize)

	return
}
