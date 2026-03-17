// Package mkfs creates EROFS filesystem images.
//
// Two entry points are provided: [Create] walks a [Source] and writes a
// complete image, while [Builder] allows incremental construction.
//
// Sources abstract over different input formats. Adapters are provided for
// ext4 block images ([Ext4]), tar streams ([Tar]), and Go [fs.FS] trees ([FS]).
package mkfs

import (
	"io"
	"os"
)

// Source is a walkable filesystem tree. Walk invokes fn for every
// entry in depth-first order. Parents are visited before children.
type Source interface {
	Walk(fn func(path string, entry *Entry) error) error
}

// Entry describes a single filesystem entry.
type Entry struct {
	Mode       uint16            // Unix mode (type + permissions)
	UID        uint32
	GID        uint32
	Mtime      uint64            // seconds since epoch
	MtimeNs    uint32
	Nlink      uint32
	Size       uint64            // file size (regular files)
	Rdev       uint32            // device number (device files)
	Xattrs     map[string]string
	LinkTarget string            // symlink target

	// For regular files, exactly one of Data or Chunks should be set.
	Data   io.Reader // file content (full-image mode)
	Chunks []Chunk   // physical block refs (metadata-only mode)
}

// Chunk maps a range of logical blocks to physical blocks on a device.
type Chunk struct {
	PhysicalBlock uint64 // physical block address
	Count         uint16 // number of contiguous blocks
	DeviceID      uint16 // 0 = primary, 1+ = extra device
}

// Opt configures EROFS image creation.
type Opt func(*options)

type options struct {
	metadataOnly bool
	buildTime    uint64
	buildTimeNs  uint32
	hasBuildTime bool
	deviceBlocks uint64   // total blocks in source device (for device slot)
	dataFile     *os.File // external data file for metadata-only mode
	tempDir      string   // temp directory for spool file
}

// MetadataOnly configures the builder to emit only metadata.
// Regular files use chunk-based layout referencing an external device.
func MetadataOnly() Opt {
	return func(o *options) {
		o.metadataOnly = true
	}
}

// WithBuildTime sets the filesystem build timestamp.
func WithBuildTime(sec uint64, nsec uint32) Opt {
	return func(o *options) {
		o.buildTime = sec
		o.buildTimeNs = nsec
		o.hasBuildTime = true
	}
}

// WithDeviceBlocks sets the source device block count for the device slot in metadata-only mode.
func WithDeviceBlocks(n uint64) Opt {
	return func(o *options) {
		o.deviceBlocks = n
	}
}

// WithDataFile sets an external data file for metadata-only mode.
// File.Write appends to this file at block-aligned offsets; chunk
// indexes reference those blocks with DeviceID=1.
func WithDataFile(f *os.File) Opt {
	return func(o *options) {
		o.dataFile = f
	}
}

// WithTempDir overrides the temp directory for the spool file.
// Only used when no data file is provided.
func WithTempDir(dir string) Opt {
	return func(o *options) {
		o.tempDir = dir
	}
}

// Create walks a Source and writes an EROFS image to w.
func Create(src Source, w io.Writer, opts ...Opt) error {
	b := NewBuilder(opts...)
	err := src.Walk(func(path string, e *Entry) error {
		return b.Add(path, e)
	})
	if err != nil {
		return err
	}
	return b.WriteTo(w)
}
