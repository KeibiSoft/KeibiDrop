//go:build darwin
// +build darwin

package checkfuse

import "log/slog"

func isFUSEPresent() bool {
	path1 := `/usr/local/lib/libfuse.dylib`
	path2 := `/Library/Filesystems/macfuse.fs`
	exists1, exists2 := exists(path1), exists(path2)
	slog.Warn("FUSE darwin check", "path1", path1, "exists1", exists1, "path2", path2, "exists2", exists2)
	return exists1 || exists2
}
