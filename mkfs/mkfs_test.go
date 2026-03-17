package mkfs

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/disk"
	"github.com/erofs/go-erofs/internal/erofstest"
)

// TestExt4RoundTrip creates an ext4 image, converts it to EROFS using
// Create(Ext4(...), MetadataOnly()), and verifies the result.
func TestExt4RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}

	srcDir := t.TempDir()
	createTestContent(t, srcDir)

	ext4Path := filepath.Join(t.TempDir(), "test.ext4")
	createExt4Image(t, srcDir, ext4Path)

	ext4File, err := os.Open(ext4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer ext4File.Close()

	ext4Info, err := ext4File.Stat()
	if err != nil {
		t.Fatal(err)
	}

	ext4Blks, err := Ext4Blocks(ext4File)
	if err != nil {
		t.Fatal("Ext4Blocks failed:", err)
	}

	var erofsBuf bytes.Buffer
	err = Create(
		Ext4(ext4File, ext4Info.Size()),
		&erofsBuf,
		MetadataOnly(),
		WithDeviceBlocks(ext4Blks),
	)
	if err != nil {
		t.Fatal("Create failed:", err)
	}

	t.Logf("ext4 size: %d, EROFS metadata size: %d", ext4Info.Size(), erofsBuf.Len())

	erofsReader := bytes.NewReader(erofsBuf.Bytes())
	efs, err := erofs.EroFS(erofsReader, erofs.WithExtraDevices(ext4File))
	if err != nil {
		t.Fatal("EroFS open failed:", err)
	}

	erofstest.CheckFile(t, efs, "in-root.txt", "hello from root\n")
	erofstest.CheckFile(t, efs, "subdir/file.txt", "file in subdir\n")
	erofstest.CheckFile(t, efs, "emptyfile", "")

	expectedLarge := make([]byte, 8192)
	for i := range expectedLarge {
		expectedLarge[i] = byte(i % 251)
	}
	erofstest.CheckFile(t, efs, "largefile", string(expectedLarge))

	erofstest.CheckDirEntries(t, efs, ".", []string{"emptydir", "emptyfile", "in-root.txt", "largefile", "lost+found", "subdir", "symlink"})
	erofstest.CheckDirEntries(t, efs, "subdir", []string{"file.txt"})
	erofstest.CheckDirEntries(t, efs, "emptydir", nil)

	erofstest.CheckSymlink(t, efs, "symlink", "in-root.txt")
}

// TestExt4Fsck converts an ext4 image and runs fsck.erofs on the result.
func TestExt4Fsck(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not available")
	}

	srcDir := t.TempDir()
	createTestContent(t, srcDir)

	ext4Path := filepath.Join(t.TempDir(), "test.ext4")
	createExt4Image(t, srcDir, ext4Path)

	ext4File, err := os.Open(ext4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer ext4File.Close()

	ext4Info, err := ext4File.Stat()
	if err != nil {
		t.Fatal(err)
	}

	ext4Blks, err := Ext4Blocks(ext4File)
	if err != nil {
		t.Fatal("Ext4Blocks failed:", err)
	}

	var erofsBuf bytes.Buffer
	err = Create(
		Ext4(ext4File, ext4Info.Size()),
		&erofsBuf,
		MetadataOnly(),
		WithDeviceBlocks(ext4Blks),
	)
	if err != nil {
		t.Fatal("Create failed:", err)
	}

	erofsPath := filepath.Join(t.TempDir(), "test.erofs")
	if err := os.WriteFile(erofsPath, erofsBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("fsck.erofs", "--device", ext4Path, erofsPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck.erofs failed: %v\nOutput: %s", err, out)
	}
	t.Logf("fsck.erofs: %s", out)
}

// TestExt4EdgeCases exercises ext4-to-EROFS conversion with edge cases:
// large files (multi-extent), sparse files, deep nesting, long filenames,
// permission bits, timestamps, many-entry directories, and long symlinks.
func TestExt4EdgeCases(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}

	srcDir := t.TempDir()

	// 1. Large file (1MB) — forces multi-extent in ext4, many chunk indexes
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 251)
	}
	writeFile(t, filepath.Join(srcDir, "large-1m"), string(largeData))

	// 2. Exact block-boundary files
	writeFile(t, filepath.Join(srcDir, "exact-4k"), string(bytes.Repeat([]byte("ABCD"), 1024)))
	writeFile(t, filepath.Join(srcDir, "exact-8k"), string(bytes.Repeat([]byte("EFGH"), 2048)))

	// 3. File with all zeros (tests that zero blocks read correctly,
	// not the same as a sparse file but exercises the common case)
	writeFile(t, filepath.Join(srcDir, "zeros-64k"), string(make([]byte, 64*1024)))

	// 4. Deep nesting — 10 levels
	deepPath := srcDir
	for i := range 10 {
		deepPath = filepath.Join(deepPath, fmt.Sprintf("d%d", i))
	}
	if err := os.MkdirAll(deepPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(deepPath, "deep.txt"), "deep content\n")

	// 5. Long filename (255 chars)
	longName := strings.Repeat("x", 255)
	writeFile(t, filepath.Join(srcDir, longName), "long name content\n")

	// 6. Permission bits
	writeFile(t, filepath.Join(srcDir, "setuid"), "setuid file\n")
	os.Chmod(filepath.Join(srcDir, "setuid"), fs.ModeSetuid|0o755)
	writeFile(t, filepath.Join(srcDir, "setgid"), "setgid file\n")
	os.Chmod(filepath.Join(srcDir, "setgid"), fs.ModeSetgid|0o755)
	writeFile(t, filepath.Join(srcDir, "sticky-file"), "sticky file\n")
	os.Chmod(filepath.Join(srcDir, "sticky-file"), fs.ModeSticky|0o644)

	// 7. Many files in one directory
	manyDir := filepath.Join(srcDir, "manyfiles")
	os.MkdirAll(manyDir, 0o755)
	for i := range 500 {
		writeFile(t, filepath.Join(manyDir, fmt.Sprintf("f%04d", i)), "")
	}

	// 8. Long symlink target (> 60 bytes, forces extent-based in ext4)
	longTarget := "/" + strings.Repeat("a/", 40) + "target"
	os.Symlink(longTarget, filepath.Join(srcDir, "long-symlink"))

	// 9. Timestamp — use a known mtime
	knownTime := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(srcDir, "timestamped"), "ts content\n")
	os.Chtimes(filepath.Join(srcDir, "timestamped"), knownTime, knownTime)

	// Create ext4 image (16MB to fit everything)
	ext4Path := filepath.Join(t.TempDir(), "test.ext4")
	cmd := exec.Command("mkfs.ext4", "-d", srcDir, "-b", "4096", ext4Path, "16M")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4 failed: %v\nOutput: %s", err, out)
	}

	ext4File, err := os.Open(ext4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer ext4File.Close()

	ext4Info, _ := ext4File.Stat()
	ext4Blks, _ := Ext4Blocks(ext4File)

	var erofsBuf bytes.Buffer
	if err := Create(
		Ext4(ext4File, ext4Info.Size()),
		&erofsBuf,
		MetadataOnly(),
		WithDeviceBlocks(ext4Blks),
	); err != nil {
		t.Fatal("Create failed:", err)
	}

	t.Logf("ext4 size: %d, EROFS size: %d", ext4Info.Size(), erofsBuf.Len())

	efs, err := erofs.EroFS(bytes.NewReader(erofsBuf.Bytes()), erofs.WithExtraDevices(ext4File))
	if err != nil {
		t.Fatal("EroFS open failed:", err)
	}

	// 1. Large file content
	erofstest.CheckFileBytes(t, efs, "large-1m", largeData)

	// 2. Block-boundary files
	erofstest.CheckFileBytes(t, efs, "exact-4k", bytes.Repeat([]byte("ABCD"), 1024))
	erofstest.CheckFileBytes(t, efs, "exact-8k", bytes.Repeat([]byte("EFGH"), 2048))

	// 3. All-zeros file
	erofstest.CheckFileBytes(t, efs, "zeros-64k", make([]byte, 64*1024))

	// 4. Deep nesting
	deepErofsPath := "d0/d1/d2/d3/d4/d5/d6/d7/d8/d9/deep.txt"
	erofstest.CheckFile(t, efs, deepErofsPath, "deep content\n")

	// 5. Long filename
	erofstest.CheckFile(t, efs, longName, "long name content\n")

	// 6. Permission bits
	t.Run("permissions", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			perm fs.FileMode
		}{
			{"setuid", fs.ModeSetuid | 0o755},
			{"setgid", fs.ModeSetgid | 0o755},
			{"sticky-file", fs.ModeSticky | 0o644},
		} {
			st := erofstest.Stat(t, efs, tc.name)
			got := st.Mode.Perm() | st.Mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
			if got != tc.perm {
				t.Errorf("%s: mode %v, want %v", tc.name, got, tc.perm)
			}
		}
	})

	// 7. Many files in one directory
	erofstest.CheckDirSize(t, efs, "manyfiles", 500)

	// 8. Long symlink
	erofstest.CheckSymlink(t, efs, "long-symlink", longTarget)

	// 9. Timestamp
	t.Run("timestamp", func(t *testing.T) {
		st := erofstest.Stat(t, efs, "timestamped")
		if st.Mtime != uint64(knownTime.Unix()) {
			t.Errorf("mtime: got %d, want %d", st.Mtime, knownTime.Unix())
		}
	})
}

// TestBuilderProgrammatic builds an image entry-by-entry and reads it back.
func TestBuilderProgrammatic(t *testing.T) {
	b := NewBuilder()

	b.Add("/", &Entry{Mode: disk.StatTypeDir | 0o755, Nlink: 2})
	b.Add("/hello.txt", &Entry{
		Mode:  disk.StatTypeReg | 0o644,
		Size:  6,
		Nlink: 1,
		Data:  strings.NewReader("hello\n"),
	})
	b.Add("/subdir/", &Entry{Mode: disk.StatTypeDir | 0o755, Nlink: 2})
	b.Add("/subdir/world.txt", &Entry{
		Mode:  disk.StatTypeReg | 0o644,
		Size:  6,
		Nlink: 1,
		Data:  strings.NewReader("world\n"),
	})
	b.Add("/link", &Entry{
		Mode:       disk.StatTypeSymlink | 0o777,
		Nlink:      1,
		LinkTarget: "hello.txt",
	})
	b.Add("/emptyfile", &Entry{
		Mode:  disk.StatTypeReg | 0o644,
		Nlink: 1,
	})

	var buf bytes.Buffer
	if err := b.WriteTo(&buf); err != nil {
		t.Fatal("WriteTo failed:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS open failed:", err)
	}

	erofstest.CheckFile(t, efs, "hello.txt", "hello\n")
	erofstest.CheckFile(t, efs, "subdir/world.txt", "world\n")
	erofstest.CheckFile(t, efs, "emptyfile", "")
	erofstest.CheckSymlink(t, efs, "link", "hello.txt")
	erofstest.CheckDirEntries(t, efs, ".", []string{"emptyfile", "hello.txt", "link", "subdir"})
	erofstest.CheckDirEntries(t, efs, "subdir", []string{"world.txt"})
}

// tarConverter creates an EROFS image from a tar stream using Create(Tar(...)).
func tarConverter(t testing.TB, wt erofstest.WriterToTar) fs.FS {
	t.Helper()
	tarStream := erofstest.TarFromWriterTo(wt)
	defer tarStream.Close()

	var buf bytes.Buffer
	if err := Create(Tar(tarStream), &buf); err != nil {
		t.Fatal("Create:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}
	return efs
}

// tarFileConverter is like tarConverter but writes the tar to an *os.File
// first, exercising the io.ReaderAt zero-copy path.
func tarFileConverter(t testing.TB, wt erofstest.WriterToTar) fs.FS {
	t.Helper()
	tarPath := filepath.Join(t.TempDir(), "test.tar")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tarStream := erofstest.TarFromWriterTo(wt)
	if _, err := io.Copy(tarFile, tarStream); err != nil {
		t.Fatal("write tar:", err)
	}
	tarStream.Close()
	if _, err := tarFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Create(Tar(tarFile), &buf); err != nil {
		t.Fatal("Create:", err)
	}
	tarFile.Close()

	efs, err := erofs.EroFS(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("EroFS:", err)
	}
	return efs
}

// TestTarRoundTrip builds a tar stream using the standard test cases,
// creates an EROFS image via Create(Tar(...)), and reads it back with
// erofs.EroFS to verify a full go-erofs round trip.
func TestTarRoundTrip(t *testing.T) {
	t.Run("Basic", func(t *testing.T) { erofstest.Basic.Run(t, tarConverter) })
	t.Run("FileSizes", func(t *testing.T) { erofstest.FileSizes.Run(t, tarConverter) })
	t.Run("SparseFiles", func(t *testing.T) { erofstest.SparseFiles.Run(t, tarConverter) })
	t.Run("LongXattrs", func(t *testing.T) { erofstest.LongXattrs.Run(t, tarConverter) })
}

// TestTarFileRoundTrip is the same as TestTarRoundTrip but passes an
// *os.File (which implements io.ReaderAt) to exercise the zero-copy path.
func TestTarFileRoundTrip(t *testing.T) {
	t.Run("Basic", func(t *testing.T) { erofstest.Basic.Run(t, tarFileConverter) })
	t.Run("FileSizes", func(t *testing.T) { erofstest.FileSizes.Run(t, tarFileConverter) })
	t.Run("SparseFiles", func(t *testing.T) { erofstest.SparseFiles.Run(t, tarFileConverter) })
	t.Run("LongXattrs", func(t *testing.T) { erofstest.LongXattrs.Run(t, tarFileConverter) })
}

// ext4Converter extracts tar to a directory (preserving xattrs and devices),
// creates an ext4 image via mkfs.ext4 -d, then converts ext4 → erofs.
// Device creation requires root; the test is skipped if mknod fails.
func ext4Converter(t testing.TB, wt erofstest.WriterToTar) fs.FS {
	t.Helper()
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}

	// Extract tar to a directory, preserving xattrs and devices.
	// Directories are created with 0755 initially so children can be
	// created inside them; final permissions are applied after all
	// entries are extracted.
	srcDir := t.TempDir()
	tarStream := erofstest.TarFromWriterTo(wt)
	tr := tar.NewReader(tarStream)

	type dirPerm struct {
		path string
		mode os.FileMode
	}
	var dirPerms []dirPerm

	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		name := filepath.FromSlash(hdr.Name)
		target := filepath.Join(srcDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755)
			dirPerms = append(dirPerms, dirPerm{target, os.FileMode(hdr.Mode).Perm()})
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			data, _ := io.ReadAll(tr)
			os.WriteFile(target, data, os.FileMode(hdr.Mode).Perm())
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Symlink(hdr.Linkname, target)
		case tar.TypeChar:
			os.MkdirAll(filepath.Dir(target), 0o755)
			rdev := uint32(hdr.Devmajor<<8 | hdr.Devminor)
			if err := syscall.Mknod(target, syscall.S_IFCHR|uint32(hdr.Mode&0o7777), int(rdev)); err != nil {
				t.Skipf("mknod %s: %v (requires root)", name, err)
			}
		case tar.TypeBlock:
			os.MkdirAll(filepath.Dir(target), 0o755)
			rdev := uint32(hdr.Devmajor<<8 | hdr.Devminor)
			if err := syscall.Mknod(target, syscall.S_IFBLK|uint32(hdr.Mode&0o7777), int(rdev)); err != nil {
				t.Skipf("mknod %s: %v (requires root)", name, err)
			}
		case tar.TypeFifo:
			os.MkdirAll(filepath.Dir(target), 0o755)
			if err := syscall.Mkfifo(target, uint32(hdr.Mode&0o7777)); err != nil {
				t.Skipf("mkfifo %s: %v (requires root)", name, err)
			}
		}
		// Set xattrs from PAX records.
		for k, v := range hdr.PAXRecords {
			const prefix = "SCHILY.xattr."
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				xattrName := k[len(prefix):]
				setxattr(t, target, xattrName, v)
			}
		}
	}
	tarStream.Close()

	// Apply final directory permissions (reverse order for depth-first).
	for i := len(dirPerms) - 1; i >= 0; i-- {
		os.Chmod(dirPerms[i].path, dirPerms[i].mode)
	}

	// Create ext4 image from the directory.
	ext4Path := filepath.Join(t.TempDir(), "test.ext4")
	cmd := exec.Command("mkfs.ext4", "-d", srcDir, "-b", "4096", ext4Path, "64M")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4 failed: %v\nOutput: %s", err, out)
	}

	ext4File, err := os.Open(ext4Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ext4File.Close() })

	ext4Info, err := ext4File.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ext4Blks, err := Ext4Blocks(ext4File)
	if err != nil {
		t.Fatal("Ext4Blocks:", err)
	}

	// Convert ext4 → EROFS (metadata-only).
	var erofsBuf bytes.Buffer
	if err := Create(
		Ext4(ext4File, ext4Info.Size()),
		&erofsBuf,
		MetadataOnly(),
		WithDeviceBlocks(ext4Blks),
	); err != nil {
		t.Fatal("Create:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(erofsBuf.Bytes()), erofs.WithExtraDevices(ext4File))
	if err != nil {
		t.Fatal("EroFS:", err)
	}
	return efs
}

func setxattr(t testing.TB, path, name, value string) {
	t.Helper()
	if err := syscall.Setxattr(path, name, []byte(value), 0); err != nil {
		t.Fatalf("setxattr %s on %s: %v", name, path, err)
	}
}

// TestExt4TarRoundTrip converts Basic tar content through ext4 → erofs.
func TestExt4TarRoundTrip(t *testing.T) {
	erofstest.Basic.Run(t, ext4Converter)
}

// TestTarWhiteoutConversion verifies that AUFS-style whiteout files in OCI
// layer tars are converted to overlayfs equivalents: .wh.<name> becomes a
// character device 0/0, and .wh..wh..opq sets trusted.overlay.opaque=y.
func TestTarWhiteoutConversion(t *testing.T) {
	tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	writerTo := erofstest.TarAll(
		tc.Dir("/", 0755),
		tc.File("/keep.txt", []byte("kept\n"), 0644),
		tc.Dir("/mydir", 0755),
		tc.File("/mydir/a.txt", []byte("aaa\n"), 0644),
		// AUFS whiteout: delete /removed.txt
		tc.File("/.wh.removed.txt", []byte{}, 0644),
		// AUFS opaque: /mydir becomes opaque (hides lower layer contents)
		tc.File("/mydir/.wh..wh..opq", []byte{}, 0644),
	)

	tarStream := erofstest.TarFromWriterTo(writerTo)
	defer tarStream.Close()

	var erofsBuf bytes.Buffer
	if err := Create(Tar(tarStream, ConvertWhiteouts()), &erofsBuf); err != nil {
		t.Fatal("Create failed:", err)
	}

	efs, err := erofs.EroFS(bytes.NewReader(erofsBuf.Bytes()))
	if err != nil {
		t.Fatal("EroFS open failed:", err)
	}

	// Regular files should be present
	erofstest.CheckFile(t, efs, "keep.txt", "kept\n")
	erofstest.CheckFile(t, efs, "mydir/a.txt", "aaa\n")

	// .wh.removed.txt should become a char device 0/0 named "removed.txt"
	erofstest.CheckDevice(t, efs, "removed.txt", fs.ModeCharDevice, 0)

	// .wh..wh..opq should set trusted.overlay.opaque=y on /mydir
	erofstest.CheckXattrs(t, efs, "mydir", map[string]string{
		"trusted.overlay.opaque": "y",
	})

	// The .wh. files themselves should NOT exist
	erofstest.CheckNotExists(t, efs, ".wh.removed.txt")
	erofstest.CheckNotExists(t, efs, "mydir/.wh..wh..opq")
}

func createTestContent(t *testing.T, dir string) {
	t.Helper()

	writeFile(t, filepath.Join(dir, "in-root.txt"), "hello from root\n")
	writeFile(t, filepath.Join(dir, "emptyfile"), "")

	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "subdir", "file.txt"), "file in subdir\n")

	if err := os.MkdirAll(filepath.Join(dir, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	largeData := make([]byte, 8192)
	for i := range largeData {
		largeData[i] = byte(i % 251)
	}
	writeFile(t, filepath.Join(dir, "largefile"), string(largeData))

	if err := os.Symlink("in-root.txt", filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
}

func createExt4Image(t *testing.T, srcDir, imgPath string) {
	t.Helper()
	cmd := exec.Command("mkfs.ext4", "-d", srcDir, "-b", "4096", imgPath, "4M")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs.ext4 failed: %v\nOutput: %s", err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
