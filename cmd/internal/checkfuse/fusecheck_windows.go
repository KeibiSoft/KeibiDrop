//go:build windows
// +build windows

package checkfuse

import "log/slog"

func isFUSEPresent() bool {
	path := `C:\Windows\System32\winfsp-x64.dll`
	e := exists(path)
	slog.Warn("FUSE windows check", "path", path, "exists", e)
	return e
}
