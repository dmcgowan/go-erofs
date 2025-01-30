package disk

const (
	MagicNumber      = 0xe0f5e1e2
	SuperBlockOffset = 1024

	SizeSuperBlock    = 128
	SizeInodeCompact  = 32
	SizeInodeExtended = 64
	SizeDirent        = 12
)

type SuperBlock struct {
	MagicNumber      uint32
	Checksum         uint32
	FeatureCompat    uint32
	BlkSizeBits      uint8
	ExtSlots         uint8
	RootNid          uint16
	Inos             uint64
	BuildTime        uint64
	BuildTimeNs      uint32
	Blocks           uint32
	MetaBlkAddr      uint32
	XattrBlkAddr     uint32
	UUID             [16]uint8
	VolumeName       [16]uint8
	FeatureIncompat  uint32
	ComprAlgs        uint16
	ExtraDevices     uint16
	DevtSlotOff      uint16
	DirBlkBits       uint8
	XattrPrefixCount uint8
	XattrPrefixStart uint32
	PackedNid        uint64
	XattrFilterRes   uint8
	Reserved         [23]uint8
}

type InodeCompact struct {
	Format       uint16
	XattrCount   uint16
	Mode         uint16
	Nlink        uint16
	Size         uint32
	Reserved     uint32
	RawBlockAddr int32
	RawBlockSize uint16
	Inode        uint32
	UID          uint16
	GID          uint16
	Reserved2    uint32
}

type InodeExtended struct {
	Format       uint16
	XattrCount   uint16
	Mode         uint16
	Reserved     uint16
	Size         uint64
	RawBlockAddr int32
	Inode        uint32
	UID          uint32
	GID          uint32
	Mtime        uint64
	MtimeNs      uint32
	Nlink        uint32
	Reserved2    [16]uint8
}

type Dirent struct {
	Nid      uint64
	NameOff  uint16
	FileType uint8
	Reserved uint8
}

// XattrHeader is the header after an inode containing xattr information
//
// Original defintion:
// inline xattrs (n == i_xattr_icount):
// erofs_xattr_ibody_header(1) + (n - 1) * 4 bytes
//
//	12 bytes           /                   \
//	                  /                     \
//	                 /-----------------------\
//	                 |  erofs_xattr_entries+ |
//	                 +-----------------------+
//
// inline xattrs must starts in erofs_xattr_ibody_header,
// for read-only fs, no need to introduce h_refcount
type XattrHeader struct {
	NameFilter   uint32 // bit value 1 indicate not-present
	SharedCount  uint8
	Resolved     [7]uint8
	SharedXattrs []uint32 // Variable in length, must be initialized before decode
}
