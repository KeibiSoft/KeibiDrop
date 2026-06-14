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
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds all user-configurable settings.
// Resolution order: built-in defaults → config file → environment variables.
type Config struct {
	Relay             string `toml:"relay"`
	SavePath          string `toml:"save_path"`
	MountPath         string `toml:"mount_path"`
	LogFile           string `toml:"log_file"`
	InboundPort       int    `toml:"inbound_port"`
	OutboundPort      int    `toml:"outbound_port"`
	BridgeAddr        string `toml:"bridge_addr"` // TCP bridge relay address
	StrictMode        bool   `toml:"strict_mode"` // Disable data relay fallback
	NoFUSE            bool   `toml:"no_fuse"`
	Incognito         bool   `toml:"incognito"`
	PrefetchOnOpen    bool   `toml:"prefetch_on_open"`
	PrefetchAutoMB    int    `toml:"prefetch_auto_mb"` // auto-prefetch files >= this many MB (sequential fill + on-demand jumps). Default 0 = off (pure on-demand) — aggressive prefetch saturates constrained/relay links and starves seeks. Set >0 only on a fat link. prefetch_on_open=true forces prefetch for any size.
	PushOnWrite       bool   `toml:"push_on_write"`
	LiveCollab        bool   `toml:"live_collab"`        // prioritize seeing peers' same-size in-place edits live (macFUSE auto_cache); trades off local mmap-write integrity (git). See note in template.
	PassphraseProtect bool   `toml:"passphrase_protect"` // opt into Tier 2 (Argon2id passphrase) for identity at-rest encryption
}

const DefaultRelay = "https://keibidroprelay.keibisoft.com/"
const DefaultBridge = "bridge.keibisoft.com:26600"

// DefaultConfig returns platform-aware defaults.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cfg := Config{
		Relay:          DefaultRelay,
		SavePath:       filepath.Join(home, "KeibiDrop", "Received"),
		MountPath:      filepath.Join(home, "KeibiDrop", "Mount"),
		InboundPort:    InboundPort,
		OutboundPort:   OutboundPort,
		BridgeAddr:     DefaultBridge,
		PrefetchAutoMB: 0, // default off: pure on-demand (instant open, fetch only what's read). Aggressive prefetch saturates constrained/relay links and freezes seeks; opt in (>0) only on a fat link.
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

// EnsureDirectories creates save_path, the log dir, and (except on Windows) mount_path:
// WinFSP makes the mount point itself and rejects an existing path, while libfuse and
// macFUSE require it to exist.
func EnsureDirectories(cfg Config) error {
	for _, d := range directoriesToEnsure(cfg, runtime.GOOS) {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0750); err != nil { // #nosec G301
			return fmt.Errorf("create directory %s: %w", d, err)
		}
	}
	return nil
}

// directoriesToEnsure returns the dirs to create; mount_path is excluded on Windows.
func directoriesToEnsure(cfg Config, goos string) []string {
	dirs := []string{cfg.SavePath, filepath.Dir(cfg.LogFile)}
	if goos != "windows" {
		dirs = append(dirs, cfg.MountPath)
	}
	return dirs
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
#
# live_collab: see a peer's in-place edits immediately even when the edited
# file keeps the same byte length. Default false = git-safe: files written via
# mmap (notably git's index) stay intact, and same-size live edits are NOT
# reflected until the next size change (macOS/macFUSE limitation). Set true to
# prioritize live collaboration — on macOS this enables the auto_cache mount
# option, which can corrupt mmap-written files such as git's index, so only
# enable it on machines used for document/code collaboration, not git work.
# On Linux and Windows both work regardless of this setting.
# live_collab = false
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

# FUSE download strategy (persisted so the choice survives restarts).
# prefetch_auto_mb: 0 (default) = pure on-demand — open instantly and fetch only
#   what's read; seeks stay responsive on any link. Set to N>0 to also background-
#   prefetch files >= N MB on open (sequential fill). Only do this on a fat link:
#   on a constrained or relayed connection the bulk prefetch saturates the pipe
#   and freezes interactive seeks.
# prefetch_on_open: force prefetch for ANY size (overrides prefetch_auto_mb).
prefetch_auto_mb = %v
prefetch_on_open = %v

# Prioritize live collaboration over local mmap-write integrity (default false).
# false = git-safe: mmap writes (git's index) stay intact; on macOS a peer's
# same-size in-place edit is not seen live until the next size change.
# true = on macOS enables the auto_cache mount option so same-size edits show up
# live, at the risk of corrupting mmap-written files (git). Linux/Windows handle
# both regardless. Leave false unless this machine is for document/code collab.
live_collab = %v
`, cfg.Relay, cfg.SavePath, cfg.MountPath, cfg.LogFile,
		cfg.InboundPort, cfg.OutboundPort, cfg.BridgeAddr,
		cfg.NoFUSE, cfg.StrictMode, cfg.Incognito, cfg.PrefetchAutoMB, cfg.PrefetchOnOpen, cfg.LiveCollab)

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
	if b, ok := envBool("STRICT_MODE", "KD_STRICT"); ok {
		cfg.StrictMode = b
	}
	if b, ok := envBool("NO_FUSE", "KD_NO_FUSE"); ok {
		cfg.NoFUSE = b
	}
	if b, ok := envBool("KEIBIDROP_INCOGNITO", "KD_INCOGNITO"); ok {
		cfg.Incognito = b
	}
	if b, ok := envBool("KEIBIDROP_PASSPHRASE_PROTECT", "KD_PASSPHRASE_PROTECT"); ok {
		cfg.PassphraseProtect = b
	}
	if b, ok := envBool("KEIBIDROP_PREFETCH_ON_OPEN", "PREFETCH_ON_OPEN_ENV"); ok {
		cfg.PrefetchOnOpen = b
	}
	if v := envFirst("KEIBIDROP_PREFETCH_AUTO_MB", "PREFETCH_AUTO_MB_ENV"); v != "" {
		if mb, err := strconv.Atoi(v); err == nil {
			cfg.PrefetchAutoMB = mb
		}
	}
	if b, ok := envBool("KEIBIDROP_PUSH_ON_WRITE", "PUSH_ON_WRITE_ENV"); ok {
		cfg.PushOnWrite = b
	}
	if b, ok := envBool("KEIBIDROP_LIVE_COLLAB", "LIVE_COLLAB_ENV"); ok {
		cfg.LiveCollab = b
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

// envBool resolves a boolean-valued env var from the given names. The second
// return value reports whether any of the vars was set; when false the caller
// should leave the existing (default / config-file) value untouched rather than
// forcing it to false. A present value is parsed leniently: "0", "false",
// "no", "off" (any case) → false; anything else → true. This fixes the old
// presence-only behaviour where e.g. NO_FUSE=false wrongly enabled the flag.
func envBool(names ...string) (val bool, set bool) {
	v := envFirst(names...)
	if v == "" {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false, true
	default:
		return true, true
	}
}

// Warnings returns notes about flag combinations that are redundant or carry a
// known tradeoff, for the caller to log at startup. None are fatal — there is
// no combination that crashes or that the program must refuse. The cases are:
//
//   - no_fuse disables the FUSE mount, so the mount-tuning flags prefetch_on_open
//     and live_collab have nothing to act on (the no-FUSE AddFile/PullFile API is
//     used instead). They are silently ignored; we surface that here.
//   - live_collab on macOS enables the macFUSE auto_cache mount option, which
//     fixes same-size live-edit visibility but can corrupt files written via mmap
//     (notably git's index). That is a deliberate tradeoff, flagged so it is not
//     a surprise. On Linux/Windows live_collab is a no-op (both already work).
//
// Note prefetch_on_open and live_collab are NOT mutually exclusive: prefetch
// fills the local disk cache while auto_cache governs the kernel page cache —
// they operate at different layers and compose cleanly.
func (c Config) Warnings() []string {
	var w []string
	if c.NoFUSE {
		if c.PrefetchOnOpen {
			w = append(w, "prefetch_on_open has no effect with no_fuse: it triggers on FUSE Open(), which never fires without a mount")
		}
		if c.LiveCollab {
			w = append(w, "live_collab has no effect with no_fuse: it tunes the FUSE mount cache, while no_fuse uses the AddFile/PullFile API")
		}
	}
	if c.LiveCollab && runtime.GOOS == "darwin" {
		w = append(w, "live_collab enables macFUSE auto_cache: peers' same-size in-place edits become visible live, but files written via mmap on the mount (e.g. git's index) may be corrupted — do not run git operations on the mount in this mode")
	}
	return w
}
