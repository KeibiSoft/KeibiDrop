package common

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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
	logger := kd.logger.New("method", "add-per-fingerprint")
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

func (kd *KeibiDrop) CreateRoom() error {
	logger := kd.logger.New("method", "create-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
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

	logger.Info("Success")
	return nil
}

func (kd *KeibiDrop) JoinRoom(fp string) error {
	logger := kd.logger.New("method", "join-room")
	if kd.session == nil {
		logger.Warn("Nil pointer deference")
		return ErrNilPointer
	}

	// This is the "Bob" flow

	sec := 0
	for {
		if sec >= Timeout {
			logger.Error("Timeout reached", "error", ErrTimeoutReached)
			return ErrTimeoutReached
		}
		if kd.session.ExpectedPeerFingerprint == "" {
			sec++
			time.Sleep(time.Second)
			continue
		}
		break
	}

	err := kd.getRoomFromRelay(kd.session.ExpectedPeerFingerprint)
	if err != nil {
		return err
	}

	err = session.PerformOutboundHandshake(kd.session, net.JoinHostPort(kd.peerIPv6IP, strconv.Itoa(kd.session.PeerPort)))
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

	logger.Info("Success", "fingerprint", fp)
	return nil
}

func (kd *KeibiDrop) MountFilesystem() error {
	kd.logger.Info("Mounting virtual filesystem")
	return nil
}

func (kd *KeibiDrop) UnmountFilesystem() error {
	kd.logger.Info("Unmounting virtual filesystem")
	return nil
}

func (kd *KeibiDrop) ResetSession() {
	kd.logger.Info("Resetting session state")

	// You probably want to close any existing net.Conn, etc.
}

func (kd *KeibiDrop) RegenerateKeys() error {
	kd.logger.Info("Regenerating ephemeral keys")

	return nil
}

func (kd *KeibiDrop) connectGRPCClient() error {
	// Your custom dialer using pre-established SecureConn
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return kd.session.Session.Outbound, nil
	}

	// Create the gRPC client connection
	conn, err := grpc.NewClient(
		"keibipipe", // fake target name, not used by your dialer
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()), // you're doing your own encryption
	)
	if err != nil {
		return fmt.Errorf("failed to create gRPC client: %w", err)
	}

	// Wait until connection is ready
	for conn.GetState() != connectivity.Ready {
		if !conn.WaitForStateChange(context.Background(), conn.GetState()) {
			return fmt.Errorf("gRPC never reached READY state")
		}
	}

	// Store the typed client
	kd.session.GRPCClient = bindings.NewKeibiServiceClient(conn)
	return nil
}

func (kd *KeibiDrop) startGRPCServer() error {
	listener := kd.session.GRPCListener
	if listener == nil {
		return fmt.Errorf("GRPCListener not initialized in session")
	}

	grpcServer := grpc.NewServer()
	bindings.RegisterKeibiServiceServer(grpcServer, &service.KeibidropServiceImpl{
		Session: kd.session,
		Logger:  kd.logger.New("component", "keibidrop-server"),
	})

	go func() {
		kd.logger.Info("Starting gRPC server...")
		if err := grpcServer.Serve(listener); err != nil {
			kd.logger.Error("gRPC server exited with error", "err", err)
		}
	}()

	return nil
}
