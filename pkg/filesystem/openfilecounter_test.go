// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package filesystem

import (
	"sync"
	"testing"
)

// TestOpenFileCounter_OpenIfActive: OpenIfActive bumps only while count > 0 and
// refuses to resurrect a handle torn down to 0.
func TestOpenFileCounter_OpenIfActive(t *testing.T) {
	c := OpenFileCounter{mu: &sync.Mutex{}}

	// Count is 0 (no live handle): must NOT bump, must report inactive.
	if c.OpenIfActive() {
		t.Fatal("OpenIfActive() = true with count 0; want false (no resurrect)")
	}
	if got := c.CountOpenDescriptors(); got != 0 {
		t.Fatalf("count = %d after refused OpenIfActive; want 0", got)
	}

	// One live handle: OpenIfActive must bump and report active.
	c.Open()
	if !c.OpenIfActive() {
		t.Fatal("OpenIfActive() = false with count 1; want true")
	}
	if got := c.CountOpenDescriptors(); got != 2 {
		t.Fatalf("count = %d after Open+OpenIfActive; want 2", got)
	}

	// Release back down to 0: OpenIfActive must refuse again.
	c.Release()
	c.Release()
	if c.OpenIfActive() {
		t.Fatal("OpenIfActive() = true after count returned to 0; want false")
	}
}

// TestOpenFileCounter_OpenIfActive_Race stresses the check-and-bump under
// concurrency (run with -race): a non-atomic split of the count==0 test and the
// increment would trip the race detector or leave the count unbalanced.
func TestOpenFileCounter_OpenIfActive_Race(t *testing.T) {
	c := OpenFileCounter{mu: &sync.Mutex{}}
	c.Open() // one live handle keeps the count > 0 so every revive succeeds

	const workers = 50
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				if c.OpenIfActive() {
					c.Release() // balance every successful bump
				}
			}
		}()
	}
	wg.Wait()

	if got := c.CountOpenDescriptors(); got != 1 {
		t.Fatalf("count = %d after balanced concurrent OpenIfActive/Release; want 1", got)
	}
}
