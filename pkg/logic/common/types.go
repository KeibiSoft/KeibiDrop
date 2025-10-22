package common

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"google.golang.org/grpc"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

type KeibiDrop struct {
	logger       *slog.Logger
	relayClient  *http.Client
	RelayEndoint *url.URL

	OpInProgress atomic.Int32

	session *session.Session

	PeerIPv6IP string

	LocalIPv6IP string
	inboundPort int
	listener    net.Listener

	// Filesystem.
	FS       *filesystem.FS
	KDSvc    *service.KeibidropServiceImpl
	KDClient bindings.KeibiServiceClient

	// Non-FUSE fallback.
	SyncTracker *synctracker.SyncTracker

	// Paths for virtual mount point and for save folder.
	ToMount string
	ToSave  string

	// Signals for loop management.
	signals chan TaskSignal
	running bool
	ctx     context.Context
	mu      sync.Mutex

	// For session refresh.
	refreshSession func() *session.Session

	// For stopping the grpc server.
	grpcServer      *grpc.Server
	filesystemReady chan struct{}
	serverReadyMu   sync.Mutex
}

type TaskSignal int

const (
	Start TaskSignal = iota
	Stop
)

// Factory-style constructor
func NewKeibiDrop(ctx context.Context, logger *slog.Logger, relayURL *url.URL, inboundPort int, defaultOutboundPort int, toMount string, toSave string) (*KeibiDrop, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	refreshSession := func() *session.Session {
		session, err := session.InitSession(logger, defaultOutboundPort, inboundPort)
		if err != nil {
			logger.Error("Failed to init session", "error", err)
			return nil
		}

		return session
	}

	session, err := session.InitSession(logger, defaultOutboundPort, inboundPort)
	if err != nil {
		logger.Error("Failed to init session", "error", err)
		return nil, err
	}

	addr := net.JoinHostPort("::", strconv.Itoa(inboundPort))
	listener, err := net.Listen("tcp6", addr)
	if err != nil {
		return nil, err
	}

	ipv6, err := GetGlobalIPv6() //GetLocalIPv6()
	if err != nil {
		logger.Error("Failed to get local IPv6", "error", err)
		return nil, err
	}

	kd := &KeibiDrop{
		logger:          logger,
		relayClient:     client,
		RelayEndoint:    relayURL,
		session:         session,
		LocalIPv6IP:     ipv6,
		inboundPort:     inboundPort,
		listener:        listener,
		signals:         make(chan TaskSignal, 2),
		running:         false,
		ctx:             ctx,
		mu:              sync.Mutex{},
		refreshSession:  refreshSession,
		ToMount:         toMount,
		ToSave:          toSave,
		filesystemReady: make(chan struct{}),
		serverReadyMu:   sync.Mutex{},
		SyncTracker:     synctracker.NewSyncTracker(),
	}

	return kd, nil
}

type PeerRegistration struct {
	Fingerprint string            `json:"fingerprint"`
	PublicKeys  map[string]string `json:"public_keys"` // base64 encoded
	Listen      *ConnectionHint   `json:"listen"`
	Reverse     *ConnectionHint   `json:"reverse,omitempty"`
	Timestamp   int64             `json:"timestamp"`
}

type ConnectionHint struct {
	IP    string `json:"ip"`             // public IP address (either v4 or v6)
	Port  int    `json:"port"`           // where peer is listening
	IPv6  bool   `json:"ipv6"`           // does this prefer IPv6?
	Proto string `json:"proto"`          // e.g., "tcp"
	Note  string `json:"note,omitempty"` // optional: NAT behavior, etc.
}

// Map server status errors to semantic errors.
type ErrorMapperFunc func(statusCode int, err error) error

// Running process for KeibiDrop.
func (kd *KeibiDrop) Start() {
	kd.signals <- Start
}

func (kd *KeibiDrop) Stop() {
	kd.signals <- Stop
}

// Run as a go-routine.
func (kd *KeibiDrop) Run() {
	logger := kd.logger.With("method", "run-state")
	for {
		select {
		case <-kd.ctx.Done():
			logger.Info("Stopping KeibiDrop run instance")
			if kd.FS != nil {
				kd.FS.Unmount()
				kd.FS = nil
			}
			if kd.grpcServer != nil {
				kd.grpcServer.GracefulStop()
				kd.grpcServer = nil
			}
			return
		case s := <-kd.signals:
			switch s {
			case Start:
				logger.Info("Signal start")
				if kd.session == nil || kd.session.Session == nil || kd.session.Session.Inbound == nil {
					logger.Warn("Nil session")
					continue
				}

				logger.Info("Signal start success")

				kd.filesystemReady = make(chan struct{})
				// prepare serverReady channel
				go func() {
					err := kd.startGRPCServer()
					if err != nil {
						logger.Error("Failed to start gRPC server", "error", err)
					}
				}()

				// mount filesystem here
				<-kd.filesystemReady

				if kd.FS != nil {
					logger.Info("Mounting filesystem", "mount", kd.ToMount, "save", kd.ToSave)
					kd.KDSvc.FS = kd.FS
					kd.FS.Mount(filepath.Clean(kd.ToMount), false, filepath.Clean(kd.ToSave))
					logger.Info("Filesystem mounted successfully")
				} else {
					logger.Warn("No FS to mount")
				}

				kd.running = true

			case Stop:
				logger.Info("Stop signal")
				if kd.FS != nil {
					kd.FS.Unmount()
					kd.FS = nil
				}
				if kd.grpcServer != nil {
					kd.grpcServer.GracefulStop()
					kd.grpcServer = nil
				}

				// Close and nil gRPC client
				if kd.KDClient != nil {
					kd.KDClient = nil
				}

				if kd.KDSvc != nil {
					kd.KDSvc = nil
				}

				if kd.session != nil {
					kd.session = nil
				}

				kd.PeerIPv6IP = ""
				kd.session = kd.refreshSession()

				kd.running = false
			}
		}
	}
}
