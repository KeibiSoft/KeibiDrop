// mount-fuse is a minimal program that mounts a KeibiDrop FUSE filesystem
// at a given directory without needing a peer connection or relay.
// Used for fstest POSIX compliance testing.
//
// Usage: mount-fuse <mount-point> <save-dir>
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

type stubStreamProvider struct{}

func (s *stubStreamProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return nil, errors.New("no remote peer (standalone mount)")
}

func (s *stubStreamProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	return nil, io.EOF
}

func main() {
	if len(os.Args) < 3 {
		os.Stderr.WriteString("usage: mount-fuse <mount-point> <save-dir>\n")
		os.Exit(1)
	}
	mountPoint := os.Args[1]
	saveDir := os.Args[2]

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fs := filesystem.NewFS(logger)

	fs.OnLocalChange = func(_ types.FileEvent) {}
	fs.OpenStreamProvider = func() types.FileStreamProvider {
		return &stubStreamProvider{}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		logger.Warn("Unmounting...")
		fs.Unmount()
	}()

	fs.Mount(mountPoint, false, saveDir)
}
