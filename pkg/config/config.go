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
// Values resolve in this order: built-in defaults, config file, environment variables.
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
	PrefetchAutoMB    int    `toml:"prefetch_auto_mb"`     // Prefetch files at or above this many MB. Default 0 is off because prefetch saturates slow links and starves seeks.
	ReadAheadWindowMB int    `toml:"read_ahead_window_mb"` // Cap in MB for sequential read-ahead; prevents stalls at block boundaries on high-RTT links. Self-tunes below the cap. Default 64; 0 disables.
	PushOnWrite       bool   `toml:"push_on_write"`
	LiveCollab        bool   `toml:"live_collab"`          // Show peers' same-size in-place edits live (macFUSE auto_cache); risks local mmap-write integrity (git).
	PassphraseProtect bool   `toml:"passphrase_protect"`   // Enable Tier 2 identity at-rest encryption with an Argon2id passphrase.
	ScanSharedOnStart bool   `toml:"scan_shared_on_start"` // Announce files already in save_path when a session starts.
	AutoConnectPeer   string `toml:"auto_connect_peer"`    // Saved contact (name or fingerprint) to connect to on startup, with retry.
	ShareReadOnly     bool   `toml:"share_read_only"`      // Refuse every peer mutation of this node's data (origin-enforced).
	MountReadOnly     bool   `toml:"mount_read_only"`      // Local FUSE mount returns EROFS on write ops. Keeps the local view equal to the origin.
	PreserveMetadata  bool   `toml:"preserve_metadata"`    // Apply the origin's mode and timestamps to files saved on disk.
	UpdateCheck       bool   `toml:"update_check"`         // Fetch the newest version number from keibidrop.com at start and show a notice when this build is older.
}

const DefaultRelay = "https://keibidroprelay.keibisoft.com/"
const DefaultBridge = "bridge.keibisoft.com:26600"

// DefaultConfig returns platform-aware defaults.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cfg := Config{
		Relay:             DefaultRelay,
		SavePath:          filepath.Join(home, "KeibiDrop", "Received"),
		MountPath:         filepath.Join(home, "KeibiDrop", "Mount"),
		InboundPort:       InboundPort,
		OutboundPort:      OutboundPort,
		BridgeAddr:        DefaultBridge,
		PrefetchAutoMB:    0,  // Off by default: prefetch saturates slow links and freezes seeks.
		ReadAheadWindowMB: 64, // On by default: prevents sequential-read stalls on high-RTT links.
		UpdateCheck:       true,
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
// KEIBIDROP_CONFIG_DIR overrides it, so tests can run multiple instances on one machine.
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

// Load reads the config file, if present, and applies environment variable overrides.
// A missing config file is not an error; defaults apply.
func Load() (Config, error) {
	cfg := DefaultConfig()

	path := ConfigPath()
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	// Overrides accept the KEIBIDROP_ prefix (Rust UI, CLI) and the KD_ prefix (kd daemon).
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

// EnsureDirectories creates save_path, the log directory, and, except on Windows, mount_path.
// WinFSP creates the mount point itself and rejects an existing path; libfuse and macFUSE require it.
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

// directoriesToEnsure returns the directories to create. The list excludes mount_path on Windows.
func directoriesToEnsure(cfg Config, goos string) []string {
	dirs := []string{cfg.SavePath, filepath.Dir(cfg.LogFile)}
	if goos != "windows" {
		dirs = append(dirs, cfg.MountPath)
	}
	return dirs
}

// WriteDefault creates a commented default config file at the standard path.
// It does nothing when the file exists.
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
# prioritize live collaboration: on macOS this enables the auto_cache mount
# option, which can corrupt mmap-written files such as git's index, so only
# enable it on machines used for document/code collaboration, not git work.
# On Linux and Windows both work regardless of this setting.
# live_collab = false
#
# scan_shared_on_start: announce files already in save_path when a session
# starts, so a pre-populated folder appears on the peer without re-adding
# each file. Metadata only; content streams on demand.
# scan_shared_on_start = false
#
# auto_connect_peer: connect to this saved contact (name or fingerprint) on
# startup and keep retrying while the peer is offline. Requires a persistent
# identity and a saved contact (save-contact / add-contact).
# auto_connect_peer = ""
#
# share_read_only: refuse every peer mutation of this node's files, enforced
# on this node before disk. The share becomes one-way (peer reads only).
# Serving reads also avoid updating file access times (Linux O_NOATIME,
# Windows handle sentinel), so evidence atime stays the original.
# share_read_only = false
#
# mount_read_only: local FUSE mount returns EROFS on write ops. Without it a
# local write is kept in save_path and hides the origin's copy in the mount,
# with no error. Keep it on to read the origin and nothing else.
# mount_read_only = false
#
# preserve_metadata: when a received file completes on disk, apply the
# origin's permission bits and timestamps (mtime, atime) to the saved bytes.
# For forensics/evidence workflows where original timestamps must survive
# the transfer. Birth time stays view-only (not settable on most systems).
# Later reads follow the local filesystem's atime policy: for strict work,
# record stat+hashes right after transfer and keep save_path on a
# noatime/read-only mount during analysis. The announced origin times also
# stay recorded in the sync tracker, independent of disk.
# preserve_metadata = false
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
# prefetch_auto_mb: 0 (default) = pure on-demand: open instantly and fetch only
#   what's read; seeks stay responsive on any link. Set to N>0 to also background-
#   prefetch files >= N MB on open (sequential fill). Only do this on a fat link:
#   on a constrained or relayed connection the bulk prefetch saturates the pipe
#   and freezes interactive seeks.
# prefetch_on_open: force prefetch for ANY size (overrides prefetch_auto_mb).
prefetch_auto_mb = %v
prefetch_on_open = %v

# Predictive read-ahead window (MB) for on-demand reads: keep up to this many MB of
# upcoming 16 MiB blocks in flight on sequential access so streaming/video over a
# high-RTT link does not stall at each block boundary. Self-tuning under this cap
# (a slow reader stays well below it; a bulk reader pipelines the link); 0 disables.
read_ahead_window_mb = %v

# Prioritize live collaboration over local mmap-write integrity (default false).
# false = git-safe: mmap writes (git's index) stay intact; on macOS a peer's
# same-size in-place edit is not seen live until the next size change.
# true = on macOS enables the auto_cache mount option so same-size edits show up
# live, at the risk of corrupting mmap-written files (git). Linux/Windows handle
# both regardless. Leave false unless this machine is for document/code collab.
live_collab = %v

# Announce files already in save_path when a session starts. Off by default:
# only files shared explicitly (or by the folder watcher) reach the peer. Set
# true to share a pre-populated folder: on connect, every file under save_path
# is announced to the peer (metadata only; content streams on demand).
scan_shared_on_start = %v

# Connect to this saved contact (name or fingerprint) on startup and keep
# retrying with backoff while the peer is offline. Empty = off. Requires a
# persistent identity (incognito off) and a contact saved via save-contact
# or add-contact.
auto_connect_peer = %q

# Refuse every peer mutation of this node's files (add/edit/delete/rename).
# Enforced on this node, before disk: the real read-only guarantee, and it
# works in no-FUSE mode too. The share becomes one-way: the peer reads, and
# their own shares to this node are refused while this is on. Serving reads
# also avoid updating file access times (Linux O_NOATIME, Windows handle
# sentinel), so evidence atime stays the original.
share_read_only = %v

# Present the local FUSE mount read-only: local write ops return EROFS.
# The mount shows save_path over the peer's files, so without this a local
# write hides the origin's copy with no error. share_read_only protects the
# origin; this protects what you see. No effect with no_fuse.
mount_read_only = %v

# Apply the origin's permission bits and timestamps (mtime, atime) to files
# saved on disk, once their content completes. For forensics/evidence
# workflows where original timestamps must survive the transfer. Birth time
# stays view-only (not settable on most systems). On Windows only the
# read-only permission bit applies. Later reads follow the local
# filesystem's atime policy: for strict work, record stat+hashes right
# after transfer and keep save_path on a noatime/read-only mount during
# analysis. The announced origin times also stay recorded in the sync
# tracker, independent of disk.
preserve_metadata = %v

# Fetch the newest version number from keibidrop.com at start and show a
# notice when this build is older. The request carries nothing about this
# install. Set false to disable.
update_check = %v
`, cfg.Relay, cfg.SavePath, cfg.MountPath, cfg.LogFile,
		cfg.InboundPort, cfg.OutboundPort, cfg.BridgeAddr,
		cfg.NoFUSE, cfg.StrictMode, cfg.Incognito, cfg.PrefetchAutoMB, cfg.PrefetchOnOpen, cfg.ReadAheadWindowMB, cfg.LiveCollab, cfg.ScanSharedOnStart, cfg.AutoConnectPeer,
		cfg.ShareReadOnly, cfg.MountReadOnly, cfg.PreserveMetadata, cfg.UpdateCheck)

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
	if v := envFirst("KEIBIDROP_READ_AHEAD_WINDOW_MB", "READ_AHEAD_WINDOW_MB_ENV"); v != "" {
		if mb, err := strconv.Atoi(v); err == nil {
			cfg.ReadAheadWindowMB = mb
		}
	}
	if b, ok := envBool("KEIBIDROP_PUSH_ON_WRITE", "PUSH_ON_WRITE_ENV"); ok {
		cfg.PushOnWrite = b
	}
	if b, ok := envBool("KEIBIDROP_LIVE_COLLAB", "LIVE_COLLAB_ENV"); ok {
		cfg.LiveCollab = b
	}
	if b, ok := envBool("KEIBIDROP_SCAN_SHARED_ON_START", "KD_SCAN_SHARED_ON_START"); ok {
		cfg.ScanSharedOnStart = b
	}
	if v := envFirst("KEIBIDROP_AUTO_CONNECT_PEER", "KD_AUTO_CONNECT_PEER"); v != "" {
		cfg.AutoConnectPeer = v
	}
	if b, ok := envBool("KEIBIDROP_SHARE_READ_ONLY", "KD_SHARE_READ_ONLY"); ok {
		cfg.ShareReadOnly = b
	}
	if b, ok := envBool("KEIBIDROP_MOUNT_READ_ONLY", "KD_MOUNT_READ_ONLY"); ok {
		cfg.MountReadOnly = b
	}
	if b, ok := envBool("KEIBIDROP_PRESERVE_METADATA", "KD_PRESERVE_METADATA"); ok {
		cfg.PreserveMetadata = b
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

// envBool resolves a boolean env var from the given names; set=false means keep the current value.
// "0", "false", "no", "off" (any case) parse false, all else true, so NO_FUSE=false stays off.
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

// Warnings returns startup notes for redundant or tradeoff flag combinations; none are fatal.
// prefetch_on_open and live_collab act on different cache layers and compose cleanly.
func (c Config) Warnings() []string {
	var w []string
	if c.NoFUSE {
		if c.PrefetchOnOpen {
			w = append(w, "prefetch_on_open has no effect with no_fuse: it triggers on FUSE Open(), which never fires without a mount")
		}
		if c.LiveCollab {
			w = append(w, "live_collab has no effect with no_fuse: it tunes the FUSE mount cache, while no_fuse uses the AddFile/PullFile API")
		}
		if c.MountReadOnly {
			w = append(w, "mount_read_only has no effect with no_fuse: there is no mount; share_read_only is the enforced guarantee")
		}
	}
	if c.LiveCollab && runtime.GOOS == "darwin" {
		w = append(w, "live_collab enables macFUSE auto_cache: peers' same-size in-place edits become visible live, but files written via mmap on the mount (e.g. git's index) may be corrupted; do not run git operations on the mount in this mode")
	}
	return w
}
