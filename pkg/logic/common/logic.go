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

const Timeout = 10*60 - 5

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

	file := &synctracker.File{
		Name:           name,
		RelativePath:   name,
		RealPathOfFile: cleanPath,
		Size:           uint64(finfo.Size()),
		LastEditTime:   uint64(finfo.ModTime().UnixNano()),
		CreatedTime:    uint64(finfo.ModTime().UnixNano()),
	}

	kd.SyncTracker.LocalFilesMu.Lock()
	defer kd.SyncTracker.LocalFilesMu.Unlock()
	kd.SyncTracker.LocalFiles[name] = file // upsert: allows retry after failed notification

	_, err = kd.session.GRPCClient.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.AddFile),
		Path: file.RelativePath,
		Attr: &bindings.Attr{
			Dev:              0,
			Ino:              0,
			Mode:             uint32(finfo.Mode().Perm()) | syscall.S_IFREG,
			Size:             finfo.Size(),
			AccessTime:       file.LastEditTime,
			ModificationTime: file.LastEditTime,
			ChangeTime:       file.LastEditTime,
			BirthTime:        file.LastEditTime,
			Flags:            0o444,
		},
	})
	if err != nil {
		logger.Error("Failed to notify peer", "error", err)
		return err
	}

	logger.Info("Success")

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

	file := &synctracker.File{
		Name:           filepath.Base(remoteName),
		RelativePath:   remoteName,
		RealPathOfFile: cleanPath,
		Size:           uint64(finfo.Size()),
		LastEditTime:   uint64(finfo.ModTime().UnixNano()),
		CreatedTime:    uint64(finfo.ModTime().UnixNano()),
	}

	kd.SyncTracker.LocalFilesMu.Lock()
	defer kd.SyncTracker.LocalFilesMu.Unlock()
	kd.SyncTracker.LocalFiles[remoteName] = file

	_, err = kd.session.GRPCClient.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.AddFile),
		Path: remoteName,
		Attr: &bindings.Attr{
			Mode:             uint32(finfo.Mode().Perm()) | syscall.S_IFREG,
			Size:             finfo.Size(),
			AccessTime:       file.LastEditTime,
			ModificationTime: file.LastEditTime,
			ChangeTime:       file.LastEditTime,
			BirthTime:        file.LastEditTime,
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
			f, err = os.OpenFile(localPath, os.O_WRONLY, 0644)
			if err != nil {
				logger.Error("Failed to open partial file for resume", "error", err)
				return err
			}
			logger.Info("Resuming download", "progress", bitmap.Progress(), "have", bitmap.Have(), "total", bitmap.Total())
		}
	}

	if bitmap == nil {
		// Fresh download.
		f, err = os.Create(localPath)
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
		dlCtx, dlCancel := context.WithCancel(kd.ctx)
		defer dlCancel()
		kd.registerDownload(remoteName, dlCancel)
		defer kd.unregisterDownload(remoteName)

		if err := kd.pullParallelRead(dlCtx, dlCancel, bitmap, f, relPath, fileSize, config.BlockSize, bitmapPath, logger); err != nil {
			return err
		}
		_ = bitmap.Save(bitmapPath)
	}

	// Download complete. Clean up bitmap file.
	os.Remove(bitmapPath)

updateTracker:
	fileCopy.RealPathOfFile = localPath

	kd.SyncTracker.RemoteFilesMu.Lock()
	if rf, ok := kd.SyncTracker.RemoteFiles[remoteName]; ok {
		rf.RealPathOfFile = localPath
	}
	kd.SyncTracker.RemoteFilesMu.Unlock()

	kd.SyncTracker.LocalFilesMu.Lock()
	kd.SyncTracker.LocalFiles[localPath] = &fileCopy
	kd.SyncTracker.LocalFilesMu.Unlock()

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
	if kd.session == nil || kd.session.GRPCClient == nil {
		return ErrInvalidSession
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
	f, err := os.Create(localPath)
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

		dlCtx, dlCancel := context.WithCancel(kd.ctx)
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
				stream, err := kd.session.GRPCClient.Read(dlCtx)
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
	fileCopy.RealPathOfFile = localPath
	kd.SyncTracker.RemoteFilesMu.Lock()
	if rf, ok := kd.SyncTracker.RemoteFiles[remoteName]; ok {
		rf.RealPathOfFile = localPath
	}
	kd.SyncTracker.RemoteFilesMu.Unlock()
	kd.SyncTracker.LocalFilesMu.Lock()
	kd.SyncTracker.LocalFiles[localPath] = &fileCopy
	kd.SyncTracker.LocalFilesMu.Unlock()
	return nil
}

func (kd *KeibiDrop) registerDownload(name string, cancel context.CancelFunc) {
	kd.activeDownloadsMu.Lock()
	kd.activeDownloads[name] = cancel
	kd.activeDownloadsMu.Unlock()
}

func (kd *KeibiDrop) unregisterDownload(name string) {
	kd.activeDownloadsMu.Lock()
	delete(kd.activeDownloads, name)
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
	// Check if there's a partial bitmap on disk.
	kd.SyncTracker.RemoteFilesMu.RLock()
	rf, ok := kd.SyncTracker.RemoteFiles[remoteName]
	kd.SyncTracker.RemoteFilesMu.RUnlock()
	if !ok {
		return -1
	}

	// Try loading bitmap from the save path.
	localPath := filepath.Join(kd.ToSave, rf.RelativePath)
	bitmapPath := filesystem.BitmapPath(localPath)
	bm, err := filesystem.LoadChunkBitmap(bitmapPath, int64(rf.Size))
	if err != nil {
		return -1
	}
	return bm.Progress()
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

func (kd *KeibiDrop) JoinRoom() error {
	logger := kd.logger.With("method", "join-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	if kd.running.Load() {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	// Wait for expected peer fingerprint
	elapsed := 0
	for {
		if elapsed >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			elapsed++
			time.Sleep(time.Second)
			continue
		}
		break
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
				case <-kd.ctx.Done():
					return kd.ctx.Err()
				}
			}
		}
		if relayErr != nil {
			return fmt.Errorf("peer not found on relay after %d attempts: %w", relayMaxRetries, relayErr)
		}
	}

	// Connection priority: LAN (2s per addr) → direct IPv6 (15s) → bridge relay.
	{
		// 1. Try LAN addresses first (only in local mode — internet mode skips this).
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
				// LAN connection succeeded — do handshake on this connection.
				if err := session.PerformOutboundHandshakeOnConn(kd.session, conn); err != nil {
					conn.Close()
					logger.Warn("LAN handshake failed", "addr", addr, "error", err)
					continue
				}
				logger.Info("LAN connection succeeded!", "addr", addr)
				kd.PeerIPv6IP = localAddr
				lanConnected = true

				// Accept inbound from peer (LAN, should be fast — 5s timeout).
				_ = kd.listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
				inConn, err := kd.listener.Accept()
				_ = kd.listener.(*net.TCPListener).SetDeadline(time.Time{}) // clear deadline
				if err != nil {
					logger.Warn("LAN inbound accept failed", "error", err)
					// Close outbound, fall through to direct/bridge.
					if kd.session.Session != nil && kd.session.Session.Outbound != nil {
						kd.session.Session.Outbound.Close()
						kd.session.Session.Outbound = nil
					}
					kd.session.SEKOutbound = nil
					kd.session.CipherMu.Lock()
					kd.session.CipherSuite = ""
					kd.session.CipherMu.Unlock()
					lanConnected = false
					break
				}
				if err := session.PerformInboundHandshake(kd.session, inConn); err != nil {
					logger.Warn("LAN inbound handshake failed", "error", err)
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
			kd.session.SEKOutbound = nil
			kd.session.CipherMu.Lock()
			kd.session.CipherSuite = ""
			kd.session.CipherMu.Unlock()
		}

		// 2. Try direct IPv6 P2P (skip if peer has no IPv6, e.g. mobile).
		var directErr error
		if kd.PeerIPv6IP != "" {
			peerAddr := net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(kd.session.PeerPort))
			directErr = session.PerformOutboundHandshake(kd.session, peerAddr)
		} else {
			directErr = fmt.Errorf("peer has no IPv6 address")
		}

		needBridge := false

		switch {
		case directErr == nil:
			// Direct outbound succeeded. Accept inbound with 15s timeout.
			// If the peer can't reach us (firewall blocks inbound IPv6),
			// fall back to bridge for both directions.
			logger.Info("Direct P2P outbound connected, waiting for inbound (15s timeout)")

			type acceptResult struct {
				conn net.Conn
				err  error
			}
			ch := make(chan acceptResult, 1)
			go func() {
				c, e := kd.listener.Accept()
				ch <- acceptResult{c, e}
			}()

			select {
			case res := <-ch:
				if res.err != nil {
					logger.Warn("Inbound accept failed", "error", res.err)
					needBridge = kd.BridgeAddr != ""
					if !needBridge {
						return res.err
					}
				} else {
					if err := session.PerformInboundHandshake(kd.session, res.conn); err != nil {
						logger.Error("Failed inbound handshake", "error", err)
						return err
					}
					kd.ConnectionMode = "direct"
					if kd.OnEvent != nil {
						kd.OnEvent("connection_mode:direct")
					}
				}
			case <-time.After(15 * time.Second):
				logger.Warn("Inbound accept timed out (15s), peer likely behind firewall")
				needBridge = kd.BridgeAddr != ""
				if !needBridge {
					return fmt.Errorf("inbound accept timed out and no bridge configured")
				}
			}

			if needBridge {
				logger.Info("Falling back to bridge for both directions")
				// Close the direct outbound — we'll redo both via bridge.
				if kd.session.Session != nil && kd.session.Session.Outbound != nil {
					kd.session.Session.Outbound.Close()
					kd.session.Session.Outbound = nil
				}
				kd.session.SEKOutbound = nil
				kd.session.CipherMu.Lock()
				kd.session.CipherSuite = ""
				kd.session.CipherMu.Unlock()
			}
		case kd.BridgeAddr != "":
			logger.Warn("Direct P2P failed, falling back to bridge", "error", directErr, "bridge", kd.BridgeAddr)
			needBridge = true
			kd.session.SEKOutbound = nil
			kd.session.CipherMu.Lock()
			kd.session.CipherSuite = ""
			kd.session.CipherMu.Unlock()
		default:
			logger.Error("Direct P2P failed and no bridge configured", "error", directErr)
			return directErr
		}

		if needBridge {
			outConn, err := kd.dialBridge(logger)
			if err != nil {
				return fmt.Errorf("bridge dial (outbound): %w", err)
			}
			if err := session.PerformOutboundHandshakeOnConn(kd.session, outConn); err != nil {
				outConn.Close()
				return fmt.Errorf("bridge outbound handshake: %w", err)
			}

			inConn, err := kd.dialBridge(logger)
			if err != nil {
				return fmt.Errorf("bridge dial (inbound): %w", err)
			}
			if err := session.PerformInboundHandshake(kd.session, inConn); err != nil {
				inConn.Close()
				return fmt.Errorf("bridge inbound handshake: %w", err)
			}
			kd.ConnectionMode = "bridge"
			if kd.OnEvent != nil {
				kd.OnEvent("connection_mode:bridge")
			}
		}
	}

connected:
	kd.filesystemReady = make(chan struct{})
	kd.filesystemReadyOnce = sync.Once{}
	kd.Start()

	// retry dialing until gRPC server is ready
	if err := kd.connectGRPCClientWithRetry(15 * time.Second); err != nil {
		logger.Error("Failed to connect to grpc server after retries", "error", err)
		return err
	}

	// Start health monitoring, reconnection, and relay keepalive.
	if err := kd.InitConnectionResilience(); err != nil {
		logger.Warn("Failed to init connection resilience", "error", err)
	}

	if !kd.IsFUSE {
		// Unblock Run()'s <-filesystemReady so it can process signals.
		kd.filesystemReadyOnce.Do(func() { close(kd.filesystemReady) })
		logger.Info("Success, starting without FUSE")
		return nil
	}

	if err := kd.setupFilesystem(logger, kd.filesystemReady); err != nil {
		return err
	}

	logger.Info("Success")
	return nil
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

func (kd *KeibiDrop) CreateRoom() error {
	logger := kd.logger.With("method", "create-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}
	if kd.running.Load() {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	if !kd.IsLocalMode {
		if err := kd.registerRoomToRelay(); err != nil {
			return err
		}
	}

	logger.Info("Waiting for peer to join...")

	// Wait for expected peer fingerprint (skip if already set, e.g. local mode TOFU).
	elapsed := 0
	for {
		if elapsed >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			time.Sleep(time.Second)
			elapsed++
			continue
		}
		break
	}

	// In local mode, exchange public keys before the PQC handshake.
	if kd.IsLocalMode {
		keyConn, err := kd.listener.Accept()
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

	// Try direct P2P: accept inbound with timeout. If no peer arrives and
	// bridge is configured, fall back to bridge relay.
	{
		useBridge := false

		// Set accept deadline: 15s if bridge available (fallback), no deadline otherwise.
		if kd.BridgeAddr != "" {
			_ = kd.listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second))
		}

		conn, acceptErr := kd.listener.Accept()
		if kd.BridgeAddr != "" {
			_ = kd.listener.(*net.TCPListener).SetDeadline(time.Time{}) // clear deadline
		}

		if acceptErr != nil {
			if kd.BridgeAddr != "" {
				logger.Warn("Direct P2P accept timed out, falling back to bridge", "error", acceptErr)
				useBridge = true
			} else {
				logger.Error("Failed to accept", "error", acceptErr)
				return acceptErr
			}
		}

		if !useBridge {
			// Direct inbound succeeded.
			if err := session.PerformInboundHandshake(kd.session, conn); err != nil {
				return err
			}

			addr, ok := conn.RemoteAddr().(*net.TCPAddr)
			if !ok {
				return fmt.Errorf("failed to cast TCP address")
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
					kd.session.SEKOutbound = nil
					kd.session.CipherMu.Lock()
					kd.session.CipherSuite = ""
					kd.session.CipherMu.Unlock()
					// Close direct inbound too — we need both via bridge.
					if kd.session.Session != nil && kd.session.Session.Inbound != nil {
						kd.session.Session.Inbound.Close()
						kd.session.Session.Inbound = nil
					}
					kd.session.SEKInbound = nil
					useBridge = true
				} else {
					return err
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

			inConn, err := kd.dialBridge(logger)
			if err != nil {
				return fmt.Errorf("bridge dial (inbound): %w", err)
			}
			if err := session.PerformInboundHandshake(kd.session, inConn); err != nil {
				inConn.Close()
				return fmt.Errorf("bridge inbound handshake: %w", err)
			}

			outConn, err := kd.dialBridge(logger)
			if err != nil {
				return fmt.Errorf("bridge dial (outbound): %w", err)
			}
			if err := session.PerformOutboundHandshakeOnConn(kd.session, outConn); err != nil {
				outConn.Close()
				return fmt.Errorf("bridge outbound handshake: %w", err)
			}
		}
	}

	kd.filesystemReady = make(chan struct{})
	kd.filesystemReadyOnce = sync.Once{}
	kd.Start()

	if err := kd.connectGRPCClientWithRetry(15 * time.Second); err != nil {
		logger.Error("Failed to connect to grpc server after retries", "error", err)
		return err
	}

	if err := kd.InitConnectionResilience(); err != nil {
		logger.Warn("Failed to init connection resilience", "error", err)
	}

	if !kd.IsFUSE {
		kd.filesystemReadyOnce.Do(func() { close(kd.filesystemReady) })
		logger.Info("Success, starting without FUSE")
		return nil
	}

	if err := kd.setupFilesystem(logger, kd.filesystemReady); err != nil {
		return err
	}

	logger.Info("Success")
	return nil
}

// NotifyDisconnect sends a best-effort DISCONNECT notification to the peer
// so they can clean up immediately instead of waiting for health monitor timeout.
func (kd *KeibiDrop) NotifyDisconnect() {
	logger := kd.logger.With("method", "notify-disconnect")
	if kd.session == nil || kd.session.GRPCClient == nil {
		logger.Warn("Skipping disconnect notification: no session or gRPC client")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := kd.session.GRPCClient.Notify(ctx, &bindings.NotifyRequest{
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
	kd.KDSvc.FS = fs

	if err := fs.Mount(filepath.Clean(toMount), isSecond, filepath.Clean(toSave)); err != nil {
		logger.Error("Filesystem mount failed", "error", err)
		return err
	}
	return nil
}

func (kd *KeibiDrop) UnmountFilesystem() error {
	logger := kd.logger.With("method", "unmonut-filesystem")
	logger.Info("Unmounting virtual filesystem")
	if kd.FS == nil {
		logger.Warn("Nil filesystem", "error", ErrNilFilesystem)
		return ErrNilFilesystem
	}

	if kd.KDSvc != nil && kd.KDSvc.FS != nil {
		kd.KDSvc.FS = nil
	}

	kd.FS.Unmount()
	kd.FS = nil

	logger.Info("Success")
	return nil
}

/*
	func (kd *KeibiDrop) ResetSession() {
		kd.logger.Info("Resetting session state")

		// You probably want to close any existing net.Conn, etc.
	}

	func (kd *KeibiDrop) RegenerateKeys() error {
		kd.logger.Info("Regenerating ephemeral keys")

		return nil
	}
*/
