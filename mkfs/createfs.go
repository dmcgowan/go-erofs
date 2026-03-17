package mkfs

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/erofs/go-erofs/internal/disk"
)

// WriteFS is a writable filesystem that produces an EROFS image on Close.
type WriteFS struct {
	out     io.Writer
	opts    []Opt
	closed  bool
	entries []*fsEntry          // insertion order
	dirs    map[string]*fsEntry // path → dir entry
	paths   map[string]struct{} // all registered paths
	byPath  map[string]*fsEntry // path → entry (all types)

	dataFile *os.File // external data file (nil = spool mode)
	dataOff  int64    // current byte offset in data file
	spool    *os.File // temp spool (created lazily)
	spoolOff int64    // current byte offset in spool
	tempDir  string   // from WithTempDir
}

type fsEntry struct {
	path       string
	mode       uint16
	uid, gid   uint32
	atime      uint64
	atimeNs    uint32
	mtime      uint64
	mtimeNs    uint32
	nlink      uint32
	nlinkSet   bool // true if SetNlink was called
	size       uint64
	rdev       uint32
	xattrs     map[string]string
	linkTarget string
	chunks     []Chunk

	// data location in spool file
	spoolOff     int64
	dataStartOff int64 // byte offset where file data begins (spool or data file)
	fileClosed   bool  // true after File.Close() is called
}

// File is a writable regular file.
type File struct {
	fs           *WriteFS
	entry        *fsEntry
	dataStartOff int64 // byte offset where this file's data begins
	written      int64
	closed       bool
}

// CreateFS returns a writable FS that produces an EROFS image on Close.
func CreateFS(out io.Writer, opts ...Opt) *WriteFS {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	fsys := &WriteFS{
		out:      out,
		opts:     opts,
		dirs:     make(map[string]*fsEntry),
		paths:    make(map[string]struct{}),
		byPath:   make(map[string]*fsEntry),
		dataFile: o.dataFile,
		tempDir:  o.tempDir,
	}

	// Always create root directory.
	root := &fsEntry{
		path: "/",
		mode: disk.StatTypeDir | 0o755,
	}
	fsys.entries = append(fsys.entries, root)
	fsys.dirs["/"] = root
	fsys.paths["/"] = struct{}{}
	fsys.byPath["/"] = root

	if o.dataFile != nil {
		off, err := o.dataFile.Seek(0, io.SeekEnd)
		if err == nil {
			fsys.dataOff = off
		}
	}

	return fsys
}

// Create creates a regular file with default mode 0644. The caller must
// Close the returned File.
func (fsys *WriteFS) Create(name string) (*File, error) {
	name = cleanPath(name)
	if name == "/" {
		return nil, fmt.Errorf("mkfs: cannot create file at root")
	}
	if err := fsys.checkPath(name); err != nil {
		return nil, err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path: name,
		mode: disk.StatTypeReg | 0o644,
	}
	fsys.entries = append(fsys.entries, e)
	fsys.paths[name] = struct{}{}
	fsys.byPath[name] = e

	f := &File{
		fs:    fsys,
		entry: e,
	}

	if fsys.dataFile != nil {
		f.dataStartOff = fsys.dataOff
		e.dataStartOff = fsys.dataOff
	} else {
		if err := fsys.ensureSpool(); err != nil {
			return nil, err
		}
		f.dataStartOff = fsys.spoolOff
		e.spoolOff = fsys.spoolOff
		e.dataStartOff = fsys.spoolOff
	}

	return f, nil
}

// Mkdir creates a directory. Only permission bits from perm are used;
// type bits are forced to directory. Mkdir("/", perm) sets root permissions.
func (fsys *WriteFS) Mkdir(name string, perm fs.FileMode) error {
	name = cleanPath(name)
	if name == "/" {
		e := fsys.dirs["/"]
		e.mode = disk.StatTypeDir | uint16(perm.Perm())
		return nil
	}
	if err := fsys.checkPath(name); err != nil {
		return err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path: name,
		mode: disk.StatTypeDir | uint16(perm.Perm()),
	}
	fsys.entries = append(fsys.entries, e)
	fsys.dirs[name] = e
	fsys.paths[name] = struct{}{}
	fsys.byPath[name] = e

	return nil
}

// Symlink creates newname as a symbolic link to oldname (mode 0777).
func (fsys *WriteFS) Symlink(oldname, newname string) error {
	newname = cleanPath(newname)
	if newname == "/" {
		return fmt.Errorf("mkfs: cannot create symlink at root")
	}
	if err := fsys.checkPath(newname); err != nil {
		return err
	}

	fsys.ensureParent(newname)

	e := &fsEntry{
		path:       newname,
		mode:       disk.StatTypeSymlink | 0o777,
		linkTarget: oldname,
	}
	fsys.entries = append(fsys.entries, e)
	fsys.paths[newname] = struct{}{}
	fsys.byPath[newname] = e

	return nil
}

// Mknod creates a device, FIFO, or socket. mode must include type bits
// (e.g. disk.StatTypeChrdev | 0o666).
func (fsys *WriteFS) Mknod(name string, mode uint16, rdev uint32) error {
	name = cleanPath(name)
	if name == "/" {
		return fmt.Errorf("mkfs: cannot mknod at root")
	}
	if err := fsys.checkPath(name); err != nil {
		return err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path: name,
		mode: mode,
		rdev: rdev,
	}
	fsys.entries = append(fsys.entries, e)
	fsys.paths[name] = struct{}{}
	fsys.byPath[name] = e

	return nil
}

// Close writes the EROFS image. The FS must not be used after Close.
func (fsys *WriteFS) Close() error {
	if fsys.closed {
		return fmt.Errorf("mkfs: FS already closed")
	}
	fsys.closed = true

	if fsys.spool != nil {
		defer fsys.spool.Close()
	}

	// Compute directory nlinks: 2 + number of child directories.
	dirChildCount := make(map[string]uint32) // dir path → child dir count
	for _, e := range fsys.entries {
		if e.path == "/" {
			continue
		}
		parent := path.Dir(e.path)
		if e.mode&disk.StatTypeMask == disk.StatTypeDir {
			dirChildCount[parent]++
		}
	}

	opts := fsys.opts
	if fsys.dataFile != nil {
		// Metadata-only mode: include MetadataOnly and device blocks.
		blocks := (fsys.dataOff + disk.BlockSize - 1) / disk.BlockSize
		opts = append(opts, MetadataOnly(), WithDeviceBlocks(uint64(blocks)))
	}

	b := NewBuilder(opts...)

	for _, e := range fsys.entries {
		entry := &Entry{
			Mode:       e.mode,
			UID:        e.uid,
			GID:        e.gid,
			Mtime:      e.mtime,
			MtimeNs:    e.mtimeNs,
			Size:       e.size,
			Rdev:       e.rdev,
			Xattrs:     e.xattrs,
			LinkTarget: e.linkTarget,
			Chunks:     e.chunks,
		}

		// Set nlink.
		switch {
		case e.nlinkSet:
			entry.Nlink = e.nlink
		case e.mode&disk.StatTypeMask == disk.StatTypeDir:
			entry.Nlink = 2 + dirChildCount[e.path]
		default:
			entry.Nlink = 1
		}

		// Set data reader for spool mode regular files.
		if fsys.dataFile == nil && e.mode&disk.StatTypeMask == disk.StatTypeReg && e.size > 0 {
			entry.Data = io.NewSectionReader(fsys.spool, e.spoolOff, int64(e.size))
		}

		if err := b.Add(e.path, entry); err != nil {
			return fmt.Errorf("mkfs: add %s: %w", e.path, err)
		}
	}

	return b.WriteTo(fsys.out)
}

// checkPath validates that a path hasn't already been registered.
func (fsys *WriteFS) checkPath(name string) error {
	if fsys.closed {
		return fmt.Errorf("mkfs: FS is closed")
	}
	if _, ok := fsys.paths[name]; ok {
		return fmt.Errorf("mkfs: duplicate path %q", name)
	}
	return nil
}

// ensureParent creates implicit parent directories for name.
func (fsys *WriteFS) ensureParent(name string) {
	dir := path.Dir(name)
	if dir == "/" {
		return
	}
	// Walk up to create all missing ancestors.
	var missing []string
	for d := dir; d != "/"; d = path.Dir(d) {
		if _, ok := fsys.dirs[d]; ok {
			break
		}
		missing = append(missing, d)
	}
	// Create in top-down order.
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		e := &fsEntry{
			path: d,
			mode: disk.StatTypeDir | 0o755,
		}
		fsys.entries = append(fsys.entries, e)
		fsys.dirs[d] = e
		fsys.paths[d] = struct{}{}
		fsys.byPath[d] = e
	}
}

// ensureSpool lazily creates the spool temp file.
func (fsys *WriteFS) ensureSpool() error {
	if fsys.spool != nil {
		return nil
	}
	tmp, err := os.CreateTemp(fsys.tempDir, "erofs-createfs-*")
	if err != nil {
		return fmt.Errorf("mkfs: create spool: %w", err)
	}
	os.Remove(tmp.Name())
	fsys.spool = tmp
	return nil
}

func (fsys *WriteFS) lookup(name string) (*fsEntry, error) {
	name = cleanPath(name)
	e, ok := fsys.byPath[name]
	if !ok {
		return nil, fmt.Errorf("mkfs: path not found %q", name)
	}
	return e, nil
}

// Chmod sets permission bits on the named path, preserving type bits.
func (fsys *WriteFS) Chmod(name string, mode fs.FileMode) error {
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	perm := goModeToUnixMode(mode) & 0o7777
	e.mode = (e.mode & disk.StatTypeMask) | perm
	return nil
}

// Chown sets the owner UID and GID on the named path.
func (fsys *WriteFS) Chown(name string, uid, gid int) error {
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.uid = uint32(uid)
	e.gid = uint32(gid)
	return nil
}

// Chtimes sets the access and modification times on the named path.
// EROFS only stores mtime; atime is retained for read-back before Close.
func (fsys *WriteFS) Chtimes(name string, atime time.Time, mtime time.Time) error {
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.atime = uint64(atime.Unix())
	e.atimeNs = uint32(atime.Nanosecond())
	e.mtime = uint64(mtime.Unix())
	e.mtimeNs = uint32(mtime.Nanosecond())
	return nil
}

// Setxattr sets an extended attribute on the named path.
func (fsys *WriteFS) Setxattr(name, attr, value string) error {
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	if e.xattrs == nil {
		e.xattrs = make(map[string]string)
	}
	e.xattrs[attr] = value
	return nil
}

// SetNlink overrides the computed link count on the named path.
func (fsys *WriteFS) SetNlink(name string, nlink uint32) error {
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.nlink = nlink
	e.nlinkSet = true
	return nil
}

// Chmod sets permission bits on the file, matching os.File.Chmod.
func (f *File) Chmod(mode fs.FileMode) error {
	perm := goModeToUnixMode(mode) & 0o7777
	f.entry.mode = (f.entry.mode & disk.StatTypeMask) | perm
	return nil
}

// Chown sets the owner UID and GID on the file, matching os.File.Chown.
func (f *File) Chown(uid, gid int) error {
	f.entry.uid = uint32(uid)
	f.entry.gid = uint32(gid)
	return nil
}

// Write appends data to the file.
func (f *File) Write(p []byte) (int, error) {
	if f.closed {
		return 0, fmt.Errorf("mkfs: write to closed file")
	}

	if f.fs.dataFile != nil {
		n, err := f.fs.dataFile.Write(p)
		f.written += int64(n)
		f.fs.dataOff += int64(n)
		return n, err
	}

	n, err := f.fs.spool.Write(p)
	f.written += int64(n)
	f.fs.spoolOff += int64(n)
	return n, err
}

// Close commits the file entry. For data file mode, pads to block
// boundary and records chunk indexes.
func (f *File) Close() error {
	if f.closed {
		return fmt.Errorf("mkfs: file already closed")
	}
	f.closed = true
	f.entry.fileClosed = true
	f.entry.size = uint64(f.written)

	if f.fs.dataFile != nil {
		return f.closeDataFile()
	}
	return nil
}

// Stat returns file info for the named path. The name is cleaned the same
// way as other WriteFS methods (leading slash, no trailing slash).
func (fsys *WriteFS) Stat(name string) (fs.FileInfo, error) {
	name = cleanPath(name)
	e, ok := fsys.byPath[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return &fileInfo{entry: e}, nil
}

// Open opens the named file for reading. For regular files, the file must
// have been closed (data finalized) before it can be opened for reading.
// For directories, the returned file implements fs.ReadDirFile.
func (fsys *WriteFS) Open(name string) (fs.File, error) {
	name = cleanPath(name)
	e, ok := fsys.byPath[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	typ := e.mode & disk.StatTypeMask
	switch typ {
	case disk.StatTypeDir:
		return &readDir{fsys: fsys, entry: e}, nil

	case disk.StatTypeReg:
		if !e.fileClosed {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("file not yet closed for writing")}
		}
		var sr *io.SectionReader
		if fsys.dataFile != nil {
			sr = io.NewSectionReader(fsys.dataFile, e.dataStartOff, int64(e.size))
		} else if fsys.spool != nil && e.size > 0 {
			sr = io.NewSectionReader(fsys.spool, e.dataStartOff, int64(e.size))
		}
		return &readFile{entry: e, reader: sr}, nil

	default:
		// Symlinks, devices, etc.: stat-only, no readable data.
		return &readFile{entry: e}, nil
	}
}

// fileInfo implements fs.FileInfo for an fsEntry.
type fileInfo struct {
	entry *fsEntry
}

func (fi *fileInfo) Name() string      { return path.Base(fi.entry.path) }
func (fi *fileInfo) Size() int64       { return int64(fi.entry.size) }
func (fi *fileInfo) Mode() fs.FileMode { return disk.EroFSModeToGoFileMode(fi.entry.mode) }
func (fi *fileInfo) ModTime() time.Time {
	return time.Unix(int64(fi.entry.mtime), int64(fi.entry.mtimeNs))
}
func (fi *fileInfo) IsDir() bool  { return fi.entry.mode&disk.StatTypeMask == disk.StatTypeDir }
func (fi *fileInfo) Sys() any     { return nil }

// readFile implements fs.File for reading back a finalized file's data.
type readFile struct {
	entry  *fsEntry
	reader *io.SectionReader // nil for empty files or non-regular types
	closed bool
}

func (f *readFile) Stat() (fs.FileInfo, error) {
	return &fileInfo{entry: f.entry}, nil
}

func (f *readFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fmt.Errorf("mkfs: read from closed file")
	}
	if f.reader == nil {
		return 0, io.EOF
	}
	return f.reader.Read(p)
}

func (f *readFile) Close() error {
	if f.closed {
		return fmt.Errorf("mkfs: file already closed")
	}
	f.closed = true
	return nil
}

// readDir implements fs.ReadDirFile for a directory in WriteFS.
type readDir struct {
	fsys     *WriteFS
	entry    *fsEntry
	children []fs.DirEntry // lazily populated
	offset   int
	closed   bool
}

func (d *readDir) Stat() (fs.FileInfo, error) {
	return &fileInfo{entry: d.entry}, nil
}

func (d *readDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.entry.path, Err: fmt.Errorf("is a directory")}
}

func (d *readDir) Close() error {
	if d.closed {
		return fmt.Errorf("mkfs: dir already closed")
	}
	d.closed = true
	return nil
}

func (d *readDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, fmt.Errorf("mkfs: read from closed dir")
	}
	if d.children == nil {
		d.children = d.collectChildren()
	}

	if n <= 0 {
		entries := d.children[d.offset:]
		d.offset = len(d.children)
		return entries, nil
	}

	remaining := d.children[d.offset:]
	if len(remaining) == 0 {
		return nil, io.EOF
	}
	if n > len(remaining) {
		n = len(remaining)
	}
	entries := remaining[:n]
	d.offset += n
	if d.offset >= len(d.children) {
		return entries, io.EOF
	}
	return entries, nil
}

func (d *readDir) collectChildren() []fs.DirEntry {
	prefix := d.entry.path
	if prefix != "/" {
		prefix += "/"
	}

	var children []fs.DirEntry
	for _, e := range d.fsys.entries {
		if e.path == d.entry.path {
			continue
		}
		if !strings.HasPrefix(e.path, prefix) {
			continue
		}
		// Only direct children: no further slashes after prefix.
		rest := e.path[len(prefix):]
		if strings.Contains(rest, "/") {
			continue
		}
		children = append(children, &dirEntry{entry: e})
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Name() < children[j].Name()
	})
	return children
}

// dirEntry implements fs.DirEntry for an fsEntry.
type dirEntry struct {
	entry *fsEntry
}

func (de *dirEntry) Name() string               { return path.Base(de.entry.path) }
func (de *dirEntry) IsDir() bool                 { return de.entry.mode&disk.StatTypeMask == disk.StatTypeDir }
func (de *dirEntry) Type() fs.FileMode           { return disk.EroFSModeToGoFileMode(de.entry.mode).Type() }
func (de *dirEntry) Info() (fs.FileInfo, error)   { return &fileInfo{entry: de.entry}, nil }

// closeDataFile pads the data file to a block boundary and records chunks.
func (f *File) closeDataFile() error {
	if f.written == 0 {
		return nil
	}

	// Pad to block boundary.
	rem := f.fs.dataOff % disk.BlockSize
	if rem != 0 {
		pad := make([]byte, disk.BlockSize-rem)
		n, err := f.fs.dataFile.Write(pad)
		f.fs.dataOff += int64(n)
		if err != nil {
			return fmt.Errorf("mkfs: pad data file: %w", err)
		}
	}

	// Compute chunks from the start offset and written bytes.
	startBlock := uint64(f.dataStartOff) / disk.BlockSize
	totalBlocks := (uint64(f.written) + disk.BlockSize - 1) / disk.BlockSize

	for totalBlocks > 0 {
		count := totalBlocks
		if count > 65535 {
			count = 65535
		}
		f.entry.chunks = append(f.entry.chunks, Chunk{
			PhysicalBlock: startBlock,
			Count:         uint16(count),
			DeviceID:      1,
		})
		startBlock += count
		totalBlocks -= count
	}

	return nil
}

