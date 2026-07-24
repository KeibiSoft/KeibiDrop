package transport

import (
	"context"
	"testing"
)

// BenchmarkBulkGet measures the resumable bulk path (Manager.BulkGet over TCP).
// Compare to BenchmarkDownload/tcp: the resume wrapper should add no throughput cost,
// because a clean transfer never takes the retry branch.
func BenchmarkBulkGet(b *testing.B) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	qsrv, qaddr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		b.Fatal(err)
	}
	defer qsrv.Stop()
	tsrv, taddr, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		qsrv.Stop()
		b.Fatal(err)
	}
	defer tsrv.Stop()

	ctx := context.Background()
	m, err := DialControl(ctx, qaddr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, taddr.String()); err != nil {
		b.Fatal(err)
	}

	const total = 64 << 20 // 64 MiB per op
	b.SetBytes(total)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var got uint64
		if err := m.BulkGet(ctx, total, 64<<10, func(_ uint64, data []byte) { got += uint64(len(data)) }); err != nil {
			b.Fatal(err)
		}
		if got != total {
			b.Fatalf("short read: %d/%d", got, total)
		}
	}
}
