// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package queue

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"
)

// WriteQueue persists pending filesystem operations to survive crashes.
// When the peer is disconnected, operations are queued locally and
// replayed when the connection is restored.
//
// Storage format: One JSON file per operation in a directory.
// Files are named: {id}_{timestamp}.json
type WriteQueue struct {
	mu        sync.Mutex
	dir       string // Directory for persisted operations
	ops       []*QueuedOperation
	enabled   atomic.Bool // Queueing enabled when disconnected
	nextID    atomic.Uint64
	sessionID string
	logger    *slog.Logger

	// Configuration
	MaxOps   int   // Maximum queued operations (default 1000)
	MaxBytes int64 // Maximum queued data bytes (default 100 MB)

	// Statistics
	totalEnqueued atomic.Uint64
	totalReplayed atomic.Uint64
	totalFailed   atomic.Uint64
}

// persistedOp is the JSON structure for a persisted operation.
type persistedOp struct {
	ID        uint64 `json:"id"`
	SessionID string `json:"session_id"`
	Type      int    `json:"type"`
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Data      string `json:"data,omitempty"` // base64 encoded
	Offset    int64  `json:"offset,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	Mtime     int64  `json:"mtime,omitempty"`
	Checksum  uint64 `json:"checksum"`
	CreatedAt int64  `json:"created_at"`
	Retries   int    `json:"retries"`
}

// NewWriteQueue creates a new write queue with file-based persistence.
// Operations are stored as JSON files in the specified directory.
func NewWriteQueue(dir string, sessionID string, logger *slog.Logger) (*WriteQueue, error) {
	// Ensure directory exists
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create queue directory: %w", err)
	}

	wq := &WriteQueue{
		dir:       dir,
		ops:       make([]*QueuedOperation, 0),
		sessionID: sessionID,
		logger:    logger.With("component", "write-queue"),
		MaxOps:    1000,
		MaxBytes:  100 * 1024 * 1024, // 100 MB
	}

	// Load any pending operations from previous session/crash
	if err := wq.loadPendingOps(); err != nil {
		logger.Warn("Failed to load pending ops", "error", err)
	}

	return wq, nil
}

// Close is a no-op for file-based storage (for interface compatibility).
func (q *WriteQueue) Close() error {
	return nil
}

// EnableQueueing enables operation queueing (call when disconnected).
func (q *WriteQueue) EnableQueueing() {
	q.enabled.Store(true)
	q.logger.Info("Write queueing enabled")
}

// DisableQueueing disables operation queueing (call when connected).
func (q *WriteQueue) DisableQueueing() {
	q.enabled.Store(false)
	q.logger.Info("Write queueing disabled")
}

// IsEnabled returns true if queueing is currently enabled.
func (q *WriteQueue) IsEnabled() bool {
	return q.enabled.Load()
}

// Count returns the number of pending operations.
func (q *WriteQueue) Count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.ops)
}

// TotalBytes returns the total size of queued data.
func (q *WriteQueue) TotalBytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.totalBytes()
}

func (q *WriteQueue) totalBytes() int64 {
	var total int64
	for _, op := range q.ops {
		total += int64(len(op.Data))
	}
	return total
}

// Enqueue adds an operation to the queue.
// Returns nil if queueing is disabled (operation should be sent directly).
func (q *WriteQueue) Enqueue(op *QueuedOperation) error {
	if !q.enabled.Load() {
		return nil // Pass-through when connected
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// Check limits
	if len(q.ops) >= q.MaxOps {
		return fmt.Errorf("queue full: %d operations", q.MaxOps)
	}

	totalBytes := q.totalBytes()
	dataSize := int64(len(op.Data))
	if totalBytes+dataSize > q.MaxBytes {
		return fmt.Errorf("queue bytes exceeded: %d + %d > %d", totalBytes, dataSize, q.MaxBytes)
	}

	// Assign ID and compute checksum
	op.ID = q.nextID.Add(1)
	if len(op.Data) > 0 {
		op.Checksum = xxh3.Hash(op.Data)
	}
	op.CreatedAt = time.Now()

	// Persist to file
	if err := q.persistOp(op); err != nil {
		return fmt.Errorf("failed to persist operation: %w", err)
	}

	q.ops = append(q.ops, op)
	q.totalEnqueued.Add(1)

	q.logger.Debug("Operation enqueued",
		"id", op.ID,
		"type", op.Type,
		"path", op.Path,
		"size", len(op.Data))

	return nil
}

// EnqueueWrite is a convenience method for write operations.
func (q *WriteQueue) EnqueueWrite(path string, data []byte, offset int64) error {
	return q.Enqueue(&QueuedOperation{
		Type:   OpWrite,
		Path:   path,
		Data:   data,
		Offset: offset,
	})
}

// EnqueueCreate is a convenience method for file creation.
func (q *WriteQueue) EnqueueCreate(path string, size int64, mode uint32, mtime int64) error {
	return q.Enqueue(&QueuedOperation{
		Type:  OpCreate,
		Path:  path,
		Size:  size,
		Mode:  mode,
		Mtime: mtime,
	})
}

// EnqueueDelete is a convenience method for file deletion.
func (q *WriteQueue) EnqueueDelete(path string) error {
	return q.Enqueue(&QueuedOperation{
		Type: OpDelete,
		Path: path,
	})
}

// EnqueueRename is a convenience method for file rename.
func (q *WriteQueue) EnqueueRename(oldPath, newPath string) error {
	return q.Enqueue(&QueuedOperation{
		Type:    OpRename,
		Path:    newPath,
		OldPath: oldPath,
	})
}

// ReplayFunc is called for each operation during flush.
// Returns nil on success, error on failure.
type ReplayFunc func(op *QueuedOperation) error

// Flush replays all queued operations using the provided replay function.
// Successfully replayed operations are removed from the queue.
// Failed operations are retried up to 3 times before being marked as conflicts.
func (q *WriteQueue) Flush(replay ReplayFunc) []error {
	q.mu.Lock()
	ops := make([]*QueuedOperation, len(q.ops))
	copy(ops, q.ops)
	q.mu.Unlock()

	if len(ops) == 0 {
		return nil
	}

	q.logger.Info("Flushing write queue", "count", len(ops))

	// Sort by timestamp to maintain ordering
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].CreatedAt.Before(ops[j].CreatedAt)
	})

	var errors []error
	var succeeded []uint64

	for _, op := range ops {
		// Verify data integrity
		if len(op.Data) > 0 {
			checksum := xxh3.Hash(op.Data)
			if checksum != op.Checksum {
				q.logger.Error("Data corruption detected",
					"id", op.ID,
					"path", op.Path,
					"expected", op.Checksum,
					"got", checksum)
				errors = append(errors, fmt.Errorf("op %d (%s %s): data corrupted", op.ID, op.Type, op.Path))
				continue
			}
		}

		if err := replay(op); err != nil {
			op.Retries++
			q.updateRetryCount(op)

			if op.Retries > 3 {
				q.logger.Error("Operation failed after max retries",
					"id", op.ID,
					"type", op.Type,
					"path", op.Path,
					"error", err)
				q.totalFailed.Add(1)
				// Move to conflict handling (remove from queue)
				succeeded = append(succeeded, op.ID)
			}

			errors = append(errors, fmt.Errorf("op %d (%s %s): %w", op.ID, op.Type, op.Path, err))
			continue
		}

		succeeded = append(succeeded, op.ID)
		q.totalReplayed.Add(1)
		q.logger.Debug("Operation replayed", "id", op.ID, "type", op.Type, "path", op.Path)
	}

	// Remove succeeded operations
	q.removeOps(succeeded)

	q.logger.Info("Flush complete",
		"replayed", len(succeeded),
		"failed", len(errors),
		"remaining", q.Count())

	return errors
}

// Clear removes all queued operations (use with caution).
func (q *WriteQueue) Clear() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Remove all operation files
	for _, op := range q.ops {
		q.deleteOpFile(op.ID)
	}

	q.ops = make([]*QueuedOperation, 0)
	return nil
}

// Stats returns queue statistics.
func (q *WriteQueue) Stats() (enqueued, replayed, failed uint64) {
	return q.totalEnqueued.Load(), q.totalReplayed.Load(), q.totalFailed.Load()
}

func (q *WriteQueue) opFilename(id uint64) string {
	return filepath.Join(q.dir, fmt.Sprintf("%d.json", id))
}

func (q *WriteQueue) persistOp(op *QueuedOperation) error {
	p := persistedOp{
		ID:        op.ID,
		SessionID: q.sessionID,
		Type:      int(op.Type),
		Path:      op.Path,
		OldPath:   op.OldPath,
		Offset:    op.Offset,
		Size:      op.Size,
		Mode:      op.Mode,
		Mtime:     op.Mtime,
		Checksum:  op.Checksum,
		CreatedAt: op.CreatedAt.UnixNano(),
		Retries:   op.Retries,
	}

	if len(op.Data) > 0 {
		p.Data = base64.StdEncoding.EncodeToString(op.Data)
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	// Write to temp file first, then rename (atomic)
	tmpPath := q.opFilename(op.ID) + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}

	return os.Rename(tmpPath, q.opFilename(op.ID))
}

func (q *WriteQueue) loadPendingOps() error {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return err
	}

	var maxID uint64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		// Skip temp files
		if strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(q.dir, entry.Name()))
		if err != nil {
			q.logger.Warn("Failed to read operation file", "file", entry.Name(), "error", err)
			continue
		}

		var p persistedOp
		if err := json.Unmarshal(data, &p); err != nil {
			q.logger.Warn("Failed to parse operation file", "file", entry.Name(), "error", err)
			continue
		}

		// Only load operations from this session
		if p.SessionID != q.sessionID {
			continue
		}

		op := &QueuedOperation{
			ID:        p.ID,
			Type:      OperationType(p.Type),
			Path:      p.Path,
			OldPath:   p.OldPath,
			Offset:    p.Offset,
			Size:      p.Size,
			Mode:      p.Mode,
			Mtime:     p.Mtime,
			Checksum:  p.Checksum,
			CreatedAt: time.Unix(0, p.CreatedAt),
			Retries:   p.Retries,
		}

		if p.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				q.logger.Warn("Failed to decode operation data", "id", p.ID, "error", err)
				continue
			}
			op.Data = decoded

			// Verify integrity
			if xxh3.Hash(op.Data) != op.Checksum {
				q.logger.Warn("Skipping corrupted operation", "id", op.ID, "path", op.Path)
				continue
			}
		}

		q.ops = append(q.ops, op)
		if op.ID > maxID {
			maxID = op.ID
		}
	}

	q.nextID.Store(maxID)

	// Sort by creation time
	sort.Slice(q.ops, func(i, j int) bool {
		return q.ops[i].CreatedAt.Before(q.ops[j].CreatedAt)
	})

	if len(q.ops) > 0 {
		q.logger.Info("Loaded pending operations from previous session", "count", len(q.ops))
	}

	return nil
}

func (q *WriteQueue) removeOps(ids []uint64) {
	if len(ids) == 0 {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// Remove from memory
	idSet := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}

	newOps := make([]*QueuedOperation, 0, len(q.ops))
	for _, op := range q.ops {
		if _, found := idSet[op.ID]; !found {
			newOps = append(newOps, op)
		}
	}
	q.ops = newOps

	// Remove files
	for _, id := range ids {
		q.deleteOpFile(id)
	}
}

func (q *WriteQueue) deleteOpFile(id uint64) {
	os.Remove(q.opFilename(id))
}

func (q *WriteQueue) updateRetryCount(op *QueuedOperation) {
	// Re-persist with updated retry count
	q.persistOp(op)
}

// parseOpID extracts the operation ID from a filename like "123.json"
func parseOpID(filename string) (uint64, bool) {
	name := strings.TrimSuffix(filename, ".json")
	id, err := strconv.ParseUint(name, 10, 64)
	return id, err == nil
}
