package transport

import (
	"context"
	"sync/atomic"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

const defaultChunkSize = 64 << 10 // 64 KiB, a typical file-streaming chunk

// benchService implements pb.BenchServiceServer. The same instance is served over
// both QUIC and TCP so the two transports run identical server code.
//
// sent, if non-nil, records the last completed Download send offset — lightweight
// instrumentation (atomic store, no timing impact) used by the cancel-wedge
// diagnostic to see exactly where the server blocks.
type benchService struct {
	pb.UnimplementedBenchServiceServer
	sent     *atomic.Uint64
	done     *atomic.Bool
	maxStart *atomic.Uint64 // largest Download StartOffset seen; proves a resume happened
}

func (benchService) Echo(_ context.Context, req *pb.EchoRequest) (*pb.EchoReply, error) {
	return &pb.EchoReply{Payload: req.Payload}, nil
}

// Read serves a byte range: the FUSE cache-miss path. The pattern is position-dependent
// (data[i] = byte(offset+i)) so the client can verify it fetched the right bytes.
func (benchService) Read(_ context.Context, req *pb.ReadRequest) (*pb.ReadReply, error) {
	data := make([]byte, req.Length)
	for i := range data {
		data[i] = byte(req.Offset + uint64(i))
	}
	return &pb.ReadReply{Data: data}, nil
}

// verifyRange checks a range read against Read's position-dependent pattern.
func verifyRange(data []byte, offset uint64) bool {
	for i, c := range data {
		if c != byte(offset+uint64(i)) {
			return false
		}
	}
	return true
}

// Download streams req.TotalBytes to the client in chunk_size chunks. Each chunk
// is filled with a deterministic pattern (data[j] = byte(j)) so a receiver can
// verify integrity — the migration test uses this to prove bytes survive a path
// switch. The buffer is filled once and reused, so this stays a throughput test.
//
// The loop checks stream.Context() before every send. This is not optional
// bookkeeping: it is what makes a cancelled or disconnected client stop the
// server promptly. Without it, a handler that blindly sends can bury itself in a
// flow-control-blocked transport write that gRPC cannot interrupt — the bug the
// cancel test exists to catch.
func (s benchService) Download(req *pb.DownloadRequest, stream pb.BenchService_DownloadServer) error {
	if s.done != nil {
		defer s.done.Store(true)
	}
	if s.maxStart != nil && req.StartOffset > s.maxStart.Load() {
		s.maxStart.Store(req.StartOffset)
	}
	ctx := stream.Context()
	chunkSize := int(req.ChunkSize)
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	buf := make([]byte, chunkSize)
	fillPattern(buf)

	// Resume from StartOffset. The pattern is chunk-local (byte(j)), so a chunk-aligned
	// resume produces identical bytes; the absolute Offset rides on each Chunk.
	dbg("Download START total=%d chunk=%d start=%d", req.TotalBytes, chunkSize, req.StartOffset)
	sent := req.StartOffset
	for sent < req.TotalBytes {
		if err := ctx.Err(); err != nil { // client cancelled/disconnected: stop
			dbg("Download ctx done at sent=%d: %v", sent, err)
			return err
		}
		n := chunkSize
		if rem := req.TotalBytes - sent; rem < uint64(n) {
			n = int(rem)
		}
		if err := stream.Send(&pb.Chunk{Data: buf[:n], Offset: sent}); err != nil {
			dbg("Download Send err at sent=%d: %v", sent, err)
			return err
		}
		sent += uint64(n)
		if s.sent != nil {
			s.sent.Store(sent)
		}
	}
	dbg("Download COMPLETE sent=%d", sent)
	return nil
}

// fillPattern writes data[j] = byte(j) so a chunk is cheaply verifiable.
func fillPattern(b []byte) {
	for i := range b {
		b[i] = byte(i)
	}
}

// verifyChunk reports whether a received chunk matches fillPattern's output.
func verifyChunk(data []byte) bool {
	for i, c := range data {
		if c != byte(i) {
			return false
		}
	}
	return true
}
