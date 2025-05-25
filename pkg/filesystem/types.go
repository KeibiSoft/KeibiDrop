package filesystem

type Dir struct {
	Inode uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name  string `json:"name"`

	RelativePath   string `json:"relativePath"`      // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string `json:"pathOnLocalSystem"` // The Path on the local system.

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	Parent *Dir
	Root   *Dir

	FileChildren []*File
	DirChildren  []*Dir
}

type File struct {
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
}
