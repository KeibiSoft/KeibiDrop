//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Background sibling warmer. A cold read of a small file batches its
// ABOUTME: announced small siblings into one ReadBatch request and caches them.

package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/zeebo/xxh3"
)

// warmBatchTimeout bounds one warm batch. Waiters on claimed files block until
// their claim settles, so a hung batch must time out, never hang FUSE reads.
const warmBatchTimeout = 60 * time.Second

// warmFailCooldown delays the next warm attempt for a directory after a
// failed batch, so a broken link does not retrigger on every read.
const warmFailCooldown = 5 * time.Second

// warmClaim is one file claimed for a warm batch.
type warmClaim struct {
	path     string
	f        *File
	bf       *blockFetch
	bm       *ChunkBitmap // Bitmap snapshot at claim time. A swap mid-warm voids the claim.
	size     int64
	lf       *os.File
	openPath string
	done     bool
}

// finish settles the claim. err nil marks success; the claim's writer handle,
// if open, is always balanced with endDiskWriter.
func (c *warmClaim) finish(d *Dir, err error) {
	if c.done {
		return
	}
	c.done = true
	if c.lf != nil {
		_ = c.lf.Close()
		finalPath := c.openPath
		if err == nil && c.bm.IsComplete() {
			// The file may have been renamed while landing (git .lock dance).
			d.RemoteFilesLock.RLock()
			currentDiskPath := c.f.RealPathOfFile
			d.RemoteFilesLock.RUnlock()
			if currentDiskPath != c.openPath {
				if mkErr := os.MkdirAll(filepath.Dir(currentDiskPath), 0o755); mkErr == nil {
					if rnErr := os.Rename(c.openPath, currentDiskPath); rnErr == nil {
						finalPath = currentDiskPath
					}
				}
			}
		}
		c.f.endDiskWriter(d, finalPath)
	}
	c.f.finishBlockFetch(0, c.bf, err)
}

// maybeWarmSiblings starts a warm batch for the directory of a cold small-file
// read, at most one per directory at a time. Cheap on the read path: two flag
// loads and one map lookup.
func (d *Dir) maybeWarmSiblings(path string) {
	r := d.Root
	if r == nil {
		r = d
	}
	if r.warmDisabled || r.batchUnsupported.Load() {
		return
	}
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return
	}
	dirPath := path[:idx]
	if dirPath == "" {
		dirPath = "/"
	}
	r.warmMu.Lock()
	if r.warmDirs == nil {
		r.warmDirs = make(map[string]time.Time)
	}
	next, exists := r.warmDirs[dirPath]
	if exists && (next.IsZero() || time.Now().Before(next)) {
		r.warmMu.Unlock()
		return
	}
	r.warmDirs[dirPath] = time.Time{} // In flight.
	r.warmMu.Unlock()
	go d.warmSiblings(dirPath, path)
}

// collectWarmCandidates lists the announced, uncached, unedited small files of
// dirPath, in name order rotated to start after the trigger file, bounded by
// the batch budget and the server's path cap.
func (d *Dir) collectWarmCandidates(dirPath, triggerPath string) []*warmClaim {
	r := d.Root
	if r == nil {
		r = d
	}
	prefix := dirPath
	if prefix != "/" {
		prefix += "/"
	}

	type cand struct {
		path string
		f    *File
	}
	var cands []cand
	r.RemoteFilesLock.RLock()
	for k, f := range r.RemoteFiles {
		if k == triggerPath || !strings.HasPrefix(k, prefix) {
			continue
		}
		if strings.Contains(k[len(prefix):], "/") {
			continue // Not a direct child.
		}
		cands = append(cands, cand{path: k, f: f})
	}
	r.RemoteFilesLock.RUnlock()

	sort.Slice(cands, func(i, j int) bool { return cands[i].path < cands[j].path })
	// Rotate so the extraction frontier (the names after the trigger) warms first.
	start := sort.Search(len(cands), func(i int) bool { return cands[i].path > triggerPath })
	rotated := make([]cand, 0, len(cands))
	rotated = append(rotated, cands[start:]...)
	rotated = append(rotated, cands[:start]...)

	var claims []*warmClaim
	var budget int64
	for _, c := range rotated {
		if len(claims) >= WarmBatchMaxFiles || budget >= WarmBatchBudget {
			break
		}
		c.f.metaMu.RLock()
		bm := c.f.Bitmap
		notSynced := c.f.NotLocalSynced
		edited := c.f.LocalNewer || c.f.HadEdits
		gone := c.f.PeerStoppedSharing
		c.f.metaMu.RUnlock()
		if bm == nil || !notSynced || edited || gone {
			continue
		}
		size := bm.FileSize()
		if size <= 0 || size > SmallFileWarmThreshold || bm.IsComplete() {
			continue
		}
		bf, leader := c.f.beginBlockFetch(0)
		if !leader {
			continue // A reader or another warm owns it.
		}
		c.f.Download.Reset(uint64(size))
		claims = append(claims, &warmClaim{path: c.path, f: c.f, bf: bf, bm: bm, size: size})
		budget += size
	}
	return claims
}

// warmSiblings runs one warm batch for dirPath. Every claim settles on every
// exit path: an unsettled claim would hang the FUSE readers waiting on it.
func (d *Dir) warmSiblings(dirPath, triggerPath string) {
	r := d.Root
	if r == nil {
		r = d
	}
	ok := false
	defer func() {
		r.warmMu.Lock()
		if ok {
			delete(r.warmDirs, dirPath)
		} else {
			r.warmDirs[dirPath] = time.Now().Add(warmFailCooldown)
		}
		r.warmMu.Unlock()
	}()

	ctx := d.Ctx()
	// One prefetch-semaphore slot: warm batches and prefetches share the same
	// concurrency bound on the wire.
	select {
	case r.PrefetchSem <- struct{}{}:
		defer func() { <-r.PrefetchSem }()
	case <-ctx.Done():
		return
	}

	claims := d.collectWarmCandidates(dirPath, triggerPath)
	if len(claims) == 0 {
		ok = true // Nothing to warm. The next cold miss rescans.
		return
	}
	// Claims are settled on every exit: a batch failure, a timeout, or a
	// panic must release waiting readers to their own fetch.
	defer func() {
		for _, c := range claims {
			c.finish(d, errBlockFetchIncomplete)
		}
	}()

	provider := d.OpenStreamProvider()
	br, hasBatch := provider.(types.BatchReader)
	if provider == nil || !hasBatch {
		// A provider without ReadBatch (test fake, teardown) behaves like an
		// old peer: stop warming for this session.
		r.batchUnsupported.Store(true)
		return
	}

	paths := make([]string, len(claims))
	claimByPath := make(map[string]*warmClaim, len(claims))
	for i, c := range claims {
		paths[i] = c.path
		claimByPath[c.path] = c
	}

	bctx, cancel := context.WithTimeout(ctx, warmBatchTimeout)
	defer cancel()
	recv, err := br.ReadBatch(bctx, paths, WarmBatchBudget)
	if err != nil {
		if errors.Is(err, types.ErrBatchUnsupported) {
			r.batchUnsupported.Store(true)
		}
		return
	}
	r.warmBatches.Add(1)

	for {
		frame, recvErr := recv.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, types.ErrBatchUnsupported) {
				r.batchUnsupported.Store(true)
			}
			// io.EOF is the normal end. Claims the budget cut off settle via
			// the deferred backstop and self-fetch on their next read.
			ok = errors.Is(recvErr, io.EOF)
			return
		}
		c := claimByPath[frame.Path]
		if c == nil || c.done {
			continue
		}
		if frame.ErrCode != 0 {
			c.finish(d, errBlockFetchIncomplete)
			continue
		}
		// The warm threshold is below the server frame cap, so a warmed file
		// arrives as exactly one frame. Anything else means the origin's view
		// of the file drifted: skip it, the per-file path recovers.
		if frame.Offset != 0 || !frame.FileDone ||
			int64(frame.TotalSize) != c.size || int64(len(frame.Data)) != c.size {
			c.finish(d, errBlockFetchIncomplete)
			continue
		}
		d.landWarmFile(c, frame.Data)
	}
}

// landWarmFile writes one warmed file to its cache path with the same
// discipline as prefetch: pre-allocate, write before bitmap marks, mark and
// hash only fully covered chunks, register with the preserve_metadata
// arbiter, and re-verify the bitmap pointer around every step so an announce
// that swapped the bitmap mid-warm voids the claim instead of landing stale
// bytes as current.
func (d *Dir) landWarmFile(c *warmClaim, data []byte) {
	r := d.Root
	if r == nil {
		r = d
	}
	c.f.metaMu.RLock()
	cur := c.f.Bitmap
	fileMode := os.FileMode(0o644)
	if c.f.stat != nil {
		if m := os.FileMode(c.f.stat.Mode & 0o777); m != 0 {
			fileMode = m
		}
	}
	c.f.metaMu.RUnlock()
	if cur != c.bm {
		c.finish(d, errBlockFetchIncomplete)
		return
	}

	d.RemoteFilesLock.RLock()
	realPath := c.f.RealPathOfFile
	d.RemoteFilesLock.RUnlock()
	if realPath == "" {
		c.finish(d, errBlockFetchIncomplete)
		return
	}
	if mkErr := os.MkdirAll(filepath.Dir(realPath), 0o755); mkErr != nil {
		c.finish(d, errBlockFetchIncomplete)
		return
	}
	lf, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, fileMode) // #nosec G304
	if err != nil {
		c.finish(d, errBlockFetchIncomplete)
		return
	}
	c.lf = lf
	c.openPath = realPath
	c.f.beginDiskWriter()
	if info, statErr := lf.Stat(); statErr == nil && info.Size() < c.size {
		_ = lf.Truncate(c.size)
	}

	if _, wErr := lf.WriteAt(data, 0); wErr != nil {
		c.finish(d, errBlockFetchIncomplete)
		return
	}

	// Write landed before any mark (sparse-hole rule). Re-verify the bitmap
	// is still the one we claimed before marking chunks present.
	c.f.metaMu.RLock()
	cur = c.f.Bitmap
	c.f.metaMu.RUnlock()
	if cur != c.bm {
		c.finish(d, errBlockFetchIncomplete)
		return
	}
	cs := int64(c.bm.ChunkSizeBytes())
	for chunk := 0; int64(chunk)*cs < c.size; chunk++ {
		begin := int64(chunk) * cs
		end := min(begin+cs, c.size)
		c.bm.Set(chunk)
		c.bm.SetHash(chunk, xxh3.Hash(data[begin:end]))
	}
	c.f.Download.UpdateProgress(0, len(data))

	c.f.metaMu.Lock()
	if c.f.Bitmap == c.bm && c.bm.IsComplete() {
		c.f.NotLocalSynced = false
	}
	c.f.metaMu.Unlock()

	r.warmFiles.Add(1)
	c.finish(d, nil)
}
