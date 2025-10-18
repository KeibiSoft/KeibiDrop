package filesystem

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/inconshreveable/log15"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

type FS struct {
	logger log15.Logger

	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider

	// Host.
	host *winfuse.FileSystemHost
	Root *Dir
}

func NewFS(logger log15.Logger) *FS {
	return &FS{
		logger: logger,
	}
}

func (fs *FS) Mount(mountPoint string, isSecond bool, downloadPath string) {
	cleanMountPoint := filepath.Clean(mountPoint)
	pt := strings.Split(cleanMountPoint, "/")
	if len(pt) < 1 {
		return
	}

	root := &Dir{
		logger: fs.logger.New("mount", "root"),
		Inode:  0,
		Name:   "",

		RelativePath:   "/",
		RealPathOfFile: downloadPath,

		IsLocalPresent: true,
		PeerLastEdit:   0,
		Parent:         nil,

		// IDK about this one.
		LocalDownloadFolder: filepath.Clean(downloadPath),

		OpenMapLock:      sync.RWMutex{},
		OpenFileHandlers: make(map[uint64]*File),

		Adm:       sync.RWMutex{},
		AllDirMap: make(map[string]*Dir),

		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*File),

		OnLocalChange:      fs.OnLocalChange,
		OpenStreamProvider: fs.OpenStreamProvider,
	}

	root.Root = root
	fs.Root = root

	host := winfuse.NewFileSystemHost(root)

	// I think this is windows specific.
	host.SetCapReaddirPlus(true)
	// Fuse3 only.
	host.SetUseIno(true)

	fs.host = host

	// opts := []string{"volname=KeibiDrop", "local"}
	fs.host.Mount(cleanMountPoint, nil)
}

func (fs *FS) Unmount() {
	if fs.host == nil {
		return
	}

	// TODO: Also call umount on the MountPath in case its stuck or something.

	fs.host.Unmount()
	fs.Root = nil
}
