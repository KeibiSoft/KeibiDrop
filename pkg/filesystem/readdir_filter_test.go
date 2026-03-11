// ABOUTME: Tests for remoteChildrenForDir — filters remote files to direct children of a directory.
// ABOUTME: Prevents phantom entries at wrong directory level (issue #63).

package filesystem

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRemoteChildrenForDir_RootWithFlatFiles(t *testing.T) {
	remoteFiles := map[string]*File{
		"/hello.txt":   {},
		"/world.txt":   {},
		"/readme.md":   {},
	}

	files, dirs := remoteChildrenForDir(remoteFiles, "/")

	assert.Equal(t, map[string]struct{}{
		"hello.txt": {},
		"world.txt": {},
		"readme.md": {},
	}, files)
	assert.Empty(t, dirs)
}

func TestRemoteChildrenForDir_RootWithNestedFiles(t *testing.T) {
	remoteFiles := map[string]*File{
		"/top.txt":                          {},
		"/test_repo/Documentation/git.adoc": {},
		"/test_repo/README.md":              {},
		"/photos/vacation/beach.jpg":        {},
	}

	files, dirs := remoteChildrenForDir(remoteFiles, "/")

	assert.Equal(t, map[string]struct{}{
		"top.txt": {},
	}, files, "only direct file children at root")

	assert.Equal(t, map[string]struct{}{
		"test_repo": {},
		"photos":    {},
	}, dirs, "nested paths emit directory names, not file basenames")
}

func TestRemoteChildrenForDir_SubdirectoryListing(t *testing.T) {
	remoteFiles := map[string]*File{
		"/a/file1.txt":   {},
		"/a/file2.txt":   {},
		"/a/b/deep.txt":  {},
		"/other/file.go": {},
	}

	files, dirs := remoteChildrenForDir(remoteFiles, "/a")

	assert.Equal(t, map[string]struct{}{
		"file1.txt": {},
		"file2.txt": {},
	}, files)
	assert.Equal(t, map[string]struct{}{
		"b": {},
	}, dirs)
}

func TestRemoteChildrenForDir_DeepNestingDeduplication(t *testing.T) {
	remoteFiles := map[string]*File{
		"/a/b/c.txt": {},
		"/a/b/d.txt": {},
		"/a/b/e/f.txt": {},
	}

	files, dirs := remoteChildrenForDir(remoteFiles, "/a")

	assert.Empty(t, files, "no direct file children under /a")
	assert.Equal(t, map[string]struct{}{
		"b": {},
	}, dirs, "multiple files under /a/b/ produce single 'b' dir entry")
}

func TestRemoteChildrenForDir_NonExistentDirectory(t *testing.T) {
	remoteFiles := map[string]*File{
		"/a/file.txt": {},
		"/b/file.txt": {},
	}

	files, dirs := remoteChildrenForDir(remoteFiles, "/nonexistent")

	assert.Empty(t, files)
	assert.Empty(t, dirs)
}

func TestRemoteChildrenForDir_EmptyPathNormalization(t *testing.T) {
	remoteFiles := map[string]*File{
		"/hello.txt": {},
		"/sub/deep.txt": {},
	}

	// Empty string should behave like root
	files, dirs := remoteChildrenForDir(remoteFiles, "")

	assert.Equal(t, map[string]struct{}{
		"hello.txt": {},
	}, files)
	assert.Equal(t, map[string]struct{}{
		"sub": {},
	}, dirs)
}
