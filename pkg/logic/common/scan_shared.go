// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/winfsp/cgofuse/fuse"
)

// persistFlushBatch bounds how many scanned files wait in memory before a
// shared-store write. Each flush rewrites the whole store, so the batch
// trades store rewrites against buffer growth.
const persistFlushBatch = 512

// announceBatchSize caps notifies per BatchNotify RPC. One RPC per file makes
// a big scan RTT-bound on WAN; one giant batch stalls the receiver. A capped
// batch bounds both, and bounds scan memory.
const announceBatchSize = 256

// announceRetries and announceRetryDelay bound the in-session retry of one
// announce batch. A conn replacement mid-scan heals within seconds; past
// that the scan stops and the reconnect re-scan finishes the job.
const (
	announceRetries    = 3
	announceRetryDelay = 500 * time.Millisecond
)

// ScanAndShareSaveDir walks the save folder breadth-first and announces every
// regular file that is not yet tracked, over the same announce path AddFile
// uses: upsert LocalFiles, send ADD_FILE (in capped batches), persist to the
// shared store. Breadth-first, so the peer sees the top of the tree before
// deep subtrees. Nested files keep their save-root-relative path. Idempotent:
// tracked files are skipped, so restore, watcher events, and repeated scans
// compose. Returns the number of files announced. A send error retries
// briefly; on final failure the walk stops and the failed batch is untracked
// again, so the reconnect re-scan or the next session announces the rest.
func (kd *KeibiDrop) ScanAndShareSaveDir(ctx context.Context) (int, error) {
	logger := kd.logger.With("method", "scan-shared-on-start")
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	if sess == nil || kd.SyncTracker == nil {
		return 0, ErrInvalidSession
	}

	root := filepath.Clean(kd.ToSave)
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, syscall.ENOTDIR
	}

	var announced int
	var batchSeq uint64
	batchFiles := make([]*synctracker.File, 0, announceBatchSize)
	batchReqs := make([]*bindings.NotifyRequest, 0, announceBatchSize)
	pendingPersist := make([]*synctracker.File, 0, persistFlushBatch)

	flushPersist := func() {
		kd.persistSharedFilesBatch(pendingPersist)
		pendingPersist = pendingPersist[:0]
	}
	// flushAnnounce sends the accumulated batch. Files count as announced only
	// after their batch is accepted; a per-item shortfall on the peer is logged
	// and self-heals on the next session (the files stay tracked locally).
	// A send error retries briefly: one transient failure must not silence
	// the rest of the scan for the whole session. On final failure the
	// batch's files are untracked again; they were upserted before the send
	// and a tracked file is skipped by every later scan.
	flushAnnounce := func() error {
		if len(batchReqs) == 0 {
			return nil
		}
		batchSeq++
		req := &bindings.BatchNotifyRequest{
			Notifications: batchReqs,
			Seq:           batchSeq,
			Timestamp:     uint64(time.Now().UnixNano()),
		}
		var resp *bindings.BatchNotifyResponse
		var err error
		for attempt := 1; ; attempt++ {
			resp, err = kd.sendBatchNotify(ctx, req)
			if err == nil || attempt >= announceRetries || ctx.Err() != nil {
				break
			}
			logger.Warn("Announce batch send failed, retrying",
				"attempt", attempt, "of", announceRetries, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(attempt) * announceRetryDelay):
			}
		}
		if err != nil {
			kd.SyncTracker.LocalFilesMu.Lock()
			for _, f := range batchFiles {
				if cur, ok := kd.SyncTracker.LocalFiles[f.RelativePath]; ok && cur == f {
					delete(kd.SyncTracker.LocalFiles, f.RelativePath)
				}
			}
			kd.SyncTracker.LocalFilesMu.Unlock()
			return err
		}
		if resp != nil && int(resp.Processed) < len(batchReqs) {
			logger.Warn("Peer processed only part of an announce batch",
				"sent", len(batchReqs), "processed", resp.Processed)
		}
		announced += len(batchReqs)
		pendingPersist = append(pendingPersist, batchFiles...)
		batchFiles = batchFiles[:0]
		batchReqs = batchReqs[:0]
		if len(pendingPersist) >= persistFlushBatch {
			flushPersist()
		}
		return nil
	}

	queue := []string{root}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir)
		if err != nil {
			logger.Warn("Scan skipping unreadable directory", "path", dir, "error", err)
			continue
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				flushPersist()
				return announced, ctx.Err()
			}
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				queue = append(queue, path)
				continue
			}
			if !entry.Type().IsRegular() {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if service.IsInternalPath(rel) {
				continue
			}

			// Files the peer announced to us are theirs; do not announce them back.
			kd.SyncTracker.RemoteFilesMu.RLock()
			_, isRemote := kd.SyncTracker.RemoteFiles[rel]
			kd.SyncTracker.RemoteFilesMu.RUnlock()
			if isRemote {
				continue
			}

			finfo, err := entry.Info()
			if err != nil {
				continue
			}

			kd.SyncTracker.LocalFilesMu.Lock()
			if _, tracked := kd.SyncTracker.LocalFiles[rel]; tracked {
				kd.SyncTracker.LocalFilesMu.Unlock()
				continue
			}
			atime, btime := statTimes(finfo)
			file := &synctracker.File{
				Name:           finfo.Name(),
				RelativePath:   rel,
				RealPathOfFile: filepath.Clean(path),
				Size:           uint64(finfo.Size()),
				LastEditTime:   uint64(finfo.ModTime().UnixNano()),
				CreatedTime:    btime,
				Atime:          atime,
				Mode:           uint32(finfo.Mode().Perm()) | syscall.S_IFREG,
			}
			kd.SyncTracker.LocalFiles[rel] = file // upsert first: a failed announce retries next scan
			kd.SyncTracker.LocalFilesMu.Unlock()

			batchFiles = append(batchFiles, file)
			// Base -1 is the fresh-create class: no prior version was held.
			// A scan announce only covers untracked files, and on a receiver
			// that holds dirty local bytes for the same name -1 proves the
			// concurrency, so the local version is preserved as a sibling.
			// Base 0 (the old value) means unknown and disables preservation.
			batchReqs = append(batchReqs, &bindings.NotifyRequest{
				Type:        bindings.NotifyType(types.AddFile),
				Path:        rel,
				BaseMtimeNs: -1,
				Attr: &bindings.Attr{
					Mode:             file.Mode,
					Size:             finfo.Size(),
					AccessTime:       atime,
					ModificationTime: file.LastEditTime,
					ChangeTime:       file.LastEditTime,
					BirthTime:        btime,
					Flags:            0o444,
				},
			})
			if len(batchReqs) >= announceBatchSize {
				if err := flushAnnounce(); err != nil {
					flushPersist()
					return announced, err
				}
			}
		}
	}
	if err := flushAnnounce(); err != nil {
		flushPersist()
		return announced, err
	}
	flushPersist()
	return announced, nil
}

// persistSharedFilesBatch appends the files to the shared store in one write.
// A per-file persist rewrites the encrypted store once per file, quadratic on
// large folders.
func (kd *KeibiDrop) persistSharedFilesBatch(files []*synctracker.File) {
	if len(files) == 0 || kd.sharedStore == nil || kd.dlRegistry == nil {
		return
	}
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	if sess == nil {
		return
	}
	tag := kd.dlRegistry.peerTag(sess.ExpectedPeerFingerprint, kd.registryKey)
	entries := kd.sharedStore.Load()
	for _, f := range files {
		entries = append(entries, sharedEntry{
			PeerTag: tag,
			Path:    f.RealPathOfFile,
			Size:    f.Size,
			ModTime: f.LastEditTime,
			Rel:     f.RelativePath,
		})
	}
	kd.sharedStore.Save(entries)
}

// BackfillRemoteFilesIntoFS adds tracker-known remote files to the FUSE tree.
// ADD_FILE notifies that arrive before the first SetFS land only in
// SyncTracker.RemoteFiles; without this they never appear in the mount. Run
// calls it right after it publishes the mounted FS. Files already in the tree
// are left alone.
func (kd *KeibiDrop) BackfillRemoteFilesIntoFS() {
	fsRoot := kd.FS
	if fsRoot == nil || fsRoot.Root() == nil || kd.SyncTracker == nil {
		return
	}
	root := fsRoot.Root()

	kd.SyncTracker.RemoteFilesMu.RLock()
	files := make([]*synctracker.File, 0, len(kd.SyncTracker.RemoteFiles))
	for _, f := range kd.SyncTracker.RemoteFiles {
		files = append(files, f)
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()

	for _, f := range files {
		treePath := f.RelativePath
		if len(treePath) == 0 {
			continue
		}
		if treePath[0] != '/' {
			treePath = "/" + treePath
		}
		root.RemoteFilesLock.Lock()
		_, known := root.RemoteFiles[treePath]
		root.RemoteFilesLock.Unlock()
		if known {
			continue
		}
		name := f.Name
		if name == "" {
			name = filepath.Base(f.RelativePath)
		}
		mode := f.Mode
		if mode == 0 {
			mode = uint32(0o644) | syscall.S_IFREG
		}
		mtime := time.Unix(0, int64(f.LastEditTime))
		atime := mtime
		if f.Atime != 0 {
			atime = time.Unix(0, int64(f.Atime))
		}
		stat := &fuse.Stat_t{
			Mode:     mode,
			Nlink:    1,
			Uid:      uint32(os.Getuid()),
			Gid:      uint32(os.Getgid()),
			Size:     int64(f.Size),
			Atim:     fuse.NewTimespec(atime),
			Mtim:     fuse.NewTimespec(mtime),
			Ctim:     fuse.NewTimespec(mtime),
			Birthtim: fuse.NewTimespec(time.Unix(0, int64(f.CreatedTime))),
		}
		_ = root.AddRemoteFileWithBase(kd.logger, f.RelativePath, name, stat, 0)
	}
}
