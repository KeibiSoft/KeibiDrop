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

// DetectConflict reports a conflict between the local and remote versions.
// It returns nil when either version is nil or the checksums match.
func (r *ConflictResolver) DetectConflict(path string, localVer, remoteVer *FileVersion) *Conflict {
	if localVer == nil || remoteVer == nil {
		return nil
	}

	if localVer.Checksum == remoteVer.Checksum {
		return nil
	}

	return &Conflict{
		Path:          path,
		LocalVersion:  localVer,
		RemoteVersion: remoteVer,
		DetectedAt:    time.Now(),
	}
}

// Resolve applies the configured strategy to a conflict.
// It returns the path where the local version moved (KeepBoth), or an empty string.
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
		// UI integration is absent; fall back to KeepBoth.
		return r.resolveKeepBoth(conflict)
	default:
		return r.resolveKeepBoth(conflict)
	}
}

// resolveKeepBoth renames the local file with a .conflict.TIMESTAMP suffix.
// The remote version then lands at the original path.
func (r *ConflictResolver) resolveKeepBoth(conflict *Conflict) (string, error) {
	localPath := filepath.Join(r.BaseDir, conflict.Path)

	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		return "", nil // No local file to conflict with.
	}

	timestamp := time.Now().Format("20060102-150405")
	ext := filepath.Ext(conflict.Path)
	base := conflict.Path[:len(conflict.Path)-len(ext)]
	conflictName := fmt.Sprintf("%s.conflict.%s%s", base, timestamp, ext)
	conflictPath := filepath.Join(r.BaseDir, conflictName)

	if err := os.Rename(localPath, conflictPath); err != nil {
		return "", fmt.Errorf("failed to rename local file: %w", err)
	}

	return conflictName, nil
}

// resolveLastWriteWins keeps the version with the newer mtime.
// It returns an empty string when the remote wins.
func (r *ConflictResolver) resolveLastWriteWins(conflict *Conflict) (string, error) {
	// The remote wins ties.
	if conflict.RemoteVersion.Mtime >= conflict.LocalVersion.Mtime {
		// Remove the local file; the remote version overwrites it.
		localPath := filepath.Join(r.BaseDir, conflict.Path)
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to remove local file: %w", err)
		}
		return "", nil
	}

	// The marker tells the caller to keep the local file and discard the remote.
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

	// The checksum needs the full file content.
	data, err := os.ReadFile(fullPath) // #nosec G304
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
			return nil // Skip errors.
		}
		if info.IsDir() {
			return nil
		}

		relPath, _ := filepath.Rel(r.BaseDir, path)
		if matched, _ := filepath.Match("*.conflict.*", filepath.Base(path)); matched {
			conflicts = append(conflicts, relPath)
		}
		return nil
	})

	return conflicts, err
}

// CleanupConflict removes a conflict file after the user resolves it.
func (r *ConflictResolver) CleanupConflict(conflictPath string) error {
	return os.Remove(filepath.Join(r.BaseDir, conflictPath))
}
