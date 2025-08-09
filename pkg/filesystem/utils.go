package filesystem

import (
	"errors"
	"os"
	"syscall"
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

	return targetErr
}
