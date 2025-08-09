package filesystem

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// On windows `copy`` works with 1 MiB file block size.
// Setting this value for the StatFS ensures optimal speed.

// Note about the file block size:
// I do not remember where I found the block size information, but
// from my empirical tests on a different project, I found
// that if you use POSIX `dd` on windows, it will be 10 times slower.
// But using the Command Prompt copy, with the same block size as on linux,
// it will yield speeds of 4 times slower.
// I think I settled for 1 MiB or 13 MiB, that had the same speed as on linux (same machine).

// Note on the above note:
// The maintainer of WinFSP project (Bill), suggested to trace the `dd` vs `copy` problem
// to the system call, but I never had the time, nor interest to pursue deeper.
// For those interested, more details can be found in this thread:
// https://github.com/winfsp/cgofuse/issues/86#issuecomment-2098295044

const FilesystemBlockSize = 2 << 18

func GetFreeDiskSpace(path string) (freeBytesAvail, totalNumberOfBytes, totalNumberFreeBytes uint64, err error) {
	err = windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(filepath.Clean(path)),
		&freeBytesAvail, &totalNumberOfBytes, &totalNumberFreeBytes)

	return
}
