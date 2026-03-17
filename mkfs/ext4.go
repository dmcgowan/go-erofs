package mkfs

import (
	"fmt"
	"io"
	"sort"

	"github.com/erofs/go-erofs/mkfs/internal/ext4"
)

// ext4Source implements Source for ext4 block images.
type ext4Source struct {
	r    io.ReaderAt
	size int64
	er   *ext4.Reader
}

// Ext4 returns a Source that reads an ext4 block image.
// In metadata-only mode, chunk indexes reference ext4 physical blocks.
// In full-image mode, file data is read from the ext4 extents.
func Ext4(r io.ReaderAt, size int64) Source {
	return &ext4Source{r: r, size: size}
}

func (s *ext4Source) Walk(fn func(string, *Entry) error) error {
	var err error
	s.er, err = ext4.NewReader(s.r)
	if err != nil {
		return fmt.Errorf("read ext4: %w", err)
	}

	return s.walkTree(fn, ext4.RootInode, "/")
}

func (s *ext4Source) walkTree(fn func(string, *Entry) error, inum uint32, path string) error {
	ino, err := s.er.ReadInode(inum)
	if err != nil {
		return fmt.Errorf("read inode %d (%s): %w", inum, path, err)
	}

	e := &Entry{
		Mode:  ino.Mode,
		UID:   ino.FullUID(),
		GID:   ino.FullGID(),
		Mtime: uint64(ino.Mtime),
		Nlink: uint32(ino.LinksCount),
		Size:  ino.Size(),
	}

	xattrs, err := s.er.ReadXattrs(ino)
	if err != nil {
		return fmt.Errorf("read xattrs for %s: %w", path, err)
	}
	if len(xattrs) > 0 {
		e.Xattrs = xattrs
	}

	switch ino.Mode & ext4.ModeTypeMask {
	case ext4.ModeTypeReg:
		if e.Size > 0 {
			extents, err := s.er.ReadExtents(ino)
			if err != nil {
				return fmt.Errorf("read extents for %s: %w", path, err)
			}
			for _, ext := range extents {
				e.Chunks = append(e.Chunks, Chunk{
					PhysicalBlock: ext.Physical(),
					Count:         ext.Count,
					DeviceID:      1, // ext4 data is extra device 1
				})
			}
			e.Data = &ext4FileReader{
				er:      s.er,
				extents: extents,
				size:    e.Size,
			}
		}

	case ext4.ModeTypeDir:
		if err := fn(path, e); err != nil {
			return err
		}

		dirEnts, err := s.er.ReadDirEntries(ino)
		if err != nil {
			return fmt.Errorf("read dir entries for %s: %w", path, err)
		}

		sort.Slice(dirEnts, func(i, j int) bool {
			return dirEnts[i].Name < dirEnts[j].Name
		})

		for _, de := range dirEnts {
			if de.Name == "." || de.Name == ".." {
				continue
			}
			childPath := path
			if childPath != "/" {
				childPath += "/"
			}
			childPath += de.Name

			if err := s.walkTree(fn, de.InodeNum, childPath); err != nil {
				return err
			}
		}
		return nil

	case ext4.ModeTypeLnk:
		target, err := s.er.ReadSymlink(ino)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", path, err)
		}
		e.LinkTarget = target

	case ext4.ModeTypeChr, ext4.ModeTypeBlk, ext4.ModeTypeFifo, ext4.ModeTypeSock:
		e.Rdev = ino.Rdev()
		e.Size = 0
	}

	return fn(path, e)
}

// Ext4Blocks returns the total block count of an ext4 image.
// Used by callers to set up the device slot.
func Ext4Blocks(r io.ReaderAt) (uint64, error) {
	er, err := ext4.NewReader(r)
	if err != nil {
		return 0, err
	}
	return er.BlocksCount(), nil
}

// ext4FileReader reads file data from ext4 extents.
type ext4FileReader struct {
	er      *ext4.Reader
	extents []ext4.Extent
	size    uint64
	offset  uint64
}

func (r *ext4FileReader) Read(p []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}

	remaining := r.size - r.offset
	if uint64(len(p)) > remaining {
		p = p[:remaining]
	}

	blockSize := uint64(r.er.BlockSize())
	n := 0
	for len(p) > 0 && r.offset < r.size {
		logBlock := uint32(r.offset / blockSize)
		blockOff := int(r.offset % blockSize)

		var physBlock uint64
		found := false
		for _, ext := range r.extents {
			end := ext.LogicalBlock + uint32(ext.Count)
			if logBlock >= ext.LogicalBlock && logBlock < end {
				physBlock = ext.Physical() + uint64(logBlock-ext.LogicalBlock)
				found = true
				break
			}
		}

		canRead := min(int(blockSize)-blockOff, len(p))
		if uint64(canRead) > r.size-r.offset {
			canRead = int(r.size - r.offset)
		}

		if !found {
			clear(p[:canRead])
		} else {
			diskOff := int64(physBlock)*int64(blockSize) + int64(blockOff)
			if _, err := r.er.ReaderAt().ReadAt(p[:canRead], diskOff); err != nil {
				return n, fmt.Errorf("ext4: read file data: %w", err)
			}
		}

		n += canRead
		r.offset += uint64(canRead)
		p = p[canRead:]
	}

	if r.offset >= r.size {
		return n, io.EOF
	}
	return n, nil
}
