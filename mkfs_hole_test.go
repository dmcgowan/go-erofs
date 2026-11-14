package erofs_test

import (
	"bytes"
	"io"
	"io/fs"
	"math"
	"strconv"
	"testing"

	erofs "github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/disk"
)

// TestCreateFSHole verifies that CreateHole writes a regular-file inode
// carrying the requested size but no data: the image holds no bytes for it, a
// read returns that many zeros, and — the property the method exists for — the
// inode's i_size is the size asked for, not zero.
//
// The zero-length-metadata-inode failure this guards against is subtle: an
// overlayfs metacopy copy-up sizes the data it copies by the lower inode's
// i_size, so a metadata file written with no size copies up zero bytes and
// silently drops its redirected content. A hole of the true size is what makes
// a metadata-only image with overlay redirects round-trip, so the metadata,
// mode and overlay xattrs are all exercised here alongside the size.
func TestCreateFSHole(t *testing.T) {
	const (
		holeSize = 149                 // arbitrary; must survive to i_size
		redirect = "/aa/bb/ccddeeff01" // an overlay redirect target
	)

	var buf testBuffer
	fsys := erofs.Create(&buf)
	if err := fsys.CreateHole("/meta.txt", holeSize); err != nil {
		t.Fatalf("CreateHole: %v", err)
	}
	// Metacopy use: the hole carries the overlay redirect and metacopy
	// marker and a real mode, all set after creation exactly as for a file
	// created any other way.
	if err := fsys.Chmod("/meta.txt", 0o640); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := fsys.Setxattr("/meta.txt", "trusted.overlay.redirect", redirect); err != nil {
		t.Fatalf("Setxattr redirect: %v", err)
	}
	if err := fsys.Setxattr("/meta.txt", "trusted.overlay.metacopy", ""); err != nil {
		t.Fatalf("Setxattr metacopy: %v", err)
	}
	// A zero-size hole is a valid empty file.
	if err := fsys.CreateHole("/empty", 0); err != nil {
		t.Fatalf("CreateHole(0): %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The hole is chunk-based even though it maps no chunks, so the
	// superblock must advertise chunked files.
	if sb := readSuperBlock(t, buf.Bytes()); sb.FeatureIncompat&disk.FeatureIncompatChunkedFile == 0 {
		t.Errorf("FeatureIncompat = %#x, want chunked_file (%#x) set", sb.FeatureIncompat, disk.FeatureIncompatChunkedFile)
	}

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	fi, err := fs.Stat(efs, "meta.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// The whole point: the size survives to i_size.
	if fi.Size() != holeSize {
		t.Errorf("hole Size = %d, want %d", fi.Size(), holeSize)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("hole mode = %o, want %o", got, 0o640)
	}
	if fi.IsDir() {
		t.Error("hole IsDir = true, want regular file")
	}

	// The overlay xattrs round-trip.
	xg, ok := fi.(interface {
		GetXattr(string) (string, bool)
	})
	if !ok {
		t.Fatal("FileInfo does not expose GetXattr")
	}
	if v, ok := xg.GetXattr("trusted.overlay.redirect"); !ok || v != redirect {
		t.Errorf("redirect xattr = %q (ok=%v), want %q", v, ok, redirect)
	}
	if v, ok := xg.GetXattr("trusted.overlay.metacopy"); !ok || v != "" {
		t.Errorf("metacopy xattr = %q (ok=%v), want empty", v, ok)
	}

	// The image holds no data for the hole, so a read returns holeSize zeros.
	got, err := fs.ReadFile(efs, "meta.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != holeSize {
		t.Errorf("ReadFile returned %d bytes, want %d", len(got), holeSize)
	}
	if !bytes.Equal(got, make([]byte, holeSize)) {
		t.Errorf("hole content is not all zeros: %x", got)
	}

	// The zero-size hole is a valid empty regular file.
	efi, err := fs.Stat(efs, "empty")
	if err != nil {
		t.Fatalf("Stat empty: %v", err)
	}
	if efi.Size() != 0 {
		t.Errorf("empty hole Size = %d, want 0", efi.Size())
	}
}

// TestCreateFSHoleErrors covers CreateHole's argument refusals.
func TestCreateFSHoleErrors(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)
	if err := fsys.CreateHole("/", 10); err == nil {
		t.Error("CreateHole at root: got nil error, want a refusal")
	}
	if err := fsys.CreateHole("/neg", -1); err == nil {
		t.Error("CreateHole with negative size: got nil error, want a refusal")
	}
}

// TestCreateFSHoleBlockSizeDeferred verifies CreateHole leaves the block size
// unset, so a later CopyFrom can still adopt the source's block size.
func TestCreateFSHoleBlockSizeDeferred(t *testing.T) {
	var src testBuffer
	sw := erofs.Create(&src, erofs.WithBlockSize(512))
	if err := sw.CreateHole("/a", 10); err != nil {
		t.Fatalf("CreateHole: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	img, err := erofs.Open(bytes.NewReader(src.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var buf testBuffer
	fsys := erofs.Create(&buf)
	if err := fsys.CreateHole("/h", 10); err != nil {
		t.Fatalf("CreateHole: %v", err)
	}
	if err := fsys.CopyFrom(img); err != nil {
		t.Fatalf("CopyFrom with 512-byte block size after CreateHole: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Both holes read back at the adopted block size.
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, name := range []string{"h", "a"} {
		got, err := fs.ReadFile(efs, name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if !bytes.Equal(got, make([]byte, 10)) {
			t.Errorf("%s = %x, want 10 zeros", name, got)
		}
	}
}

// TestCreateFSHoleLarge verifies a hole's metadata does not grow with its size
// and that sizes up to the int64 maximum round-trip.
func TestCreateFSHoleLarge(t *testing.T) {
	for _, size := range []int64{1 << 40, 1<<63 - 1} {
		var buf testBuffer
		fsys := erofs.Create(&buf)
		if err := fsys.CreateHole("/h", size); err != nil {
			t.Fatalf("CreateHole(%d): %v", size, err)
		}
		if err := fsys.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if size == 1<<40 && len(buf.Bytes()) > 3*4096 {
			t.Errorf("1 TiB hole image is %d bytes, want at most %d", len(buf.Bytes()), 3*4096)
		}

		efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		f, err := efs.Open("h")
		if err != nil {
			t.Fatalf("Open h: %v", err)
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi.Size() != size {
			t.Errorf("Size = %d, want %d", fi.Size(), size)
		}
		// The reader counts blocks in an int, so it cannot read a file of
		// more than 2^31 blocks where int is 32 bits.
		if strconv.IntSize == 32 && size>>12 > math.MaxInt32 {
			if err := f.Close(); err != nil {
				t.Fatalf("Close h: %v", err)
			}
			continue
		}
		p := make([]byte, 8192)
		if _, err := io.ReadFull(f, p); err != nil {
			t.Fatalf("read %d-byte hole: %v", size, err)
		}
		if !bytes.Equal(p, make([]byte, len(p))) {
			t.Errorf("%d-byte hole head is not all zeros", size)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close h: %v", err)
		}
	}
}

// TestCreateFSHoleWriterRead verifies a hole reads as zeros through the Writer
// before the image is written, rather than as another file's spooled data, and
// that copying such a Writer keeps the hole's size and zero content.
func TestCreateFSHoleWriterRead(t *testing.T) {
	const holeSize = 3000

	var src testBuffer
	sw := erofs.Create(&src)
	f, err := sw.Create("/data")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("X"), 5000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close data: %v", err)
	}
	if err := sw.CreateHole("/h", holeSize); err != nil {
		t.Fatalf("CreateHole: %v", err)
	}

	got, err := fs.ReadFile(sw, "h")
	if err != nil {
		t.Fatalf("ReadFile from Writer: %v", err)
	}
	if !bytes.Equal(got, make([]byte, holeSize)) {
		t.Errorf("hole read through Writer: %d bytes, not %d zeros", len(got), holeSize)
	}

	var dst testBuffer
	dw := erofs.Create(&dst)
	if err := dw.CopyFrom(sw); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	efs, err := erofs.Open(bytes.NewReader(dst.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err = fs.ReadFile(efs, "h")
	if err != nil {
		t.Fatalf("ReadFile from copy: %v", err)
	}
	if !bytes.Equal(got, make([]byte, holeSize)) {
		t.Errorf("copied hole: %d bytes, not %d zeros", len(got), holeSize)
	}
}
