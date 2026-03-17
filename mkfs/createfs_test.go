package mkfs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/disk"
	"github.com/erofs/go-erofs/internal/erofstest"
)

// TestCreateFSSpool exercises spool mode: CreateFS without a data file.
func TestCreateFSSpool(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	// Create a regular file.
	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello world\n"))
	f.Close()

	// Create a directory and a file inside it.
	fsys.Mkdir("/subdir", 0o755)
	f2, err := fsys.Create("/subdir/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	f2.Write([]byte("nested\n"))
	f2.Close()

	// Create a symlink.
	fsys.Symlink("hello.txt", "/link")

	// Create an empty file.
	f3, err := fsys.Create("/empty")
	if err != nil {
		t.Fatal(err)
	}
	f3.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Read back the image.
	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "hello.txt", "hello world\n")
	erofstest.CheckFile(t, efs, "subdir/nested.txt", "nested\n")
	erofstest.CheckFile(t, efs, "empty", "")
	erofstest.CheckSymlink(t, efs, "link", "hello.txt")
	erofstest.CheckDirEntries(t, efs, ".", []string{"empty", "hello.txt", "link", "subdir"})
	erofstest.CheckDirEntries(t, efs, "subdir", []string{"nested.txt"})
}

// TestCreateFSDataFile exercises data file mode (metadata-only).
func TestCreateFSDataFile(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()

	var metaBuf bytes.Buffer
	fsys := CreateFS(&metaBuf, WithDataFile(df))

	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("data file mode\n"))
	f.Close()

	f2, err := fsys.Create("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("ABCDEFGH"), 1024) // 8KB
	f2.Write(data)
	f2.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Re-open data file as ReaderAt for verification.
	dfRead, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dfRead.Close()

	efs, err := erofs.EroFS(bytes.NewReader(metaBuf.Bytes()), erofs.WithExtraDevices(dfRead))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "hello.txt", "data file mode\n")
	erofstest.CheckFileBytes(t, efs, "big.bin", data)
}

// TestCreateFSMetadata verifies Chmod, Chown, Setxattr, SetMtime.
func TestCreateFSMetadata(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("content\n"))
	f.Chmod(0o755)
	f.Chown(1000, 2000)
	f.Close()

	fsys.Setxattr("/file.txt", "user.test", "value123")
	fsys.Chtimes("/file.txt", time.Time{}, time.Unix(1700000000, 123456789))

	if err := fsys.Mkdir("/mydir", 0o755); err != nil {
		t.Fatal(err)
	}
	fsys.Chmod("/mydir", 0o700)
	fsys.Chown("/mydir", 500, 600)

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	// Check file metadata.
	st := erofstest.Stat(t, efs, "file.txt")
	if st.Mode.Perm() != 0o755 {
		t.Errorf("file perm: got %o, want 755", st.Mode.Perm())
	}
	if st.UID != 1000 || st.GID != 2000 {
		t.Errorf("file uid/gid: got %d/%d, want 1000/2000", st.UID, st.GID)
	}
	if st.Mtime != 1700000000 {
		t.Errorf("file mtime: got %d, want 1700000000", st.Mtime)
	}
	if st.MtimeNs != 123456789 {
		t.Errorf("file mtimeNs: got %d, want 123456789", st.MtimeNs)
	}
	erofstest.CheckXattrs(t, efs, "file.txt", map[string]string{"user.test": "value123"})

	// Check dir metadata.
	dst := erofstest.Stat(t, efs, "mydir")
	if dst.Mode.Perm() != 0o700 {
		t.Errorf("dir perm: got %o, want 700", dst.Mode.Perm())
	}
	if dst.UID != 500 || dst.GID != 600 {
		t.Errorf("dir uid/gid: got %d/%d, want 500/600", dst.UID, dst.GID)
	}
}

// TestCreateFSMknod verifies char and block device creation.
func TestCreateFSMknod(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	fsys.Mknod("/null", disk.StatTypeChrdev|0o666, 1<<8|3) // major 1, minor 3
	fsys.Mknod("/sda", disk.StatTypeBlkdev|0o660, 8<<8|0)  // major 8, minor 0

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckDevice(t, efs, "null", fs.ModeCharDevice, 1<<8|3)
	erofstest.CheckDevice(t, efs, "sda", fs.ModeDevice, 8<<8|0)
}

// TestCreateFSLargeFile tests a file that spans many blocks and exercises
// the Chunk.Count uint16 split for files > 65535 blocks.
func TestCreateFSLargeFile(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	// 128KB file — enough to span multiple blocks but not absurdly large.
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	f, err := fsys.Create("/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data)
	f.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFileBytes(t, efs, "large.bin", data)
}

// TestCreateFSLargeFileDataFile tests a large file with data file mode,
// including chunk splitting for files > 65535 blocks.
func TestCreateFSLargeFileDataFile(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()

	var metaBuf bytes.Buffer
	fsys := CreateFS(&metaBuf, WithDataFile(df))

	// 128KB file.
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	f, err := fsys.Create("/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data)
	f.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	dfRead, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dfRead.Close()

	efs, err := erofs.EroFS(bytes.NewReader(metaBuf.Bytes()), erofs.WithExtraDevices(dfRead))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFileBytes(t, efs, "large.bin", data)
}

// TestCreateFSErrors tests error cases.
func TestCreateFSErrors(t *testing.T) {
	t.Run("duplicate path", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		f, err := fsys.Create("/dup.txt")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		_, err = fsys.Create("/dup.txt")
		if err == nil {
			t.Fatal("expected error for duplicate path")
		}
	})

	t.Run("write after close", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		f, err := fsys.Create("/file.txt")
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("data"))
		f.Close()

		_, err = f.Write([]byte("more"))
		if err == nil {
			t.Fatal("expected error writing to closed file")
		}
	})

	t.Run("file double close", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		f, err := fsys.Create("/file.txt")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		err = f.Close()
		if err == nil {
			t.Fatal("expected error on double close")
		}
	})

	t.Run("FS double close", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		if err := fsys.Close(); err != nil {
			t.Fatal(err)
		}
		err := fsys.Close()
		if err == nil {
			t.Fatal("expected error on FS double close")
		}
	})

	t.Run("create after FS close", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		fsys.Close()
		_, err := fsys.Create("/file.txt")
		if err == nil {
			t.Fatal("expected error creating after FS close")
		}
	})
}

// TestCreateFSImplicitDirs verifies that parent directories are created
// implicitly when creating deeply nested files.
func TestCreateFSImplicitDirs(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	f, err := fsys.Create("/a/b/c/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("deep\n"))
	f.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "a/b/c/deep.txt", "deep\n")

	// Verify implicit directories exist.
	for _, dir := range []string{"a", "a/b", "a/b/c"} {
		fi, err := fs.Stat(efs, dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s: not a directory", dir)
		}
	}
}

// TestCreateFSEmpty verifies that an empty FS produces a valid image.
func TestCreateFSEmpty(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	entries, err := fs.ReadDir(efs, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty root dir, got %d entries", len(entries))
	}
}

// TestCreateFSSetNlink verifies that SetNlink overrides computed nlink.
func TestCreateFSSetNlink(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("data"))
	f.Close()

	fsys.SetNlink("/file.txt", 42)

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	st := erofstest.Stat(t, efs, "file.txt")
	if st.Nlink != 42 {
		t.Errorf("nlink: got %d, want 42", st.Nlink)
	}
}

// TestCreateFSDirNlink verifies that directory nlink = 2 + child_dir_count.
func TestCreateFSDirNlink(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	fsys.Mkdir("/parent", 0o755)
	fsys.Mkdir("/parent/child1", 0o755)
	fsys.Mkdir("/parent/child2", 0o755)

	f, err := fsys.Create("/parent/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	st := erofstest.Stat(t, efs, "parent")
	// nlink should be 2 (self + parent) + 2 (child dirs) = 4
	if st.Nlink != 4 {
		t.Errorf("parent nlink: got %d, want 4", st.Nlink)
	}
}

// TestCreateFSWithTempDir verifies that WithTempDir is respected.
func TestCreateFSWithTempDir(t *testing.T) {
	tmpDir := t.TempDir()
	var buf bytes.Buffer
	fsys := CreateFS(&buf, WithTempDir(tmpDir))

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello"))
	f.Close()

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	erofstest.CheckFile(t, efs, "file.txt", "hello")
}

// TestCreateFSRootMetadata verifies that Mkdir("/") sets root permissions
// and path-based methods can set metadata on it.
func TestCreateFSRootMetadata(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	if err := fsys.Mkdir("/", 0o700); err != nil {
		t.Fatal(err)
	}
	fsys.Chown("/", 1000, 2000)

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	st := erofstest.Stat(t, efs, ".")
	if st.Mode.Perm() != 0o700 {
		t.Errorf("root perm: got %o, want 700", st.Mode.Perm())
	}
	if st.UID != 1000 || st.GID != 2000 {
		t.Errorf("root uid/gid: got %d/%d, want 1000/2000", st.UID, st.GID)
	}
}

// TestCreateFSMultipleFiles tests creating many files to exercise the
// spool and verify ordering.
func TestCreateFSMultipleFiles(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	for i := range 100 {
		f, err := fsys.Create(fmt.Sprintf("/file%03d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(fmt.Sprintf("content %d\n", i)))
		f.Close()
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}

	for i := range 100 {
		name := fmt.Sprintf("file%03d.txt", i)
		expected := fmt.Sprintf("content %d\n", i)
		erofstest.CheckFile(t, efs, name, expected)
	}

	// Verify directory has all 100 entries.
	entries, err := fs.ReadDir(efs, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100 {
		t.Errorf("got %d entries, want 100", len(entries))
	}
}

// TestWriteFSOpen tests Open and Read for regular files in spool mode.
func TestWriteFSOpen(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello world\n"))
	f.Close()

	// Open and read back.
	rf, err := fsys.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world\n" {
		t.Errorf("got %q, want %q", got, "hello world\n")
	}
}

// TestWriteFSOpenDataFile tests Open and Read for regular files in data file mode.
func TestWriteFSOpenDataFile(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()

	var metaBuf bytes.Buffer
	fsys := CreateFS(&metaBuf, WithDataFile(df))

	f, err := fsys.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("data file content\n"))
	f.Close()

	rf, err := fsys.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data file content\n" {
		t.Errorf("got %q, want %q", got, "data file content\n")
	}
}

// TestWriteFSOpenEmpty tests Open on an empty file.
func TestWriteFSOpenEmpty(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	f, err := fsys.Create("/empty")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	rf, err := fsys.Open("/empty")
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d bytes", len(got))
	}
}

// TestWriteFSStat tests Stat for various entry types.
func TestWriteFSStat(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	f, err := fsys.Create("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("content\n"))
	f.Chmod(0o755)
	f.Close()

	fsys.Chtimes("/file.txt", time.Time{}, time.Unix(1700000000, 123456789))
	fsys.Mkdir("/dir", 0o700)
	fsys.Symlink("file.txt", "/link")
	fsys.Mknod("/null", disk.StatTypeChrdev|0o666, 1<<8|3)

	// Stat regular file.
	fi, err := fsys.Stat("/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Name() != "file.txt" {
		t.Errorf("name: got %q, want %q", fi.Name(), "file.txt")
	}
	if fi.Size() != 8 {
		t.Errorf("size: got %d, want 8", fi.Size())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode: got %o, want 755", fi.Mode().Perm())
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("expected regular file mode")
	}
	if fi.ModTime() != time.Unix(1700000000, 123456789) {
		t.Errorf("modtime: got %v, want %v", fi.ModTime(), time.Unix(1700000000, 123456789))
	}

	// Stat directory.
	di, err := fsys.Stat("/dir")
	if err != nil {
		t.Fatal(err)
	}
	if !di.IsDir() {
		t.Error("expected directory")
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode: got %o, want 700", di.Mode().Perm())
	}

	// Stat symlink.
	li, err := fsys.Stat("/link")
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode().Type() != fs.ModeSymlink {
		t.Errorf("expected symlink, got %v", li.Mode().Type())
	}

	// Stat device.
	ni, err := fsys.Stat("/null")
	if err != nil {
		t.Fatal(err)
	}
	if ni.Mode()&fs.ModeCharDevice == 0 {
		t.Errorf("expected char device, got %v", ni.Mode())
	}

	// Stat root.
	ri, err := fsys.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if !ri.IsDir() {
		t.Error("root: expected directory")
	}
	if ri.Name() != "/" {
		t.Errorf("root name: got %q, want %q", ri.Name(), "/")
	}

	// Stat not found.
	_, err = fsys.Stat("/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

// TestWriteFSReadDir tests Open on directories and ReadDir.
func TestWriteFSReadDir(t *testing.T) {
	var buf bytes.Buffer
	fsys := CreateFS(&buf)

	fsys.Mkdir("/subdir", 0o755)

	f1, _ := fsys.Create("/hello.txt")
	f1.Write([]byte("hello"))
	f1.Close()

	f2, _ := fsys.Create("/subdir/nested.txt")
	f2.Write([]byte("nested"))
	f2.Close()

	fsys.Symlink("hello.txt", "/link")

	// ReadDir on root.
	d, err := fsys.Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rdf, ok := d.(fs.ReadDirFile)
	if !ok {
		t.Fatal("root Open did not return ReadDirFile")
	}

	entries, err := rdf.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"hello.txt", "link", "subdir"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry[%d]: got %q, want %q", i, names[i], want[i])
		}
	}

	// Verify subdir entry is a directory.
	for _, e := range entries {
		if e.Name() == "subdir" && !e.IsDir() {
			t.Error("subdir should be a directory")
		}
	}

	// ReadDir on subdir.
	sd, err := fsys.Open("/subdir")
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close()

	sdEntries, err := sd.(fs.ReadDirFile).ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sdEntries) != 1 || sdEntries[0].Name() != "nested.txt" {
		t.Errorf("subdir entries: got %v, want [nested.txt]", sdEntries)
	}
}

// writeFSConverter converts tar → erofs (via Go-native tar path), then
// walks the result into a WriteFS, producing a second erofs image. This
// exercises the full WriteFS round-trip including mid-write readback.
func writeFSConverter(t testing.TB, wt erofstest.WriterToTar) fs.FS {
	t.Helper()

	// Build source EROFS from tar.
	srcFS := tarConverter(t, wt)

	// Walk source FS and copy into WriteFS.
	var dstBuf bytes.Buffer
	w := CreateFS(&dstBuf)

	if err := fs.WalkDir(srcFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("info %s: %w", p, err)
		}
		st := info.Sys().(*erofs.Stat)

		wp := "/" + p
		if p == "." {
			wp = "/"
		}

		switch {
		case info.IsDir():
			if err := w.Mkdir(wp, info.Mode().Perm()); err != nil {
				return fmt.Errorf("mkdir %s: %w", wp, err)
			}

		case info.Mode().IsRegular():
			f, err := w.Create(wp)
			if err != nil {
				return fmt.Errorf("create %s: %w", wp, err)
			}
			if info.Size() > 0 {
				src, err := srcFS.Open(p)
				if err != nil {
					return fmt.Errorf("open source %s: %w", p, err)
				}
				if _, err := io.Copy(f, src); err != nil {
					src.Close()
					return fmt.Errorf("copy %s: %w", p, err)
				}
				src.Close()
			}
			f.Chmod(info.Mode().Perm())
			f.Close()

		case info.Mode().Type() == fs.ModeSymlink:
			rlfs := srcFS.(interface{ ReadLink(string) (string, error) })
			target, err := rlfs.ReadLink(p)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", p, err)
			}
			if err := w.Symlink(target, wp); err != nil {
				return fmt.Errorf("symlink %s: %w", wp, err)
			}

		case info.Mode()&fs.ModeCharDevice != 0:
			w.Mknod(wp, disk.StatTypeChrdev|uint16(info.Mode().Perm()), st.Rdev)

		case info.Mode().Type() == fs.ModeDevice:
			w.Mknod(wp, disk.StatTypeBlkdev|uint16(info.Mode().Perm()), st.Rdev)

		case info.Mode().Type() == fs.ModeNamedPipe:
			w.Mknod(wp, disk.StatTypeFifo|uint16(info.Mode().Perm()), 0)

		case info.Mode().Type() == fs.ModeSocket:
			w.Mknod(wp, disk.StatTypeSock|uint16(info.Mode().Perm()), 0)
		}

		// Copy metadata.
		w.Chown(wp, int(st.UID), int(st.GID))
		w.Chtimes(wp, time.Unix(int64(st.Mtime), int64(st.MtimeNs)),
			time.Unix(int64(st.Mtime), int64(st.MtimeNs)))
		for k, v := range st.Xattrs {
			w.Setxattr(wp, k, v)
		}

		return nil
	}); err != nil {
		t.Fatal("walk:", err)
	}

	// Mid-write comparison: verify WriteFS readback matches source.
	fs.WalkDir(srcFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		st := info.Sys().(*erofs.Stat)

		wp := "/" + p
		if p == "." {
			wp = "/"
		}

		wfi, err := w.Stat(wp)
		if err != nil {
			t.Errorf("mid-write stat %s: %v", wp, err)
			return nil
		}
		if info.Mode().IsRegular() && wfi.Size() != info.Size() {
			t.Errorf("mid-write %s size: got %d, want %d", wp, wfi.Size(), info.Size())
		}
		if wfi.Mode().Perm() != info.Mode().Perm() {
			t.Errorf("mid-write %s perm: got %o, want %o", wp, wfi.Mode().Perm(), info.Mode().Perm())
		}
		if wfi.Mode().Type() != info.Mode().Type() {
			t.Errorf("mid-write %s type: got %v, want %v", wp, wfi.Mode().Type(), info.Mode().Type())
		}
		if wfi.ModTime() != time.Unix(int64(st.Mtime), int64(st.MtimeNs)) {
			t.Errorf("mid-write %s modtime: got %v, want %v", wp, wfi.ModTime(), time.Unix(int64(st.Mtime), int64(st.MtimeNs)))
		}

		if info.Mode().IsRegular() {
			wf, err := w.Open(wp)
			if err != nil {
				t.Errorf("mid-write open %s: %v", wp, err)
				return nil
			}
			wdata, _ := io.ReadAll(wf)
			wf.Close()

			sf, _ := srcFS.Open(p)
			sdata, _ := io.ReadAll(sf)
			sf.Close()

			if !bytes.Equal(wdata, sdata) {
				t.Errorf("mid-write %s data mismatch: %d bytes vs %d bytes", wp, len(wdata), len(sdata))
			}
		}
		return nil
	})

	// Close writer and produce final EROFS image.
	if err := w.Close(); err != nil {
		t.Fatal("WriteFS Close:", err)
	}

	dstFS, err := erofs.EroFS(bytes.NewReader(dstBuf.Bytes()))
	if err != nil {
		t.Fatal("EroFS open dest:", err)
	}
	return dstFS
}

// TestWriteFSRoundTrip creates an EROFS image from the standard test tar,
// walks it into a WriteFS, verifies mid-write state, closes the writer,
// and verifies the final EROFS image matches expectations.
func TestWriteFSRoundTrip(t *testing.T) {
	t.Run("Basic", func(t *testing.T) { erofstest.Basic.Run(t, writeFSConverter) })
	t.Run("FileSizes", func(t *testing.T) { erofstest.FileSizes.Run(t, writeFSConverter) })
	t.Run("LongXattrs", func(t *testing.T) { erofstest.LongXattrs.Run(t, writeFSConverter) })
}

// TestWriteFSOpenErrors tests error cases for Open.
func TestWriteFSOpenErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		_, err := fsys.Open("/nonexistent")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("file not yet closed", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		f, err := fsys.Create("/file.txt")
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("data"))

		_, err = fsys.Open("/file.txt")
		if err == nil {
			t.Fatal("expected error opening file still being written")
		}

		f.Close()

		// Should succeed now.
		rf, err := fsys.Open("/file.txt")
		if err != nil {
			t.Fatal("expected success after file closed:", err)
		}
		rf.Close()
	})

	t.Run("read from dir", func(t *testing.T) {
		var buf bytes.Buffer
		fsys := CreateFS(&buf)
		fsys.Mkdir("/dir", 0o755)

		d, err := fsys.Open("/dir")
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()

		_, err = d.Read(make([]byte, 10))
		if err == nil {
			t.Fatal("expected error reading from directory")
		}
	})
}
