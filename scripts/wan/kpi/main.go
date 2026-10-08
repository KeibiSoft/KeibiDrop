// kpi measures what an on-demand filesystem is actually judged on: whether a
// paced reader stutters, and how long a seek takes while a bulk read saturates
// the link. Bulk MB/s is deliberately NOT the headline here.
//
// Method copied from tests/streaming_rekey_test.go's paced Blu-ray test and
// from the seek-latency measurement in the gRPC-over-QUIC post, so the numbers
// are comparable with the ones already published.
//
//	kpi -mode paced -file <path-on-mount> -bitrate 3 -block 262144
//	kpi -mode seek  -file <path-on-mount> -reads 30 -size 16384
//
// Run it against a file on the FUSE mount, never a local copy: the point is the
// read that has to cross the wire.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync/atomic"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run holds the body so defers actually run: os.Exit skips them, and gocritic is
// right to refuse the combination.
func run() error {
	mode := flag.String("mode", "paced", "paced | seek")
	path := flag.String("file", "", "file on the FUSE mount")
	bitrateMB := flag.Float64("bitrate", 3, "playback rate in MB/s (paced mode)")
	block := flag.Int("block", 256<<10, "read block in bytes (paced mode)")
	jitterMS := flag.Int("jitter", 2000, "player buffer in ms; a longer read is a freeze")
	reads := flag.Int("reads", 30, "number of small reads (seek mode)")
	size := flag.Int("size", 16<<10, "small read size in bytes (seek mode)")
	windowMB := flag.Int("window", 64, "read-ahead window in MB; seek offsets inside it are redrawn")
	bulk := flag.Bool("bulk", true, "run a saturating bulk reader during seek mode; false measures the idle-link floor")
	flag.Parse()

	if *path == "" {
		return fmt.Errorf("-file is required")
	}
	f, err := os.Open(*path) //#nosec G304 -- operator-supplied test path
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	switch *mode {
	case "paced":
		paced(f, st.Size(), *bitrateMB, *block, time.Duration(*jitterMS)*time.Millisecond)
	case "seek":
		seek(f, st.Size(), *reads, *size, *windowMB, *bulk)
	default:
		return fmt.Errorf("mode must be paced or seek")
	}
	return nil
}

// paced reads the file at a playback rate and reports every read that missed the
// budget the player had for it. A read that beats its budget sleeps out the
// difference, exactly as a player would; a read that misses skips the sleep,
// which is what drains a real jitter buffer.
func paced(f *os.File, total int64, bitrateMB float64, block int, jitter time.Duration) {
	bps := bitrateMB * 1024 * 1024
	blockInterval := time.Duration(float64(block) / bps * float64(time.Second))
	buf := make([]byte, block)

	var freezes int
	var frozen, worst time.Duration
	var worstOff, off int64
	start := time.Now()

	for off < total {
		t0 := time.Now()
		n, rerr := f.Read(buf)
		fetch := time.Since(t0)
		if n <= 0 || rerr != nil {
			break
		}
		if fetch > worst {
			worst, worstOff = fetch, off
		}
		if fetch > jitter {
			freezes++
			frozen += fetch - jitter
		}
		off += int64(n)
		if fetch < blockInterval {
			time.Sleep(blockInterval - fetch)
		}
	}

	wall := time.Since(start)
	ideal := time.Duration(float64(off) / bps * float64(time.Second))
	fmt.Printf("PACED  %.0f MiB @ %.1f MB/s, block %d KiB, buffer %s\n",
		float64(off)/(1<<20), bitrateMB, block>>10, jitter)
	fmt.Printf("  freezes      %d\n", freezes)
	fmt.Printf("  frozen total %s\n", frozen.Round(time.Millisecond))
	fmt.Printf("  worst read   %s at offset %d MiB\n", worst.Round(time.Millisecond), worstOff>>20)
	fmt.Printf("  wall %s vs ideal %s (%.1fx real time)\n",
		wall.Round(time.Millisecond), ideal.Round(time.Millisecond),
		wall.Seconds()/ideal.Seconds())
}

// seek measures small random reads while a bulk reader saturates the link, which
// is the head-of-line question: does the seek queue behind the prefetch.
//
// Offsets inside the bulk reader's read-ahead window are REDRAWN. A seek that lands in
// already-prefetched bytes measures the page cache, not the link, and it silently flatters
// the result: with uniform offsets over 700 MB and a 64 MiB window, about one seek in six
// is a free hit.
func seek(f *os.File, total int64, n, size, windowMB int, withBulk bool) {
	var bulkPos atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // the saturating bulk reader
		defer close(done)
		if !withBulk {
			<-stop
			return
		}
		g, err := os.Open(f.Name()) //#nosec G304 -- same operator-supplied path
		if err != nil {
			return
		}
		defer g.Close() //nolint:errcheck // read-only handle
		buf := make([]byte, 1<<20)
		var at int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			r, err := g.Read(buf)
			if r > 0 {
				at += int64(r)
				bulkPos.Store(at)
			}
			if err != nil {
				if _, err := g.Seek(0, 0); err != nil {
					return
				}
				at = 0
				bulkPos.Store(0)
			}
		}
	}()
	if withBulk {
		time.Sleep(2 * time.Second) // let the bulk reader fill the pipe
	}

	window := int64(windowMB) << 20
	buf := make([]byte, size)
	lat := make([]time.Duration, 0, n)
	rnd := rand.New(rand.NewSource(1)) //#nosec G404 -- offsets only, not crypto
	span := total - int64(size)
	var redraws int
	for i := 0; i < n; i++ {
		var off int64
		for try := 0; ; try++ {
			off = rnd.Int63n(span)
			at := bulkPos.Load()
			if off < at-window || off > at+window {
				break // clear of the bulk reader and its read-ahead
			}
			redraws++
			if try >= 64 { // window covers the file: take it and say so
				break
			}
		}
		t0 := time.Now()
		if _, err := f.ReadAt(buf, off); err != nil {
			continue
		}
		lat = append(lat, time.Since(t0))
		time.Sleep(100 * time.Millisecond) // a human scrubbing, not a tight loop
	}
	close(stop)
	<-done

	if len(lat) == 0 {
		fmt.Println("SEEK  no successful reads")
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) time.Duration {
		i := int(q * float64(len(lat)-1))
		return lat[i]
	}
	load := "under a saturating bulk read"
	if !withBulk {
		load = "on an idle link (no bulk reader)"
	}
	fmt.Printf("SEEK  %d reads of %d KiB at random offsets, %s\n", len(lat), size>>10, load)
	fmt.Printf("  offsets kept clear of the bulk reader +/- %d MB (%d redraws)\n", windowMB, redraws)
	fmt.Printf("  p50 %s\n", p(0.50).Round(time.Millisecond))
	fmt.Printf("  p99 %s\n", p(0.99).Round(time.Millisecond))
	fmt.Printf("  max %s\n", lat[len(lat)-1].Round(time.Millisecond))
}
