// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package config

import (
	"fmt"
	"strconv"
	"strings"
)

// The settings the app's Settings card changes, and its Reset puts back.
// Paths, servers, ports and the identity stay out: a wrong value there loses
// the connection or the files, so those change in the config file.

// Limits of the number settings.
const (
	MaxReadAheadWindowMB   = 1024    // 64 read-ahead blocks of 16 MiB
	MaxPrefetchAutoMB      = 1 << 20 // 1 TiB
	MinRescanSharedSeconds = 5       // a shorter walk only loads the disk
	MaxRescanSharedSeconds = 86400   // one day
)

// SetTunable sets one Settings card value from its text. It refuses an
// unknown key, a value of the wrong kind and a number out of range, and then
// changes nothing.
func (c *Config) SetTunable(key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case "share_read_only":
		return parseBool(&c.ShareReadOnly, value)
	case "mount_read_only":
		return parseBool(&c.MountReadOnly, value)
	case "preserve_metadata":
		return parseBool(&c.PreserveMetadata, value)
	case "strict_mode":
		return parseBool(&c.StrictMode, value)
	case "update_check":
		return parseBool(&c.UpdateCheck, value)
	case "prefetch_on_open":
		return parseBool(&c.PrefetchOnOpen, value)
	case "live_collab":
		return parseBool(&c.LiveCollab, value)
	case "scan_shared_on_start":
		return parseBool(&c.ScanSharedOnStart, value)
	case "read_ahead_window_mb":
		return parseInt(&c.ReadAheadWindowMB, value, 0, MaxReadAheadWindowMB)
	case "prefetch_auto_mb":
		return parseInt(&c.PrefetchAutoMB, value, 0, MaxPrefetchAutoMB)
	case "rescan_shared_seconds":
		s := c.RescanSharedSeconds
		if err := parseInt(&s, value, 0, MaxRescanSharedSeconds); err != nil {
			return err
		}
		if s != 0 && s < MinRescanSharedSeconds {
			return fmt.Errorf("use 0 to turn it off, or %d seconds or more", MinRescanSharedSeconds)
		}
		c.RescanSharedSeconds = s
		return nil
	}
	return fmt.Errorf("unknown setting %q", key)
}

// ResetTunables puts every value SetTunable changes back to its default.
func (c *Config) ResetTunables() {
	d := DefaultConfig()
	c.ShareReadOnly = d.ShareReadOnly
	c.MountReadOnly = d.MountReadOnly
	c.PreserveMetadata = d.PreserveMetadata
	c.StrictMode = d.StrictMode
	c.UpdateCheck = d.UpdateCheck
	c.PrefetchOnOpen = d.PrefetchOnOpen
	c.LiveCollab = d.LiveCollab
	c.ScanSharedOnStart = d.ScanSharedOnStart
	c.ReadAheadWindowMB = d.ReadAheadWindowMB
	c.PrefetchAutoMB = d.PrefetchAutoMB
	c.RescanSharedSeconds = d.RescanSharedSeconds
}

// parseBool takes only "true" or "false": the app sends one of the two.
func parseBool(dst *bool, s string) error {
	switch s {
	case "true":
		*dst = true
	case "false":
		*dst = false
	default:
		return fmt.Errorf("use true or false, not %q", s)
	}
	return nil
}

// parseInt takes a whole number from lo to hi.
func parseInt(dst *int, s string, lo, hi int) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return fmt.Errorf("use a whole number from %d to %d", lo, hi)
	}
	*dst = n
	return nil
}
