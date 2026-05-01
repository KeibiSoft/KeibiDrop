// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config holds all user-configurable settings.
// Resolution order: built-in defaults → config file → environment variables.
type Config struct {
	Relay          string `toml:"relay"`
	SavePath       string `toml:"save_path"`
	MountPath      string `toml:"mount_path"`
	LogFile        string `toml:"log_file"`
	InboundPort    int    `toml:"inbound_port"`
	OutboundPort   int    `toml:"outbound_port"`
	BridgeAddr     string `toml:"bridge_addr"` // TCP bridge relay address
	StrictMode     bool   `toml:"strict_mode"` // Disable data relay fallback
	NoFUSE         bool   `toml:"no_fuse"`
	Incognito      bool   `toml:"incognito"`
	PrefetchOnOpen bool   `toml:"prefetch_on_open"`
	PushOnWrite    bool   `toml:"push_on_write"`
}

const DefaultRelay = "https://keibidroprelay.keibisoft.com/"

// DefaultConfig returns platform-aware defaults.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cfg := Config{
		Relay:        DefaultRelay,
		SavePath:     filepath.Join(home, "KeibiDrop", "Received"),
		MountPath:    filepath.Join(home, "KeibiDrop", "Mount"),
		InboundPort:  InboundPort,
		OutboundPort: OutboundPort,
	}
	switch runtime.GOOS {
	case "darwin":
		cfg.LogFile = filepath.Join(home, "Library", "Logs", "KeibiDrop", "keibidrop.log")
	default:
		cfg.LogFile = filepath.Join(home, ".local", "share", "keibidrop", "keibidrop.log")
	}
	return cfg
}

// ConfigDir returns the config directory path (~/.config/keibidrop/).
// Override with KEIBIDROP_CONFIG_DIR for testing multiple instances on one machine.
func ConfigDir() string {
	if d := os.Getenv("KEIBIDROP_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "keibidrop")
}

// ConfigPath returns the config file path.
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.toml")
}

// Load reads the config file (if it exists) and applies environment variable overrides.
// Missing config file is not an error — defaults are used.
func Load() (Config, error) {
	cfg := DefaultConfig()

	// Load config file if it exists.
	path := ConfigPath()
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	// Environment variables override config file.
	// Support both KEIBIDROP_ prefix (Rust UI / CLI) and KD_ prefix (kd daemon).
	applyEnvOverrides(&cfg)

	// Resolve relative paths to absolute.
	if cfg.SavePath != "" {
		if abs, err := filepath.Abs(cfg.SavePath); err == nil {
			cfg.SavePath = abs
		}
	}
	if cfg.MountPath != "" {
		if abs, err := filepath.Abs(cfg.MountPath); err == nil {
			cfg.MountPath = abs
		}
	}
	if cfg.LogFile != "" {
		if abs, err := filepath.Abs(cfg.LogFile); err == nil {
			cfg.LogFile = abs
		}
	}

	return cfg, nil
}

// EnsureDirectories creates save_path, mount_path, and log directory if they don't exist.
func EnsureDirectories(cfg Config) error {
	dirs := []string{cfg.SavePath, cfg.MountPath, filepath.Dir(cfg.LogFile)}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0750); err != nil { // #nosec G301
			return fmt.Errorf("create directory %s: %w", d, err)
		}
	}
	return nil
}

// WriteDefault creates a default config file with comments at the standard path.
// Does nothing if the file already exists.
func WriteDefault() error {
	path := ConfigPath()
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil { // #nosec G301
		return err
	}
	cfg := DefaultConfig()
	content := fmt.Sprintf(`# KeibiDrop configuration
# https://github.com/KeibiSoft/KeibiDrop
#
# Environment variables override these values.
# KEIBIDROP_RELAY, TO_SAVE_PATH, TO_MOUNT_PATH, etc.

# Relay server for peer discovery.
relay = %q

# Where received files are saved.
save_path = %q

# FUSE mount point (if FUSE is enabled).
mount_path = %q

# Log file path.
log_file = %q

# TCP ports for peer connections (must be in 26000-27000 range).
inbound_port = %d
outbound_port = %d

# Set to true to disable FUSE (faster for bulk file transfers).
# FUSE mode is better for real-time workspace sync.
no_fuse = %v

# Collaborative sync options (experimental).
# prefetch_on_open = false
# push_on_write = false
`, cfg.Relay, cfg.SavePath, cfg.MountPath, cfg.LogFile,
		cfg.InboundPort, cfg.OutboundPort, cfg.NoFUSE)

	return os.WriteFile(path, []byte(content), 0600) // #nosec G306
}

// Save writes the config to the standard path (~/.config/keibidrop/config.toml).
func Save(cfg Config) error {
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil { // #nosec G301
		return err
	}
	content := fmt.Sprintf(`# KeibiDrop configuration
# https://keibidrop.com

# Relay server for peer discovery.
relay = %q

# Where received files are saved.
save_path = %q

# FUSE mount point (if FUSE is enabled).
mount_path = %q

# Log file path.
log_file = %q

# TCP ports for peer connections (must be in 26000-27000 range).
inbound_port = %d
outbound_port = %d

# Bridge relay address for fallback when direct P2P fails.
bridge_addr = %q

# Set to true to disable FUSE.
no_fuse = %v

# Set to true to disable data relay fallback (direct connections only).
strict_mode = %v

# Set to true to force ephemeral keys every session (no persistent identity).
incognito = %v
`, cfg.Relay, cfg.SavePath, cfg.MountPath, cfg.LogFile,
		cfg.InboundPort, cfg.OutboundPort, cfg.BridgeAddr,
		cfg.NoFUSE, cfg.StrictMode, cfg.Incognito)

	return os.WriteFile(path, []byte(content), 0600) // #nosec G306
}

func applyEnvOverrides(cfg *Config) {
	if v := envFirst("KEIBIDROP_RELAY", "KD_RELAY"); v != "" {
		cfg.Relay = v
	}
	if v := envFirst("TO_SAVE_PATH", "KD_SAVE_PATH"); v != "" {
		cfg.SavePath = v
	}
	if v := envFirst("TO_MOUNT_PATH", "KD_MOUNT_PATH"); v != "" {
		cfg.MountPath = v
	}
	if v := envFirst("LOG_FILE", "KD_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := envFirst("INBOUND_PORT", "KD_INBOUND_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.InboundPort = port
		}
	}
	if v := envFirst("OUTBOUND_PORT", "KD_OUTBOUND_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.OutboundPort = port
		}
	}
	if v := envFirst("BRIDGE_ADDR", "KD_BRIDGE"); v != "" {
		cfg.BridgeAddr = v
	}
	if v := envFirst("STRICT_MODE", "KD_STRICT"); v != "" {
		cfg.StrictMode = true
	}
	if v := envFirst("NO_FUSE", "KD_NO_FUSE"); v != "" {
		cfg.NoFUSE = true
	}
	if v := envFirst("KEIBIDROP_INCOGNITO", "KD_INCOGNITO"); v != "" {
		cfg.Incognito = true
	}
	if v := envFirst("KEIBIDROP_PREFETCH_ON_OPEN", "PREFETCH_ON_OPEN_ENV"); v != "" {
		cfg.PrefetchOnOpen = true
	}
	if v := envFirst("KEIBIDROP_PUSH_ON_WRITE", "PUSH_ON_WRITE_ENV"); v != "" {
		cfg.PushOnWrite = true
	}
}

// envFirst returns the first non-empty value from the given environment variable names.
func envFirst(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
