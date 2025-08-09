package filesystem

import (
	"path/filepath"

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
