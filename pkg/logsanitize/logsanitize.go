// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// Package logsanitize takes personal data out of kd log text. It redacts file
// names (extensions stay), device names, relay tokens, fingerprints, IP and MAC
// addresses, and the account name. It keeps timestamps, log levels, method
// names, error types, sizes, and connection events. It uses the standard
// library only, so the browser peer applies the same rules as the desktop.
package logsanitize

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Content sanitizes log text in memory.
func Content(raw string) string {
	user := userRedactor()
	lines := strings.Split(raw, "\n")
	result := make([]string, 0, len(lines))

	for _, line := range lines {
		line = redactValues(line)
		line = redactFingerprints(line)
		line = redactIPv4(line)
		line = redactIPv6(line)
		line = user(line)
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// pathKeys hold paths: every name in them goes, extensions stay.
var pathKeys = map[string]bool{
	"path": true, "file": true, "localPath": true, "remoteName": true,
	"cleanPath": true, "realPath": true, "src": true, "dst": true,
	"mount": true, "save": true, "prefetch": true, "oldPath": true,
	"newPath": true, "mountPoint": true, "configDir": true, "relPath": true,
}

// maybePathKeys hold a path only sometimes, a state or a direction otherwise:
// their value is redacted as a path when it has a separator in it.
var maybePathKeys = map[string]bool{
	"from": true, "to": true, "old": true, "new": true, "dir": true,
	"base": true, "target": true,
}

// secretKeys lose their whole value: device names and relay credit tokens.
var secretKeys = map[string]bool{"name": true, "hostname": true, "token": true}

// keyValueRe matches one key=value of a slog text line. A quoted value runs
// to its closing quote, escapes included; a bare value runs to a space.
var keyValueRe = regexp.MustCompile(`(^|[ \t])([A-Za-z_][A-Za-z0-9_]*)=("(?:[^"\\]|\\.)*"|[^ \t"]*)`)

// safeNames lists directory names that redaction keeps.
var safeNames = map[string]bool{
	"Documents": true, "KeibiDrop": true, "Received": true,
	"Mount": true, "Library": true, "Logs": true, "tmp": true,
	".git": true, "objects": true, "refs": true, "hooks": true,
	"logs": true, "pack": true, "info": true, "heads": true,
	"remotes": true, "origin": true,
}

func redactValues(line string) string {
	matches := keyValueRe.FindAllStringSubmatchIndex(line, -1)
	if matches == nil {
		return line
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		key := line[m[4]:m[5]]
		value := line[m[6]:m[7]]
		redacted, ok := redactValue(key, value)
		if !ok {
			continue
		}
		b.WriteString(line[last:m[6]])
		b.WriteString(redacted)
		last = m[7]
	}
	b.WriteString(line[last:])
	return b.String()
}

// redactValue returns the value to log for key, and false when it stays.
func redactValue(key, value string) (string, bool) {
	inner, quoted := value, false
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		inner, quoted = value[1:len(value)-1], true
	}
	if inner == "" {
		return "", false
	}
	var out string
	switch {
	case secretKeys[key]:
		out = "<redacted>"
	case pathKeys[key]:
		out = redactPath(inner)
	case maybePathKeys[key] && strings.ContainsAny(inner, `/\`):
		out = redactPath(inner)
	default:
		return "", false
	}
	if quoted {
		out = `"` + out + `"`
	}
	return out, true
}

func redactPath(path string) string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return path
	}
	// Keep the separators where they were: rebuild from the original.
	var b strings.Builder
	i := 0
	for _, part := range parts {
		j := strings.Index(path[i:], part) + i
		b.WriteString(path[i:j])
		b.WriteString(redactPart(part))
		i = j + len(part)
	}
	b.WriteString(path[i:])
	return b.String()
}

func redactPart(part string) string {
	if safeNames[part] || strings.HasPrefix(part, ".") {
		return part
	}
	// An extension is a short word: "v2.0 final" has none to keep.
	if ext := filepath.Ext(part); len(ext) > 1 && len(ext) <= 10 && ext != part && isWord(ext[1:]) {
		return "<redacted>" + ext
	}
	return "<redacted>"
}

func isWord(s string) bool {
	for _, r := range s {
		isLetter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !isLetter && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// fingerprintRe matches base64url strings of 40 or more chars.
var fingerprintRe = regexp.MustCompile(`[A-Za-z0-9_-]{40,}`)

func redactFingerprints(line string) string {
	return fingerprintRe.ReplaceAllString(line, "<fingerprint-redacted>")
}

var ipv4Re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b`)

func redactIPv4(line string) string {
	return ipv4Re.ReplaceAllString(line, "<ip-redacted>")
}

// ipv6Re matches IPv6 addresses, with optional zone, brackets, and port, and
// also clock times and hex; redactIPv6 tells them apart.
var ipv6Re = regexp.MustCompile(`\[?[0-9a-fA-F:]{4,39}(%[a-zA-Z0-9]+)?\]?(:\d+)?`)

var macRe = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)

func redactIPv6(line string) string {
	return ipv6Re.ReplaceAllStringFunc(line, func(match string) string {
		core := strings.TrimPrefix(match, "[")
		if i := strings.IndexAny(core, "%]"); i >= 0 {
			core = core[:i]
		}
		// An IPv6 address has eight groups or a "::". A time such as
		// 07:41:23 has neither, so the timestamps stay.
		if strings.Contains(core, "::") || strings.Count(core, ":") == 7 || macRe.MatchString(core) {
			return "<ip-redacted>"
		}
		return match
	})
}

// homePathRe matches a path under the home folder in free text, up to a
// space or a quote.
var homePathRe = regexp.MustCompile(`<home>[/\\][^ \t"]*`)

// userRedactor replaces the home folder with <home> and redacts the names
// in a path under it, and the account name wherever it stands between
// separators (a temp path can spell the home folder with dashes).
func userRedactor() func(string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return func(line string) string { return line }
	}
	user := filepath.Base(home)
	var userRe *regexp.Regexp
	if len(user) >= 3 {
		userRe = regexp.MustCompile(`(^|[^A-Za-z0-9])` + regexp.QuoteMeta(user) + `($|[^A-Za-z0-9])`)
	}
	return func(line string) string {
		line = strings.ReplaceAll(line, home, "<home>")
		line = homePathRe.ReplaceAllStringFunc(line, func(m string) string {
			return "<home>" + redactPath(m[len("<home>"):])
		})
		if userRe != nil {
			line = userRe.ReplaceAllString(line, "${1}<user>${2}")
		}
		return line
	}
}
