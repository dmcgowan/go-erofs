package mkfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/erofs/go-erofs/internal/disk"
)

// erofsWriter serializes EROFS metadata to an io.Writer.
type erofsWriter struct {
	buf         bytes.Buffer
	entries     []*erofsEntry // all entries in NID order
	rootNid     uint64
	metaBlkAddr uint32
	totalInodes uint64
	buildTime   uint64
	buildTimeNs uint32
	deviceBlocks  uint64 // total blocks in the source device (for device slot)
	metaOnly    bool   // metadata-only mode (chunk-based, no data blocks)
}

func (w *erofsWriter) write(out io.Writer) error {
	if err := w.writeBlock0(); err != nil {
		return err
	}
	if err := w.writeMetadata(); err != nil {
		return err
	}
	if err := w.writeDataBlocks(); err != nil {
		return err
	}
	_, err := w.buf.WriteTo(out)
	return err
}

func (w *erofsWriter) writeBlock0() error {
	var block0 [disk.BlockSize]byte

	totalMetaBytes := 0
	for _, e := range w.entries {
		sz := disk.SizeInodeExtended + e.xattrSize + e.trailingSize
		if sz%32 != 0 {
			sz = (sz + 31) & ^31
		}
		totalMetaBytes += sz
	}
	metaBlocks := (totalMetaBytes + disk.BlockSize - 1) / disk.BlockSize
	totalBlocks := 1 + metaBlocks

	// Count data blocks for flat-plain entries (dirs/symlinks in all modes,
	// regular files only in full-image mode)
	for _, e := range w.entries {
		if ds := w.flatPlainDataSize(e); ds > 0 {
			totalBlocks += (ds + disk.BlockSize - 1) / disk.BlockSize
		}
	}

	var featureIncompat uint32
	var extraDevices uint16
	var devtSlotOff uint16

	if w.metaOnly {
		featureIncompat = disk.FeatureIncompatChunkedFile | disk.FeatureIncompatDeviceTable
		extraDevices = 1
		devtSlotOff = uint16(disk.SizeSuperBlock / 16)
	} else {
		// Full-image mode: may still need chunked file feature if any chunks present
		for _, e := range w.entries {
			if len(e.chunks) > 0 {
				featureIncompat |= disk.FeatureIncompatChunkedFile
				break
			}
		}
	}

	sb := disk.SuperBlock{
		MagicNumber:     disk.MagicNumber,
		BlkSizeBits:     disk.BlkBits,
		RootNid:         uint16(w.rootNid),
		Inos:            w.totalInodes,
		BuildTime:       w.buildTime,
		BuildTimeNs:     w.buildTimeNs,
		Blocks:          uint32(totalBlocks),
		MetaBlkAddr:     w.metaBlkAddr,
		FeatureIncompat: featureIncompat,
		ExtraDevices:    extraDevices,
		DevtSlotOff:     devtSlotOff,
	}

	sbBuf := &bytes.Buffer{}
	if err := binary.Write(sbBuf, binary.LittleEndian, &sb); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}
	copy(block0[disk.SuperBlockOffset:], sbBuf.Bytes())

	if w.metaOnly {
		// Device slot right after superblock
		devSlot := disk.DeviceSlot{
			BlocksLo: uint32(w.deviceBlocks),
			BlocksHi: uint32(w.deviceBlocks >> 32),
		}
		devBuf := &bytes.Buffer{}
		if err := binary.Write(devBuf, binary.LittleEndian, &devSlot); err != nil {
			return fmt.Errorf("write device slot: %w", err)
		}
		copy(block0[disk.SuperBlockOffset+disk.SizeSuperBlock:], devBuf.Bytes())
	}

	_, err := w.buf.Write(block0[:])
	return err
}

func (w *erofsWriter) writeMetadata() error {
	// Assign data block addresses for flat-plain entries.
	// Data blocks come after all metadata blocks.
	totalMetaBytes := 0
	for _, e := range w.entries {
		expectedOff := int(e.nid) * 32
		if expectedOff > totalMetaBytes {
			totalMetaBytes = expectedOff
		}
		sz := disk.SizeInodeExtended + e.xattrSize + e.trailingSize
		if sz%32 != 0 {
			sz = (sz + 31) & ^31
		}
		totalMetaBytes = expectedOff + sz
	}
	metaBlocks := (totalMetaBytes + disk.BlockSize - 1) / disk.BlockSize
	dataBlkAddr := uint32(1 + metaBlocks) // block 0 + metadata blocks

	for _, e := range w.entries {
		if ds := w.flatPlainDataSize(e); ds > 0 {
			e.dataBlkAddr = dataBlkAddr
			dataBlkAddr += (uint32(ds) + disk.BlockSize - 1) / disk.BlockSize
		}
	}

	metaStart := 0
	for _, e := range w.entries {
		expectedOff := int(e.nid) * 32
		if expectedOff > metaStart {
			padding := make([]byte, expectedOff-metaStart)
			w.buf.Write(padding)
			metaStart = expectedOff
		}

		if err := w.writeInode(e); err != nil {
			return fmt.Errorf("write inode for %s: %w", e.path, err)
		}
		metaStart += disk.SizeInodeExtended

		// Write xattr area
		if e.xattrSize > 0 {
			if err := w.writeXattrs(e); err != nil {
				return fmt.Errorf("write xattrs for %s: %w", e.path, err)
			}
			metaStart += e.xattrSize
		}

		// Write trailing data
		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeReg:
			if e.layout == disk.LayoutChunkBased && (e.size > 0 || len(e.chunks) > 0) {
				if err := w.writeChunkIndexes(e); err != nil {
					return fmt.Errorf("write chunks for %s: %w", e.path, err)
				}
				metaStart += e.trailingSize
			} else if e.layout == disk.LayoutFlatInline && e.size > 0 && e.data != nil {
				n, err := io.Copy(&w.buf, io.LimitReader(e.data, int64(e.size)))
				if err != nil {
					return fmt.Errorf("write inline data for %s: %w", e.path, err)
				}
				metaStart += int(n)
			}
		case disk.StatTypeDir:
			if e.layout == disk.LayoutFlatInline {
				n, err := w.writeDirents(e)
				if err != nil {
					return fmt.Errorf("write dirents for %s: %w", e.path, err)
				}
				metaStart += n
			}
		case disk.StatTypeSymlink:
			if e.layout == disk.LayoutFlatInline {
				w.buf.WriteString(e.symTarget)
				metaStart += len(e.symTarget)
			}
		}

		// Pad to 32-byte boundary
		totalWritten := disk.SizeInodeExtended + e.xattrSize + e.trailingSize
		if totalWritten%32 != 0 {
			padSize := 32 - (totalWritten % 32)
			w.buf.Write(make([]byte, padSize))
			metaStart += padSize
		}
	}

	// Pad metadata to full block boundary
	if metaStart%disk.BlockSize != 0 {
		padSize := disk.BlockSize - (metaStart % disk.BlockSize)
		w.buf.Write(make([]byte, padSize))
	}

	return nil
}

func (w *erofsWriter) writeInode(e *erofsEntry) error {
	var inodeData uint32

	switch e.mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		if e.layout == disk.LayoutChunkBased {
			inodeData = disk.LayoutChunkFormatIndexes
		} else if e.layout == disk.LayoutFlatPlain && e.data != nil && e.size > 0 {
			inodeData = e.dataBlkAddr
		}
	case disk.StatTypeDir, disk.StatTypeSymlink:
		if e.layout == disk.LayoutFlatPlain {
			inodeData = e.dataBlkAddr
		}
	case disk.StatTypeChrdev, disk.StatTypeBlkdev, disk.StatTypeFifo, disk.StatTypeSock:
		inodeData = e.rdev
	}

	fileSize := e.size
	if e.mode&disk.StatTypeMask == disk.StatTypeDir {
		fileSize = uint64(w.direntDataSize(e))
	} else if e.mode&disk.StatTypeMask == disk.StatTypeSymlink {
		fileSize = uint64(len(e.symTarget))
	}

	ino := disk.InodeExtended{
		Format:     inodeFormat(e.layout),
		XattrCount: xattrCount(e.xattrSize),
		Mode:       e.mode,
		Size:       fileSize,
		InodeData:  inodeData,
		UID:        e.uid,
		GID:        e.gid,
		Mtime:      e.mtime,
		MtimeNs:    e.mtimeNs,
		Nlink:      e.nlink,
	}

	return binary.Write(&w.buf, binary.LittleEndian, &ino)
}

func (w *erofsWriter) writeXattrs(e *erofsEntry) error {
	hdr := disk.XattrHeader{
		NameFilter: 0xFFFFFFFF, // unused, set to all-ones
	}
	if err := binary.Write(&w.buf, binary.LittleEndian, &hdr); err != nil {
		return err
	}

	for _, name := range sortedXattrKeys(e.xattrs) {
		value := e.xattrs[name]
		nameIndex, suffix := xattrSplit(name)

		entry := disk.XattrEntry{
			NameLen:   uint8(len(suffix)),
			NameIndex: nameIndex,
			ValueLen:  uint16(len(value)),
		}
		if err := binary.Write(&w.buf, binary.LittleEndian, &entry); err != nil {
			return err
		}
		w.buf.WriteString(suffix)
		w.buf.WriteString(value)

		// Pad to 4-byte boundary
		entryLen := disk.SizeXattrEntry + len(suffix) + len(value)
		if entryLen%4 != 0 {
			w.buf.Write(make([]byte, 4-entryLen%4))
		}
	}
	return nil
}

// writeChunkIndexes writes chunk index entries for a regular file.
func (w *erofsWriter) writeChunkIndexes(e *erofsEntry) error {
	nblocks := (int(e.size) + disk.BlockSize - 1) / disk.BlockSize

	if len(e.chunks) > 0 {
		logToPhys := make(map[int]Chunk)
		logBlock := 0
		for _, c := range e.chunks {
			for i := 0; i < int(c.Count); i++ {
				logToPhys[logBlock] = Chunk{
					PhysicalBlock: c.PhysicalBlock + uint64(i),
					Count:         1,
					DeviceID:      c.DeviceID,
				}
				logBlock++
			}
		}

		for bn := 0; bn < nblocks; bn++ {
			var idx disk.InodeChunkIndex
			c, ok := logToPhys[bn]
			if !ok {
				idx.StartBlkLo = disk.NullAddr
				idx.StartBlkHi = 0xFFFF
			} else {
				idx.DeviceID = c.DeviceID
				idx.StartBlkLo = uint32(c.PhysicalBlock)
				idx.StartBlkHi = uint16(c.PhysicalBlock >> 32)
			}
			if err := binary.Write(&w.buf, binary.LittleEndian, &idx); err != nil {
				return err
			}
		}
	} else {
		for bn := 0; bn < nblocks; bn++ {
			idx := disk.InodeChunkIndex{
				StartBlkLo: disk.NullAddr,
				StartBlkHi: 0xFFFF,
			}
			if err := binary.Write(&w.buf, binary.LittleEndian, &idx); err != nil {
				return err
			}
		}
	}

	return nil
}

// writeDirents writes EROFS directory entries packed into block-sized chunks.
func (w *erofsWriter) writeDirents(e *erofsEntry) (int, error) {
	type direntInfo struct {
		name     string
		nid      uint64
		fileType uint8
	}

	var allEnts []direntInfo
	allEnts = append(allEnts, direntInfo{".", e.nid, disk.FileTypeDir})
	allEnts = append(allEnts, direntInfo{"..", e.parentNid, disk.FileTypeDir})

	for _, c := range e.children {
		allEnts = append(allEnts, direntInfo{
			name:     c.name,
			nid:      c.nid,
			fileType: c.erofsFileType,
		})
	}

	totalWritten := 0
	i := 0
	for i < len(allEnts) {
		// Determine how many entries fit in this block
		start := i
		blockUsed := 0
		for j := i; j < len(allEnts); j++ {
			headerSize := (j - start + 1) * disk.SizeDirent
			nameSize := 0
			for k := start; k <= j; k++ {
				nameSize += len(allEnts[k].name)
			}
			needed := headerSize + nameSize
			if needed > disk.BlockSize {
				break
			}
			blockUsed = needed
			i = j + 1
		}
		if i == start {
			// Single entry too large for a block (shouldn't happen)
			blockUsed = disk.SizeDirent + len(allEnts[i].name)
			i++
		}

		blockEnts := allEnts[start:i]
		blockHeaderSize := len(blockEnts) * disk.SizeDirent

		// Write dirent headers
		for j, de := range blockEnts {
			nameOff := uint16(blockHeaderSize)
			for k := 0; k < j; k++ {
				nameOff += uint16(len(blockEnts[k].name))
			}
			d := disk.Dirent{
				Nid:      de.nid,
				NameOff:  nameOff,
				FileType: de.fileType,
			}
			if err := binary.Write(&w.buf, binary.LittleEndian, &d); err != nil {
				return totalWritten, err
			}
			totalWritten += disk.SizeDirent
		}

		// Write names
		for _, de := range blockEnts {
			n, err := w.buf.WriteString(de.name)
			if err != nil {
				return totalWritten, err
			}
			totalWritten += n
		}

		// Pad to block boundary if there are more entries
		if i < len(allEnts) && blockUsed%disk.BlockSize != 0 {
			padSize := disk.BlockSize - (blockUsed % disk.BlockSize)
			w.buf.Write(make([]byte, padSize))
			totalWritten += padSize
		}
	}

	return totalWritten, nil
}

// writeDataBlocks writes data blocks for flat-plain entries.
func (w *erofsWriter) writeDataBlocks() error {
	for _, e := range w.entries {
		ds := w.flatPlainDataSize(e)
		if ds == 0 {
			continue
		}

		var n int
		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeReg:
			written, err := io.Copy(&w.buf, e.data)
			if err != nil {
				return fmt.Errorf("write data for %s: %w", e.path, err)
			}
			n = int(written)
		case disk.StatTypeDir:
			written, err := w.writeDirents(e)
			if err != nil {
				return fmt.Errorf("write dirents for %s: %w", e.path, err)
			}
			n = written
		case disk.StatTypeSymlink:
			written, _ := w.buf.WriteString(e.symTarget)
			n = written
		}

		if n%disk.BlockSize != 0 {
			padSize := disk.BlockSize - (n % disk.BlockSize)
			w.buf.Write(make([]byte, padSize))
		}
	}
	return nil
}

// flatPlainDataSize returns the data size for a flat-plain entry, or 0.
func (w *erofsWriter) flatPlainDataSize(e *erofsEntry) int {
	if e.layout != disk.LayoutFlatPlain {
		return 0
	}
	switch e.mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		if !w.metaOnly && e.size > 0 && e.data != nil {
			return int(e.size)
		}
	case disk.StatTypeDir:
		return w.direntDataSize(e)
	case disk.StatTypeSymlink:
		return len(e.symTarget)
	}
	return 0
}
