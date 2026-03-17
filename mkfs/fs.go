package mkfs

import (
	"fmt"
	"io"
	"io/fs"
	"syscall"

	"github.com/erofs/go-erofs/internal/disk"
)

// fsSource implements Source by walking an fs.FS.
type fsSource struct {
	fsys fs.FS
}

// FS returns a Source that walks an fs.FS. Full-image mode only.
// Extended metadata (UID, GID, etc.) is extracted from FileInfo.Sys()
// if the underlying type is *syscall.Stat_t.
func FS(fsys fs.FS) Source {
	return &fsSource{fsys: fsys}
}

func (s *fsSource) Walk(fn func(string, *Entry) error) error {
	return fs.WalkDir(s.fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}

		e := &Entry{
			Mode:  goModeToUnixMode(info.Mode()),
			Size:  uint64(info.Size()),
			Nlink: 1,
		}

		// Extract extended metadata from syscall.Stat_t if available
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			e.UID = st.Uid
			e.GID = st.Gid
			e.Mtime = uint64(st.Mtim.Sec)
			e.MtimeNs = uint32(st.Mtim.Nsec)
			e.Nlink = uint32(st.Nlink)
			e.Rdev = uint32(st.Rdev)
		}

		if info.Mode().IsDir() {
			if e.Nlink < 2 {
				e.Nlink = 2
			}
		}

		// Normalize path to absolute
		p := "/" + path
		if path == "." {
			p = "/"
		}

		switch {
		case info.Mode().IsRegular() && info.Size() > 0:
			f, err := s.fsys.Open(path)
			if err != nil {
				return fmt.Errorf("open %s: %w", path, err)
			}
			e.Data = &lazyFileReader{f: f}

		case info.Mode()&fs.ModeSymlink != 0:
			if rl, ok := s.fsys.(interface{ ReadLink(string) (string, error) }); ok {
				target, err := rl.ReadLink(path)
				if err != nil {
					return fmt.Errorf("readlink %s: %w", path, err)
				}
				e.LinkTarget = target
			}
		}

		return fn(p, e)
	})
}

// lazyFileReader wraps an fs.File for deferred reading.
type lazyFileReader struct {
	f fs.File
}

func (r *lazyFileReader) Read(p []byte) (int, error) {
	return r.f.(io.Reader).Read(p)
}

// goModeToUnixMode converts Go fs.FileMode to Unix mode bits.
func goModeToUnixMode(m fs.FileMode) uint16 {
	mode := uint16(m.Perm())

	if m&fs.ModeSetuid != 0 {
		mode |= disk.StatTypeIsUID
	}
	if m&fs.ModeSetgid != 0 {
		mode |= disk.StatTypeIsGID
	}
	if m&fs.ModeSticky != 0 {
		mode |= disk.StatTypeIsVTX
	}

	switch m.Type() {
	case 0: // regular file
		mode |= disk.StatTypeReg
	case fs.ModeDir:
		mode |= disk.StatTypeDir
	case fs.ModeSymlink:
		mode |= disk.StatTypeSymlink
	case fs.ModeDevice | fs.ModeCharDevice:
		mode |= disk.StatTypeChrdev
	case fs.ModeDevice:
		mode |= disk.StatTypeBlkdev
	case fs.ModeNamedPipe:
		mode |= disk.StatTypeFifo
	case fs.ModeSocket:
		mode |= disk.StatTypeSock
	}

	return mode
}
