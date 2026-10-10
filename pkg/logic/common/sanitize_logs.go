// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"io"
	"os"
	"strings"

	"github.com/KeibiSoft/KeibiDrop/pkg/logsanitize"
)

// SanitizeLogs reads a log file and returns sanitized content. It redacts file
// names (extensions stay), device names, relay tokens, fingerprints, IP and
// MAC addresses, and the account name. It keeps timestamps, log levels, method
// names, error types, sizes, and connection events.
func SanitizeLogs(logPath string) (string, error) {
	data, err := os.ReadFile(logPath) // #nosec G304
	if err != nil {
		return "", err
	}
	return SanitizeLogContent(string(data)), nil
}

// SanitizeLogContent sanitizes log text in memory (pkg/logsanitize).
func SanitizeLogContent(raw string) string {
	return logsanitize.Content(raw)
}

// SanitizeLogsToFile reads a log, sanitizes it, and writes to destPath.
func SanitizeLogsToFile(logPath, destPath string) error {
	sanitized, err := SanitizeLogs(logPath)
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, []byte(sanitized), 0600)
}

// SanitizedLogTail returns the newest max bytes of the log at logPath,
// sanitized, from whole lines. When the current file is shorter it starts in
// the file rotated before it (logPath.1, config.OpenLogFile).
func SanitizedLogTail(logPath string, max int) (string, error) {
	text, err := tailLines(logPath, max)
	if err != nil {
		return "", err
	}
	if len(text) < max {
		if prev, err := tailLines(logPath+".1", max-len(text)); err == nil {
			text = prev + text
		}
	}
	return newestLines(SanitizeLogContent(text), max), nil
}

// newestLines returns the last n bytes of s, from the first line start in them.
func newestLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// tailLines reads the last n bytes of the file at path, from a line start.
func tailLines(path string, n int) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the app's own log file
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := st.Size() - int64(n)
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return "", err
	}
	s := string(buf)
	// The cut may land inside a record: start at the next line.
	if off > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, nil
}
