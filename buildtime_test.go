package erofs_test

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"testing"
	"time"

	erofs "github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/disk"
)

type mtimeEntry struct {
	name string
	sec  int64
	nsec int64
}

// writeMtimeImage writes "/" and one regular file per entry. The root uses
// rootSec; every file uses its own mtime.
func writeMtimeImage(t *testing.T, rootSec int64, entries []mtimeEntry, opts ...erofs.CreateOpt) []byte {
	t.Helper()
	buf := &testBuffer{}
	w := erofs.Create(buf, opts...)
	for _, e := range entries {
		f, err := w.Create(e.name)
		if err != nil {
			t.Fatalf("Create(%q): %v", e.name, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close(%q): %v", e.name, err)
		}
		if err := w.Chtimes(e.name, time.Time{}, time.Unix(e.sec, e.nsec)); err != nil {
			t.Fatalf("Chtimes(%q): %v", e.name, err)
		}
	}
	if err := w.Chtimes("/", time.Time{}, time.Unix(rootSec, 0)); err != nil {
		t.Fatalf("Chtimes(/): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func readSuperBlock(t *testing.T, img []byte) disk.SuperBlock {
	t.Helper()
	var sb disk.SuperBlock
	if _, err := binary.Decode(img[disk.SuperBlockOffset:], binary.LittleEndian, &sb); err != nil {
		t.Fatalf("decode superblock: %v", err)
	}
	return sb
}

// isCompact reports whether the inode for name is a compact inode by
// reading its format bit directly from the image.
func isCompact(t *testing.T, img []byte, fsys fs.FS, name string) bool {
	t.Helper()
	fi, err := fs.Stat(fsys, name)
	if err != nil {
		t.Fatalf("Stat(%q): %v", name, err)
	}
	st := fi.Sys().(*erofs.Stat)
	sb := readSuperBlock(t, img)
	off := int64(sb.MetaBlkAddr)<<sb.BlkSizeBits + st.Ino*disk.SizeInodeCompact
	return binary.LittleEndian.Uint16(img[off:])&1 == 0
}

func openImage(t *testing.T, img []byte) fs.FS {
	t.Helper()
	fsys, err := erofs.EroFS(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("EroFS: %v", err)
	}
	return fsys
}

func checkMtimes(t *testing.T, fsys fs.FS, entries []mtimeEntry) {
	t.Helper()
	for _, e := range entries {
		fi, err := fs.Stat(fsys, e.name)
		if err != nil {
			t.Fatalf("Stat(%q): %v", e.name, err)
		}
		if got, want := fi.ModTime(), time.Unix(e.sec, e.nsec); !got.Equal(want) {
			t.Errorf("Stat(%q).ModTime() = %v, want %v", e.name, got, want)
		}
	}
}

func TestBuildTimeDeterministic(t *testing.T) {
	entries := []mtimeEntry{{"/a", 100, 0}, {"/b", 200, 5}, {"/c", 1700000000, 0}}
	img1 := writeMtimeImage(t, 100, entries)
	time.Sleep(1100 * time.Millisecond)
	img2 := writeMtimeImage(t, 100, entries)
	if !bytes.Equal(img1, img2) {
		t.Error("images written without WithBuildTime differ")
	}
	checkMtimes(t, openImage(t, img1), entries)
}

func TestBuildTimeDerived(t *testing.T) {
	for _, tc := range []struct {
		name    string
		root    int64
		entries []mtimeEntry
		want    uint64
		compact []string
	}{
		{
			name:    "most frequent",
			root:    300,
			entries: []mtimeEntry{{"/a", 100, 0}, {"/b", 100, 0}, {"/c", 100, 0}, {"/d", 200, 0}},
			want:    100,
			compact: []string{"a", "b", "c"},
		},
		{
			name:    "tie picks smallest",
			root:    50,
			entries: []mtimeEntry{{"/a", 200, 0}, {"/b", 200, 0}, {"/c", 100, 0}, {"/d", 100, 0}},
			want:    100,
			compact: []string{"c", "d"},
		},
		{
			name:    "no whole seconds",
			root:    0,
			entries: []mtimeEntry{{"/a", 100, 1}, {"/b", 100, 1}, {"/c", 200, 999999999}},
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := writeMtimeImage(t, tc.root, tc.entries)
			sb := readSuperBlock(t, img)
			if sb.BuildTime != tc.want || sb.BuildTimeNs != 0 {
				t.Errorf("build time = (%d, %d), want (%d, 0)", sb.BuildTime, sb.BuildTimeNs, tc.want)
			}
			fsys := openImage(t, img)
			compact := map[string]bool{}
			for _, n := range tc.compact {
				compact[n] = true
			}
			for _, e := range tc.entries {
				n := e.name[1:]
				if got := isCompact(t, img, fsys, n); got != compact[n] {
					t.Errorf("%s compact = %v, want %v", n, got, compact[n])
				}
			}
			checkMtimes(t, fsys, tc.entries)
		})
	}
}

// setMtime sets the mtime of name in w.
func setMtime(t *testing.T, w *erofs.Writer, name string, sec, nsec int64) {
	t.Helper()
	if err := w.Chtimes(name, time.Time{}, time.Unix(sec, nsec)); err != nil {
		t.Fatalf("Chtimes(%q): %v", name, err)
	}
}

// createEmpty creates an empty regular file name in w with the given mtime.
func createEmpty(t *testing.T, w *erofs.Writer, name string, sec, nsec int64) {
	t.Helper()
	f, err := w.Create(name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", name, err)
	}
	setMtime(t, w, name, sec, nsec)
}

func TestBuildTimeNoWholeSecondsRoot(t *testing.T) {
	buf := &testBuffer{}
	w := erofs.Create(buf)
	createEmpty(t, w, "/a", 100, 7)
	setMtime(t, w, "/", 200, 9)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if sb := readSuperBlock(t, buf.Bytes()); sb.BuildTime != 0 {
		t.Errorf("build time = %d, want 0", sb.BuildTime)
	}
	checkMtimes(t, openImage(t, buf.Bytes()), []mtimeEntry{{".", 200, 9}, {"a", 100, 7}})
}

func TestBuildTimeHardLinksCountOnce(t *testing.T) {
	buf := &testBuffer{}
	w := erofs.Create(buf)
	createEmpty(t, w, "/x", 200, 0)
	createEmpty(t, w, "/y", 200, 0)
	createEmpty(t, w, "/h", 100, 0)
	for _, n := range []string{"/h2", "/h3"} {
		if err := w.Link("/h", n); err != nil {
			t.Fatalf("Link(%q): %v", n, err)
		}
	}
	setMtime(t, w, "/", 300, 0)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Three names at 100 but one inode; two inodes at 200.
	if sb := readSuperBlock(t, buf.Bytes()); sb.BuildTime != 200 {
		t.Errorf("build time = %d, want 200", sb.BuildTime)
	}
	checkMtimes(t, openImage(t, buf.Bytes()), []mtimeEntry{
		{"x", 200, 0}, {"y", 200, 0}, {"h", 100, 0}, {"h2", 100, 0}, {"h3", 100, 0}, {".", 300, 0},
	})
}

func TestBuildTimeExplicitWins(t *testing.T) {
	entries := []mtimeEntry{{"/a", 100, 0}, {"/b", 100, 0}, {"/c", 5, 0}}
	img := writeMtimeImage(t, 100, entries, erofs.WithBuildTime(5, 0))
	if sb := readSuperBlock(t, img); sb.BuildTime != 5 {
		t.Errorf("build time = %d, want 5", sb.BuildTime)
	}
	fsys := openImage(t, img)
	if !isCompact(t, img, fsys, "c") || isCompact(t, img, fsys, "a") {
		t.Error("only c should be compact")
	}
	checkMtimes(t, fsys, entries)
}

func TestBuildTimeCopyFromImageWins(t *testing.T) {
	entries := []mtimeEntry{{"/a", 100, 0}, {"/b", 100, 0}}
	src := writeMtimeImage(t, 100, entries, erofs.WithBuildTime(42, 0))

	buf := &testBuffer{}
	w := erofs.Create(buf)
	if err := w.CopyFrom(openImage(t, src)); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if sb := readSuperBlock(t, buf.Bytes()); sb.BuildTime != 42 {
		t.Errorf("build time = %d, want 42", sb.BuildTime)
	}
	checkMtimes(t, openImage(t, buf.Bytes()), entries)
}

func TestBuildTimeNanosecondCompact(t *testing.T) {
	entries := []mtimeEntry{{"/a", 100, 0}, {"/b", 100, 500}}
	img := writeMtimeImage(t, 100, entries, erofs.WithBuildTime(100, 500))
	fsys := openImage(t, img)
	if isCompact(t, img, fsys, "a") {
		t.Error("a (100, 0) is compact with build time (100, 500)")
	}
	if !isCompact(t, img, fsys, "b") {
		t.Error("b (100, 500) is not compact with build time (100, 500)")
	}
	checkMtimes(t, fsys, entries)
}
