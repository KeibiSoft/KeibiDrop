// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"
)

const (
	remountDelayMin = 3 * time.Second
	remountDelayMax = 60 * time.Second
)

// mountLoop keeps the folder mounted for as long as the session lives. Mount
// blocks for the lifetime of one FUSE session; when it returns with the
// session context still alive the volume was unmounted from outside (a Finder
// eject, diskutil, a driver restart) or the mount failed. Seen 2026-09-08
// 21:46: the volume went away, the daemon kept a healthy session and reported
// fuse true over an empty folder for thirteen hours. Retry with a backoff, so
// a driver that is busy or missing gets another chance without a log storm.
func (kd *KeibiDrop) mountLoop(ctx context.Context, logger *slog.Logger) {
	delay := remountDelayMin
	for ctx.Err() == nil {
		logger.Info("Mounting filesystem", "mount", kd.ToMount, "save", kd.ToSave)
		mountDone := make(chan error, 1)
		go func() {
			mountDone <- kd.FS.Mount(filepath.Clean(kd.ToMount), false, filepath.Clean(kd.ToSave))
		}()
		var err error
		select {
		case err = <-mountDone:
		case <-ctx.Done():
			logger.Info("Context cancelled while FUSE mounted")
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Error("Filesystem mount failed, retrying", "error", err, "in", delay)
		} else {
			logger.Warn("Mount ended while the session is up, remounting", "in", delay)
			delay = remountDelayMin
		}
		if kd.OnEvent != nil {
			kd.OnEvent("mount_gone:")
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		if delay < remountDelayMax {
			delay *= 2
		}
	}
}
