// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// The file entry points guard on kd.session, which the run loop nils under
// kd.mu at teardown. A guard that reads it without the lock is a data race.

package common

import (
	"path/filepath"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

func TestFileGuardsDoNotRaceTeardown(t *testing.T) {
	kd := newBareKD()
	absent := filepath.Join(t.TempDir(), "absent")

	// Teardown write, then a fresh session, on repeat.
	stop := make(chan struct{})
	flip := testkit.Go(func() error {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return nil
			default:
			}
			kd.mu.Lock()
			if i%2 == 0 {
				kd.session = &session.Session{}
			} else {
				kd.session = nil
			}
			kd.mu.Unlock()
		}
	})

	type entry struct {
		name string
		call func() error
	}
	entries := []entry{
		{"AddFile", func() error { return kd.AddFile(absent) }},
		{"AddFileAs", func() error { return kd.AddFileAs(absent, "absent") }},
		{"PullFile", func() error { return kd.PullFile("absent", absent) }},
	}
	testkit.RunTable(t, entries, func(e entry) string { return e.name }, func(t *testing.T, e entry) error {
		for i := 0; i < 2000; i++ {
			if err := fp.ErrIs(e.name, e.call(), ErrInvalidSession); err != nil {
				return err
			}
		}
		return nil
	})
	close(stop)
	testkit.Run(t, flip)
}
