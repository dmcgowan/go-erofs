package ext4

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// --- 1. Struct method tests ---

func TestExtentPhysical(t *testing.T) {
	tests := []struct {
		name string
		hi   uint16
		lo   uint32
		want uint64
	}{
		{"lo_only", 0, 100, 100},
		{"hi_set", 1, 0, 1 << 32},
		{"both", 2, 500, 2<<32 | 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Extent{PhysicalHi: tt.hi, PhysicalLo: tt.lo}
			if got := e.Physical(); got != tt.want {
				t.Errorf("Physical() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInodeSize(t *testing.T) {
	tests := []struct {
		name string
		hi   uint32
		lo   uint32
		want uint64
	}{
		{"lo_only", 0, 4096, 4096},
		{"hi_set", 1, 0, 1 << 32},
		{"both", 3, 1000, 3<<32 | 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ino := &Inode{SizeHi: tt.hi, SizeLo: tt.lo}
			if got := ino.Size(); got != tt.want {
				t.Errorf("Size() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInodeFullUID(t *testing.T) {
	tests := []struct {
		name string
		uid  uint16
		osd2 [12]byte
		want uint32
	}{
		{"lo_only", 1000, [12]byte{}, 1000},
		{"full_32bit", func() uint16 {
			// 70000 = 0x11170; lo = 0x1170, hi = 0x0001
			return uint16(70000 & 0xFFFF)
		}(), func() [12]byte {
			var o [12]byte
			// hi bytes at osd2[4:6], little-endian
			hi := uint16(70000 >> 16)
			o[4] = byte(hi)
			o[5] = byte(hi >> 8)
			return o
		}(), 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ino := &Inode{UID: tt.uid, Osd2: tt.osd2}
			if got := ino.FullUID(); got != tt.want {
				t.Errorf("FullUID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInodeFullGID(t *testing.T) {
	tests := []struct {
		name string
		gid  uint16
		osd2 [12]byte
		want uint32
	}{
		{"lo_only", 500, [12]byte{}, 500},
		{"full_32bit", func() uint16 {
			return uint16(70000 & 0xFFFF)
		}(), func() [12]byte {
			var o [12]byte
			hi := uint16(70000 >> 16)
			o[6] = byte(hi)
			o[7] = byte(hi >> 8)
			return o
		}(), 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ino := &Inode{GID: tt.gid, Osd2: tt.osd2}
			if got := ino.FullGID(); got != tt.want {
				t.Errorf("FullGID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInodeRdev(t *testing.T) {
	tests := []struct {
		name  string
		block [60]byte
		want  uint32
	}{
		{"old_encoding", func() [60]byte {
			var b [60]byte
			binary.LittleEndian.PutUint32(b[0:4], 0x0501) // major 5, minor 1
			return b
		}(), 0x0501},
		{"new_encoding", func() [60]byte {
			var b [60]byte
			// block[0:4] = 0 (old is zero), block[4:8] = new rdev
			binary.LittleEndian.PutUint32(b[4:8], 0x0802)
			return b
		}(), 0x0802},
		{"zero", [60]byte{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ino := &Inode{Block: tt.block}
			if got := ino.Rdev(); got != tt.want {
				t.Errorf("Rdev() = %#x, want %#x", got, tt.want)
			}
		})
	}
}

// --- 2. parseDirBlock tests ---

func makeDirEntry(inum uint32, name string, fileType uint8, recLen uint16) []byte {
	buf := make([]byte, recLen)
	binary.LittleEndian.PutUint32(buf[0:4], inum)
	binary.LittleEndian.PutUint16(buf[4:6], recLen)
	buf[6] = byte(len(name))
	buf[7] = fileType
	copy(buf[8:], name)
	return buf
}

func TestParseDirBlock(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		var data []byte
		data = append(data, makeDirEntry(2, ".", 2, 12)...)
		data = append(data, makeDirEntry(2, "..", 2, 12)...)
		data = append(data, makeDirEntry(11, "hello", 1, 24)...)
		// pad to block size
		data = append(data, make([]byte, 4096-len(data))...)

		entries := parseDirBlock(data)
		if len(entries) != 3 {
			t.Fatalf("got %d entries, want 3", len(entries))
		}
		if entries[2].Name != "hello" || entries[2].InodeNum != 11 {
			t.Errorf("entry[2] = %+v, want hello/11", entries[2])
		}
	})

	t.Run("deleted", func(t *testing.T) {
		var data []byte
		data = append(data, makeDirEntry(2, ".", 2, 12)...)
		data = append(data, makeDirEntry(0, "gone", 1, 16)...) // inum=0, deleted
		data = append(data, makeDirEntry(5, "kept", 1, 16)...)
		data = append(data, make([]byte, 4096-len(data))...)

		entries := parseDirBlock(data)
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2 (deleted skipped)", len(entries))
		}
		if entries[1].Name != "kept" {
			t.Errorf("entry[1].Name = %q, want %q", entries[1].Name, "kept")
		}
	})

	t.Run("reclen_zero", func(t *testing.T) {
		var data []byte
		data = append(data, makeDirEntry(2, ".", 2, 12)...)
		// Append an entry with recLen=0 to terminate
		data = append(data, make([]byte, 8)...) // all zeros: inum=0, recLen=0
		data = append(data, make([]byte, 4096-len(data))...)

		entries := parseDirBlock(data)
		if len(entries) != 1 {
			t.Fatalf("got %d entries, want 1 (recLen=0 stops)", len(entries))
		}
	})

	t.Run("truncated_name", func(t *testing.T) {
		// Create a buffer that's too short for the declared name
		buf := make([]byte, 16)
		binary.LittleEndian.PutUint32(buf[0:4], 10) // inum
		binary.LittleEndian.PutUint16(buf[4:6], 16)  // recLen
		buf[6] = 20                                   // nameLen=20, but only 8 bytes remain
		buf[7] = 1                                    // fileType

		entries := parseDirBlock(buf)
		if len(entries) != 0 {
			t.Fatalf("got %d entries, want 0 (name truncated)", len(entries))
		}
	})

	t.Run("empty", func(t *testing.T) {
		data := make([]byte, 4096) // all zeros
		entries := parseDirBlock(data)
		if len(entries) != 0 {
			t.Fatalf("got %d entries, want 0", len(entries))
		}
	})
}

// --- 3. SuperBlock validation (NewReader) ---

// makeMinimalImage builds a byte buffer with a valid-enough ext4 layout for
// NewReader to succeed. The caller can customize the superblock fields before
// calling this function. The returned buffer contains:
//   - 1024 bytes padding
//   - 1024 bytes superblock
//   - padding to block boundary
//   - one group descriptor at block 1 pointing inode table to block 2
//   - inode table space at block 2 (enough for a few inodes)
func makeMinimalImage(sb SuperBlock) []byte {
	// We need at least 3 blocks of 4096 bytes = 12288 bytes.
	const blockSize = 4096
	buf := make([]byte, 4*blockSize)

	// Write superblock at offset 1024.
	off := SuperBlockOffset
	binary.LittleEndian.PutUint32(buf[off+0:], sb.InodesCount)
	binary.LittleEndian.PutUint32(buf[off+4:], sb.BlocksCountLo)
	binary.LittleEndian.PutUint32(buf[off+20:], sb.FirstDataBlock)
	binary.LittleEndian.PutUint32(buf[off+24:], sb.LogBlockSize)
	binary.LittleEndian.PutUint32(buf[off+32:], sb.BlocksPerGroup)
	binary.LittleEndian.PutUint32(buf[off+40:], sb.InodesPerGroup)
	binary.LittleEndian.PutUint16(buf[off+56:], sb.Magic)
	binary.LittleEndian.PutUint16(buf[off+88:], sb.InodeSize)
	binary.LittleEndian.PutUint32(buf[off+96:], sb.FeatureIncompat)
	binary.LittleEndian.PutUint16(buf[off+254:], sb.DescSize)
	binary.LittleEndian.PutUint32(buf[off+336:], sb.BlocksCountHi)

	// Group descriptor at block 1 (offset 4096 for FirstDataBlock=0).
	// bg_inode_table_lo at offset 8 within the descriptor = block 2.
	gdOff := int((sb.FirstDataBlock + 1) * blockSize)
	if gdOff+12 <= len(buf) {
		binary.LittleEndian.PutUint32(buf[gdOff+8:], 2) // inode table at block 2
	}

	return buf
}

func validSuperBlock() SuperBlock {
	return SuperBlock{
		InodesCount:    128,
		BlocksCountLo:  100,
		FirstDataBlock: 0,
		LogBlockSize:   2, // 1024 << 2 = 4096
		BlocksPerGroup: 8192,
		InodesPerGroup: 128,
		Magic:          MagicNumber,
		InodeSize:      256,
	}
}

func TestNewReaderBadMagic(t *testing.T) {
	sb := validSuperBlock()
	sb.Magic = 0xBEEF
	img := makeMinimalImage(sb)

	_, err := NewReader(bytes.NewReader(img))
	if err == nil || !strings.Contains(err.Error(), "invalid magic") {
		t.Fatalf("expected 'invalid magic' error, got: %v", err)
	}
}

func TestNewReaderBadBlockSize(t *testing.T) {
	sb := validSuperBlock()
	sb.LogBlockSize = 0 // 1024 << 0 = 1024
	img := makeMinimalImage(sb)

	_, err := NewReader(bytes.NewReader(img))
	if err == nil || !strings.Contains(err.Error(), "unsupported block size") {
		t.Fatalf("expected 'unsupported block size' error, got: %v", err)
	}
}

func TestNewReader64Bit(t *testing.T) {
	sb := validSuperBlock()
	sb.FeatureIncompat = Feature64Bit
	sb.DescSize = 64
	sb.BlocksCountHi = 1
	sb.BlocksCountLo = 500
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if !er.sb.Is64Bit {
		t.Error("expected Is64Bit to be true")
	}
	want := uint64(1)<<32 | 500
	if got := er.BlocksCount(); got != want {
		t.Errorf("BlocksCount() = %d, want %d", got, want)
	}
}

// --- 4. ReadInode error paths ---

func TestReadInodeZero(t *testing.T) {
	sb := validSuperBlock()
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	_, err = er.ReadInode(0)
	if err == nil || !strings.Contains(err.Error(), "invalid inode 0") {
		t.Fatalf("expected 'invalid inode 0' error, got: %v", err)
	}
}

// --- 5. ReadExtents error paths ---

func TestReadExtentsNoFlag(t *testing.T) {
	sb := validSuperBlock()
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	ino := &Inode{Flags: 0} // no InodeFlagExtents
	_, err = er.ReadExtents(ino)
	if err == nil || !strings.Contains(err.Error(), "does not use extents") {
		t.Fatalf("expected 'does not use extents' error, got: %v", err)
	}
}

// --- 6. Extent tree parsing ---

func TestWalkExtentTreeDepth0(t *testing.T) {
	sb := validSuperBlock()
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	// Build a depth-0 extent tree with 2 entries in inode.Block.
	ino := &Inode{Flags: InodeFlagExtents}

	// Extent header: magic, entries=2, max=4, depth=0
	binary.LittleEndian.PutUint16(ino.Block[0:2], ExtentMagic)
	binary.LittleEndian.PutUint16(ino.Block[2:4], 2) // entries
	binary.LittleEndian.PutUint16(ino.Block[4:6], 4) // max
	binary.LittleEndian.PutUint16(ino.Block[6:8], 0) // depth

	// Extent 1: logical=0, count=5, physical=100
	off := 12
	binary.LittleEndian.PutUint32(ino.Block[off:off+4], 0)   // logical
	binary.LittleEndian.PutUint16(ino.Block[off+4:off+6], 5)  // count
	binary.LittleEndian.PutUint16(ino.Block[off+6:off+8], 0)  // physicalHi
	binary.LittleEndian.PutUint32(ino.Block[off+8:off+12], 100) // physicalLo

	// Extent 2: logical=10, count=3, physical=200
	off = 24
	binary.LittleEndian.PutUint32(ino.Block[off:off+4], 10)   // logical
	binary.LittleEndian.PutUint16(ino.Block[off+4:off+6], 3)  // count
	binary.LittleEndian.PutUint16(ino.Block[off+6:off+8], 0)  // physicalHi
	binary.LittleEndian.PutUint32(ino.Block[off+8:off+12], 200) // physicalLo

	extents, err := er.ReadExtents(ino)
	if err != nil {
		t.Fatalf("ReadExtents: %v", err)
	}
	if len(extents) != 2 {
		t.Fatalf("got %d extents, want 2", len(extents))
	}
	if extents[0].LogicalBlock != 0 || extents[0].Physical() != 100 || extents[0].Count != 5 {
		t.Errorf("extent[0] = %+v, want logical=0 physical=100 count=5", extents[0])
	}
	if extents[1].LogicalBlock != 10 || extents[1].Physical() != 200 || extents[1].Count != 3 {
		t.Errorf("extent[1] = %+v, want logical=10 physical=200 count=3", extents[1])
	}
}

func TestWalkExtentTreeBadMagic(t *testing.T) {
	sb := validSuperBlock()
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	ino := &Inode{Flags: InodeFlagExtents}
	binary.LittleEndian.PutUint16(ino.Block[0:2], 0xDEAD) // wrong magic

	_, err = er.ReadExtents(ino)
	if err == nil || !strings.Contains(err.Error(), "invalid extent magic") {
		t.Fatalf("expected 'invalid extent magic' error, got: %v", err)
	}
}

// --- 7. Inline symlink ---

func TestReadSymlinkInline(t *testing.T) {
	sb := validSuperBlock()
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	target := "/usr/lib/target"
	ino := &Inode{
		Mode:   ModeTypeLnk | 0777,
		Flags:  0, // no extents
		SizeLo: uint32(len(target)),
	}
	copy(ino.Block[:], target)

	got, err := er.ReadSymlink(ino)
	if err != nil {
		t.Fatalf("ReadSymlink: %v", err)
	}
	if got != target {
		t.Errorf("ReadSymlink() = %q, want %q", got, target)
	}
}

func TestReadSymlinkEmpty(t *testing.T) {
	sb := validSuperBlock()
	img := makeMinimalImage(sb)

	er, err := NewReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	ino := &Inode{
		Mode:   ModeTypeLnk | 0777,
		SizeLo: 0,
	}

	got, err := er.ReadSymlink(ino)
	if err != nil {
		t.Fatalf("ReadSymlink: %v", err)
	}
	if got != "" {
		t.Errorf("ReadSymlink() = %q, want empty", got)
	}
}
