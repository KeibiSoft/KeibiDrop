// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeMount stands in for a FUSE host: mount blocks until the host is taken
// down or the test ends it, mounted follows the host, live is whatever the
// test says the OS answers.
type fakeMount struct {
	mu       sync.Mutex
	answers  bool
	hostUp   bool
	release  chan error
	mountErr error
	mounts   atomic.Int32
	unmounts atomic.Int32
	resets   atomic.Int32
}

func newFakeMount() *fakeMount { return &fakeMount{answers: true} }

func (f *fakeMount) driver() mountDriver {
	t := mountTiming{remountMin: 5 * time.Millisecond, remountMax: 20 * time.Millisecond, probe: 10 * time.Millisecond, liveTimeout: 40 * time.Millisecond}
	return mountDriver{
		mount: func() error {
			f.mounts.Add(1)
			f.mu.Lock()
			if f.mountErr != nil {
				err := f.mountErr
				f.mu.Unlock()
				return err
			}
			f.hostUp = true
			f.release = make(chan error, 1)
			rel := f.release
			f.mu.Unlock()
			err := <-rel
			f.mu.Lock()
			f.hostUp = false
			f.mu.Unlock()
			return err
		},
		mounted: func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.hostUp },
		live: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.hostUp && f.answers
		},
		unmount: func() {
			f.unmounts.Add(1)
			f.endHost(nil)
		},
		reset:  func() { f.resets.Add(1) },
		timing: t,
	}
}

// endHost makes the blocked mount return, as an eject or Unmount does.
func (f *fakeMount) endHost(err error) {
	f.mu.Lock()
	rel := f.release
	f.release = nil
	f.mu.Unlock()
	if rel != nil {
		rel <- err
	}
}

func (f *fakeMount) setAnswers(v bool) { f.mu.Lock(); f.answers = v; f.mu.Unlock() }

func (f *fakeMount) setMountErr(err error) { f.mu.Lock(); f.mountErr = err; f.mu.Unlock() }

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) { l.mu.Lock(); l.events = append(l.events, e); l.mu.Unlock() }
func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}
func (l *eventLog) waitFor(t *testing.T, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, e := range l.list() {
			if e == want {
				return true
			}
		}
		return false
	}, 2*time.Second, time.Millisecond, "waiting for %q, got %v", want, l.list())
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestMountLoop_EjectRemountsAndSaysSo(t *testing.T) {
	f := newFakeMount()
	kd := &KeibiDrop{logger: quietLogger(), ToMount: "/x"}
	var log eventLog
	kd.OnEvent = log.add
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { kd.runMountLoop(ctx, quietLogger(), f.driver()); close(done) }()

	require.Eventually(t, func() bool { return f.mounts.Load() == 1 && f.driver().mounted() }, time.Second, time.Millisecond)
	require.Empty(t, log.list(), "a first mount is not news")

	f.endHost(nil) // the eject: the host returns on its own
	log.waitFor(t, "mount_gone:")
	log.waitFor(t, "mount_back:")
	require.Equal(t, int32(2), f.mounts.Load(), "one remount")
	require.Equal(t, int32(0), f.unmounts.Load(), "the host was already gone, nothing to take down")
	require.Equal(t, int32(1), f.resets.Load(), "the next host gets a fresh context")
	require.Equal(t, []string{"mount_gone:", "mount_back:"}, log.list())
	require.Equal(t, "", kd.mountFailedReason())

	cancel()
	<-done
}

func TestMountLoop_FolderThatStopsAnsweringIsTakenDownAndRemounted(t *testing.T) {
	f := newFakeMount()
	kd := &KeibiDrop{logger: quietLogger(), ToMount: "/x"}
	var log eventLog
	kd.OnEvent = log.add
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { kd.runMountLoop(ctx, quietLogger(), f.driver()); close(done) }()
	require.Eventually(t, func() bool { return f.mounts.Load() == 1 }, time.Second, time.Millisecond)

	f.setAnswers(false) // the host runs on, the OS says the folder is dead
	log.waitFor(t, "mount_gone:")
	require.Equal(t, int32(1), f.unmounts.Load(), "two misses take the host down")
	f.setAnswers(true)
	log.waitFor(t, "mount_back:")
	require.Equal(t, int32(2), f.mounts.Load())
	cancel()
	<-done
}

func TestMountLoop_InheritedHostIsWatchedNotMountedOver(t *testing.T) {
	f := newFakeMount()
	kd := &KeibiDrop{logger: quietLogger(), ToMount: "/x"}
	var log eventLog
	kd.OnEvent = log.add
	d := f.driver()
	// A host from an earlier session is already up.
	go func() { _ = d.mount() }()
	require.Eventually(t, d.mounted, time.Second, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { kd.runMountLoop(ctx, quietLogger(), d); close(done) }()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), f.mounts.Load(), "the loop mounts nothing over a live host")
	require.Empty(t, log.list())

	f.endHost(nil) // ejected during the second session
	log.waitFor(t, "mount_gone:")
	log.waitFor(t, "mount_back:")
	require.Equal(t, int32(2), f.mounts.Load())
	cancel()
	<-done
}

func TestMountLoop_RepeatedFailuresSayFailedOnceAndKeepTrying(t *testing.T) {
	f := newFakeMount()
	f.setMountErr(errors.New("FUSE mount failed for /x: driver missing"))
	kd := &KeibiDrop{logger: quietLogger(), ToMount: "/x"}
	var log eventLog
	kd.OnEvent = log.add
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { kd.runMountLoop(ctx, quietLogger(), f.driver()); close(done) }()

	log.waitFor(t, "mount_failed:FUSE mount failed for /x: driver missing")
	require.Equal(t, "FUSE mount failed for /x: driver missing", kd.mountFailedReason())
	require.GreaterOrEqual(t, f.mounts.Load(), int32(mountFailedAfter))
	require.Eventually(t, func() bool { return f.mounts.Load() > mountFailedAfter }, time.Second, time.Millisecond, "it keeps trying after saying so")
	gone, failed := 0, 0
	for _, e := range log.list() {
		switch e {
		case "mount_gone:":
			gone++
		case "mount_failed:FUSE mount failed for /x: driver missing":
			failed++
		}
	}
	require.Equal(t, 1, gone, "one outage, one mount_gone")
	require.Equal(t, 1, failed, "said once")

	f.setMountErr(nil) // the driver is back
	log.waitFor(t, "mount_back:")
	require.Equal(t, "", kd.mountFailedReason())
	cancel()
	<-done
}

func TestWatchMount_FreshMountThatNeverAnswersCountsAsFailed(t *testing.T) {
	f := newFakeMount()
	f.setAnswers(false)
	kd := &KeibiDrop{logger: quietLogger(), ToMount: "/x"}
	d := f.driver()
	mountDone := make(chan error, 1)
	go func() { mountDone <- d.mount() }()
	liveCalls := 0
	ended, err := kd.watchMount(context.Background(), quietLogger(), d, mountDone, func() { liveCalls++ })
	require.True(t, ended)
	require.ErrorIs(t, err, errMountNotAnswering)
	require.Equal(t, 0, liveCalls)
	require.Equal(t, int32(1), f.unmounts.Load())
}

func TestWatchMount_ContextEndLeavesTheHostUp(t *testing.T) {
	f := newFakeMount()
	kd := &KeibiDrop{logger: quietLogger(), ToMount: "/x"}
	d := f.driver()
	mountDone := make(chan error, 1)
	go func() { mountDone <- d.mount() }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	ended, err := kd.watchMount(ctx, quietLogger(), d, mountDone, func() {})
	require.False(t, ended)
	require.NoError(t, err)
	require.True(t, d.mounted(), "a disconnect keeps the mount for the next session")
	require.Equal(t, int32(0), f.unmounts.Load())
	f.endHost(nil)
}
