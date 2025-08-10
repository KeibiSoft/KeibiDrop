package filesystem

import (
	"path/filepath"
	"sync"

	"github.com/inconshreveable/log15"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

type FS struct {
	logger log15.Logger

	// Add duplex conn.

	// Keep state.

	// Host.
	host *winfuse.FileSystemHost
}

func NewFS(logger log15.Logger) *FS {
	return &FS{
		logger: logger,
	}
}

func (fs *FS) Mount(mountPoint string, isSecond bool, downloadPath string) {
	cleanMountPoint := filepath.Clean(mountPoint)

	nodeGen := NewNodeIDGen(isSecond)

	root := &Dir{
		logger:   fs.logger.New("mount", "root"),
		Inode:    0,
		inodeGen: nodeGen,
		Name:     cleanMountPoint,

		RelativePath:   "/",
		RealPathOfFile: "",
		IsLocalPresent: true,
		PeerLastEdit:   0,
		Parent:         nil,

		// IDK about this one.
		LocalDownloadFolder: filepath.Clean(downloadPath),

		OpenMapLock:      sync.RWMutex{},
		OpenFileHandlers: make(map[uint64]*File),

		fcl:          sync.RWMutex{},
		FileChildren: make(map[uint64]*File),
		dcl:          sync.RWMutex{},
		DirChildren:  make(map[uint64]*Dir),
	}

	root.Root = root

	host := winfuse.NewFileSystemHost(root)

	fs.host = host

	fs.host.Mount(cleanMountPoint, nil)
}

func (fs *FS) Unmount() {
	fs.host.Unmount()
}
