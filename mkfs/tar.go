package mkfs

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/erofs/go-erofs/internal/disk"
)

const (
	whiteoutPrefix     = ".wh."
	opaqueWhiteout     = ".wh..wh..opq"
	overlayOpaqueXattr = "trusted.overlay.opaque"
)

// TarOpt configures the tar source adapter.
type TarOpt func(*tarSource)

// ConvertWhiteouts enables AUFS-to-overlayfs whiteout conversion.
// AUFS .wh.<name> files become overlayfs character device 0/0 whiteouts,
// and .wh..wh..opq markers become trusted.overlay.opaque xattrs.
func ConvertWhiteouts() TarOpt {
	return func(s *tarSource) {
		s.convertWhiteouts = true
	}
}

type tarSource struct {
	r                io.Reader
	convertWhiteouts bool
}

// Tar returns a Source that reads a tar stream.
//
// If r implements io.ReaderAt, file data is read directly from the
// source during the write phase (zero-copy). Otherwise, file data is
// spooled to a temporary file during parsing and read back during write.
func Tar(r io.Reader, opts ...TarOpt) Source {
	s := &tarSource{r: r}
	for _, o := range opts {
		o(s)
	}
	return s
}

type tarEntry struct {
	path  string
	entry *Entry
}

func (s *tarSource) Walk(fn func(string, *Entry) error) error {
	ds, err := newDataStore(s.r)
	if err != nil {
		return err
	}

	cr := &countingReader{r: s.r}
	tr := tar.NewReader(cr)

	var entries []tarEntry
	var dirsByPath map[string]*Entry
	if s.convertWhiteouts {
		dirsByPath = make(map[string]*Entry)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		p := path.Clean("/" + hdr.Name)

		if s.convertWhiteouts {
			base := path.Base(p)
			if strings.HasPrefix(base, whiteoutPrefix) {
				if base == opaqueWhiteout {
					dir := path.Dir(p)
					if de, ok := dirsByPath[dir]; ok {
						if de.Xattrs == nil {
							de.Xattrs = make(map[string]string)
						}
						de.Xattrs[overlayOpaqueXattr] = "y"
					}
					continue
				}
				origName := base[len(whiteoutPrefix):]
				entries = append(entries, tarEntry{
					path: path.Join(path.Dir(p), origName),
					entry: &Entry{
						Mode:  disk.StatTypeChrdev | 0o666,
						UID:   uint32(hdr.Uid),
						GID:   uint32(hdr.Gid),
						Mtime: uint64(hdr.ModTime.Unix()),
						Nlink: 1,
					},
				})
				continue
			}
		}

		dataPos := cr.pos
		e, err := s.headerToEntry(ds, tr, hdr, dataPos)
		if err != nil {
			return fmt.Errorf("tar: %s: %w", p, err)
		}
		if e == nil {
			continue
		}

		if s.convertWhiteouts && e.Mode&disk.StatTypeMask == disk.StatTypeDir {
			dirsByPath[p] = e
		}

		entries = append(entries, tarEntry{path: p, entry: e})
	}

	for _, te := range entries {
		if err := fn(te.path, te.entry); err != nil {
			return err
		}
	}
	return nil
}

// dataStore manages file data spooling. For ReaderAt-backed tars,
// it records offsets into the source. For streaming readers, it
// writes data to a temp file.
type dataStore struct {
	ra   io.ReaderAt
	tmp  *os.File
	toff int64
}

func newDataStore(r io.Reader) (*dataStore, error) {
	if ra, ok := r.(io.ReaderAt); ok {
		return &dataStore{ra: ra}, nil
	}
	tmp, err := os.CreateTemp("", "erofs-tar-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	os.Remove(tmp.Name())
	return &dataStore{tmp: tmp}, nil
}

// store records file data and returns a reader for deferred access.
func (ds *dataStore) store(tr *tar.Reader, pos int64, size int64) (io.Reader, error) {
	if ds.ra != nil {
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return nil, err
		}
		return io.NewSectionReader(ds.ra, pos, size), nil
	}
	off := ds.toff
	n, err := io.Copy(ds.tmp, tr)
	if err != nil {
		return nil, err
	}
	ds.toff += n
	return io.NewSectionReader(ds.tmp, off, size), nil
}

// countingReader wraps a reader and tracks bytes consumed.
type countingReader struct {
	r   io.Reader
	pos int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.pos += int64(n)
	return n, err
}

// headerToEntry converts a tar header to an Entry. Returns nil for
// unsupported entry types.
func (s *tarSource) headerToEntry(ds *dataStore, tr *tar.Reader, hdr *tar.Header, dataPos int64) (*Entry, error) {
	e := &Entry{
		UID:    uint32(hdr.Uid),
		GID:    uint32(hdr.Gid),
		Mtime:  uint64(hdr.ModTime.Unix()),
		Size:   uint64(hdr.Size),
		Nlink:  1,
		Xattrs: make(map[string]string),
	}

	const xattrPrefix = "SCHILY.xattr."
	for k, v := range hdr.PAXRecords {
		if len(k) > len(xattrPrefix) && k[:len(xattrPrefix)] == xattrPrefix {
			e.Xattrs[k[len(xattrPrefix):]] = v
		}
	}

	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		e.Mode = disk.StatTypeReg | uint16(hdr.Mode&0o7777)
		if hdr.Size > 0 {
			data, err := ds.store(tr, dataPos, hdr.Size)
			if err != nil {
				return nil, err
			}
			e.Data = data
		}
	case tar.TypeDir:
		e.Mode = disk.StatTypeDir | uint16(hdr.Mode&0o7777)
		e.Size = 0
		e.Nlink = 2
	case tar.TypeSymlink:
		e.Mode = disk.StatTypeSymlink | uint16(hdr.Mode&0o7777)
		e.LinkTarget = hdr.Linkname
		e.Size = 0
	case tar.TypeChar:
		e.Mode = disk.StatTypeChrdev | uint16(hdr.Mode&0o7777)
		e.Rdev = uint32(hdr.Devmajor<<8 | hdr.Devminor)
		e.Size = 0
	case tar.TypeBlock:
		e.Mode = disk.StatTypeBlkdev | uint16(hdr.Mode&0o7777)
		e.Rdev = uint32(hdr.Devmajor<<8 | hdr.Devminor)
		e.Size = 0
	case tar.TypeFifo:
		e.Mode = disk.StatTypeFifo | uint16(hdr.Mode&0o7777)
		e.Size = 0
	default:
		return nil, nil
	}

	return e, nil
}
