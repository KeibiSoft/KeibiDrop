package common

import (
	"context"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"google.golang.org/grpc"
)

// Proxy methods to avoid import cycles between the filesystem and the duplex server.

type ImplFileStreamProvider struct {
	cli bindings.KeibiServiceClient
}

func NewImplStreamProvider(cli bindings.KeibiServiceClient) *ImplFileStreamProvider {
	return &ImplFileStreamProvider{cli: cli}
}

type ImplRemoteFileStream struct {
	stream grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse]
	handle uint64
	path   string
}

func NewImplRemoteFileStream(stream grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse], inode uint64, path string) *ImplRemoteFileStream {
	return &ImplRemoteFileStream{
		stream: stream,
		handle: inode,
		path:   path,
	}
}
func (rfs *ImplRemoteFileStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	err := rfs.stream.Send(&bindings.ReadRequest{Handle: rfs.handle, Path: rfs.path, Offset: uint64(offset), Size: uint32(size)})
	if err != nil {
		// TODO: Log err.
		return nil, err
	}

	resp, err := rfs.stream.Recv()
	if err != nil {
		// TODO: Log err.
		return nil, err
	}

	return resp.Data, nil
}

func (rfs *ImplRemoteFileStream) Close() error {
	return rfs.stream.CloseSend()
}

func (sp *ImplFileStreamProvider) OpenRemoteFile(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	stream, err := sp.cli.Read(ctx)
	if err != nil {
		return nil, err
	}

	return NewImplRemoteFileStream(stream, inode, path), nil
}
