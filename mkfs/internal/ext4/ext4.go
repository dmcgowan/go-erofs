// Package ext4 reads ext4 filesystem structures from a block image.
package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// On-disk constants.
const (
	SuperBlockOffset = 1024

	MagicNumber = 0xEF53

	InodeFlagExtents = 0x00080000

	Feature64Bit = 0x0080

	RootInode = 2

	ExtentMagic = 0xF30A

	ModeTypeMask = 0xF000
	ModeTypeReg  = 0x8000
	ModeTypeDir  = 0x4000
	ModeTypeChr  = 0x2000
	ModeTypeBlk  = 0x6000
	ModeTypeFifo = 0x1000
	ModeTypeSock = 0xC000
	ModeTypeLnk  = 0xA000
)

// SuperBlock holds the fields we need from the ext4 superblock.
type SuperBlock struct {
	InodesCount     uint32
	BlocksCountLo   uint32
	FirstDataBlock  uint32
	LogBlockSize    uint32
	BlocksPerGroup  uint32
	InodesPerGroup  uint32
	Magic           uint16
	InodeSize       uint16
	FeatureIncompat uint32
	DescSize        uint16
	BlocksCountHi   uint32

	BlockSize uint32 // computed
	Is64Bit   bool   // computed
}

// Extent is a leaf extent entry from the extent tree.
type Extent struct {
	LogicalBlock uint32
	Count        uint16
	PhysicalHi   uint16
	PhysicalLo   uint32
}

// Physical returns the 48-bit physical block address.
func (e *Extent) Physical() uint64 {
	return (uint64(e.PhysicalHi) << 32) | uint64(e.PhysicalLo)
}

type extentHeader struct {
	Magic   uint16
	Entries uint16
	Max     uint16
	Depth   uint16
}

type extentIndex struct {
	LogicalBlock uint32
	LeafHi       uint16
	LeafLo       uint32
}

func (idx *extentIndex) leaf() uint64 {
	return (uint64(idx.LeafHi) << 32) | uint64(idx.LeafLo)
}

// Inode holds the fields we need from an ext4 inode.
type Inode struct {
	Mode       uint16
	UID        uint16
	SizeLo     uint32
	Mtime      uint32
	GID        uint16
	LinksCount uint16
	Flags      uint32
	Block      [60]byte
	SizeHi     uint32
	FileACL    uint32 // block number of external xattr block
	Osd2       [12]byte
	ExtraISize uint16 // bytes used beyond 128-byte base inode
	Raw        []byte // full on-disk inode bytes (for inline xattrs)
}

// Size returns the full 64-bit file size.
func (ino *Inode) Size() uint64 {
	return (uint64(ino.SizeHi) << 32) | uint64(ino.SizeLo)
}

// FullUID returns the 32-bit UID (lo from i_uid, hi from osd2).
func (ino *Inode) FullUID() uint32 {
	hi := uint32(ino.Osd2[4]) | uint32(ino.Osd2[5])<<8
	return (hi << 16) | uint32(ino.UID)
}

// FullGID returns the 32-bit GID (lo from i_gid, hi from osd2).
func (ino *Inode) FullGID() uint32 {
	hi := uint32(ino.Osd2[6]) | uint32(ino.Osd2[7])<<8
	return (hi << 16) | uint32(ino.GID)
}

// Rdev returns the device number stored in i_block.
func (ino *Inode) Rdev() uint32 {
	raw := binary.LittleEndian.Uint32(ino.Block[0:4])
	if raw != 0 {
		return raw
	}
	return binary.LittleEndian.Uint32(ino.Block[4:8])
}

// DirEntry represents a parsed ext4 directory entry.
type DirEntry struct {
	InodeNum uint32
	Name     string
	FileType uint8
}

// Reader reads ext4 structures from a block image.
type Reader struct {
	r  io.ReaderAt
	sb SuperBlock
}

// NewReader opens an ext4 image for reading.
func NewReader(r io.ReaderAt) (*Reader, error) {
	er := &Reader{r: r}
	if err := er.readSuperBlock(); err != nil {
		return nil, err
	}
	return er, nil
}

// BlocksCount returns the total block count.
func (er *Reader) BlocksCount() uint64 {
	if er.sb.Is64Bit {
		return (uint64(er.sb.BlocksCountHi) << 32) | uint64(er.sb.BlocksCountLo)
	}
	return uint64(er.sb.BlocksCountLo)
}

// ReaderAt returns the underlying io.ReaderAt.
func (er *Reader) ReaderAt() io.ReaderAt { return er.r }

// BlockSize returns the filesystem block size.
func (er *Reader) BlockSize() uint32 { return er.sb.BlockSize }

func (er *Reader) readSuperBlock() error {
	var raw [1024]byte
	if _, err := er.r.ReadAt(raw[:], SuperBlockOffset); err != nil {
		return fmt.Errorf("ext4: read superblock: %w", err)
	}

	sb := &er.sb
	sb.InodesCount = binary.LittleEndian.Uint32(raw[0:4])
	sb.BlocksCountLo = binary.LittleEndian.Uint32(raw[4:8])
	sb.FirstDataBlock = binary.LittleEndian.Uint32(raw[20:24])
	sb.LogBlockSize = binary.LittleEndian.Uint32(raw[24:28])
	sb.BlocksPerGroup = binary.LittleEndian.Uint32(raw[32:36])
	sb.InodesPerGroup = binary.LittleEndian.Uint32(raw[40:44])
	sb.Magic = binary.LittleEndian.Uint16(raw[56:58])
	sb.InodeSize = binary.LittleEndian.Uint16(raw[88:90])
	sb.FeatureIncompat = binary.LittleEndian.Uint32(raw[96:100])
	sb.DescSize = binary.LittleEndian.Uint16(raw[254:256])
	sb.BlocksCountHi = binary.LittleEndian.Uint32(raw[336:340])

	if sb.Magic != MagicNumber {
		return fmt.Errorf("ext4: invalid magic %#x", sb.Magic)
	}

	sb.BlockSize = 1024 << sb.LogBlockSize
	sb.Is64Bit = sb.FeatureIncompat&Feature64Bit != 0

	if sb.BlockSize != 4096 {
		return fmt.Errorf("ext4: unsupported block size %d (only 4096 supported)", sb.BlockSize)
	}

	return nil
}

func (er *Reader) inodeTableBlock(group uint32) (uint64, error) {
	descSize := uint32(32)
	if er.sb.Is64Bit && er.sb.DescSize > 32 {
		descSize = uint32(er.sb.DescSize)
	}

	descBlockStart := uint64(er.sb.FirstDataBlock+1) * uint64(er.sb.BlockSize)
	descOff := descBlockStart + uint64(group)*uint64(descSize)

	var buf [64]byte
	readLen := min(descSize, 64)
	if _, err := er.r.ReadAt(buf[:readLen], int64(descOff)); err != nil {
		return 0, fmt.Errorf("ext4: read group desc %d: %w", group, err)
	}

	lo := uint64(binary.LittleEndian.Uint32(buf[8:12]))
	if er.sb.Is64Bit && descSize >= 64 {
		hi := uint64(binary.LittleEndian.Uint32(buf[40:44]))
		return (hi << 32) | lo, nil
	}
	return lo, nil
}

// ReadInode reads an ext4 inode by number.
func (er *Reader) ReadInode(inum uint32) (*Inode, error) {
	if inum == 0 {
		return nil, fmt.Errorf("ext4: invalid inode 0")
	}
	group := (inum - 1) / er.sb.InodesPerGroup
	index := (inum - 1) % er.sb.InodesPerGroup

	tableBlock, err := er.inodeTableBlock(group)
	if err != nil {
		return nil, err
	}

	offset := int64(tableBlock)*int64(er.sb.BlockSize) + int64(index)*int64(er.sb.InodeSize)
	buf := make([]byte, er.sb.InodeSize)
	if _, err := er.r.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("ext4: read inode %d: %w", inum, err)
	}

	ino := &Inode{Raw: buf}
	ino.Mode = binary.LittleEndian.Uint16(buf[0:2])
	ino.UID = binary.LittleEndian.Uint16(buf[2:4])
	ino.SizeLo = binary.LittleEndian.Uint32(buf[4:8])
	ino.Mtime = binary.LittleEndian.Uint32(buf[16:20])
	ino.GID = binary.LittleEndian.Uint16(buf[24:26])
	ino.LinksCount = binary.LittleEndian.Uint16(buf[26:28])
	ino.Flags = binary.LittleEndian.Uint32(buf[32:36])
	copy(ino.Block[:], buf[40:100])
	ino.FileACL = binary.LittleEndian.Uint32(buf[104:108])
	ino.SizeHi = binary.LittleEndian.Uint32(buf[108:112])
	copy(ino.Osd2[:], buf[116:128])
	if len(buf) > 128 {
		ino.ExtraISize = binary.LittleEndian.Uint16(buf[128:130])
	}

	return ino, nil
}

// ReadExtents traverses the extent tree and returns all leaf extents, sorted by logical block.
func (er *Reader) ReadExtents(ino *Inode) ([]Extent, error) {
	if ino.Flags&InodeFlagExtents == 0 {
		return nil, fmt.Errorf("ext4: inode does not use extents (flags=%#x)", ino.Flags)
	}
	return er.walkExtentTree(ino.Block[:])
}

func (er *Reader) walkExtentTree(data []byte) ([]Extent, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("ext4: extent data too short")
	}

	var hdr extentHeader
	hdr.Magic = binary.LittleEndian.Uint16(data[0:2])
	hdr.Entries = binary.LittleEndian.Uint16(data[2:4])
	hdr.Depth = binary.LittleEndian.Uint16(data[6:8])

	if hdr.Magic != ExtentMagic {
		return nil, fmt.Errorf("ext4: invalid extent magic %#x", hdr.Magic)
	}

	if hdr.Depth == 0 {
		extents := make([]Extent, 0, hdr.Entries)
		for i := range int(hdr.Entries) {
			off := 12 + i*12
			if off+12 > len(data) {
				break
			}
			var ext Extent
			ext.LogicalBlock = binary.LittleEndian.Uint32(data[off : off+4])
			ext.Count = binary.LittleEndian.Uint16(data[off+4 : off+6])
			ext.PhysicalHi = binary.LittleEndian.Uint16(data[off+6 : off+8])
			ext.PhysicalLo = binary.LittleEndian.Uint32(data[off+8 : off+12])
			extents = append(extents, ext)
		}
		return extents, nil
	}

	var allExtents []Extent
	for i := range int(hdr.Entries) {
		off := 12 + i*12
		if off+12 > len(data) {
			break
		}
		var idx extentIndex
		idx.LogicalBlock = binary.LittleEndian.Uint32(data[off : off+4])
		idx.LeafHi = binary.LittleEndian.Uint16(data[off+4 : off+6])
		idx.LeafLo = binary.LittleEndian.Uint32(data[off+6 : off+10])

		childBlock := idx.leaf()
		childOff := int64(childBlock) * int64(er.sb.BlockSize)
		childData := make([]byte, er.sb.BlockSize)
		if _, err := er.r.ReadAt(childData, childOff); err != nil {
			return nil, fmt.Errorf("ext4: read extent tree node at block %d: %w", childBlock, err)
		}

		childExtents, err := er.walkExtentTree(childData)
		if err != nil {
			return nil, err
		}
		allExtents = append(allExtents, childExtents...)
	}

	sort.Slice(allExtents, func(i, j int) bool {
		return allExtents[i].LogicalBlock < allExtents[j].LogicalBlock
	})

	return allExtents, nil
}

// ReadSymlink returns the symlink target for an inode.
func (er *Reader) ReadSymlink(ino *Inode) (string, error) {
	size := ino.Size()
	if size == 0 {
		return "", nil
	}

	if ino.Flags&InodeFlagExtents == 0 && size < 60 {
		return string(ino.Block[:size]), nil
	}

	extents, err := er.ReadExtents(ino)
	if err != nil {
		return "", fmt.Errorf("ext4: read symlink extents: %w", err)
	}

	target := make([]byte, size)
	for _, ext := range extents {
		off := int64(ext.Physical()) * int64(er.sb.BlockSize)
		start := uint64(ext.LogicalBlock) * uint64(er.sb.BlockSize)
		count := uint64(ext.Count) * uint64(er.sb.BlockSize)
		if start >= size {
			break
		}
		if start+count > size {
			count = size - start
		}
		if _, err := er.r.ReadAt(target[start:start+count], off); err != nil {
			return "", fmt.Errorf("ext4: read symlink data: %w", err)
		}
	}

	return string(target), nil
}

// ReadDirEntries reads directory entries from an ext4 directory inode.
func (er *Reader) ReadDirEntries(ino *Inode) ([]DirEntry, error) {
	size := ino.Size()
	if size == 0 {
		return nil, nil
	}

	extents, err := er.ReadExtents(ino)
	if err != nil {
		return nil, fmt.Errorf("ext4: read dir extents: %w", err)
	}

	var entries []DirEntry
	for _, ext := range extents {
		for b := uint32(0); b < uint32(ext.Count); b++ {
			blockAddr := ext.Physical() + uint64(b)
			blockOff := int64(blockAddr) * int64(er.sb.BlockSize)
			blockData := make([]byte, er.sb.BlockSize)
			if _, err := er.r.ReadAt(blockData, blockOff); err != nil {
				return nil, fmt.Errorf("ext4: read dir block: %w", err)
			}

			ents := parseDirBlock(blockData)
			entries = append(entries, ents...)
		}
	}

	return entries, nil
}

func parseDirBlock(data []byte) []DirEntry {
	var entries []DirEntry
	pos := 0
	for pos+8 <= len(data) {
		inum := binary.LittleEndian.Uint32(data[pos : pos+4])
		recLen := binary.LittleEndian.Uint16(data[pos+4 : pos+6])
		nameLen := data[pos+6]
		fileType := data[pos+7]

		if recLen == 0 {
			break
		}

		if inum != 0 && nameLen > 0 {
			nameEnd := pos + 8 + int(nameLen)
			if nameEnd > len(data) {
				break
			}
			entries = append(entries, DirEntry{
				InodeNum: inum,
				Name:     string(data[pos+8 : nameEnd]),
				FileType: fileType,
			})
		}

		pos += int(recLen)
	}
	return entries
}

// xattr constants.
const (
	xattrMagic      = 0xEA020000
	xattrHeaderSize = 32 // ext4_xattr_header (external block)
	xattrIbodySize  = 4  // ext4_xattr_ibody_header (inline)
	xattrEntrySize  = 16 // minimum ext4_xattr_entry size (before name)

	goodOldInodeSize = 128
)

// xattr name index → prefix mapping.
var xattrPrefixes = map[uint8]string{
	1: "user.",
	2: "system.posix_acl_access",
	3: "system.posix_acl_default",
	4: "trusted.",
	6: "security.",
	7: "system.",
}

// ReadXattrs reads extended attributes from both inline (in-inode) and
// external (i_file_acl block) locations.
func (er *Reader) ReadXattrs(ino *Inode) (map[string]string, error) {
	xattrs := make(map[string]string)

	// 1. Inline xattrs: stored in inode after the base 128 bytes + i_extra_isize.
	if ino.ExtraISize > 0 && len(ino.Raw) > goodOldInodeSize {
		start := goodOldInodeSize + int(ino.ExtraISize)
		if start+xattrIbodySize <= len(ino.Raw) {
			magic := binary.LittleEndian.Uint32(ino.Raw[start : start+4])
			if magic == xattrMagic {
				entries := ino.Raw[start+xattrIbodySize:]
				// For inline xattrs, values are stored relative to the
				// start of the first entry (not the ibody header).
				parseXattrEntries(entries, entries, xattrs)
			}
		}
	}

	// 2. External xattr block.
	if ino.FileACL != 0 {
		blockOff := int64(ino.FileACL) * int64(er.sb.BlockSize)
		block := make([]byte, er.sb.BlockSize)
		if _, err := er.r.ReadAt(block, blockOff); err != nil {
			return xattrs, fmt.Errorf("ext4: read xattr block %d: %w", ino.FileACL, err)
		}
		magic := binary.LittleEndian.Uint32(block[0:4])
		if magic == xattrMagic {
			entries := block[xattrHeaderSize:]
			// For block xattrs, value offsets are relative to the
			// start of the block (including the header).
			parseXattrEntries(entries, block, xattrs)
		}
	}

	return xattrs, nil
}

// parseXattrEntries parses ext4_xattr_entry structs from data and stores
// results in xattrs. valueBase is the byte slice that e_value_offs indexes
// into (for inline: the entry area; for block: the full block).
func parseXattrEntries(data, valueBase []byte, xattrs map[string]string) {
	pos := 0
	for pos+xattrEntrySize <= len(data) {
		nameLen := data[pos]
		nameIndex := data[pos+1]

		// End of list: first 4 bytes are all zero.
		if binary.LittleEndian.Uint32(data[pos:pos+4]) == 0 {
			break
		}

		valueOffs := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
		valueSize := binary.LittleEndian.Uint32(data[pos+8 : pos+12])

		nameStart := pos + xattrEntrySize
		nameEnd := nameStart + int(nameLen)
		if nameEnd > len(data) {
			break
		}
		name := string(data[nameStart:nameEnd])

		// Prepend the prefix for the name index.
		if prefix, ok := xattrPrefixes[nameIndex]; ok {
			name = prefix + name
		}

		// Read value.
		vOff := int(valueOffs)
		vEnd := vOff + int(valueSize)
		if vOff >= 0 && vEnd <= len(valueBase) {
			xattrs[name] = string(valueBase[vOff:vEnd])
		}

		// Entries are 4-byte aligned.
		entryLen := xattrEntrySize + int(nameLen)
		entryLen = (entryLen + 3) &^ 3
		pos += entryLen
	}
}
