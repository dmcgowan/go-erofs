package erofs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/dmcgowan/go-erofs/internal/disk"
)

type Stat struct {
	InodeLayout  int8
	XattrCount   int16
	Mode         fs.FileMode
	Size         int64
	RawBlockAddr int32
	Inode        int64
	UID          uint32
	GID          uint32
	Mtime        uint64
	MtimeNs      uint32
	Nlink        int
}

// EroFS returns a FileSystem reading from the given readerat.
// The readerat must be a valid erofs block file.
// No additional memory mapping is done and must be handled by
// the caller.
func EroFS(r io.ReaderAt) (fs.FS, error) {
	var superBlock [disk.SizeSuperBlock]byte
	n, err := r.ReadAt(superBlock[:], disk.SuperBlockOffset)
	if err != nil {
		return nil, err
	}

	if n != disk.SizeSuperBlock {
		return nil, fmt.Errorf("invalid super block: read %d bytes", n)
	}

	i := image{
		meta: r,
	}
	if err = decodeSuperBlock(superBlock, &i.sb); err != nil {
		return nil, err
	}

	return &i, nil
}

type image struct {
	sb disk.SuperBlock

	meta io.ReaderAt
}

func (i *image) dirEntry(nid uint64, name string) (uint64, fs.FileMode, error) {
	return 0, 0, errors.New("direntry: not implemented")
}

func (i *image) Open(name string) (fs.File, error) {
	var err error
	original := name
	if filepath.IsAbs(name) {
		name, err = filepath.Rel("/", name)
		if err != nil {
			return nil, err
		}
	} else {
		name = filepath.Clean(name)
	}
	if name == "." {
		name = ""
	}

	nid := uint64(i.sb.RootNid)
	ftype := fs.ModeDir

	basename := name
	for name != "" {
		var sep int
		for sep < len(name) && !os.IsPathSeparator(name[sep]) {
			sep++
		}
		if sep < len(name) {
			basename = name[:sep]
			name = name[sep+1:]
		} else {
			basename = name
			name = ""
		}

		if ftype != fs.ModeDir {
			// TODO: Path error
			return nil, errors.New("not a directory")
		}
		dir := &dir{
			base: base{
				name:    basename,
				blkAddr: i.sb.MetaBlkAddr,
				blkSize: uint32(2 ^ i.sb.BlkSizeBits),
				inode:   nid,
				ftype:   ftype,
				meta:    i.meta,
			},
		}
		entries, err := dir.ReadDir(-1)
		if err != nil {
			return nil, fmt.Errorf("failed to read dir: %w", err)
		}
		var found bool
		for _, e := range entries {
			if e.Name() == basename {
				nid = uint64(e.(*direntry).base.inode)
				ftype = e.(*direntry).base.ftype & fs.ModeType
				found = true
			}
		}
		if !found {
			return nil, errors.New("directory not found")
		}
	}

	if basename == "" {
		basename = original
	}

	b := base{
		name:    basename,
		blkAddr: i.sb.MetaBlkAddr,
		blkSize: uint32(2 ^ i.sb.BlkSizeBits),
		inode:   nid,
		ftype:   ftype,
		meta:    i.meta,
	}
	if ftype.IsDir() {
		return &dir{base: b}, nil
	}

	return &b, nil
}

type base struct {
	name    string
	blkAddr uint32
	blkSize uint32
	inode   uint64
	ftype   fs.FileMode
	meta    io.ReaderAt
}

func (b *base) readInfo() (*fileInfo, error) {
	var ino [disk.SizeInodeExtended]byte
	_, err := b.meta.ReadAt(ino[:], int64(b.blkAddr)*int64(b.blkSize)+int64(b.inode*disk.SizeInodeCompact))
	if err != nil {
		return nil, err
	}
	// TODO: Check bytes read, force 32 read to be compact
	var format uint16
	if _, err := binary.Decode(ino[:2], binary.LittleEndian, &format); err != nil {
		return nil, err
	}

	// Layout
	// 0: Flat Plain
	// 2: Flat Inline
	// 4: Chunk based
	layout := int8((format & 0x0E) >> 1)
	if layout != 0 && layout != 2 {
		return nil, fmt.Errorf("not supported layout, only flat supported: %d", layout)
	}
	if format&0x01 == 0 {
		var inode disk.InodeCompact
		if _, err := binary.Decode(ino[:disk.SizeInodeCompact], binary.LittleEndian, &inode); err != nil {
			return nil, err
		}
		return &fileInfo{
			name:  b.name,
			isize: disk.SizeInodeCompact,
			size:  int64(inode.Size),
			mode:  (fs.FileMode(inode.Mode) & ^fs.ModeType) | b.ftype,
			//modTime: time.Unix(int64(inode.Mtime), int64(inode.MtimeNs)),
			// TODO: Set mtime to zero value?
			stat: &Stat{
				InodeLayout:  layout,
				XattrCount:   int16(inode.XattrCount),
				Mode:         fs.FileMode(inode.Mode),
				Size:         int64(inode.Size),
				RawBlockAddr: inode.RawBlockAddr,
				Inode:        int64(inode.Inode),
				UID:          uint32(inode.UID),
				GID:          uint32(inode.GID),
				Nlink:        int(inode.Nlink),
				//Mtime        uint64
				//MtimeNs      uint32
			},
		}, nil
	} else {
		var inode disk.InodeExtended
		if _, err := binary.Decode(ino[:disk.SizeInodeExtended], binary.LittleEndian, &inode); err != nil {
			return nil, err
		}
		return &fileInfo{
			name:    b.name,
			isize:   disk.SizeInodeExtended,
			size:    int64(inode.Size),
			mode:    (fs.FileMode(inode.Mode) & ^fs.ModeType) | b.ftype,
			modTime: time.Unix(int64(inode.Mtime), int64(inode.MtimeNs)),
			stat: &Stat{
				InodeLayout:  layout,
				XattrCount:   int16(inode.XattrCount),
				Mode:         fs.FileMode(inode.Mode),
				Size:         int64(inode.Size),
				RawBlockAddr: inode.RawBlockAddr,
				Inode:        int64(inode.Inode),
				UID:          uint32(inode.UID),
				GID:          uint32(inode.GID),
				Nlink:        int(inode.Nlink),
				Mtime:        inode.Mtime,
				MtimeNs:      inode.MtimeNs,
			},
		}, nil
	}
}

func (b *base) Stat() (fs.FileInfo, error) {
	return b.readInfo()
}

func (b *base) Read([]byte) (int, error) {
	return 0, errors.New("read: not implemented")
}
func (b *base) Close() error {
	// Nothing to close
	return nil
}

type direntry struct {
	base
}

func (d *direntry) Name() string {
	return d.name
}

func (d *direntry) IsDir() bool {
	return d.ftype.IsDir()
}

func (d *direntry) Type() fs.FileMode {
	return d.ftype
}

func (d *direntry) Info() (fs.FileInfo, error) {
	return d.readInfo()
}

type dir struct {
	base

	offset int64
	end    int64
}

func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	fi, err := d.readInfo()
	if err != nil {
		return nil, fmt.Errorf("readInfo failed: %w", err)
	}
	var xattrSize int64
	if fi.stat.XattrCount != 0 {
		xattrSize = 12 + int64((fi.stat.XattrCount-1))*4
	}

	// TODO: Must handle case where directory is larger than block, then read from raw
	// block address first, only last block is inline in most cases
	// TODO: Need reader for different layouts

	// inode loc + inode size + xattr size
	start := int64(d.blkAddr)*int64(d.blkSize) + int64(d.inode*disk.SizeInodeCompact) + int64(fi.isize) + xattrSize
	end := start + fi.stat.Size

	// TODO: Track position instead
	if d.offset == 0 {
		d.offset = start
	}

	var (
		ents     []fs.DirEntry
		direntB  [disk.SizeDirent]byte
		dirent   disk.Dirent
		previous disk.Dirent
	)
	for (d.end == 0 || d.offset < d.end) && n != 0 {
		readN, err := d.meta.ReadAt(direntB[:], d.offset)
		if err != nil {
			return nil, fmt.Errorf("failed to read dirent: %w", err)
		}
		if readN != 12 {
			return nil, errors.New("invalid dirent: short read")
		}
		readN, err = binary.Decode(direntB[:], binary.LittleEndian, &dirent)
		if err != nil {
			return nil, fmt.Errorf("decode failed: %w", err)
		}
		if readN != 12 {
			return nil, errors.New("invalid dirent: not fully decoded")
		}

		if d.end == 0 {
			d.end = d.offset + int64(dirent.NameOff)
		}
		if ents == nil {
			max := int(d.end-d.offset) / disk.SizeDirent
			if n > 0 && max > n {
				max = n
			}
			ents = make([]fs.DirEntry, 0, max)
		}

		if previous.Nid != 0 {
			nameStart := start + int64(previous.NameOff)
			nameLen := int(dirent.NameOff - previous.NameOff)
			nameBuf := make([]byte, nameLen)
			if n, err := d.meta.ReadAt(nameBuf, nameStart); err != nil {
				return nil, fmt.Errorf("failed to read at %d[%d]: %w", nameStart, nameLen, err)
			} else if n != nameLen {
				return nil, errors.New("could not read correct name length")
			}

			name := string(nameBuf)
			if name != "." && name != ".." {
				b := d.base
				b.name = name
				b.ftype = disk.EroFSFtypeToFileMode(previous.FileType)
				b.inode = previous.Nid
				ents = append(ents, &direntry{b})
			}
		}
		if n > 0 {
			n--
		}
		d.offset += disk.SizeDirent
		previous = dirent
	}
	if previous.Nid != 0 {
		nameStart := start + int64(previous.NameOff)
		nameLen := int(end - nameStart)
		nameBuf := make([]byte, nameLen)
		if n, err := d.meta.ReadAt(nameBuf, nameStart); err != nil {
			return nil, err
		} else if n != nameLen {
			return nil, errors.New("could not read correct name length")
		}

		name := string(nameBuf)
		if name != "." && name != ".." {
			b := d.base
			b.name = name
			b.ftype = disk.EroFSFtypeToFileMode(previous.FileType)
			b.inode = previous.Nid
			ents = append(ents, &direntry{b})
		}
	}

	return ents, nil
}

type fileInfo struct {
	name    string
	isize   int8
	size    int64
	mode    fs.FileMode
	modTime time.Time
	stat    *Stat
}

func (fi *fileInfo) Name() string {
	return fi.name
}

func (fi *fileInfo) Size() int64 {
	return fi.size
}

func (fi *fileInfo) Mode() fs.FileMode {
	return fi.mode
}
func (fi *fileInfo) ModTime() time.Time {
	return fi.modTime
}

func (fi *fileInfo) IsDir() bool {
	return fi.mode.IsDir()
}

func (fi *fileInfo) Sys() any {
	// Return erofs stat object with extra fields and call for xattrs
	return fi.stat
}

func decodeSuperBlock(b [disk.SizeSuperBlock]byte, sb *disk.SuperBlock) error {
	n, err := binary.Decode(b[:], binary.LittleEndian, sb)
	if err != nil {
		return err
	}
	if n != disk.SizeSuperBlock {
		return fmt.Errorf("invalid super block: decoded %d bytes", n)
	}
	if sb.MagicNumber != disk.MagicNumber {
		return fmt.Errorf("invalid super block: invalid magic number %x", sb.MagicNumber)
	}
	return nil
}
