// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"
)

// mountProbeMisses is how many probes in a row must fail before the host is
// taken down: one miss can be a stat racing the driver.
const mountProbeMisses = 2

// mountFailedAfter is the number of failed mount attempts in a row after
// which the session says the folder cannot come back. It keeps trying.
const mountFailedAfter = 3

var errMountNotAnswering = errors.New("the mounted folder does not answer")

// mountTiming paces the loop. Tests shrink it.
type mountTiming struct {
	remountMin, remountMax time.Duration // backoff between mount attempts
	probe                  time.Duration // liveness probe cadence once the folder answers
	liveTimeout            time.Duration // how long a fresh mount may take to answer
}

var defaultMountTiming = mountTiming{
	remountMin:  3 * time.Second,
	remountMax:  60 * time.Second,
	probe:       3 * time.Second,
	liveTimeout: 20 * time.Second,
}

// mountDriver is everything the loop touches: the FUSE host and the folder,
// behind functions so a test can stand in for both.
type mountDriver struct {
	mount   func() error // blocks for the life of one host, as FS.Mount does
	mounted func() bool  // does the FS still hold a host and a root
	live    func() bool  // does the folder answer the OS
	unmount func()       // take the host down
	reset   func()       // give the next host a context that is not cancelled
	timing  mountTiming
}

func (kd *KeibiDrop) fsMountDriver() mountDriver {
	return mountDriver{
		mount: func() error {
			return kd.FS.Mount(filepath.Clean(kd.ToMount), false, filepath.Clean(kd.ToSave))
		},
		mounted: kd.FS.IsMounted,
		live:    func() bool { return MountIsLive(kd.ToMount) },
		unmount: kd.FS.Unmount,
		reset:   kd.FS.CancelInFlight,
		timing:  defaultMountTiming,
	}
}

// mountLoop keeps the folder mounted for as long as the session lives, and
// watches it while it is. Mount blocks for the lifetime of one FUSE host;
// when it returns with the session still alive the volume was unmounted from
// outside (a Finder eject, diskutil, a driver restart) or the mount failed.
// Seen 2026-09-08 21:46: the volume went away, the daemon kept a healthy
// session and reported fuse true over an empty folder for thirteen hours.
//
// The mount outlives a disconnect (cgofuse allows one mount per process), so
// a later session inherits a host it never started; the loop watches that
// one too, and takes it down when the folder stops answering so it can be
// mounted again. The peer's session is never touched: what it shares keeps
// being served from disk, and what it sends keeps landing in the save folder.
// Events: mount_gone once per outage, mount_failed once after
// mountFailedAfter failed attempts, mount_back when the folder answers again.
func (kd *KeibiDrop) mountLoop(ctx context.Context, logger *slog.Logger) {
	kd.runMountLoop(ctx, logger, kd.fsMountDriver())
}

func (kd *KeibiDrop) runMountLoop(ctx context.Context, logger *slog.Logger, d mountDriver) {
	delay := d.timing.remountMin
	failures := 0
	gone := false
	kd.mountFailed.Store("")
	for ctx.Err() == nil {
		var mountDone chan error
		if d.mounted() {
			logger.Info("Filesystem still mounted, watching it", "mount", kd.ToMount)
		} else {
			logger.Info("Mounting filesystem", "mount", kd.ToMount, "save", kd.ToSave)
			mountDone = make(chan error, 1)
			go func() { mountDone <- d.mount() }()
		}
		ended, err := kd.watchMount(ctx, logger, d, mountDone, func() {
			failures = 0
			delay = d.timing.remountMin
			kd.mountFailed.Store("")
			if gone {
				gone = false
				logger.Info("Filesystem is back", "mount", kd.ToMount)
				kd.emitEvent("mount_back:")
			}
		})
		if !ended {
			logger.Info("Context cancelled while FUSE mounted")
			return
		}
		d.reset()
		if err != nil {
			failures++
			logger.Error("Filesystem mount failed, retrying", "error", err, "in", delay, "failures", failures)
		} else {
			logger.Warn("Mount ended while the session is up, remounting", "in", delay)
		}
		if !gone {
			gone = true
			kd.emitEvent("mount_gone:")
		}
		if failures == mountFailedAfter {
			reason := err.Error()
			kd.mountFailed.Store(reason)
			logger.Error("Filesystem could not be remounted, still trying", "reason", reason, "every", d.timing.remountMax)
			kd.emitEvent("mount_failed:" + reason)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, d.timing.remountMax)
	}
}

// watchMount waits for the folder to answer, reports it live through onLive,
// then probes it every timing.probe. It returns ended true once the host is
// gone: with Mount's error, or errMountNotAnswering when a fresh mount never
// answered and was taken down. ended is false when ctx ended first; the host,
// if any, stays up for the next session. mountDone is nil for a host
// inherited from an earlier session; then the probe and mounted() decide.
func (kd *KeibiDrop) watchMount(ctx context.Context, logger *slog.Logger, d mountDriver, mountDone <-chan error, onLive func()) (ended bool, err error) {
	live, forced := false, false
	misses := 0
	deadline := time.Now().Add(d.timing.liveTimeout)
	// Fast until the first answer, so a mount that comes up in half a second
	// is reported in half a second.
	interval := max(d.timing.probe/10, time.Millisecond)
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, nil
		case err = <-mountDone:
			if forced && !live {
				err = errMountNotAnswering
			}
			return true, err
		case <-t.C:
		}
		if mountDone == nil && !d.mounted() {
			return true, nil // The inherited host returned on its own.
		}
		switch {
		case d.live():
			misses = 0
			if !live {
				live = true
				interval = d.timing.probe
				onLive()
			}
		case live || !time.Now().Before(deadline):
			misses++
			if misses >= mountProbeMisses && !forced {
				forced = true
				logger.Warn("Mounted folder stopped answering, taking the host down", "mount", kd.ToMount, "answered", live)
				go d.unmount()
			}
		}
		t.Reset(interval)
	}
}

// mountFailedReason is why the folder could not be remounted, or "" while it
// is mounted or a remount is still due.
func (kd *KeibiDrop) mountFailedReason() string {
	v, _ := kd.mountFailed.Load().(string)
	return v
}
