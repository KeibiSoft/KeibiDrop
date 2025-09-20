package common

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const Timeout = 10*60 - 5

func (kd *KeibiDrop) ExportFingerprint() (string, error) {
	logger := kd.logger.New("method", "export-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	fp := kd.session.OwnFingerprint

	logger.Info("Success", "fingerprint", fp)

	return fp, nil
}

func (kd *KeibiDrop) AddPeerFingerprint(fp string) error {
	logger := kd.logger.New("method", "add-peer-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	err := ValidateFingerprint(fp)
	if err != nil {
		logger.Error("Failed to validate fingerprint", "error", err)
		return err
	}

	kd.session.ExpectedPeerFingerprint = fp

	return nil
}

func (kd *KeibiDrop) GetPeerFingerprint() (string, error) {
	logger := kd.logger.New("method", "get-peer-fingerprint")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return "", ErrNilPointer
	}

	return kd.session.ExpectedPeerFingerprint, nil
}

func (kd *KeibiDrop) CreateRoom() error {
	logger := kd.logger.New("method", "create-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	if kd.running {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}
	// This is the "Alice" flow

	err := kd.registerRoomToRelay()
	if err != nil {
		return err
	}

	// Get input to for expected fingerprint.

	elapsed := 0
	for {
		if elapsed >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			time.Sleep(time.Second)
			elapsed++
			continue
		}
		break
	}

	// Listen for the Bob Connection.

	// TODO: Add retries
	conn, err := kd.listener.Accept()
	if err != nil {
		logger.Error("Failed to accept", "error", err)
		return err
	}

	// TODO: Add uniform error, use errors.Is(err, ErrCustomDefinedError) to do the retry logic.
	err = session.PerformInboundHandshake(kd.session, conn)
	if err != nil {
		return err
	}

	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		logger.Error("Failed to cast tcp address", "error", err)
		return err
	}

	ip := addr.IP.String()

	err = session.PerformOutboundHandshake(kd.session, net.JoinHostPort(ip, strconv.Itoa(kd.session.PeerPort)))
	if err != nil {
		return err
	}

	logger.Debug("Before start")
	kd.Start()
	logger.Debug("After start")

	err = kd.connectGRPCClient()
	if err != nil {
		logger.Error("Failed to start gRPC client", "error", err)
		return err
	}

	fs := filesystem.NewFS(logger)

	fs.OnLocalChange = func(event types.FileEvent) {
		if kd.session == nil || kd.session.GRPCClient == nil {
			return
		}

		res, err := kd.session.GRPCClient.Notify(context.Background(), &bindings.NotifyRequest{
			Type: bindings.NotifyType(event.Action),
			Path: event.Path,
			Attr: &bindings.Attr{
				Dev:              event.Attr.Dev,
				Ino:              event.Attr.Ino,
				Mode:             event.Attr.Mode,
				Size:             event.Attr.Size,
				AccessTime:       event.Attr.AccessTime,
				ModificationTime: event.Attr.ModificationTime,
				ChangeTime:       event.Attr.ChangeTime,
				BirthTime:        event.Attr.BirthTime,
				Flags:            event.Attr.Flags,
			},
		})
		if err != nil {
			// TODO: Handle errors in caller, and pass logger from call chain
			logger.Error("Failed to notify peer", "error", err)
		}
		_ = res
	}

	fs.OpenStreamProvider = func() types.FileStreamProvider {
		return NewImplStreamProvider(kd.session.GRPCClient)
	}

	kd.FS = fs

	logger.Debug("Before mount")
	fs.Mount(filepath.Clean(kd.ToMount), true, filepath.Clean(kd.ToSave))
	logger.Debug("After mount")

	time.Sleep(time.Second)
	_, err = kd.session.GRPCClient.Debug(context.Background(), &bindings.DebugRequest{})
	if err != nil {
		logger.Error("DEBUG", "error", err)
	}

	logger.Info("Success")
	return nil
}

func (kd *KeibiDrop) JoinRoom(fp string) error {
	logger := kd.logger.New("method", "join-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	if kd.running {
		logger.Warn("Already running, aborting...")
		return ErrAlreadyRunning
	}

	// This is the "Bob" flow

	elapsed := 0
	for {
		if elapsed >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			elapsed++
			time.Sleep(time.Second)
			continue
		}
		break
	}

	err := kd.getRoomFromRelay(kd.session.ExpectedPeerFingerprint)
	if err != nil {
		return err
	}

	err = session.PerformOutboundHandshake(kd.session, net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(kd.session.PeerPort)))
	if err != nil {
		logger.Error("Failed to perform outbound handshake", "error", err)
		return err
	}

	// TODO: Add retries - this is blocking.
	conn, err := kd.listener.Accept()
	if err != nil {
		logger.Error("Failed to accept", "error", err)
		return err
	}

	err = session.PerformInboundHandshake(kd.session, conn)
	if err != nil {
		logger.Error("Failed to perform inbound handhsake", "error", err)
		return err
	}

	logger.Debug("Before start")
	kd.Start()
	logger.Debug("After start")

	err = kd.connectGRPCClient()
	if err != nil {
		logger.Error("Failed to start gRPC client", "error", err)
		return err
	}

	fs := filesystem.NewFS(logger)

	fs.OnLocalChange = func(event types.FileEvent) {
		if kd.session == nil || kd.session.GRPCClient == nil {
			return
		}

		res, err := kd.session.GRPCClient.Notify(context.Background(), &bindings.NotifyRequest{
			Type: bindings.NotifyType(event.Action),
			Path: event.Path,
			Attr: &bindings.Attr{
				Dev:              event.Attr.Dev,
				Ino:              event.Attr.Ino,
				Mode:             event.Attr.Mode,
				Size:             event.Attr.Size,
				AccessTime:       event.Attr.AccessTime,
				ModificationTime: event.Attr.ModificationTime,
				ChangeTime:       event.Attr.ChangeTime,
				BirthTime:        event.Attr.BirthTime,
				Flags:            event.Attr.Flags,
			},
		})
		if err != nil {
			// TODO: Handle errors in caller, and pass logger from call chain
			logger.Error("Failed to notify peer", "error", err)
		}
		_ = res
	}

	fs.OpenStreamProvider = func() types.FileStreamProvider {
		return NewImplStreamProvider(kd.session.GRPCClient)
	}

	kd.FS = fs

	logger.Debug("Before mount")
	fs.Mount(filepath.Clean(kd.ToMount), false, filepath.Clean(kd.ToSave))
	logger.Debug("After mount")

	time.Sleep(time.Second)
	_, err = kd.session.GRPCClient.Debug(context.Background(), &bindings.DebugRequest{})
	if err != nil {
		logger.Error("DEBUG", "error", err)
	}

	logger.Info("Success", "fingerprint", fp)
	return nil
}

// This is blocking.
func (kd *KeibiDrop) MountFilesystem(toMount string, toSave string, isSecond bool) error {
	logger := kd.logger.New("method", "mount-filesystem")
	logger.Info("Mounting virtual filesystem", "virtual-folder", toMount, "passhtrough-folder", toSave, "isSecond", isSecond)
	if kd.session == nil || kd.KDSvc == nil {
		logger.Warn("Session not established", "error", ErrSessionNotEstablished)
		return ErrSessionNotEstablished
	}

	if kd.FS != nil {
		logger.Warn("Filesystem already mounted", "error", ErrFilesystemAlreadyMounted)
		return ErrFilesystemAlreadyMounted
	}

	fs := filesystem.NewFS(logger)
	kd.KDSvc.FS = fs

	fs.Mount(filepath.Clean(toMount), isSecond, filepath.Clean(toSave))

	return nil
}

func (kd *KeibiDrop) UnmountFilesystem() error {
	logger := kd.logger.New("method", "unmonut-filesystem")
	logger.Info("Unmounting virtual filesystem")
	if kd.FS == nil {
		logger.Warn("Nil filesystem", "error", ErrNilFilesystem)
		return ErrNilFilesystem
	}

	if kd.KDSvc != nil && kd.KDSvc.FS != nil {
		kd.KDSvc.FS = nil
	}

	kd.FS.Unmount()
	kd.FS = nil

	logger.Info("Success")
	return nil
}

/*
	func (kd *KeibiDrop) ResetSession() {
		kd.logger.Info("Resetting session state")

		// You probably want to close any existing net.Conn, etc.
	}

	func (kd *KeibiDrop) RegenerateKeys() error {
		kd.logger.Info("Regenerating ephemeral keys")

		return nil
	}
*/
func (kd *KeibiDrop) connectGRPCClient() error {
	// Your custom dialer using pre-established SecureConn
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return kd.session.Session.Outbound, nil
	}

	// Create the gRPC client connection
	conn, err := grpc.Dial(
		"keibipipe", // fake target name, not used by your dialer
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()), // you're doing your own encryption
	)
	if err != nil {
		return fmt.Errorf("failed to create gRPC client: %w", err)
	}

	/*
		// Wait until connection is ready
		for conn.GetState() != connectivity.Ready {
			if !conn.WaitForStateChange(context.Background(), conn.GetState()) {
				return fmt.Errorf("gRPC never reached READY state")
			}
		}
	*/

	// Store the typed client
	kd.session.GRPCClient = bindings.NewKeibiServiceClient(conn)
	kd.KDClient = kd.session.GRPCClient

	return nil
}

func (kd *KeibiDrop) startGRPCServer() error {
	kd.session.GRPCListener = kd.session.Session.Inbound

	grpcServer := grpc.NewServer()
	kd.grpcServer = grpcServer

	ln := NewSingleConnListener(kd.session.Session.Inbound)

	svc := &service.KeibidropServiceImpl{
		Session: kd.session,
		Logger:  kd.logger.New("component", "keibidrop-server"),
	}

	kd.KDSvc = svc
	bindings.RegisterKeibiServiceServer(grpcServer, svc)

	kd.logger.Info("Starting gRPC server...")
	if err := grpcServer.Serve(ln); err != nil {
		kd.logger.Error("gRPC server exited with error", "err", err)
	}

	return nil
}

type singleConnListener struct {
	conn   net.Conn
	done   chan struct{}
	inUse  bool
	closed chan struct{}
}

func NewSingleConnListener(conn net.Conn) net.Listener {
	return &singleConnListener{
		conn:   conn,
		done:   make(chan struct{}),
		closed: make(chan struct{}),
		inUse:  false,
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, io.EOF
	default:
		if l.inUse {
			<-l.closed
			return nil, nil
		}
		l.inUse = true
		conn := l.conn
		l.conn = nil
		return conn, nil
	}
}

func (l *singleConnListener) Close() error {
	if l.conn != nil {
		close(l.closed)
		close(l.done)
		return l.conn.Close()
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return &net.TCPAddr{IP: net.IPv6loopback, Port: 0}
}
