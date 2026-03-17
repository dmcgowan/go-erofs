package mkfs

import (
	"sort"

	"github.com/erofs/go-erofs/internal/disk"
)

// planLayout assigns NIDs and determines trailing data sizes for all entries.
func (w *erofsWriter) planLayout(root *erofsEntry) {
	// Collect all entries in a deterministic order (BFS)
	w.entries = nil
	var queue []*erofsEntry
	queue = append(queue, root)
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		w.entries = append(w.entries, e)
		if e.mode&disk.StatTypeMask == disk.StatTypeDir {
			sort.Slice(e.children, func(i, j int) bool {
				return e.children[i].name < e.children[j].name
			})
			queue = append(queue, e.children...)
		}
	}

	w.totalInodes = uint64(len(w.entries))

	// Block 0 holds: 1024-byte pad + 128-byte superblock + device slot(s) + padding
	// MetaBlkAddr = 1 (metadata starts at block 1)
	w.metaBlkAddr = 1

	// Assign NIDs sequentially.
	// NID = byte offset from metaStartPos / 32.
	// Each extended inode is 64 bytes = 2 NID slots.
	// Trailing data follows and is padded to 32-byte boundary.
	currentOff := 0 // byte offset from metaStartPos
	for _, e := range w.entries {
		e.nid = uint64(currentOff / 32)
		e.xattrSize = calcXattrSize(e)
		e.trailingSize = w.calcTrailingSize(e)

		// The inode header region is inode core + xattr area.
		// Trailing data (dirents, chunk indexes, inline data) follows.
		headerSize := disk.SizeInodeExtended + e.xattrSize

		// Determine layout
		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeReg:
			if e.size == 0 && len(e.chunks) == 0 && e.data == nil {
				e.layout = disk.LayoutFlatPlain
			} else if w.metaOnly || len(e.chunks) > 0 {
				e.layout = disk.LayoutChunkBased
			} else {
				// Full-image mode: decide inline vs plain
				if int(e.size) <= disk.BlockSize-headerSize {
					inBlockOff := (currentOff + headerSize) % disk.BlockSize
					if inBlockOff+int(e.size) <= disk.BlockSize {
						e.layout = disk.LayoutFlatInline
					} else {
						e.layout = disk.LayoutFlatPlain
					}
				} else {
					e.layout = disk.LayoutFlatPlain
				}
			}
		case disk.StatTypeDir:
			direntDataSize := w.direntDataSize(e)
			inBlockOff := (currentOff + headerSize) % disk.BlockSize
			if direntDataSize > 0 && inBlockOff+direntDataSize <= disk.BlockSize {
				e.layout = disk.LayoutFlatInline
			} else {
				e.layout = disk.LayoutFlatPlain
			}
		case disk.StatTypeSymlink:
			inBlockOff := (currentOff + headerSize) % disk.BlockSize
			if len(e.symTarget) > 0 && inBlockOff+len(e.symTarget) <= disk.BlockSize {
				e.layout = disk.LayoutFlatInline
			} else {
				e.layout = disk.LayoutFlatPlain
			}
		default:
			// Device files, fifos, sockets
			e.layout = disk.LayoutFlatPlain
		}

		// Recalculate trailing size now that layout is decided
		e.trailingSize = w.calcTrailingSize(e)

		totalInodeSize := headerSize + e.trailingSize
		// Pad to 32-byte boundary
		if totalInodeSize%32 != 0 {
			totalInodeSize = (totalInodeSize + 31) & ^31
		}

		// Check block boundary: inode core must not cross a block boundary
		blockOff := currentOff % disk.BlockSize
		if blockOff+disk.SizeInodeExtended > disk.BlockSize {
			// Align to next block
			currentOff = (currentOff + disk.BlockSize - 1) & ^(disk.BlockSize - 1)
			e.nid = uint64(currentOff / 32)
		}

		// Also check that trailing data doesn't cross block boundary for inline layouts
		if e.layout == disk.LayoutFlatInline {
			blockOff = currentOff % disk.BlockSize
			if blockOff+headerSize+e.trailingSize > disk.BlockSize {
				// Fall back to flat-plain (data would cross block boundary)
				e.layout = disk.LayoutFlatPlain
				e.trailingSize = w.calcTrailingSize(e)
				totalInodeSize = headerSize + e.trailingSize
				if totalInodeSize%32 != 0 {
					totalInodeSize = (totalInodeSize + 31) & ^31
				}
			}
		}

		currentOff += totalInodeSize
	}

	w.rootNid = root.nid
}

// calcTrailingSize returns the number of bytes following the 64-byte inode.
func (w *erofsWriter) calcTrailingSize(e *erofsEntry) int {
	switch e.mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		if w.metaOnly || len(e.chunks) > 0 {
			if e.size == 0 && len(e.chunks) == 0 {
				return 0
			}
			nblocks := (int(e.size) + disk.BlockSize - 1) / disk.BlockSize
			return nblocks * disk.SizeChunkIndex
		}
		// Full-image mode
		if e.layout == disk.LayoutFlatInline {
			return int(e.size)
		}
		return 0
	case disk.StatTypeDir:
		if e.layout == disk.LayoutFlatInline {
			return w.direntDataSize(e)
		}
		return 0
	case disk.StatTypeSymlink:
		if e.layout == disk.LayoutFlatInline {
			return len(e.symTarget)
		}
		return 0
	default:
		return 0
	}
}

// direntDataSize calculates the serialized EROFS dirent data size for a directory.
// For multi-block directories, this includes inter-block padding.
func (w *erofsWriter) direntDataSize(e *erofsEntry) int {
	if len(e.children) == 0 {
		// Empty dir still needs "." and ".." entries
		return 2*disk.SizeDirent + 1 + 2
	}

	// Entries: ".", "..", then sorted children
	allNames := make([]string, 0, len(e.children)+2)
	allNames = append(allNames, ".", "..")
	for _, c := range e.children {
		allNames = append(allNames, c.name)
	}

	totalSize := 0
	i := 0
	for i < len(allNames) {
		blockUsed := 0
		start := i
		for j := i; j < len(allNames); j++ {
			headerSize := (j - start + 1) * disk.SizeDirent
			nameSize := 0
			for k := start; k <= j; k++ {
				nameSize += len(allNames[k])
			}
			needed := headerSize + nameSize
			if needed > disk.BlockSize {
				break
			}
			blockUsed = needed
			i = j + 1
		}
		if i == start {
			blockUsed = disk.SizeDirent + len(allNames[i])
			i++
		}
		// Pad non-final blocks to block boundary
		if i < len(allNames) && blockUsed%disk.BlockSize != 0 {
			blockUsed = (blockUsed + disk.BlockSize - 1) & ^(disk.BlockSize - 1)
		}
		totalSize += blockUsed
	}

	return totalSize
}
