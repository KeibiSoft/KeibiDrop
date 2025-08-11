package filesystem

import (
	"sync"

	"github.com/inconshreveable/log15"
)

// The plan is like this:
// Mounted filesystem for Alice is visible at "mountedPath".
// When Alice adds files from her machine to the filesystem,
// *Blip blip blop*, a new File{} gets created which represents
// a "symlink" to the original file which Bob can now access.
// For folders it's a bit different, as it does not create a "symlink"
// in the sense that Bob can navigate outside of the mapped folder
// but in the sense that Dir{} is mapped to the underlying one
// and all the children of the underlying one are mapped inside our Dir{}.

// Note: I use a tree hierarchy, not the most efficient way when it comes to lookups.
// I might flatten it in the future.
// Root -> Dir -> Dir -> File
//    | -> File
//    | -> Dir -> File

type Dir struct {
	logger log15.Logger

	inodeGen NodeIDGen
	Inode    uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name     string `json:"name"`

	RelativePath   string `json:"relativePath"`      // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string `json:"pathOnLocalSystem"` // The Path on the local system.

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	LocalDownloadFolder string // The folder where the files from the peer are downloaded.

	Parent *Dir
	Root   *Dir

	OpenFileHandlers map[uint64]*File
	OpenMapLock      sync.RWMutex

	FileChildren map[uint64]*File // {Inode: *File}
	fcl          sync.RWMutex

	DirChildren map[uint64]*Dir // {Inode: *Dir}
	dcl         sync.RWMutex
}

type File struct {
	logger log15.Logger

	inodeGen NodeIDGen

	Inode uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name  string `json:"name"`

	RelativePath   string `json:"relativePath"`      // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string `json:"pathOnLocalSystem"` // The Path on the local system.

	Parent *Dir
	Root   *Dir

	LastEditTime uint64 `json:"lastEdit"` // Use time.Now().UnixNano().
	CreatedTime  uint64 `json:"createdAt"`

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	openFileCounter OpenFileCounter
}

// Inodes must be unique.
// But we have a shared filesystem between two people.
// We shift left Bobs initial state.
// Note that root is always 0 for both Alice and Bob.
type NodeIDGen struct {
	mu    *sync.Mutex
	state *uint64
}

// Use it as a singleton only when setting up the filesystem.
// (In the mount command).
// I do not enforce it as a singleton, as my philospohy
// is to not have package global var, just a
// call chain of functions from the entrypoint of
// the program.

func NewNodeIDGen(isSecond bool) NodeIDGen {
	mu := sync.Mutex{}
	st := uint64(0)
	/*
		if isSecond {
			st = st << 16
		}
	*/
	return NodeIDGen{
		mu:    &mu,
		state: &st,
	}
}

func (n *NodeIDGen) Generate() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	*n.state++
	return *n.state
}

// Create and Open calls must have a corresponding Release call.
type OpenFileCounter struct {
	mu      *sync.Mutex
	counter uint64
}

func (ofc *OpenFileCounter) Open() {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	ofc.counter++
}

func (ofc *OpenFileCounter) Release() uint64 {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	if ofc.counter == 0 {
		return 0
	}

	ofc.counter--
	return ofc.counter
}

func (ofc *OpenFileCounter) CountOpenDescriptors() uint64 {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	return ofc.counter
}
