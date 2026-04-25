// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SanitizeLogs reads a log file and returns sanitized content.
// Redacts file names (keeps extensions), fingerprints, and IP addresses.
// Keeps timestamps, log levels, method names, error types, sizes, and connection events.
func SanitizeLogs(logPath string) (string, error) {
	data, err := os.ReadFile(logPath) // #nosec G304
	if err != nil {
		return "", err
	}
	return SanitizeLogContent(string(data)), nil
}

// SanitizeLogContent sanitizes log text in memory.
func SanitizeLogContent(raw string) string {
	lines := strings.Split(raw, "\n")
	result := make([]string, 0, len(lines))

	for _, line := range lines {
		line = redactPaths(line)
		line = redactFingerprints(line)
		line = redactIPv6(line)
		line = redactIPv4(line)
		line = redactHomedir(line)
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// SanitizeLogsToFile reads a log, sanitizes it, and writes to destPath.
func SanitizeLogsToFile(logPath, destPath string) error {
	sanitized, err := SanitizeLogs(logPath)
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, []byte(sanitized), 0600)
}

var pathKeys = []string{
	"path=", "file=", "localPath=", "remoteName=",
	"cleanPath=", "realPath=", "src=", "dst=",
	"mount=", "save=", "prefetch=",
}

// Safe directory names to keep (not redacted).
var safeNames = map[string]bool{
	"Documents": true, "KeibiDrop": true, "Received": true,
	"Mount": true, "Library": true, "Logs": true, "tmp": true,
	".git": true, "objects": true, "refs": true, "hooks": true,
	"logs": true, "pack": true, "info": true, "heads": true,
	"remotes": true, "origin": true,
}

func redactPaths(line string) string {
	for _, key := range pathKeys {
		idx := strings.Index(line, key)
		if idx < 0 {
			continue
		}
		valueStart := idx + len(key)
		valueEnd := len(line)
		for i := valueStart; i < len(line); i++ {
			if line[i] == ' ' || line[i] == '\t' || line[i] == '"' {
				valueEnd = i
				break
			}
		}
		value := line[valueStart:valueEnd]
		redacted := redactPath(value)
		line = line[:valueStart] + redacted + line[valueEnd:]
	}
	return line
}

func redactPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "" || safeNames[part] || strings.HasPrefix(part, ".") {
			continue
		}
		ext := filepath.Ext(part)
		if ext != "" {
			parts[i] = "<redacted>" + ext
		} else {
			parts[i] = "<redacted>"
		}
	}
	return strings.Join(parts, "/")
}

// Fingerprints: base64url strings 40+ chars (fingerprint codes).
var fingerprintRe = regexp.MustCompile(`[A-Za-z0-9_-]{40,}`)

func redactFingerprints(line string) string {
	return fingerprintRe.ReplaceAllString(line, "<fingerprint-redacted>")
}

// IPv6 addresses: fe80::..., ::1, or full addresses with brackets.
var ipv6Re = regexp.MustCompile(`\[?[0-9a-fA-F:]{4,39}(%[a-zA-Z0-9]+)?\]?(:\d+)?`)

func redactIPv6(line string) string {
	return ipv6Re.ReplaceAllStringFunc(line, func(match string) string {
		// Don't redact short hex that isn't an IP (like error codes).
		if strings.Count(match, ":") < 2 {
			return match
		}
		return "<ip-redacted>"
	})
}

// IPv4 addresses.
var ipv4Re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b`)

func redactIPv4(line string) string {
	return ipv4Re.ReplaceAllString(line, "<ip-redacted>")
}

func redactHomedir(line string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return line
	}
	return strings.ReplaceAll(line, home, "<home>")
}
