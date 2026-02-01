// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zeebo/xxh3"
)

// ConflictResolver handles file conflicts when both peers modify the same file.
type ConflictResolver struct {
	Strategy ConflictResolution
	BaseDir  string // Local filesystem base directory
}

// NewConflictResolver creates a resolver with KeepBoth as default strategy.
func NewConflictResolver(baseDir string) *ConflictResolver {
	return &ConflictResolver{
		Strategy: KeepBoth,
		BaseDir:  baseDir,
	}
}

// DetectConflict checks if there's a conflict between local and remote versions.
// Returns nil if no conflict (identical content or one is clearly newer).
func (r *ConflictResolver) DetectConflict(path string, localVer, remoteVer *FileVersion) *Conflict {
	if localVer == nil || remoteVer == nil {
		return nil
	}

	// Same content = no conflict
	if localVer.Checksum == remoteVer.Checksum {
		return nil
	}

	// Both modified = conflict
	return &Conflict{
		Path:          path,
		LocalVersion:  localVer,
		RemoteVersion: remoteVer,
		DetectedAt:    time.Now(),
	}
}

// Resolve handles a conflict according to the configured strategy.
// Returns the path where the local version was moved (for KeepBoth) or empty string.
func (r *ConflictResolver) Resolve(conflict *Conflict) (conflictPath string, err error) {
	if conflict == nil {
		return "", nil
	}

	switch r.Strategy {
	case KeepBoth:
		return r.resolveKeepBoth(conflict)
	case LastWriteWins:
		return r.resolveLastWriteWins(conflict)
	case AskUser:
		// For now, fall back to KeepBoth (UI integration would handle this)
		return r.resolveKeepBoth(conflict)
	default:
		return r.resolveKeepBoth(conflict)
	}
}

// resolveKeepBoth renames the local file with a .conflict.TIMESTAMP suffix.
// The remote version will be written to the original path.
func (r *ConflictResolver) resolveKeepBoth(conflict *Conflict) (string, error) {
	localPath := filepath.Join(r.BaseDir, conflict.Path)

	// Check if local file exists
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		return "", nil // No local file to conflict with
	}

	// Generate conflict filename
	timestamp := time.Now().Format("20060102-150405")
	ext := filepath.Ext(conflict.Path)
	base := conflict.Path[:len(conflict.Path)-len(ext)]
	conflictName := fmt.Sprintf("%s.conflict.%s%s", base, timestamp, ext)
	conflictPath := filepath.Join(r.BaseDir, conflictName)

	// Rename local file to conflict name
	if err := os.Rename(localPath, conflictPath); err != nil {
		return "", fmt.Errorf("failed to rename local file: %w", err)
	}

	return conflictName, nil
}

// resolveLastWriteWins compares mtimes and keeps the newer version.
// Returns empty string if remote wins (no rename needed).
func (r *ConflictResolver) resolveLastWriteWins(conflict *Conflict) (string, error) {
	// Remote wins if it has higher mtime
	if conflict.RemoteVersion.Mtime >= conflict.LocalVersion.Mtime {
		// Delete local, remote will overwrite
		localPath := filepath.Join(r.BaseDir, conflict.Path)
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to remove local file: %w", err)
		}
		return "", nil
	}

	// Local wins - remote version should be discarded
	// Return special marker to indicate local should be kept
	return "LOCAL_WINS", nil
}

// GetLocalVersion reads the current local file version info.
func (r *ConflictResolver) GetLocalVersion(path string) (*FileVersion, error) {
	fullPath := filepath.Join(r.BaseDir, path)

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Read file for checksum
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, err
	}

	return &FileVersion{
		Path:       path,
		Size:       info.Size(),
		Mtime:      info.ModTime().UnixNano(),
		Checksum:   xxh3.Hash(data),
		SourcePeer: "local",
	}, nil
}

// ListConflicts returns all .conflict files in the base directory.
func (r *ConflictResolver) ListConflicts() ([]string, error) {
	var conflicts []string

	err := filepath.Walk(r.BaseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			return nil
		}

		// Check for .conflict. in filename
		relPath, _ := filepath.Rel(r.BaseDir, path)
		if matched, _ := filepath.Match("*.conflict.*", filepath.Base(path)); matched {
			conflicts = append(conflicts, relPath)
		}
		return nil
	})

	return conflicts, err
}

// CleanupConflict removes a conflict file (after user resolves it).
func (r *ConflictResolver) CleanupConflict(conflictPath string) error {
	return os.Remove(filepath.Join(r.BaseDir, conflictPath))
}
