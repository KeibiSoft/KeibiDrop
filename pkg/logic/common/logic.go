// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// Timeout is the peer-join wait budget in seconds: 10 minutes minus a 5s
// margin, matching the relay registration TTL.
const Timeout = 10*60 - 5

// bridgeRoundWait bounds one round's wait for a joiner on the bridge. It matches the
// direct accept window, so a round is the same length whichever path it takes.
const bridgeRoundWait = 15 * time.Second

// joinBridgeWait bounds the joiner's wait for the creator's hello on its bridge
// leg. A creator with an open inbound accepts our direct dial, then dials our
// listener for the full DirectDialTimeout before it takes the bridge, because a
// direct handshake carries no reachability verdict. It can also still be inside
// one direct accept window when we arrive. The window covers both, with margin.
const joinBridgeWait = 2 * (session.DirectDialTimeout + bridgeRoundWait)

// dropOutboundConn closes a completed outbound conn and forgets it. Every LAN bail-out
// needs this: the outbound handshake has already succeeded by then, and ResetOutboundCrypto
// only wipes the keys, so without it the conn is stranded when we fall through to
// direct or bridge.
func (kd *KeibiDrop) dropOutboundConn() {
	if c := kd.session.OutboundConn(); c != nil {
		c.Close()
		kd.session.SetOutboundConn(nil)
	}
}

// Add a file to be tracked.
func (kd *KeibiDrop) AddFile(path string) error {
	logger := kd.logger.With("method", "add-file")
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Error("Invalid session", "error", ErrInvalidSession)
		return ErrInvalidSession
	}

	cleanPath := filepath.Clean(path)

	finfo, err := os.Stat(cleanPath)
	if err != nil {
		logger.Error("Failed to add file", "error", err)
		return err
	}

	name := finfo.Name()

	if finfo.IsDir() {
		logger.Warn("File is a directory", "error", syscall.EISDIR)
		return syscall.EISDIR
	}

	atime, btime := statTimes(finfo)
	file := &synctracker.File{
		Name:           name,
		RelativePath:   name,
		RealPathOfFile: cleanPath,
		Size:           uint64(finfo.Size()),
		LastEditTime:   uint64(finfo.ModTime().UnixNano()),
		CreatedTime:    btime,
		Atime:          atime,
	}

	kd.SyncTracker.LocalFilesMu.Lock()
	defer kd.SyncTracker.LocalFilesMu.Unlock()
	kd.SyncTracker.LocalFiles[name] = file // upsert: allows retry after failed notification

	_, err = kd.sendNotify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.AddFile),
		Path: file.RelativePath,
		Attr: &bindings.Attr{
			Dev:              0,
			Ino:              0,
			Mode:             uint32(finfo.Mode().Perm()) | syscall.S_IFREG,
			Size:             finfo.Size(),
			AccessTime:       atime,
			ModificationTime: file.LastEditTime,
			ChangeTime:       file.LastEditTime,
			BirthTime:        btime,
			Flags:            0o444,
		},
	})
	if err != nil {
		logger.Error("Failed to notify peer", "error", err)
		return err
	}

	if kd.sharedStore != nil && kd.dlRegistry != nil && kd.session != nil {
		tag := kd.dlRegistry.peerTag(kd.session.ExpectedPeerFingerprint, kd.registryKey)
		kd.persistSharedFile(tag, file)
	}

	logger.Info("Success")

	return nil
}

func (kd *KeibiDrop) persistSharedFile(tag [16]byte, file *synctracker.File) {
	if kd.sharedStore == nil {
		return
	}
	entries := kd.sharedStore.Load()
	entries = append(entries, sharedEntry{
		PeerTag: tag,
		Path:    file.RealPathOfFile,
		Size:    file.Size,
		ModTime: file.LastEditTime,
		Rel:     file.RelativePath,
	})
	kd.sharedStore.Save(entries)
}

// UnshareFile removes a file from the shared list and notifies the peer.
// Does NOT delete the file from disk.
func (kd *KeibiDrop) UnshareFile(name string) error {
	kd.SyncTracker.LocalFilesMu.Lock()
	_, exists := kd.SyncTracker.LocalFiles[name]
	delete(kd.SyncTracker.LocalFiles, name)
	kd.SyncTracker.LocalFilesMu.Unlock()

	if !exists {
		return fmt.Errorf("file %q not shared", name)
	}

	kd.mu.Lock()
	sess := kd.session
	client := kd.KDClient
	kd.mu.Unlock()
	if sess != nil && client != nil {
		_, _ = client.Notify(context.Background(), &bindings.NotifyRequest{
			Type: bindings.NotifyType(types.RemoveFile),
			Path: name,
		})
	}
	return nil
}

// AddFileAs adds a file with a custom remote name (preserving folder structure).
// Automatically sends ADD_DIR for any parent directories the peer may not have.
func (kd *KeibiDrop) AddFileAs(localPath string, remoteName string) error {
	logger := kd.logger.With("method", "add-file-as")
	if kd.session == nil || kd.session.GRPCClient == nil {
		return ErrInvalidSession
	}

	cleanPath := filepath.Clean(localPath)
	finfo, err := os.Stat(cleanPath)
	if err != nil {
		return err
	}
	if finfo.IsDir() {
		return syscall.EISDIR
	}

	atime, btime := statTimes(finfo)
	file := &synctracker.File{
		Name:           filepath.Base(remoteName),
		RelativePath:   remoteName,
		RealPathOfFile: cleanPath,
		Size:           uint64(finfo.Size()),
		LastEditTime:   uint64(finfo.ModTime().UnixNano()),
		CreatedTime:    btime,
		Atime:          atime,
	}

	kd.SyncTracker.LocalFilesMu.Lock()
	defer kd.SyncTracker.LocalFilesMu.Unlock()
	kd.SyncTracker.LocalFiles[remoteName] = file

	_, err = kd.sendNotify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.AddFile),
		Path: remoteName,
		Attr: &bindings.Attr{
			Mode:             uint32(finfo.Mode().Perm()) | syscall.S_IFREG,
			Size:             finfo.Size(),
			AccessTime:       atime,
			ModificationTime: file.LastEditTime,
			ChangeTime:       file.LastEditTime,
			BirthTime:        btime,
			Flags:            0o444,
		},
	})
	if err != nil {
		logger.Error("Failed to notify peer", "error", err)
		return err
	}

	logger.Info("Success", "remoteName", remoteName)
	return nil
}

func (kd *KeibiDrop) ListFiles() (remote []string, local []string) {
	remote = []string{}
	local = []string{}

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()

	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()

	for k, v := range kd.SyncTracker.LocalFiles {
		local = append(local, fmt.Sprintf("[Local] Path: %v Size: %v RealPath: %v\n", k, v.Size, v.RealPathOfFile))
	}

	for k, v := range kd.SyncTracker.RemoteFiles {
		remote = append(remote, fmt.Sprintf("[Remote] Path: %v Size: %v RealPath: %v\n", k, v.Size, v.RealPathOfFile))
	}

	return remote, local
}

func (kd *KeibiDrop) PullFile(remoteName, localPath string) error {
	logger := kd.logger.With("method", "pull-file")
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Error("Invalid session", "error", ErrInvalidSession)
		return ErrInvalidSession
	}

	localPath = filepath.Clean(localPath)

	// Snapshot remote file metadata under a short read lock.
	kd.SyncTracker.RemoteFilesMu.RLock()
	remFilePtr, ok := kd.SyncTracker.RemoteFiles[remoteName]
	var fileCopy synctracker.File
	if ok {
		fileCopy = *remFilePtr
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	if !ok {
		logger.Error("Not found", "error", syscall.ENOENT)
		return syscall.ENOENT
	}

	fileSize := fileCopy.Size
	relPath := fileCopy.RelativePath
	logger.Info("PullFile starting", "remoteName", remoteName, "localPath", localPath, "fileSize", fileSize, "relPath", relPath)

	// Ensure parent directories exist.
	if dir := filepath.Dir(localPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Error("Failed to create parent directories", "error", err)
			return err
		}
	}

	// Check for resumable partial download.
	bitmapPath := filesystem.BitmapPath(localPath)
	var bitmap *filesystem.ChunkBitmap
	var f *os.File
	var err error

	if info, statErr := os.Stat(localPath); statErr == nil && info.Size() == int64(fileSize) {
		// Partial file exists at expected size. Try loading bitmap.
		if bm, loadErr := filesystem.LoadChunkBitmap(bitmapPath, int64(fileSize)); loadErr == nil {
			bitmap = bm
			f, err = filesystem.OpenShared(localPath, os.O_WRONLY, 0644)
			if err != nil {
				logger.Error("Failed to open partial file for resume", "error", err)
				return err
			}
			logger.Info("Resuming download", "progress", bitmap.Progress(), "have", bitmap.Have(), "total", bitmap.Total())
		}
	}

	if bitmap == nil {
		// Fresh download. OpenShared: this handle lives for the whole
		// transfer, and without delete sharing it blocks rename/unlink of
		// the file on Windows.
		f, err = filesystem.OpenShared(localPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			logger.Error("Failed to create local file", "error", err)
			return err
		}
		if fileSize > 0 {
			if err := f.Truncate(int64(fileSize)); err != nil {
				logger.Error("Failed to pre-allocate file", "error", err)
				f.Close()
				return err
			}
		}
		bitmap = filesystem.NewChunkBitmapWithSize(int64(fileSize), config.BlockSize)
	}
	defer f.Close()

	if bitmap != nil && bitmap.IsComplete() {
		os.Remove(bitmapPath)
		logger.Info("File already fully downloaded")
		goto updateTracker
	}

	if bitmap != nil && bitmap.Total() > 0 {
		// Snapshot kd.ctx under kd.mu: Run's reconnect branch swaps it under the same lock.
		kd.mu.Lock()
		ctx := kd.ctx
		kd.mu.Unlock()
		dlCtx, dlCancel := context.WithCancel(ctx)
		defer dlCancel()
		kd.registerDownload(remoteName, dlCancel)
		kd.activeDownloadsMu.Lock()
		kd.activeBitmaps[remoteName] = bitmap
		kd.activeDownloadsMu.Unlock()
		if kd.dlRegistry != nil && kd.session != nil {
			tag := kd.dlRegistry.peerTag(kd.session.ExpectedPeerFingerprint, kd.registryKey)
			kd.dlRegistry.Register(bitmapPath, tag)
		}
		if kd.HealthMonitor != nil {
			kd.HealthMonitor.TransferStarted()
			defer kd.HealthMonitor.TransferEnded()
		}
		defer kd.unregisterDownload(remoteName)

		if err := kd.pullStreamFile(dlCtx, bitmap, f, relPath, fileSize, config.BlockSize, bitmapPath, logger); err != nil {
			return err
		}
		_ = bitmap.Save(bitmapPath)
	}

	// Download complete. Clean up bitmap and registry entry.
	os.Remove(bitmapPath)
	if kd.dlRegistry != nil {
		kd.dlRegistry.Unregister(bitmapPath)
	}

updateTracker:
	// Close the write handle before finalizeDownload's metadata apply: Windows
	// stamps its cached LastWriteTime at handle close, which would overwrite
	// Chtimes. The deferred Close then returns ErrClosed, ignored.
	_ = f.Close()
	kd.finalizeDownload(remoteName, localPath, fileCopy)

	if fi, statErr := os.Stat(localPath); statErr == nil {
		logger.Info("PullFile complete", "expectedSize", fileSize, "actualSize", fi.Size(), "match", uint64(fi.Size()) == fileSize)
	}
	logger.Info("Success")
	return nil
}

// PullFileWithParams downloads remoteName to localPath using the specified
// blockSize (bytes per gRPC chunk) and nWorkers (parallel streams).
// Intended for benchmarking; production code uses PullFile with defaults.
func (kd *KeibiDrop) PullFileWithParams(remoteName, localPath string, blockSize, nWorkers int) error {
	logger := kd.logger.With("method", "pull-file-with-params")
	// Snapshot the session/client under kd.mu once; the worker goroutines below must
	// not re-read kd.session, which teardown can nil mid-transfer.
	kd.mu.Lock()
	s := kd.session
	kd.mu.Unlock()
	if s == nil || s.GRPCClient == nil {
		return ErrInvalidSession
	}
	client := s.GRPCClient
	if kd.HealthMonitor != nil {
		kd.HealthMonitor.TransferStarted()
		defer kd.HealthMonitor.TransferEnded()
	}

	localPath = filepath.Clean(localPath)

	kd.SyncTracker.RemoteFilesMu.RLock()
	remFilePtr, ok := kd.SyncTracker.RemoteFiles[remoteName]
	var fileCopy synctracker.File
	if ok {
		fileCopy = *remFilePtr
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	if !ok {
		return syscall.ENOENT
	}

	fileSize := fileCopy.Size
	relPath := fileCopy.RelativePath

	if dir := filepath.Dir(localPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	bitmapPath := filesystem.BitmapPath(localPath)
	// OpenShared: the transfer-long handle must not block rename/unlink on
	// Windows (no delete sharing in plain os.Create).
	f, err := filesystem.OpenShared(localPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if fileSize > 0 {
		if err := f.Truncate(int64(fileSize)); err != nil {
			return err
		}
	}

	bitmap := filesystem.NewChunkBitmapWithSize(int64(fileSize), blockSize)
	if bitmap == nil || bitmap.Total() == 0 {
		goto updateTracker
	}

	{
		totalChunks := bitmap.Total()
		if nWorkers > totalChunks {
			nWorkers = totalChunks
		}

		// Snapshot kd.ctx under kd.mu: Run's reconnect branch swaps it under the same lock.
		kd.mu.Lock()
		ctx := kd.ctx
		kd.mu.Unlock()
		dlCtx, dlCancel := context.WithCancel(ctx)
		defer dlCancel()
		kd.registerDownload(remoteName, dlCancel)
		defer kd.unregisterDownload(remoteName)

		var wg sync.WaitGroup
		errCh := make(chan error, nWorkers)
		var chunksWritten atomic.Int32

		for w := 0; w < nWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				stream, err := client.Read(dlCtx)
				if err != nil {
					errCh <- fmt.Errorf("worker %d: open stream: %w", workerID, err)
					dlCancel()
					return
				}
				defer stream.CloseSend()

				for i := workerID; i < totalChunks; i += nWorkers {
					offset := uint64(i) * uint64(blockSize)
					size := uint64(blockSize)
					if offset+size > fileSize {
						size = fileSize - offset
					}

					if err := stream.Send(&bindings.ReadRequest{
						Path:   relPath,
						Offset: offset,
						Size:   uint32(size), // #nosec G115 -- size is bounded by blockSize (4 MiB)
					}); err != nil {
						errCh <- fmt.Errorf("worker %d: send chunk %d: %w", workerID, i, err)
						dlCancel()
						return
					}

					data, err := stream.Recv()
					if err != nil {
						errCh <- fmt.Errorf("worker %d: recv chunk %d: %w", workerID, i, err)
						dlCancel()
						return
					}

					if _, err := f.WriteAt(data.Data, int64(offset)); err != nil {
						errCh <- fmt.Errorf("worker %d: write chunk %d: %w", workerID, i, err)
						dlCancel()
						return
					}

					bitmap.Set(i)
					if chunksWritten.Add(1)%100 == 0 {
						_ = bitmap.Save(bitmapPath)
					}
				}
			}(w)
		}

		wg.Wait()
		close(errCh)
		if err := <-errCh; err != nil {
			logger.Error("Download failed", "error", err)
			_ = bitmap.Save(bitmapPath)
			return err
		}
	}

	os.Remove(bitmapPath)

updateTracker:
	kd.finalizeDownload(remoteName, localPath, fileCopy)
	return nil
}

// finalizeDownload records a completed download in the sync tracker: it points
// the remote entry at the local path and registers the file as locally held.
func (kd *KeibiDrop) finalizeDownload(remoteName, localPath string, fileCopy synctracker.File) {
	fileCopy.RealPathOfFile = localPath
	kd.SyncTracker.RemoteFilesMu.Lock()
	if rf, ok := kd.SyncTracker.RemoteFiles[remoteName]; ok {
		rf.RealPathOfFile = localPath
	}
	kd.SyncTracker.RemoteFilesMu.Unlock()
	kd.SyncTracker.LocalFilesMu.Lock()
	kd.SyncTracker.LocalFiles[remoteName] = &fileCopy
	kd.SyncTracker.LocalFilesMu.Unlock()
	if kd.PreserveMetadata {
		// #nosec G115 -- unix nanos fit in int64
		if err := filesystem.ApplyOriginMetadata(localPath, fileCopy.Mode, int64(fileCopy.Atime), int64(fileCopy.LastEditTime)); err != nil {
			kd.logger.Warn("preserve_metadata apply failed", "path", localPath, "error", err)
		}
	}
}

func (kd *KeibiDrop) registerDownload(name string, cancel context.CancelFunc) {
	kd.activeDownloadsMu.Lock()
	kd.activeDownloads[name] = cancel
	kd.activeDownloadsMu.Unlock()
}

func (kd *KeibiDrop) unregisterDownload(name string) {
	kd.activeDownloadsMu.Lock()
	delete(kd.activeDownloads, name)
	delete(kd.activeBitmaps, name)
	kd.activeDownloadsMu.Unlock()
}

// CancelDownload cancels an active download. The partial file and bitmap
// are preserved on disk so the next PullFile call resumes automatically.
func (kd *KeibiDrop) CancelDownload(remoteName string) error {
	kd.activeDownloadsMu.Lock()
	cancel, ok := kd.activeDownloads[remoteName]
	kd.activeDownloadsMu.Unlock()
	if !ok {
		return fmt.Errorf("no active download for %q", remoteName)
	}
	cancel()
	return nil
}

// GetDownloadProgress returns the download progress for a file as a fraction
// [0.0, 1.0]. Returns -1 if the file has no active or resumable download.
func (kd *KeibiDrop) GetDownloadProgress(remoteName string) float64 {
	kd.activeDownloadsMu.Lock()
	resolvedBitmapKey, ok := resolveKeyWithFallback(remoteName, func(k string) bool {
		return kd.activeBitmaps[k] != nil
	})
	var bm *filesystem.ChunkBitmap
	if ok {
		bm = kd.activeBitmaps[resolvedBitmapKey]
	}
	kd.activeDownloadsMu.Unlock()
	if ok && bm != nil {
		return bm.Progress()
	}

	kd.SyncTracker.RemoteFilesMu.RLock()
	resolvedRFKey, ok := resolveKeyWithFallback(remoteName, func(k string) bool {
		_, exists := kd.SyncTracker.RemoteFiles[k]
		return exists
	})
	if !ok {
		kd.SyncTracker.RemoteFilesMu.RUnlock()
		return -1
	}
	rf := kd.SyncTracker.RemoteFiles[resolvedRFKey]
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	localPath := filepath.Join(kd.ToSave, rf.RelativePath)
	bitmapPath := filesystem.BitmapPath(localPath)
	diskBm, err := filesystem.LoadChunkBitmap(bitmapPath, int64(rf.Size))
	if err != nil {
		return -1
	}
	return diskBm.Progress()
}

func (kd *KeibiDrop) ExportFingerprint() (string, error) {
	logger := kd.logger.With("method", "export-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	fp := kd.session.OwnFingerprint

	logger.Info("Success", "fingerprint", fp)

	return fp, nil
}

func (kd *KeibiDrop) AddPeerFingerprint(fp string) error {
	logger := kd.logger.With("method", "add-peer-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	fp = strings.TrimSpace(fp)

	err := ValidateFingerprint(fp)
	if err != nil {
		logger.Error("Failed to validate fingerprint", "error", err)
		return err
	}

	kd.session.ExpectedPeerFingerprint = fp

	return nil
}

func (kd *KeibiDrop) GetPeerFingerprint() (string, error) {
	logger := kd.logger.With("method", "get-peer-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	return kd.session.ExpectedPeerFingerprint, nil
}

// SetPeerDirectAddress parses a direct LAN peer address (e.g. "fe80::1%eth0:26431"),
// stores the peer IP and port, and sets TOFU mode for the handshake.
func (kd *KeibiDrop) SetPeerDirectAddress(addr string) error {
	if kd.session == nil {
		return ErrNilPointer
	}
	ip, zone, port, err := ParsePeerDirectAddress(addr)
	if err != nil {
		return err
	}
	// Dialing needs a port; port-less forms are only valid for role decisions.
	if port == 0 {
		return fmt.Errorf("missing port in address %q", addr)
	}
	if zone != "" {
		kd.PeerIPv6IP = ip + "%" + zone
	} else {
		kd.PeerIPv6IP = ip
	}
	kd.session.PeerPort = port
	kd.session.ExpectedPeerFingerprint = "TOFU"
	if kd.IsLocalMode {
		kd.PeerLocalAddrs = []string{kd.PeerIPv6IP}
	}
	return nil
}

// waitForPeerFingerprint blocks until the expected peer fingerprint is set
// (via AddPeerFingerprint or a relay lookup), polling once a second up to the
// connection Timeout. Returns ErrTimeoutReached if it never arrives.
// CancelPendingConnect makes a CreateRoom or JoinRoom still waiting for the
// peer return ErrConnectCancelled. Disconnect calls it, so the daemon does
// not stay busy for Timeout after the operator gave up.
func (kd *KeibiDrop) CancelPendingConnect() {
	kd.connectCancelled.Store(true)
}

func (kd *KeibiDrop) waitForPeerFingerprint() error {
	for elapsed := 0; elapsed < Timeout; elapsed++ {
		if kd.session.ExpectedPeerFingerprint != "" {
			return nil
		}
		if kd.connectCancelled.Load() {
			return ErrConnectCancelled
		}
		time.Sleep(time.Second)
	}
	kd.logger.Error("Timeout reached", "error", ErrTimeoutReached)
	return ErrTimeoutReached
}

// finishConnect runs the shared post-connection setup for JoinRoom and
// CreateRoom: start the gRPC server, dial the client with retry, init
// connection resilience, then either signal ready (no-FUSE) or mount FUSE.
func (kd *KeibiDrop) finishConnect(logger *slog.Logger) error {
	// Clear the teardown latch: a prior Stop() set tearingDown to make an in-flight reconnect
	// bail, but this is a fresh CreateRoom/JoinRoom (the previous teardown already completed),
	// so the gRPC-stack publishes below must not be gated off.
	kd.tearingDown.Store(false)

	kd.filesystemReady = make(chan struct{})
	kd.filesystemReadyOnce = sync.Once{}

	// Arm the in-band ratchet on both fresh conns before the gRPC readers start. Both
	// handshakes are done, so the negotiated capability is known.
	kd.session.ApplyKeyUpdateNegotiation()

	kd.Start()

	// Retry dialing until the gRPC server is ready.
	if err := kd.connectGRPCClientWithRetry(15 * time.Second); err != nil {
		logger.Error("Failed to connect to grpc server after retries", "error", err)
		return err
	}

	// Bring up the QUIC control channel alongside TCP (fail-soft, non-blocking).
	kd.StartQUICControlChannel()

	// Start health monitoring, reconnection, and relay keepalive.
	if err := kd.InitConnectionResilience(); err != nil {
		logger.Warn("Failed to init connection resilience", "error", err)
	}

	// Eager fold: make the session forward-secret against later identity-key theft in about one
	// round trip. Best-effort and initiator-only; a no-op on the responder or with the ratchet off.
	kd.maybeStartEagerFold()

	if !kd.IsFUSE {
		// Commit the running flag before Run() does. Run() stores it only after
		// it receives the Start signal; a Stop() in that gap is a no-op, and the
		// queued Start then revives the session after the caller stopped it.
		kd.running.Store(true)
		// Unblock Run()'s <-filesystemReady so it can process signals.
		kd.filesystemReadyOnce.Do(func() { close(kd.filesystemReady) })
		logger.Info("Success, starting without FUSE")
		return nil
	}

	if err := kd.setupFilesystem(logger, kd.filesystemReady); err != nil {
		return err
	}

	// Same running-flag commit as the no-FUSE branch.
	kd.running.Store(true)
	logger.Info("Success")
	return nil
}

func (kd *KeibiDrop) JoinRoom() error {
	logger := kd.logger.With("method", "join-room")
	kd.connectCancelled.Store(false)
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	if kd.running.Load() {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	kd.prepareBridgePolicy(logger)

	// Wait for the expected peer fingerprint to be set.
	if err := kd.waitForPeerFingerprint(); err != nil {
		return err
	}

	// Get peer info from relay (needed for both direct and bridge paths).
	if kd.IsLocalMode {
		// Local mode: exchange public keys before the PQC handshake.
		peerAddr := net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(kd.session.PeerPort))
		keyConn, err := session.DialWithStableAddr("tcp", peerAddr, 15*time.Second, logger)
		if err != nil {
			logger.Error("Failed to dial for key exchange", "addr", peerAddr, "error", err)
			return err
		}
		if err := session.ExchangePublicKeysLocal(kd.session, keyConn, true); err != nil {
			keyConn.Close()
			logger.Error("Failed local key exchange", "error", err)
			return err
		}
		keyConn.Close()
		logger.Info("Local key exchange complete (join side)")
	} else {
		if kd.OnEvent != nil {
			kd.OnEvent("connect_status:Waiting for peer...")
		}
		const relayRetryDelay = 1 * time.Second
		const relayPhase1 = 15
		const relayPhase2 = 45
		relayMaxRetries := relayPhase1 + relayPhase2
		// Snapshot kd.ctx under kd.mu: Run's reconnect branch swaps it under the same lock.
		kd.mu.Lock()
		ctx := kd.ctx
		kd.mu.Unlock()
		var relayErr error
		for attempt := 0; attempt <= relayMaxRetries; attempt++ {
			relayErr = kd.getRoomFromRelay(kd.session.ExpectedPeerFingerprint)
			if relayErr == nil {
				break
			}
			if !errors.Is(relayErr, ErrNotFound) {
				return relayErr
			}
			if attempt == relayPhase1 && kd.OnEvent != nil {
				kd.OnEvent("connect_status:peer_not_ready")
			}
			if attempt < relayMaxRetries {
				select {
				case <-time.After(relayRetryDelay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if relayErr != nil {
			return fmt.Errorf("peer not found on relay after %d attempts: %w", relayMaxRetries, relayErr)
		}
	}

	// Connection priority: LAN (2s per addr) → direct IPv6 (15s) → bridge relay.
	{
		// 1. Try LAN addresses first (only in local mode; internet mode skips this).
		lanConnected := false
		if kd.IsLocalMode && len(kd.PeerLocalAddrs) > 0 {
			logger.Info("Trying LAN addresses first", "addrs", kd.PeerLocalAddrs)
			for _, localAddr := range kd.PeerLocalAddrs {
				addr := net.JoinHostPort(localAddr, strconv.Itoa(kd.session.PeerPort))
				logger.Info("Trying LAN address", "addr", addr)
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					logger.Info("LAN address failed", "addr", addr, "error", err)
					continue
				}
				// LAN connection succeeded, do handshake on this connection.
				if err := session.PerformOutboundHandshakeOnConn(kd.session, conn); err != nil {
					conn.Close()
					logger.Warn("LAN handshake failed", "addr", addr, "error", err)
					continue
				}
				logger.Info("LAN connection succeeded!", "addr", addr)
				kd.PeerIPv6IP = localAddr
				lanConnected = true

				// Snapshot under kd.mu: a prior bridge-fallback timeout or a concurrent
				// Shutdown can leave the listener nil here. Accept on a nil interface panics.
				kd.mu.Lock()
				ln := kd.listener
				kd.mu.Unlock()
				if ln == nil {
					logger.Warn("Listener not open for LAN inbound accept")
					// Outbound handshake already succeeded; close it so we don't
					// strand the conn and stale crypto state on this bail-out.
					kd.dropOutboundConn()
					kd.session.ResetOutboundCrypto()
					return ErrListenerNotOpen
				}

				// Accept inbound from peer (LAN, should be fast, 5s timeout).
				_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
				inConn, err := ln.Accept()
				_ = ln.(*net.TCPListener).SetDeadline(time.Time{}) // clear deadline
				if err != nil {
					logger.Warn("LAN inbound accept failed", "error", err)
					// Close outbound, fall through to direct/bridge.
					kd.dropOutboundConn()
					kd.session.ResetOutboundCrypto()
					lanConnected = false
					break
				}
				if err := handshakeOrClose(kd.session, inConn); err != nil {
					logger.Warn("LAN inbound handshake failed", "error", err)
					// The outbound leg already handshaked, so drop it too rather than
					// strand it when we fall through to direct or bridge.
					kd.dropOutboundConn()
					lanConnected = false
				}
				break
			}
			if lanConnected {
				kd.ConnectionMode = "lan"
				if kd.OnEvent != nil {
					kd.OnEvent("connection_mode:lan")
				}
				goto connected
			}
			// Reset crypto state after failed LAN attempts.
			kd.session.ResetOutboundCrypto()
		}

		// 2. Try direct IPv6 P2P (skip if peer has no IPv6, e.g. mobile).
		var directErr error
		switch {
		case kd.PeerInboundBlocked(): // dialing it would only burn the timeout
			directErr = fmt.Errorf("peer advertises a blocked inbound")
		case kd.PeerIPv6IP != "":
			peerAddr := net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(kd.session.PeerPort))
			directErr = session.PerformOutboundHandshake(kd.session, peerAddr)
		default:
			directErr = fmt.Errorf("peer has no IPv6 address")
		}

		needBridge := false

		switch {
		case directErr == nil:
			// Direct outbound succeeded. Accept inbound with 15s timeout.
			// If the peer can't reach us (firewall blocks inbound IPv6),
			// fall back to bridge for both directions.
			// Nothing reaches our listener, so the peer's dial cannot arrive.
			if kd.InboundBlocked() && kd.BridgeAddr != "" {
				logger.Info("Inbound is blocked on this network, taking the bridge without waiting on accept")
				needBridge = true
			} else {
				logger.Info("Direct P2P outbound connected, waiting for inbound (15s timeout)")

				// Snapshot under kd.mu: a prior bridge-fallback timeout or a concurrent
				// Shutdown can leave the listener nil here. Accept on a nil interface panics.
				kd.mu.Lock()
				ln := kd.listener
				kd.mu.Unlock()
				if ln == nil {
					logger.Warn("Listener not open for direct P2P inbound accept")
					// Outbound handshake already succeeded; close it so we don't
					// strand the conn and stale crypto state on this bail-out.
					kd.dropOutboundConn()
					kd.session.ResetOutboundCrypto()
					return ErrListenerNotOpen
				}

				_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second))
				// Same junk-connection tolerance as the create side: a relay
				// probe or a scanner must not kill the return leg.
				var inConn net.Conn
				var acceptErr error
				for {
					inConn, acceptErr = ln.Accept()
					if acceptErr != nil {
						break
					}
					if hsErr := handshakeOrClose(kd.session, inConn); hsErr != nil {
						logger.Warn("Inbound handshake failed, accepting again", "error", hsErr)
						continue
					}
					break
				}
				_ = ln.(*net.TCPListener).SetDeadline(time.Time{})

				if acceptErr != nil {
					logger.Warn("Inbound accept failed", "error", acceptErr)
					kd.noteEmptyAcceptWindow() // nothing reached us here; skip the wait next time
					needBridge = kd.BridgeAddr != ""
					if !needBridge {
						if kd.StrictMode {
							return fmt.Errorf("inbound accept timed out; strict mode keeps the relay fallback off")
						}
						return fmt.Errorf("inbound accept timed out and no bridge configured")
					}
				} else {
					kd.markInboundReachable()
					kd.ConnectionMode = "direct"
					if kd.OnEvent != nil {
						kd.OnEvent("connection_mode:direct")
					}
				}
			}

			if needBridge {
				logger.Info("Falling back to bridge for both directions")
				// Close the direct outbound; we'll redo both via bridge.
				if c := kd.session.OutboundConn(); c != nil {
					c.Close()
					kd.session.SetOutboundConn(nil)
				}
				kd.session.ResetOutboundCrypto()
			}
		case kd.BridgeAddr != "":
			logger.Warn("Direct P2P failed, falling back to bridge", "error", directErr, "bridge", kd.BridgeAddr)
			needBridge = true
			kd.session.ResetOutboundCrypto()
		default:
			logger.Error("Direct P2P failed and no bridge configured", "error", directErr)
			if kd.StrictMode {
				return fmt.Errorf("direct P2P failed; strict mode keeps the relay fallback off: %w", directErr)
			}
			return directErr
		}

		if needBridge {
			outConn, err := kd.dialBridgeDir("pair1", logger)
			if err != nil {
				return fmt.Errorf("bridge dial (outbound): %w", err)
			}
			if err := session.PerformOutboundHandshakeOnConn(kd.session, outConn); err != nil {
				outConn.Close()
				return fmt.Errorf("bridge outbound handshake: %w", err)
			}

			inConn, err := kd.dialBridgeDir("pair2", logger)
			if err != nil {
				return fmt.Errorf("bridge dial (inbound): %w", err)
			}
			if err := kd.joinBridgeInbound(inConn); err != nil {
				return fmt.Errorf("bridge inbound handshake: %w", err)
			}
			kd.ConnectionMode = "bridge"
			if kd.OnEvent != nil {
				kd.OnEvent("connection_mode:bridge")
			}
		}
	}

connected:
	return kd.finishConnect(logger)
}

// Connect determines the creator/joiner role automatically using
// deterministic fingerprint comparison and calls CreateRoom or JoinRoom.
// Lower fingerprint = creator (registers to relay, accepts inbound).
// Higher fingerprint = joiner (fetches from relay, dials out).
func (kd *KeibiDrop) Connect() error {
	logger := kd.logger.With("method", "connect")
	if kd.session == nil {
		return ErrNilPointer
	}
	ownFP := kd.session.OwnFingerprint
	peerFP := kd.session.ExpectedPeerFingerprint
	if peerFP == "" {
		return ErrEmptyFingerprint
	}
	if ownFP == peerFP {
		return ErrIdenticalFingerprints
	}
	if ownFP < peerFP {
		logger.Info("Fingerprint tiebreak: I am creator", "own", ownFP[:8], "peer", peerFP[:8])
		if kd.OnEvent != nil {
			kd.OnEvent("connect_status:Waiting for peer to connect...")
		}
		return kd.CreateRoom()
	}
	logger.Info("Fingerprint tiebreak: I am joiner", "own", ownFP[:8], "peer", peerFP[:8])
	return kd.JoinRoom()
}

// LocalConnectRole picks who creates (listens) vs joins (dials) in local mode.
// Smaller name creates; if names collide (random), smaller address breaks it.
func LocalConnectRole(myName, peerName, myAddr, peerAddr string) (create bool) {
	if myName != peerName {
		return myName < peerName
	}
	return myAddr < peerAddr
}

// DecideLocalRole reports whether this peer should create (listen) rather than join (dial) for
// a local-mode connection to peerName at peerAddr. It feeds LocalConnectRole this peer's LAN
// IPv4 and the peer's bare IP (port/zone stripped), so colliding names compare like-for-like.
func DecideLocalRole(myName, peerName, peerAddr string) bool {
	logger := slog.Default().With("fn", "DecideLocalRole")
	peerIP := peerAddr
	if ip, _, _, err := ParsePeerDirectAddress(peerAddr); err == nil {
		peerIP = ip
	}
	myAddr := GetDiscoveryLANAddress()
	if myName == peerName && myAddr == peerIP {
		logger.Warn("Local role tie unresolved, both peers may pick the same role", "peer-name", peerName, "peer-addr", peerIP)
	}
	return LocalConnectRole(myName, peerName, myAddr, peerIP)
}

func (kd *KeibiDrop) CreateRoom() error {
	logger := kd.logger.With("method", "create-room")
	kd.connectCancelled.Store(false)
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	if kd.running.Load() {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	kd.prepareBridgePolicy(logger)
	// A leg parked between rounds belongs to this create only.
	defer kd.closeParkedBridgeIn()

	if !kd.IsLocalMode {
		if err := kd.registerRoomToRelay(); err != nil {
			return err
		}
	}

	logger.Info("Waiting for peer to join...")

	// Wait for the expected peer fingerprint (skip if already set, e.g. local mode TOFU).
	if err := kd.waitForPeerFingerprint(); err != nil {
		return err
	}

	// In local mode, exchange public keys before the PQC handshake.
	if kd.IsLocalMode {
		// Snapshot under kd.mu: a prior bridge-fallback timeout or a concurrent Shutdown
		// can leave the listener nil here. Accept on a nil interface panics.
		kd.mu.Lock()
		ln := kd.listener
		kd.mu.Unlock()
		if ln == nil {
			logger.Warn("Listener not open for local key exchange")
			return ErrListenerNotOpen
		}
		keyConn, err := ln.Accept()
		if err != nil {
			logger.Error("Failed to accept key exchange connection", "error", err)
			return err
		}
		if err := session.ExchangePublicKeysLocal(kd.session, keyConn, false); err != nil {
			keyConn.Close()
			logger.Error("Failed local key exchange", "error", err)
			return err
		}
		keyConn.Close()
		logger.Info("Local key exchange complete (create side)")
	}

	// Rendezvous loop. One round is [direct accept window] then [one bridge attempt].
	// The LOOP supplies the window a human needs, not the per-round timeouts: one person
	// creates, sends the code by chat, the other pastes it. Measured on the WAN pair, the
	// old single round gave up 38s after create and the joiner arrived at 51s. The budget
	// is Timeout, the same 595s the relay registration already lives for, so one number
	// governs the whole connect flow.
	{
		budget := time.Now().Add(Timeout * time.Second)
		kd.connectAborted.Store(false)
		registered := time.Now()
		for round := 0; ; round++ {
			if kd.connectAborted.Load() {
				return fmt.Errorf("create-room: cancelled")
			}
			done, err := kd.createRendezvousRound(logger, round)
			if err != nil {
				return err
			}
			if done {
				break
			}
			if !time.Now().Before(budget) {
				return fmt.Errorf("create-room: no peer arrived within %ds", Timeout)
			}
			// The relay keepalive only starts after a peer connects, and a registration
			// lives relayEntryTTL. Without this refresh the room stops being findable
			// halfway through its own budget and the joiner gets "peer not found".
			if !kd.IsLocalMode && time.Since(registered) >= defaultKeepaliveInterval {
				if rErr := kd.registerRoomToRelay(); rErr != nil {
					logger.Warn("Could not refresh the relay registration while waiting", "error", rErr)
				} else {
					registered = time.Now()
				}
			}
			logger.Info("No peer yet; reopening the room for another round",
				"round", round, "remaining", time.Until(budget).Round(time.Second))
		}
	}

	// Reopen listener if it was closed during bridge fallback.
	if kd.listener == nil {
		addr := net.JoinHostPort("", strconv.Itoa(kd.inboundPort))
		newLn, lnErr := net.Listen("tcp", addr)
		if lnErr != nil {
			return fmt.Errorf("reopen listener: %w", lnErr)
		}
		kd.listener = newLn
	}

	return kd.finishConnect(logger)
}

// createRendezvousRound runs one direct-accept window then one bridge attempt. It reports
// whether the session is connected. A false with no error means nobody arrived, and the
// caller re-arms for another round while its budget lasts.
func (kd *KeibiDrop) createRendezvousRound(logger *slog.Logger, round int) (bool, error) {
	{
		useBridge := false

		// A previous round closed the listener on its way to the bridge. Re-arm it, so
		// this round can still take a direct joiner.
		if kd.listener == nil && kd.BridgeAddr != "" && round > 0 && !kd.InboundBlocked() {
			addr := net.JoinHostPort("", strconv.Itoa(kd.inboundPort))
			if newLn, lnErr := net.Listen("tcp", addr); lnErr == nil {
				kd.listener = newLn
			} else {
				logger.Warn("Could not re-arm the inbound listener for this round", "error", lnErr)
			}
		}

		// kd.listener is nil if a prior bridge fallback closed it and we returned
		// before reopening it; use the bridge (reopened below) instead of nil-derefing.
		if kd.listener == nil {
			if kd.BridgeAddr == "" {
				return false, fmt.Errorf("create-room: inbound listener not open and no bridge configured")
			}
			logger.Warn("Inbound listener not open (prior bridge fallback), using bridge")
			useBridge = true
		}

		if kd.LocalIPv6IP == "" && kd.BridgeAddr != "" {
			logger.Info("No IPv6, skipping direct P2P, using bridge")
			useBridge = true
		}

		// Nothing reaches our listener, so the joiner's dial cannot arrive.
		if kd.InboundBlocked() && kd.BridgeAddr != "" {
			logger.Info("Inbound is blocked on this network, skipping direct P2P, using bridge")
			useBridge = true
		}

		if !useBridge {
			_ = kd.listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second))
		}

		var conn net.Conn
		var acceptErr error
		if !useBridge {
			// The public internet delivers junk connections: the relay's
			// reachability probe and port scanners connect and close. A
			// failed handshake must not kill the room. Close that
			// connection and accept again until the deadline. This is
			// safe: the handshake writes to the session only after the
			// fingerprint check passes.
			for {
				conn, acceptErr = kd.listener.Accept()
				if acceptErr != nil {
					break
				}
				if hsErr := handshakeOrClose(kd.session, conn); hsErr != nil {
					logger.Warn("Inbound handshake failed, accepting again", "error", hsErr)
					continue
				}
				break
			}
			_ = kd.listener.(*net.TCPListener).SetDeadline(time.Time{})
		}

		if !useBridge && acceptErr != nil {
			// Close the listener so late-arriving joiners get
			// "connection refused" instead of a phantom TCP accept.
			kd.listener.Close()
			kd.listener = nil

			if kd.BridgeAddr == "" {
				logger.Error("Direct P2P accept timed out and no bridge configured", "error", acceptErr)
				if kd.StrictMode {
					return false, fmt.Errorf("direct P2P accept timed out; strict mode keeps the relay fallback off: %w", acceptErr)
				}
				return false, acceptErr
			}
			logger.Warn("Direct P2P accept timed out, falling back to bridge", "error", acceptErr)
			if round == 0 {
				// Learn reachability once. Re-marking every round would say nothing new
				// and would keep skipping the direct window for the rest of the budget.
				kd.noteEmptyAcceptWindow()
			}
			useBridge = true
		}

		if !useBridge {
			// Direct inbound succeeded; the accept loop verified the handshake.
			kd.markInboundReachable()

			addr, ok := conn.RemoteAddr().(*net.TCPAddr)
			if !ok {
				return false, fmt.Errorf("failed to cast TCP address")
			}
			peerIP := addr.IP.String()
			if addr.Zone != "" {
				peerIP = peerIP + "%" + addr.Zone
			}
			kd.PeerIPv6IP = peerIP

			// Try direct outbound to peer. If it fails (peer behind firewall),
			// fall back to bridge for the outbound direction.
			outboundAddr := net.JoinHostPort(peerIP, strconv.Itoa(kd.session.PeerPort))
			if err := session.PerformOutboundHandshake(kd.session, outboundAddr); err != nil {
				if kd.BridgeAddr != "" {
					logger.Warn("Direct outbound to peer failed, falling back to bridge for outbound", "error", err)
					// Reset outbound crypto state for bridge handshake.
					kd.session.ResetOutboundCrypto()
					// Close direct inbound too; we need both via bridge.
					if c := kd.session.InboundConn(); c != nil {
						c.Close()
						kd.session.SetInboundConn(nil)
					}
					kd.session.ResetInboundCrypto()
					useBridge = true
				} else {
					return false, err
				}
			}
		}
		if !useBridge {
			if kd.IsLocalMode {
				kd.ConnectionMode = "lan"
			} else {
				kd.ConnectionMode = "direct"
			}
			if kd.OnEvent != nil {
				kd.OnEvent("connection_mode:" + kd.ConnectionMode)
			}
		}
		if useBridge {
			kd.ConnectionMode = "bridge"
			if kd.OnEvent != nil {
				kd.OnEvent("connection_mode:bridge")
			}
			// Bridge fallback.
			logger.Info("Bridge mode: connecting to relay", "addr", kd.BridgeAddr)

			if !kd.bridgeInbound(logger) {
				return false, nil
			}

			outConn, err := kd.dialBridgeDir("pair2", logger)
			if err != nil {
				return false, fmt.Errorf("bridge dial (outbound): %w", err)
			}
			if err := session.PerformOutboundHandshakeOnConn(kd.session, outConn); err != nil {
				outConn.Close()
				return false, fmt.Errorf("bridge outbound handshake: %w", err)
			}
		}
	}
	return true, nil
}

// ConnectToContact looks up a contact by fingerprint, registers it, and connects.
func (kd *KeibiDrop) ConnectToContact(fingerprint string) error {
	if kd.AddressBook == nil {
		return fmt.Errorf("no address book loaded")
	}
	c := kd.AddressBook.Lookup(fingerprint)
	if c == nil {
		return fmt.Errorf("contact not found in address book")
	}
	if err := kd.AddPeerFingerprint(c.Fingerprint); err != nil {
		return err
	}
	return kd.Connect()
}

// SaveCurrentPeerAsContact saves the currently connected peer as a named contact.
func (kd *KeibiDrop) SaveCurrentPeerAsContact(name string) error {
	if kd.AddressBook == nil {
		return fmt.Errorf("no address book loaded")
	}
	if kd.session == nil {
		return ErrNilPointer
	}
	if !kd.session.PeerIsPersistent {
		return fmt.Errorf("peer is in incognito mode and cannot be saved")
	}
	fp := kd.session.ExpectedPeerFingerprint
	if fp == "" || fp == "TOFU" {
		return fmt.Errorf("no verified peer fingerprint")
	}
	if err := kd.AddressBook.Add(name, fp); err != nil {
		return err
	}
	kd.AddressBook.UpdateLastSeen(fp)
	return kd.AddressBook.Save()
}

// IsPeerPersistent returns whether the currently connected peer has a stable identity.
func (kd *KeibiDrop) IsPeerPersistent() bool {
	if kd.session == nil {
		return false
	}
	return kd.session.PeerIsPersistent
}

// NotifyDisconnect sends a best-effort DISCONNECT notification to the peer
// so they can clean up immediately instead of waiting for health monitor timeout.
func (kd *KeibiDrop) NotifyDisconnect() {
	logger := kd.logger.With("method", "notify-disconnect")
	// A create that is still waiting for its peer has no session yet, so kd.Stop returns
	// early and cannot reach it. The rendezvous loop watches this flag instead.
	kd.connectAborted.Store(true)
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Warn("Skipping disconnect notification: no session or gRPC client")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := kd.sendNotify(ctx, &bindings.NotifyRequest{
		Type: bindings.NotifyType_DISCONNECT,
	})
	if err != nil {
		logger.Warn("Failed to send disconnect notification", "error", err)
	} else {
		logger.Info("Disconnect notification sent successfully")
	}
}

// This is blocking.
func (kd *KeibiDrop) MountFilesystem(toMount string, toSave string, isSecond bool) error {
	logger := kd.logger.With("method", "mount-filesystem")
	logger.Info("Mounting virtual filesystem", "virtual-folder", toMount, "passhtrough-folder", toSave, "isSecond", isSecond)
	if kd.session == nil || kd.KDSvc == nil {
		logger.Warn("Session not established", "error", ErrSessionNotEstablished)
		return ErrSessionNotEstablished
	}

	if kd.FS != nil {
		logger.Warn("Filesystem already mounted", "error", ErrFilesystemAlreadyMounted)
		return ErrFilesystemAlreadyMounted
	}

	fs := filesystem.NewFS(logger)
	fs.OnRootReady = kd.BackfillRemoteFilesIntoFS // Announces that beat the mount sit in the tracker.
	kd.KDSvc.SetFS(fs)

	if err := fs.Mount(filepath.Clean(toMount), isSecond, filepath.Clean(toSave)); err != nil {
		logger.Error("Filesystem mount failed", "error", err)
		return err
	}
	return nil
}

func (kd *KeibiDrop) UnmountFilesystem() error {
	logger := kd.logger.With("method", "unmonut-filesystem")
	logger.Info("Unmounting virtual filesystem")

	kd.StopConnectionResilience()

	kd.mu.Lock()
	fs := kd.FS
	kd.mu.Unlock()
	if fs == nil {
		logger.Warn("Nil filesystem", "error", ErrNilFilesystem)
		return ErrNilFilesystem
	}

	if kd.KDSvc != nil {
		kd.KDSvc.SetFS(nil)
	}

	fs.ClearFiles()

	logger.Info("Success")
	return nil
}
