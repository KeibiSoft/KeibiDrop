//go:build linux
// +build linux

package checkfuse

import "log/slog"

func isFUSEPresent() bool {
	path1 := `/lib/x86_64-linux-gnu/libfuse.so.2`
	path2 := `/usr/lib/libfuse.so`
	path3 := `/usr/lib/x86_64-linux-gnu/libfuse3.so`
	exists1, exists2, exists3 := exists(path1), exists(path2), exists(path3)
	slog.Warn("FUSE linux check", "path1", path1, "exists1", exists1, "path2", path2, "exists2", exists2, "path3", path3, "exists3", exists3)
	return exists1 || exists2 || exists3
}
