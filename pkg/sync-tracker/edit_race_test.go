// ABOUTME: Race-detection test for concurrent read+write on SyncTracker entries.
// ABOUTME: Validates the locking contract; callers (service_android.go) must use Lock() for writes.

package synctracker

import (
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

func TestConcurrentEditAndRead_Race(t *testing.T) {
	st := NewSyncTracker()
	st.RemoteFiles["/test.txt"] = &File{
		Name:         "test.txt",
		RelativePath: "/test.txt",
		Size:         1000,
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)

		go func(n int) {
			defer wg.Done()
			st.RemoteFilesMu.Lock()
			f, ok := st.RemoteFiles["/test.txt"]
			if ok {
				f.Name = "test.txt"
				f.Size = uint64(n * 100)
				f.LastEditTime = uint64(n)
			}
			st.RemoteFilesMu.Unlock()
		}(i)

		go func() {
			defer wg.Done()
			st.RemoteFilesMu.RLock()
			f := st.RemoteFiles["/test.txt"]
			if f != nil {
				_ = f.Size
				_ = f.Name
				_ = f.LastEditTime
			}
			st.RemoteFilesMu.RUnlock()
		}()
	}
	wg.Wait()

	st.RemoteFilesMu.RLock()
	f := st.RemoteFiles["/test.txt"]
	st.RemoteFilesMu.RUnlock()

	// Dependent: the second check dereferences f, so it must not run when
	// f is nil. Steps short-circuits, All would not.
	testkit.Run(t, func() error {
		return fp.Steps(
			func() error { return fp.True("file still exists", f != nil) },
			func() error { return fp.True("a writer updated the fields", f.Size != 0 || f.LastEditTime != 0) },
		)
	})
}
