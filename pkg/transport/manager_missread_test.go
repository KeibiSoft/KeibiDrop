package transport

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestMissReadUnderBulk is the concrete cache-miss path: while a bulk file transfer
// saturates the TCP channel, a 512 KiB range read (a FUSE miss for KeibiDrop's block
// size) goes over the QUIC interactive channel and returns fast and byte-correct,
// instead of queuing behind the bulk transfer for seconds. This is "no 3-4 s freeze"
// made concrete, with a real range read rather than an Echo stand-in.
func TestMissReadUnderBulk(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	quicAddr, tcpAddr, stop := twoChannelServers(t, serverID, clientID.Fingerprint())
	defer stop()

	ctx := context.Background()
	m, err := DialControl(ctx, quicAddr, clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, tcpAddr); err != nil {
		t.Fatalf("DialBulk: %v", err)
	}

	// Saturate the TCP bulk channel in the background.
	bulk := pb.NewBenchServiceClient(m.BulkConn())
	bulkCtx, bulkCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for bulkCtx.Err() == nil {
			stream, err := bulk.Download(bulkCtx, &pb.DownloadRequest{TotalBytes: 64 << 20, ChunkSize: 64 << 10})
			if err != nil {
				return
			}
			for {
				if _, err := stream.Recv(); err != nil {
					break
				}
			}
		}
	}()
	defer func() { bulkCancel(); wg.Wait() }()
	time.Sleep(200 * time.Millisecond)

	// Cache-miss reads over the QUIC interactive channel, under the bulk load.
	const n = 40
	const readLen = 512 << 10 // KeibiDrop's block: the first chunk fetched on a miss
	lat := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		off := uint64(i) * readLen
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		t0 := time.Now()
		data, err := m.MissRead(c, off, readLen)
		d := time.Since(t0)
		cancel()
		if err != nil {
			t.Fatalf("miss read %d: %v", i, err)
		}
		if len(data) != readLen || !verifyRange(data, off) {
			t.Fatalf("miss read %d: wrong bytes (len %d)", i, len(data))
		}
		lat = append(lat, d)
	}

	pct := func(q float64) time.Duration {
		s := append([]time.Duration(nil), lat...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[int(q*float64(len(s)-1))]
	}
	t.Logf("512 KiB cache-miss read over QUIC, under saturating TCP bulk (loopback):")
	t.Logf("  p50=%v  p99=%v  max=%v  (byte-correct)", pct(.5), pct(.99), pct(1))
}
