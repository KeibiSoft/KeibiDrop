package filesystem

import (
	"errors"
	"os"
	"strings"
	"syscall"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

func convertOsErrToSyscallErrno(name string, err error) syscall.Errno {
	if err == nil {
		return 0
	}

	e := os.NewSyscallError(name, err)
	var targetErr syscall.Errno

	ok := errors.As(e, &targetErr)
	if !ok {
		return syscall.EIO
	}

	// cgoFuse uses -errno
	return -targetErr
}

func isModificationTimeNewer(a, b *winfuse.Stat_t) bool {
	return a.Mtim.Time().After(b.Mtim.Time())
}

func getNameFromPath(path string) string {
	aux := strings.Split(path, "/")
	if len(aux) == 0 {
		return path
	}

	return aux[len(aux)-1]
}

func getPathWithoutName(path string) string {
	aux := strings.Split(path, "/")
	if len(aux) == 0 {
		return path
	}

	return strings.Join(aux[:len(aux)-1], "/")
}
