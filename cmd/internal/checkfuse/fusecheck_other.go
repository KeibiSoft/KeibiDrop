//go:build !linux && !darwin && !windows
// +build !linux,!darwin,!windows

package checkfuse

import "log/slog"
import "runtime"

func isFUSEPresent() bool {
	slog.Warn("FUSE unsupported OS", "os", runtime.GOOS)
	return false
}
