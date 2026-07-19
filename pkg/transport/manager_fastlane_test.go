package transport

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// twoChannelServers starts the QUIC server (control + interactive) and the TCP server
// (bulk) for one peer identity, and returns their addresses. Mirrors a real peer
// listening on both a UDP (QUIC) and TCP endpoint.
func twoChannelServers(t *testing.T, serverID *Identity, clientFP string) (quicAddr, tcpAddr string, stop func()) {
	t.Helper()
	qsrv, qa, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientFP)
	if err != nil {
		t.Fatalf("quic server: %v", err)
	}
	tsrv, ta, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{}, serverID, clientFP)
	if err != nil {
		qsrv.Stop()
		t.Fatalf("tcp server: %v", err)
	}
	return qa.String(), ta.String(), func() { qsrv.Stop(); tsrv.Stop() }
}

// TestInteractiveFastLane is the two-channel model's payoff: with bulk saturating the
// TCP channel, a seek on the QUIC channel is isolated from it by transport, while the
// same seek muxed onto the busy TCP channel head-of-line blocks. This is the
// library-level version of urgent_bench (109 ms vs 3066 ms on a real WAN). On loopback
// the absolute gap is smaller (no bottleneck to congest), so it reports both and
// asserts correctness under load; the decisive magnitude is the WAN result in FINDINGS.
func TestInteractiveFastLane(t *testing.T) {
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

	bulk := pb.NewBenchServiceClient(m.BulkConn())
	inter := pb.NewBenchServiceClient(m.InteractiveConn())

	// Saturate the TCP bulk channel in the background.
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

	time.Sleep(200 * time.Millisecond) // let the saturator fill the pipe

	measure := func(echo func(context.Context) error) []time.Duration {
		const n = 40
		lat := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			t0 := time.Now()
			err := echo(c)
			d := time.Since(t0)
			cancel()
			if err != nil {
				t.Fatalf("urgent echo %d: %v", i, err)
			}
			lat = append(lat, d)
		}
		return lat
	}

	// Seek muxed onto the busy TCP bulk channel (head-of-line blocking).
	muxed := measure(func(c context.Context) error {
		_, err := bulk.Echo(c, &pb.EchoRequest{Payload: []byte("u")})
		return err
	})
	// Seek on the QUIC channel: isolated from TCP bulk by transport (the fast lane).
	fast := measure(func(c context.Context) error {
		_, err := inter.Echo(c, &pb.EchoRequest{Payload: []byte("u")})
		return err
	})

	pct := func(d []time.Duration, q float64) time.Duration {
		s := append([]time.Duration(nil), d...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[int(q*float64(len(s)-1))]
	}
	t.Logf("seek under saturating bulk (loopback):")
	t.Logf("  muxed on TCP bulk (HoL): p50=%v p99=%v max=%v", pct(muxed, .5), pct(muxed, .99), pct(muxed, 1))
	t.Logf("  QUIC fast lane         : p50=%v p99=%v max=%v", pct(fast, .5), pct(fast, .99), pct(fast, 1))
	if pct(fast, .5) > pct(muxed, .5) {
		t.Logf("  note: fast-lane p50 not lower on loopback (no bottleneck to congest; WAN is where it shows, see FINDINGS)")
	}
}

// TestManagerBulkReconstructOnMigrate proves the network-change reaction: after the
// QUIC control plane migrates, the TCP bulk channel (whose 5-tuple died) is
// reconstructed and works again, and it is a different connection than before (the old
// one was closed, aborting its in-flight RPCs).
func TestManagerBulkReconstructOnMigrate(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	quicAddr, tcpAddr, stop := twoChannelServers(t, serverID, clientID.Fingerprint())
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := DialControl(ctx, quicAddr, clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, tcpAddr); err != nil {
		t.Fatalf("DialBulk: %v", err)
	}

	before := m.BulkConn()
	if _, err := pb.NewBenchServiceClient(before).Echo(ctx, &pb.EchoRequest{Payload: []byte("pre")}); err != nil {
		t.Fatalf("bulk echo before migration: %v", err)
	}

	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	after := m.BulkConn()
	if after == nil {
		t.Fatal("bulk channel was not reconstructed after migration")
	}
	if after == before {
		t.Fatal("bulk channel is the same conn; it was not reconstructed")
	}
	if _, err := pb.NewBenchServiceClient(after).Echo(ctx, &pb.EchoRequest{Payload: []byte("post")}); err != nil {
		t.Fatalf("bulk echo after reconstruction: %v", err)
	}
}

// TestBulkResumeAcrossReconstruct proves resume-by-offset: a bulk transfer interrupted
// mid-flight by a channel reconstruction (the network-change case) resumes from the
// last received offset instead of restarting. It asserts completion, byte-correctness,
// contiguous offsets, and that the server actually saw a StartOffset > 0 (a real
// resume, not a silent restart).
func TestBulkResumeAcrossReconstruct(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()

	qsrv, qaddr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		t.Fatalf("quic server: %v", err)
	}
	defer qsrv.Stop()
	var maxStart atomic.Uint64
	tsrv, taddr, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{maxStart: &maxStart}, serverID, clientID.Fingerprint())
	if err != nil {
		qsrv.Stop()
		t.Fatalf("tcp server: %v", err)
	}
	defer tsrv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := DialControl(ctx, qaddr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, taddr.String()); err != nil {
		t.Fatalf("DialBulk: %v", err)
	}

	const total = 32 << 20
	const chunk = 64 << 10
	var received uint64
	interrupted := false
	err = m.BulkGet(ctx, total, chunk, func(off uint64, data []byte) {
		if !verifyChunk(data) {
			t.Errorf("corrupt chunk at offset %d", off)
		}
		if off != received {
			t.Errorf("offset gap: chunk offset=%d but received=%d", off, received)
		}
		received += uint64(len(data))
		if !interrupted && received >= total/2 {
			interrupted = true
			m.reconstructBulk(ctx) // simulate a network-change reconstruction mid-transfer
		}
	})
	if err != nil {
		t.Fatalf("BulkGet: %v", err)
	}
	if received != total {
		t.Fatalf("incomplete transfer: got %d want %d", received, total)
	}
	if maxStart.Load() == 0 {
		t.Fatal("no resume happened: server never saw StartOffset > 0 (restart, or never interrupted)")
	}
	t.Logf("interrupted mid-transfer, resumed from offset %d of %d, completed byte-correct", maxStart.Load(), uint64(total))
}
