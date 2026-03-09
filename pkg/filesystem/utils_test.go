// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecureJoin(t *testing.T) {
	// Create a temporary base directory
	base, err := os.MkdirTemp("", "keibidrop-test-base")
	require.NoError(t, err)
	defer os.RemoveAll(base)

	absBase, err := filepath.Abs(base)
	require.NoError(t, err)

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"Normal file", "file.txt", false},
		{"Subdirectory file", "dir/file.txt", false},
		{"Current directory", ".", false},
		{"Parent directory (escape)", "..", true},
		{"Parent directory with valid file (escape)", "../outside.txt", true},
		{"Deep escape", "dir/../../outside.txt", true},
		{"Absolute-looking path (safe)", "/file.txt", false},
		{"Subdirectory with same prefix (escape)", "../" + filepath.Base(absBase) + "suffix/file.txt", true},
	}

	if runtime.GOOS == "windows" {
		tests = append(tests, []struct {
			name    string
			path    string
			wantErr bool
		}{
			{"UNC path (escape)", `\server\share\file.txt`, true},
			{"Drive letter (escape)", `C:\Windows\System32\cmd.exe`, true},
		}...)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SecureJoin(base, tt.path)
			if tt.wantErr {
				assert.Error(t, err, "Expected error for path: %s", tt.path)
				assert.Empty(t, got)
			} else {
				assert.NoError(t, err, "Unexpected error for path: %s", tt.path)
				assert.True(t, got == absBase || strings.HasPrefix(got, absBase+string(filepath.Separator)), "Result %q should be within %q", got, absBase)
			}
		})
	}
}
