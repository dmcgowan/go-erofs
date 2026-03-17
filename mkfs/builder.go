package mkfs

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/erofs/go-erofs/internal/disk"
)

// Builder accumulates filesystem entries and serializes an EROFS image.
type Builder struct {
	opts options
	root *erofsEntry
	dirs map[string]*erofsEntry // path → directory entry for implicit creation
}

// erofsEntry is the internal representation of a file/dir/symlink.
type erofsEntry struct {
	mode      uint16
	uid       uint32
	gid       uint32
	mtime     uint64
	mtimeNs   uint32
	nlink     uint32
	size      uint64
	rdev      uint32

	name      string
	path      string
	children  []*erofsEntry
	symTarget string

	// For regular files — metadata-only mode
	chunks []Chunk

	// For regular files — full-image mode
	data io.Reader

	// Extended attributes
	xattrs map[string]string

	// EROFS layout (assigned during planning)
	nid           uint64
	parentNid     uint64
	erofsFileType uint8
	layout        uint8
	xattrSize     int // bytes of xattr area (0 if no xattrs)
	trailingSize  int

	// Data block address for flat-plain files (full-image mode)
	dataBlkAddr uint32
}

// NewBuilder creates a new Builder with the given options.
func NewBuilder(opts ...Opt) *Builder {
	b := &Builder{
		dirs: make(map[string]*erofsEntry),
	}
	for _, o := range opts {
		o(&b.opts)
	}
	if !b.opts.hasBuildTime {
		b.opts.buildTime = uint64(time.Now().Unix())
	}
	return b
}

// Add adds an entry at the given path. Parent directories are
// created implicitly if not already added. Entries may be added
// in any order.
func (b *Builder) Add(p string, e *Entry) error {
	p = cleanPath(p)

	// Ensure root exists
	if b.root == nil {
		b.root = &erofsEntry{
			mode:          disk.StatTypeDir | 0o755,
			nlink:         2,
			name:          "",
			path:          "/",
			erofsFileType: disk.FileTypeDir,
		}
		b.dirs["/"] = b.root
	}

	if p == "/" {
		// Update root entry with provided metadata
		b.root.mode = e.Mode
		b.root.uid = e.UID
		b.root.gid = e.GID
		b.root.mtime = e.Mtime
		b.root.mtimeNs = e.MtimeNs
		if e.Nlink > 0 {
			b.root.nlink = e.Nlink
		}
		b.root.xattrs = copyXattrs(e.Xattrs)
		b.root.erofsFileType = modeToFileType(e.Mode)
		return nil
	}

	// Ensure parent directories exist
	dir := path.Dir(p)
	b.ensureDir(dir)

	parent := b.dirs[dir]
	if parent == nil {
		return fmt.Errorf("mkfs: parent directory %q not found for %q", dir, p)
	}

	entry := &erofsEntry{
		mode:          e.Mode,
		uid:           e.UID,
		gid:           e.GID,
		mtime:         e.Mtime,
		mtimeNs:       e.MtimeNs,
		nlink:         e.Nlink,
		size:          e.Size,
		rdev:          e.Rdev,
		name:          path.Base(p),
		path:          p,
		symTarget:     e.LinkTarget,
		chunks:        e.Chunks,
		data:          e.Data,
		xattrs:        copyXattrs(e.Xattrs),
		erofsFileType: modeToFileType(e.Mode),
	}

	if entry.nlink == 0 {
		entry.nlink = 1
		if entry.mode&disk.StatTypeMask == disk.StatTypeDir {
			entry.nlink = 2
		}
	}

	parent.children = append(parent.children, entry)

	if entry.mode&disk.StatTypeMask == disk.StatTypeDir {
		b.dirs[p] = entry
	}

	return nil
}

// WriteTo serializes the EROFS image. The Builder should not be
// used after calling WriteTo.
func (b *Builder) WriteTo(w io.Writer) error {
	if b.root == nil {
		b.root = &erofsEntry{
			mode:          disk.StatTypeDir | 0o755,
			nlink:         2,
			name:          "",
			path:          "/",
			erofsFileType: disk.FileTypeDir,
		}
	}

	// Sort all directory children for deterministic output
	b.sortChildren(b.root)

	ew := &erofsWriter{
		buildTime:   b.opts.buildTime,
		buildTimeNs: b.opts.buildTimeNs,
		deviceBlocks:  b.opts.deviceBlocks,
		metaOnly:    b.opts.metadataOnly,
	}

	ew.planLayout(b.root)
	fixParentNids(b.root, b.root)

	return ew.write(w)
}

func (b *Builder) sortChildren(e *erofsEntry) {
	if e.mode&disk.StatTypeMask != disk.StatTypeDir {
		return
	}
	sort.Slice(e.children, func(i, j int) bool {
		return e.children[i].name < e.children[j].name
	})
	for _, c := range e.children {
		b.sortChildren(c)
	}
}

// ensureDir creates directory entries for the given path and all parents.
func (b *Builder) ensureDir(p string) {
	if p == "/" {
		return
	}
	if _, ok := b.dirs[p]; ok {
		return
	}

	// Ensure parent first
	parent := path.Dir(p)
	b.ensureDir(parent)

	entry := &erofsEntry{
		mode:          disk.StatTypeDir | 0o755,
		nlink:         2,
		name:          path.Base(p),
		path:          p,
		erofsFileType: disk.FileTypeDir,
	}

	parentEntry := b.dirs[parent]
	parentEntry.children = append(parentEntry.children, entry)
	b.dirs[p] = entry
}

// cleanPath normalizes a filesystem path.
func cleanPath(p string) string {
	if p == "" || p == "." || p == "/" {
		return "/"
	}
	p = path.Clean(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func copyXattrs(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// fixParentNids sets the parent NID in the ".." dirent for all directories.
// This must be called after planLayout has assigned NIDs.
func fixParentNids(e *erofsEntry, parent *erofsEntry) {
	e.parentNid = parent.nid
	for _, c := range e.children {
		if c.mode&disk.StatTypeMask == disk.StatTypeDir {
			fixParentNids(c, e)
		}
	}
}
