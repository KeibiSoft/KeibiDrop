package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
	cpb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto/control"
)

// Manager owns one peer's always-on QUIC control plane and picks the data-plane
// transport. The control plane is always QUIC: its migration lets a session survive a
// network change, and it carries the heartbeat that measures link quality. It exposes
// live RTT, the clean/degraded verdict, the migration trigger and event, and the
// bulk-transport routing decision.
//
// The control plane runs gRPC over the PQC handshake over QUIC (SecureKD), on an
// explicit quic.Transport so the connection can migrate (AddPath needs it).
type Manager struct {
	id         *Identity
	peerFP     string
	serverAddr *net.UDPAddr

	cc    *grpc.ClientConn
	ctl   cpb.ControlClient
	qconn *quic.Conn // the control connection (identity survives migration)

	mu         sync.Mutex
	transports []*quic.Transport // kept alive; quic-go ties conn lifetime to them
	onChange   func(NetworkEvent)
	bulkCC     *grpc.ClientConn // TCP bulk channel
	bulkAddr   string           // remote TCP addr, for reconstruct-on-network-change

	rtt      atomic.Int64 // last heartbeat RTT in ns; <0 means the link is dead
	hbEvery  time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

// Quality is the coarse link verdict the data plane routes on. Two buckets is enough:
// the interactive path is QUIC either way, only the bulk transport flips.
type Quality int

const (
	QualityUnknown  Quality = iota // no heartbeat sample yet
	QualityClean                   // low RTT (LAN/datacenter)
	QualityDegraded                // high RTT or jitter (Wi-Fi/WAN)
	QualityDead                    // heartbeat is failing
)

func (q Quality) String() string {
	switch q {
	case QualityClean:
		return "clean"
	case QualityDegraded:
		return "degraded"
	case QualityDead:
		return "dead"
	default:
		return "unknown"
	}
}

// CleanRTTThreshold splits clean from degraded. RTT is a proxy; a loss signal refines
// this later.
const CleanRTTThreshold = 20 * time.Millisecond

// DefaultHeartbeat is the control-plane ping interval.
const DefaultHeartbeat = 2 * time.Second

// NetworkEvent is delivered to the OnNetworkChange callback when the control plane's
// local path changes (a migration completed).
type NetworkEvent struct {
	OldLocalAddr net.Addr
	NewLocalAddr net.Addr
}

// DialControl brings up the QUIC control plane to peerAddr ("host:port"), runs the PQC
// handshake (authenticating against peerFP), does an initial heartbeat (seeds RTT,
// captures the migratable conn), and starts the heartbeat loop.
func DialControl(ctx context.Context, peerAddr string, id *Identity, peerFP string) (*Manager, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", peerAddr, err)
	}
	tr, err := newUDPTransport(serverAddr)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}

	m := &Manager{
		id:         id,
		peerFP:     peerFP,
		serverAddr: serverAddr,
		transports: []*quic.Transport{tr},
		hbEvery:    DefaultHeartbeat,
		stop:       make(chan struct{}),
	}

	qconnCh := make(chan *quic.Conn, 1)
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		qc, err := tr.Dial(ctx, serverAddr, clientTLSConfig(), quicConfig())
		if err != nil {
			return nil, err
		}
		st, err := qc.OpenStreamSync(ctx)
		if err != nil {
			_ = qc.CloseWithError(0, "")
			return nil, err
		}
		sec, err := SecureKD(newQUICConn(qc, st), id, peerFP, RoleClient)
		if err != nil {
			_ = qc.CloseWithError(0, "")
			return nil, err
		}
		select {
		case qconnCh <- qc:
		default:
		}
		return sec, nil
	}

	cc, err := grpc.NewClient("passthrough:///control",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer))
	if err != nil {
		_ = tr.Close()
		return nil, fmt.Errorf("control client: %w", err)
	}
	m.cc = cc
	m.ctl = cpb.NewControlClient(cc)

	// Initial heartbeat: triggers the lazy dial + handshake and seeds the RTT.
	t0 := time.Now()
	if err := m.pingOnce(ctx); err != nil {
		_ = cc.Close()
		_ = tr.Close()
		return nil, fmt.Errorf("control handshake/ping: %w", err)
	}
	m.rtt.Store(int64(time.Since(t0)))

	select {
	case m.qconn = <-qconnCh:
	case <-time.After(5 * time.Second):
		_ = cc.Close()
		_ = tr.Close()
		return nil, fmt.Errorf("control conn never materialized")
	}

	go m.heartbeatLoop()
	return m, nil
}

func (m *Manager) pingOnce(ctx context.Context) error {
	_, err := m.ctl.Ping(ctx, &cpb.PingRequest{})
	return err
}

func (m *Manager) heartbeatLoop() {
	t := time.NewTicker(m.hbEvery)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			t0 := time.Now()
			err := m.pingOnce(ctx)
			cancel()
			if err != nil {
				m.rtt.Store(-1) // mark the link dead
				continue
			}
			m.rtt.Store(int64(time.Since(t0)))
		}
	}
}

// RTT returns the last measured control-plane round trip, or -1 if the link is dead.
func (m *Manager) RTT() time.Duration {
	v := m.rtt.Load()
	if v < 0 {
		return -1
	}
	return time.Duration(v)
}

// LinkQuality is the coarse verdict the bulk data plane routes on.
func (m *Manager) LinkQuality() Quality {
	v := m.rtt.Load()
	switch {
	case v < 0:
		return QualityDead
	case v == 0:
		return QualityUnknown
	case time.Duration(v) < CleanRTTThreshold:
		return QualityClean
	default:
		return QualityDegraded
	}
}

// BulkTransport is the routing decision for a bulk (whole-file) transfer, from the
// measured link quality. Interactive/seek reads always use QUIC and are not routed here.
func (m *Manager) BulkTransport() Transport {
	return bulkTransportFor(m.LinkQuality())
}

// quicBulkViable reports whether QUIC bulk is competitive with TCP. Today it is not
// (quic-go measured 2.7-16x slower than kernel TCP, syscall-bound on macOS), so bulk is
// TCP everywhere. Flips to true once sendmsg_x is wired into the send loop.
var quicBulkViable = false

// bulkTransportFor is the pure routing rule, split out to be testable without a network.
// Only a clean link with viable QUIC bulk routes to QUIC; everything else uses TCP.
func bulkTransportFor(q Quality) Transport {
	if quicBulkViable && q == QualityClean {
		return QUIC()
	}
	return TCP()
}

// BulkGet streams `total` bytes from the peer's TCP bulk channel into onChunk, resuming
// from the last received offset if the channel is reconstructed mid-transfer (e.g. by a
// network change). Reissues Download with StartOffset = bytes received, so a network
// change costs a re-dial and resume, not a restart. Bounded by ctx.
func (m *Manager) BulkGet(ctx context.Context, total uint64, chunkSize uint32, onChunk func(offset uint64, data []byte)) error {
	var got uint64
	for got < total {
		if err := ctx.Err(); err != nil {
			return err
		}
		cc := m.BulkConn()
		if cc == nil {
			if !sleepCtx(ctx, 50*time.Millisecond) { // channel reconstructing; wait
				return ctx.Err()
			}
			continue
		}
		stream, err := pb.NewBenchServiceClient(cc).Download(ctx, &pb.DownloadRequest{
			TotalBytes: total, ChunkSize: chunkSize, StartOffset: got,
		})
		if err != nil {
			if !sleepCtx(ctx, 50*time.Millisecond) {
				return ctx.Err()
			}
			continue // retry from got
		}
		for {
			chunk, rerr := stream.Recv()
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				break // interrupted; the outer loop resumes from got
			}
			if onChunk != nil {
				onChunk(chunk.Offset, chunk.Data)
			}
			got += uint64(len(chunk.Data))
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// InteractiveConn returns the QUIC gRPC channel, shared by the control heartbeat and
// latency-sensitive interactive reads (seeks/cache misses). Bulk runs on the separate
// TCP channel, so an interactive read is isolated from bulk by transport: a cache miss
// is served in about one round trip instead of queuing behind bulk. The Manager owns
// this conn; do not Close it directly (Close the Manager).
func (m *Manager) InteractiveConn() *grpc.ClientConn { return m.cc }

// MissRead fetches a byte range over the QUIC interactive channel: the FUSE cache-miss
// path. Isolated by transport from the TCP bulk transfer, so a miss lands in about one
// round trip even while bulk saturates TCP.
func (m *Manager) MissRead(ctx context.Context, offset uint64, length uint32) ([]byte, error) {
	reply, err := pb.NewBenchServiceClient(m.InteractiveConn()).Read(ctx, &pb.ReadRequest{Offset: offset, Length: length})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

// DialBulk brings up the TCP gRPC channel to the peer's bulk address, which measured
// faster than QUIC for bulk on lossy links. Idempotent; cached and reused. On a network
// change the Manager reconstructs this channel (its TCP 5-tuple dies with the address
// change, which the QUIC control plane survives).
func (m *Manager) DialBulk(ctx context.Context, tcpAddr string) error {
	m.mu.Lock()
	if m.bulkCC != nil {
		m.mu.Unlock()
		return nil
	}
	m.bulkAddr = tcpAddr
	m.mu.Unlock()

	cc, err := DialGRPCKDOver(TCP(), tcpAddr, m.id, m.peerFP)
	if err != nil {
		return fmt.Errorf("dial bulk: %w", err)
	}
	m.mu.Lock()
	m.bulkCC = cc
	m.mu.Unlock()
	return nil
}

// BulkConn returns the TCP bulk channel, or nil until DialBulk succeeds.
func (m *Manager) BulkConn() *grpc.ClientConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bulkCC
}

// reconstructBulk rebuilds the TCP bulk channel after a network change: closing the old
// conn aborts its in-flight RPCs (the caller retries from the last acked offset), then
// re-dials the same remote from the new local address. Best-effort.
func (m *Manager) reconstructBulk(ctx context.Context) {
	m.mu.Lock()
	old, addr := m.bulkCC, m.bulkAddr
	m.mu.Unlock()
	if old == nil {
		return
	}
	_ = old.Close() // aborts in-flight bulk RPCs
	cc, err := DialGRPCKDOver(TCP(), addr, m.id, m.peerFP)
	m.mu.Lock()
	m.bulkCC = cc // nil on failure
	m.mu.Unlock()
	_ = err
}

// OnNetworkChange registers a callback fired when the control plane migrates to a new
// local path.
func (m *Manager) OnNetworkChange(cb func(NetworkEvent)) {
	m.mu.Lock()
	m.onChange = cb
	m.mu.Unlock()
}

// Migrate moves the control connection to a fresh local UDP socket (AddPath -> Probe ->
// Switch), then fires the network-change event. The old transport is kept alive:
// quic-go ties the connection's lifetime to it.
func (m *Manager) Migrate(ctx context.Context) error {
	if m.qconn == nil {
		return fmt.Errorf("no control connection to migrate")
	}
	before := m.qconn.LocalAddr()

	newTr, err := migratePath(ctx, m.qconn, m.serverAddr)
	if err != nil {
		return err
	}

	// The client must send from the new address so the peer migrates its send path too.
	for i := 0; i < 10; i++ {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := m.pingOnce(pctx)
		cancel()
		if err != nil {
			return fmt.Errorf("post-switch ping: %w", err)
		}
	}

	m.mu.Lock()
	m.transports = append(m.transports, newTr)
	cb := m.onChange
	m.mu.Unlock()

	// The QUIC control plane migrated in place; the TCP bulk channel's 5-tuple died with
	// the address change, so reconstruct it now.
	m.reconstructBulk(ctx)

	after := m.qconn.LocalAddr()
	if cb != nil {
		cb(NetworkEvent{OldLocalAddr: before, NewLocalAddr: after})
	}
	return nil
}

// Close stops the heartbeat and tears down the control plane.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	if m.cc != nil {
		_ = m.cc.Close()
	}
	m.mu.Lock()
	trs := m.transports
	bulk := m.bulkCC
	m.mu.Unlock()
	if bulk != nil {
		_ = bulk.Close()
	}
	for _, tr := range trs {
		_ = tr.Close()
	}
	return nil
}

// newUDPTransport binds a local UDP socket toward serverAddr and wraps it in an explicit
// quic.Transport (required for migration). Loopback targets bind to the loopback of the
// same family (an IPv4 socket cannot reach ::1); others bind to the unspecified address.
func newUDPTransport(serverAddr *net.UDPAddr) (*quic.Transport, error) {
	var bind *net.UDPAddr
	if serverAddr.IP != nil && serverAddr.IP.IsLoopback() {
		if serverAddr.IP.To4() != nil {
			bind = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
		} else {
			bind = &net.UDPAddr{IP: net.IPv6loopback}
		}
	}
	udp, err := net.ListenUDP("udp", bind)
	if err != nil {
		return nil, err
	}
	return &quic.Transport{Conn: udp}, nil
}
