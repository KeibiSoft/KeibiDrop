// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// sharedSettleWindow is how long a file's mtime must stand still before a scan
// announces it. A file still landing (FTP upload, card copy) has a moving
// size; announcing it early ships a partial size to the peer.
const sharedSettleWindow = 3 * time.Second

// rescanSharedLoop re-walks the save folder every RescanSharedSeconds for as
// long as ctx lives, announcing files that landed since the last pass. This is
// what makes a serving box pick up new footage mid-session without a
// reconnect. A newer loop (reconnect) retires this one through rescanGen. The
// walk is idempotent, so overlap with the start-of-session scan is harmless.
func (kd *KeibiDrop) rescanSharedLoop(ctx context.Context, logger *slog.Logger) {
	secs := kd.RescanSharedSeconds
	if secs <= 0 || ctx == nil {
		return
	}
	gen := kd.rescanGen.Add(1)
	ticker := time.NewTicker(time.Duration(secs) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if kd.rescanGen.Load() != gen {
			return
		}
		n, err := kd.ScanAndShareSaveDir(ctx)
		if err != nil {
			if errors.Is(err, ErrInvalidSession) || ctx.Err() != nil {
				return
			}
			logger.Warn("Save folder rescan stopped early", "announced", n, "error", err)
			continue
		}
		if n > 0 {
			logger.Info("Save folder rescan announced new files", "count", n)
		}
	}
}
