// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: OpenLogFile is the one way a binary opens its log: append mode,
// ABOUTME: rotated in place past a size cap so a long-running install stays bounded.

package config

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

// LogRotateBytes caps one log file. Past it the file becomes <path>.1, replacing
// the previous .1, and a fresh file starts, so an install keeps at most two of
// them. Nothing trimmed the log before: five months of the desktop app at debug
// level had reached 151 MB in one file (2026-09-15).
const LogRotateBytes = 32 << 20

// OpenLogFile opens path for appending and rotates it as it grows. Every binary
// that writes a log file opens it here.
func OpenLogFile(path string) (io.WriteCloser, error) {
	return openRotatingLog(filepath.Clean(path), LogRotateBytes)
}

type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotatingLog(path string, max int64) (*rotatingLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	r := &rotatingLog{path: path, max: max, f: f}
	if st, err := f.Stat(); err == nil {
		r.size = st.Size()
	}
	return r, nil
}

// Write appends p, starting a fresh file first when p would carry the current
// one past the cap. A record never straddles two files.
func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		r.rotateLocked()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotateLocked moves the current file to .1 and opens a fresh one. The rename
// runs with the file still open: the open handle follows the file, so nothing
// written in between is lost. Where the rename is refused (another process
// holds the file on Windows) the log stays where it is and appending goes on,
// since a large file beats lost lines.
func (r *rotatingLog) rotateLocked() {
	old := r.path + ".1"
	_ = os.Remove(old) // Rename does not replace on Windows.
	if err := os.Rename(r.path, old); err != nil {
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return // the old handle keeps writing into .1
	}
	_ = r.f.Close()
	r.f = f
	r.size = 0
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
