// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

//go:build (darwin || linux) && !android && cgo

package filesystem

/*
#include <dlfcn.h>
#include <stdlib.h>

// libfuse 2 and 3 both start struct fuse_context with the struct fuse pointer.
struct kd_fuse_context { void *fuse; };
typedef struct kd_fuse_context *(*kd_get_context_fn)(void);
typedef int (*kd_invalidate_path_fn)(void *, const char *);

// kd_libfuse returns the libfuse that cgofuse loaded. It loads no other.
static void *kd_libfuse(void) {
#if defined(__APPLE__)
	const char *names[] = {"/usr/local/lib/libfuse.2.dylib", "/usr/local/lib/libosxfuse.2.dylib",
		"/usr/local/lib/libfuse-t.dylib", 0};
#else
	const char *names[] = {"libfuse.so.2", "libfuse3.so.3", 0};
#endif
	for (int i = 0; names[i]; i++) {
		void *h = dlopen(names[i], RTLD_NOW | RTLD_NOLOAD);
		if (h) return h;
	}
	return 0;
}

// The mount's struct fuse and fuse_invalidate_path, returned by value: Go
// passes no pointer of its own to C.
typedef struct { void *self; void *inval; } kd_handles;

// kd_kernel_cache_handles gives the mount's handles. Call it on a FUSE
// thread: fuse_get_context is valid only there. macFUSE and libfuse 3 have
// fuse_invalidate_path; libfuse 2 on Linux has none.
static kd_handles kd_kernel_cache_handles(void) {
	kd_handles out = {0, 0};
	void *h = kd_libfuse();
	if (!h) return out;
	kd_get_context_fn get = (kd_get_context_fn)dlsym(h, "fuse_get_context");
	struct kd_fuse_context *c = get ? get() : 0;
	out.self = c ? c->fuse : 0;
	out.inval = dlsym(h, "fuse_invalidate_path");
	dlclose(h); // cgofuse holds its own reference to the library.
	return out;
}

static int kd_invalidate_path(void *inval, void *self, const char *path) {
	return ((kd_invalidate_path_fn)inval)(self, path);
}
*/
import "C"

import (
	"sync"
	"unsafe"
)

// kernelCache drops the kernel's cached pages of a path a friend changed.
// macFUSE keeps the pages of a file whose size did not change, and ignores
// KeepCache=false, so without this a same-length change (git's 41-byte refs)
// is never seen. Linux drops the pages at each open (KeepCache=false). One
// mount per process (cgofuse), so one set of handles.
var kernelCache struct {
	mu    sync.RWMutex
	self  unsafe.Pointer // the mount's struct fuse; nil when not mounted
	inval unsafe.Pointer // fuse_invalidate_path; nil when libfuse has none
}

// captureKernelCache keeps the handles for invalidateKernelCache. Init calls
// it on a FUSE thread.
func (d *Dir) captureKernelCache() {
	h := C.kd_kernel_cache_handles()
	self, inval := unsafe.Pointer(h.self), unsafe.Pointer(h.inval)
	kernelCache.mu.Lock()
	kernelCache.self, kernelCache.inval = self, inval
	kernelCache.mu.Unlock()
	d.logger.Info("Kernel cache invalidation", "available", self != nil && inval != nil)
}

// releaseKernelCache forgets the handles before the host frees the mount. It
// waits for an invalidation in progress.
func (d *Dir) releaseKernelCache() {
	kernelCache.mu.Lock()
	kernelCache.self, kernelCache.inval = nil, nil
	kernelCache.mu.Unlock()
}

// invalidateKernelCache makes the next read of path reach Read. It runs on its
// own goroutine with no lock of the caller held: the kernel can wait for a
// read of the same file in progress.
func (d *Dir) invalidateKernelCache(path string) {
	go func() {
		kernelCache.mu.RLock()
		defer kernelCache.mu.RUnlock()
		if kernelCache.self == nil || kernelCache.inval == nil {
			return
		}
		cpath := C.CString(path)
		defer C.free(unsafe.Pointer(cpath))
		// -ENOENT: the kernel never looked the path up, so it holds no pages.
		rc := C.kd_invalidate_path(kernelCache.inval, kernelCache.self, cpath)
		if fuseOpLog {
			d.logger.Debug("oplog kernel invalidate", "path", path, "rc", int(rc))
		}
	}()
}
