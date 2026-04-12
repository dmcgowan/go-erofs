package tar_test

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	erofs "github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/disk"
	"github.com/erofs/go-erofs/internal/erofstest"
	mktar "github.com/erofs/go-erofs/tar"
)

// writeFSConverter converts tar → erofs (via Go-native tar path), then
// walks the result into a Writer, producing a second erofs image. This
// exercises the full Writer round-trip including mid-write readback.
func writeFSConverter(t testing.TB, wt erofstest.WriterToTar) fs.FS {
	t.Helper()

	// Build source EROFS from tar.
	tarStream := erofstest.TarFromWriterTo(wt)
	defer func() { _ = tarStream.Close() }()
	tarFS, err := mktar.Open(tarStream)
	if err != nil {
		t.Fatal("tar.Open:", err)
	}
	defer tarFS.Close() //nolint:errcheck
	var srcBuf erofstest.TestBuffer
	tw := erofs.Create(&srcBuf)
	if err := tw.CopyFrom(tarFS); err != nil {
		t.Fatal("CopyFrom:", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal("Close:", err)
	}
	srcFS, err := erofs.Open(bytes.NewReader(srcBuf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	// Walk source FS and copy into Writer.
	var dstBuf erofstest.TestBuffer
	w := erofs.Create(&dstBuf)

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
					_ = src.Close()
					return fmt.Errorf("copy %s: %w", p, err)
				}
				_ = src.Close()
			}
			if err := f.Chmod(info.Mode().Perm()); err != nil {
				return fmt.Errorf("chmod %s: %w", wp, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", wp, err)
			}

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
			if err := w.Mknod(wp, disk.StatTypeChrdev|uint16(info.Mode().Perm()), st.Rdev); err != nil {
				return fmt.Errorf("mknod %s: %w", wp, err)
			}

		case info.Mode().Type() == fs.ModeDevice:
			if err := w.Mknod(wp, disk.StatTypeBlkdev|uint16(info.Mode().Perm()), st.Rdev); err != nil {
				return fmt.Errorf("mknod %s: %w", wp, err)
			}

		case info.Mode().Type() == fs.ModeNamedPipe:
			if err := w.Mknod(wp, disk.StatTypeFifo|uint16(info.Mode().Perm()), 0); err != nil {
				return fmt.Errorf("mknod %s: %w", wp, err)
			}

		case info.Mode().Type() == fs.ModeSocket:
			if err := w.Mknod(wp, disk.StatTypeSock|uint16(info.Mode().Perm()), 0); err != nil {
				return fmt.Errorf("mknod %s: %w", wp, err)
			}
		}

		// Copy metadata.
		if err := w.Chown(wp, int(st.UID), int(st.GID)); err != nil {
			return fmt.Errorf("chown %s: %w", wp, err)
		}
		if err := w.Chtimes(wp, time.Unix(int64(st.Mtime), int64(st.MtimeNs)),
			time.Unix(int64(st.Mtime), int64(st.MtimeNs))); err != nil {
			return fmt.Errorf("chtimes %s: %w", wp, err)
		}
		for k, v := range st.Xattrs {
			if err := w.Setxattr(wp, k, v); err != nil {
				return fmt.Errorf("setxattr %s %s: %w", wp, k, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatal("walk:", err)
	}

	// Mid-write comparison: verify Writer readback matches source.
	if err := fs.WalkDir(srcFS, ".", func(p string, d fs.DirEntry, err error) error {
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
			_ = wf.Close()

			sf, err := srcFS.Open(p)
			if err != nil {
				t.Errorf("mid-write source open %s: %v", p, err)
				return nil
			}
			sdata, _ := io.ReadAll(sf)
			_ = sf.Close()

			if !bytes.Equal(wdata, sdata) {
				t.Errorf("mid-write %s data mismatch: %d bytes vs %d bytes", wp, len(wdata), len(sdata))
			}
		}
		return nil
	}); err != nil {
		t.Fatal("mid-write walk:", err)
	}

	// Close writer and produce final EROFS image.
	if err := w.Close(); err != nil {
		t.Fatal("Writer Close:", err)
	}

	dstFS, err := erofs.Open(bytes.NewReader(dstBuf.Bytes()))
	if err != nil {
		t.Fatal("EroFS open dest:", err)
	}
	return dstFS
}

// TestWriterRoundTrip creates an EROFS image from the standard test tar,
// walks it into a Writer, verifies mid-write state, closes the writer,
// and verifies the final EROFS image matches expectations.
func TestWriterRoundTrip(t *testing.T) {
	t.Run("Basic", func(t *testing.T) { erofstest.Basic.Run(t, writeFSConverter) })
	t.Run("FileSizes", func(t *testing.T) { erofstest.FileSizes.Run(t, writeFSConverter) })
	t.Run("LongXattrs", func(t *testing.T) { erofstest.LongXattrs.Run(t, writeFSConverter) })
}

// TestMergeTarWhiteout verifies merge with tar sources containing whiteouts.
func TestMergeTarWhiteout(t *testing.T) {
	tc := erofstest.TarContext{UID: 0, GID: 0}

	baseTar := erofstest.TarFromWriterTo(erofstest.TarAll(
		tc.Dir("dir/", 0o755),
		tc.File("dir/keep.txt", []byte("keep"), 0o644),
		tc.File("dir/remove.txt", []byte("gone"), 0o644),
		tc.File("other.txt", []byte("other"), 0o644),
	))
	defer baseTar.Close() //nolint:errcheck

	overlayTar := erofstest.TarFromWriterTo(erofstest.TarAll(
		tc.Dir("dir/", 0o755),
		tc.File("dir/.wh.remove.txt", nil, 0o644),
		tc.File("dir/added.txt", []byte("added"), 0o644),
	))
	defer overlayTar.Close() //nolint:errcheck

	baseFS, err := mktar.Open(baseTar)
	if err != nil {
		t.Fatal("open base tar:", err)
	}
	defer baseFS.Close() //nolint:errcheck
	overlayFS, err := mktar.Open(overlayTar)
	if err != nil {
		t.Fatal("open overlay tar:", err)
	}
	defer overlayFS.Close() //nolint:errcheck

	var buf erofstest.TestBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(baseFS); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlayFS, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckFile(t, efs, "dir/keep.txt", "keep")
	erofstest.CheckNotExists(t, efs, "dir/remove.txt")
	erofstest.CheckFile(t, efs, "dir/added.txt", "added")
	erofstest.CheckFile(t, efs, "other.txt", "other")
}

// TestMergeMixed verifies that metadata-only and data CopyFrom calls
// can be combined. The first layer uses MetadataOnly (chunks reference
// an external tar), the second stores data in the image.
func TestMergeMixed(t *testing.T) {
	tc := erofstest.TarContext{UID: 0, GID: 0}

	// Create base tar on disk so we can provide it as a device.
	baseTarPath := filepath.Join(t.TempDir(), "base.tar")
	func() {
		f, err := os.Create(baseTarPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck
		rc := erofstest.TarFromWriterTo(erofstest.TarAll(
			tc.File("base.txt", []byte("base-content"), 0o644),
		))
		defer rc.Close() //nolint:errcheck
		if _, err := io.Copy(f, rc); err != nil {
			t.Fatal(err)
		}
	}()

	// Build a metadata-only EROFS from the base tar.
	baseTarFile, err := os.Open(baseTarPath)
	if err != nil {
		t.Fatal(err)
	}
	baseTarFS, err := mktar.Open(baseTarFile)
	_ = baseTarFile.Close()
	if err != nil {
		t.Fatal("open base tar:", err)
	}
	defer baseTarFS.Close() //nolint:errcheck
	var baseErofsBuf erofstest.TestBuffer
	bw := erofs.Create(&baseErofsBuf)
	if err := bw.CopyFrom(baseTarFS, erofs.MetadataOnly()); err != nil {
		t.Fatal("build base erofs:", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal("close base erofs:", err)
	}

	// Open the EROFS image as the MetadataOnly source.
	baseDev, err := os.Open(baseTarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer baseDev.Close() //nolint:errcheck
	baseEroFS, err := erofs.Open(bytes.NewReader(baseErofsBuf.Bytes()), erofs.WithExtraDevices(baseDev))
	if err != nil {
		t.Fatal("open base erofs:", err)
	}

	overlay := fstest.MapFS{
		"overlay.txt": {Data: []byte("overlay-content"), Mode: 0o644},
	}

	var buf erofstest.TestBuffer
	w := erofs.Create(&buf)

	if err := w.CopyFrom(baseEroFS, erofs.MetadataOnly()); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlay, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Re-open the tar as device backing for the merged image.
	baseDev2, err := os.Open(baseTarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer baseDev2.Close() //nolint:errcheck

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()), erofs.WithExtraDevices(baseDev2))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckFile(t, efs, "overlay.txt", "overlay-content")
	erofstest.CheckFile(t, efs, "base.txt", "base-content")
}

// TestMergePerCopyFromMetadata verifies that MetadataOnly is per-CopyFrom,
// not sticky across calls.
func TestMergePerCopyFromMetadata(t *testing.T) {
	tc := erofstest.TarContext{UID: 0, GID: 0}

	// Create metadata tar on disk so we can use it as a device.
	metaTarPath := filepath.Join(t.TempDir(), "meta.tar")
	func() {
		f, err := os.Create(metaTarPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck
		rc := erofstest.TarFromWriterTo(erofstest.TarAll(
			tc.File("meta.txt", []byte("meta"), 0o644),
		))
		defer rc.Close() //nolint:errcheck
		if _, err := io.Copy(f, rc); err != nil {
			t.Fatal(err)
		}
	}()

	// Build metadata-only EROFS from the meta tar.
	metaTarFile, err := os.Open(metaTarPath)
	if err != nil {
		t.Fatal(err)
	}
	metaTarFS, err := mktar.Open(metaTarFile)
	_ = metaTarFile.Close()
	if err != nil {
		t.Fatal("open meta tar:", err)
	}
	defer metaTarFS.Close() //nolint:errcheck
	var metaErofsBuf erofstest.TestBuffer
	mw := erofs.Create(&metaErofsBuf)
	if err := mw.CopyFrom(metaTarFS, erofs.MetadataOnly()); err != nil {
		t.Fatal("build meta erofs:", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal("close meta erofs:", err)
	}

	// Open the EROFS image as the MetadataOnly source.
	metaDev, err := os.Open(metaTarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer metaDev.Close() //nolint:errcheck
	metaFS, err := erofs.Open(bytes.NewReader(metaErofsBuf.Bytes()), erofs.WithExtraDevices(metaDev))
	if err != nil {
		t.Fatal("open meta erofs:", err)
	}

	dataTar := erofstest.TarFromWriterTo(erofstest.TarAll(
		tc.File("data.txt", []byte("data-content"), 0o644),
	))
	defer dataTar.Close() //nolint:errcheck

	dataFS, err := mktar.Open(dataTar)
	if err != nil {
		t.Fatal("open data tar:", err)
	}
	defer dataFS.Close() //nolint:errcheck

	var buf erofstest.TestBuffer
	w := erofs.Create(&buf)

	// First CopyFrom: metadata-only from EROFS image
	if err := w.CopyFrom(metaFS, erofs.MetadataOnly()); err != nil {
		t.Fatal("CopyFrom meta:", err)
	}
	// Second CopyFrom: NOT metadata-only (should store data)
	if err := w.CopyFrom(dataFS); err != nil {
		t.Fatal("CopyFrom data:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Provide the meta tar as the backing device.
	metaDev2, err := os.Open(metaTarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer metaDev2.Close() //nolint:errcheck

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()), erofs.WithExtraDevices(metaDev2))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckFile(t, efs, "data.txt", "data-content")
	erofstest.CheckFile(t, efs, "meta.txt", "meta")
}

// TestMergeTarOverErofs simulates an erofs layering workflow:
// lower layers exist as EROFS images backed by their tars;
// the top layer is a new tar being unpacked.
//
// Layer 1 (bottom): erofs with data
// Layer 2: erofs with layer 2 data + merged layer 1 metadata (1 blob device)
// Layer 3: erofs with layer 3 data, including white outs (no blob devices)
// Layer 4: erofs with layer 4 data + merged layer 1, 2 and 3 metadata (3 blob devices)
func TestMergeTarOverErofs(t *testing.T) {
	tc := erofstest.TarContext{UID: 0, GID: 0}
	tmpDir := t.TempDir()

	// writeTar is a helper that writes a tar to disk.
	writeTar := func(name string, entries ...erofstest.WriterToTar) string {
		p := filepath.Join(tmpDir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck
		rc := erofstest.TarFromWriterTo(erofstest.TarAll(entries...))
		defer rc.Close() //nolint:errcheck
		if _, err := io.Copy(f, rc); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// openTar opens a tar file from disk and returns the FS.
	openTar := func(path string) *mktar.FS {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		fs, err := mktar.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fs.Close() })
		return fs
	}

	// openDev opens a file as a device reader.
	openDev := func(path string) *os.File {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	// openErofs opens a previously built EROFS image with device backing.
	openErofs := func(buf []byte, devices ...io.ReaderAt) fs.FS {
		t.Helper()
		efs, err := erofs.Open(bytes.NewReader(buf), erofs.WithExtraDevices(devices...))
		if err != nil {
			t.Fatal("open erofs:", err)
		}
		return efs
	}

	// ── Layer 1 tar: the base image. ──
	layer1Path := writeTar("layer1.tar",
		tc.Dir("etc/", 0o755),
		tc.File("etc/config.json", []byte(`{"version":1}`), 0o644),
		tc.File("etc/passwd", []byte("root:x:0:0:::/bin/sh\n"), 0o644),
		tc.Dir("bin/", 0o755),
		tc.File("bin/app", []byte("#!/bin/sh\necho v1\n"), 0o755),
		tc.File("bin/helper", []byte("#!/bin/sh\necho help\n"), 0o755),
		tc.Dir("var/", 0o755),
		tc.Dir("var/cache/", 0o755),
		tc.File("var/cache/data.db", []byte("stale-cache"), 0o644),
	)

	// ── Layer 1 EROFS: standalone image with data. ──
	layer1FS := openTar(layer1Path)
	var layer1Buf erofstest.TestBuffer
	w1 := erofs.Create(&layer1Buf)
	if err := w1.CopyFrom(layer1FS); err != nil {
		t.Fatal("layer1 CopyFrom:", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal("layer1 Close:", err)
	}

	l1, err := erofs.Open(bytes.NewReader(layer1Buf.Bytes()))
	if err != nil {
		t.Fatal("open layer1 erofs:", err)
	}
	erofstest.CheckFile(t, l1, "etc/config.json", `{"version":1}`)
	erofstest.CheckFile(t, l1, "bin/app", "#!/bin/sh\necho v1\n")

	// ── Layer 2 tar: updates app, adds /lib/core.so. ──
	layer2Path := writeTar("layer2.tar",
		tc.Dir("bin/", 0o755),
		tc.File("bin/app", []byte("#!/bin/sh\necho v2\n"), 0o755),
		tc.Dir("lib/", 0o755),
		tc.File("lib/core.so", []byte("\x7fELF-core"), 0o755),
	)

	// buildMetaErofs creates a metadata-only EROFS from a tar on disk.
	// These are the images a snapshotter would have pre-built when pulling layers.
	buildMetaErofs := func(tarPath string) []byte {
		t.Helper()
		tf, err := os.Open(tarPath)
		if err != nil {
			t.Fatal(err)
		}
		tfs, err := mktar.Open(tf)
		_ = tf.Close()
		if err != nil {
			t.Fatal(err)
		}
		defer tfs.Close() //nolint:errcheck
		var buf erofstest.TestBuffer
		w := erofs.Create(&buf)
		if err := w.CopyFrom(tfs, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	// Build metadata-only EROFS for layer 1 (layer 2 & 3 built later, after their tars).
	layer1MetaImg := buildMetaErofs(layer1Path)

	// ── Layer 2 EROFS: layer 2 data + merged layer 1 metadata (1 blob). ──
	// Use the layer 1 metadata-only EROFS as the base.
	layer1EFS := openErofs(layer1MetaImg, openDev(layer1Path))
	layer2FS := openTar(layer2Path)

	var layer2Buf erofstest.TestBuffer
	w2 := erofs.Create(&layer2Buf)
	if err := w2.CopyFrom(layer1EFS, erofs.MetadataOnly()); err != nil {
		t.Fatal("layer2 CopyFrom layer1:", err)
	}
	if err := w2.CopyFrom(layer2FS, erofs.Merge()); err != nil {
		t.Fatal("layer2 CopyFrom layer2:", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal("layer2 Close:", err)
	}

	l2, err := erofs.Open(bytes.NewReader(layer2Buf.Bytes()),
		erofs.WithExtraDevices(openDev(layer1Path)))
	if err != nil {
		t.Fatal("open layer2 erofs:", err)
	}
	erofstest.CheckFile(t, l2, "etc/config.json", `{"version":1}`) // from layer 1 device
	erofstest.CheckFile(t, l2, "bin/app", "#!/bin/sh\necho v2\n")  // from layer 2 data
	erofstest.CheckFile(t, l2, "lib/core.so", "\x7fELF-core")      // from layer 2 data

	// ── Layer 3 tar: contains whiteouts + new files. ──
	//
	// This layer's tar includes raw whiteout entries. The EROFS image is
	// built standalone (no merge, no blob devices) — whiteout markers are
	// stored as regular files in the image, not applied.
	layer3Path := writeTar("layer3.tar",
		tc.Dir("bin/", 0o755),
		tc.File("bin/.wh.helper", nil, 0o644),
		tc.Dir("var/", 0o755),
		tc.Dir("var/cache/", 0o755),
		tc.File("var/cache/.wh..wh..opq", nil, 0o644),
		tc.File("var/cache/fresh.db", []byte("fresh-cache"), 0o644),
		tc.Dir("usr/", 0o755),
		tc.Dir("usr/lib/", 0o755),
		tc.File("usr/lib/new.so", []byte("\x7fELF-fake-binary"), 0o755),
	)

	// ── Layer 3 EROFS: standalone image with data (no blob devices). ──
	layer3FS := openTar(layer3Path)

	var layer3Buf erofstest.TestBuffer
	w3 := erofs.Create(&layer3Buf)
	if err := w3.CopyFrom(layer3FS); err != nil {
		t.Fatal("layer3 CopyFrom:", err)
	}
	if err := w3.Close(); err != nil {
		t.Fatal("layer3 Close:", err)
	}

	l3, err := erofs.Open(bytes.NewReader(layer3Buf.Bytes()))
	if err != nil {
		t.Fatal("open layer3 erofs:", err)
	}
	// Whiteout markers are stored as regular files — not applied.
	erofstest.CheckFile(t, l3, "var/cache/fresh.db", "fresh-cache")
	erofstest.CheckFile(t, l3, "usr/lib/new.so", "\x7fELF-fake-binary")

	// Build metadata-only EROFS images for layers 2 and 3.
	layer2MetaImg := buildMetaErofs(layer2Path)
	layer3MetaImg := buildMetaErofs(layer3Path)

	// ── Layer 4 tar: updates config, removes /usr/lib/new.so, adds /opt/tool. ──
	layer4Tar := erofstest.TarFromWriterTo(erofstest.TarAll(
		tc.Dir("etc/", 0o755),
		tc.File("etc/config.json", []byte(`{"version":4}`), 0o644),
		tc.Dir("usr/", 0o755),
		tc.Dir("usr/lib/", 0o755),
		tc.File("usr/lib/.wh.new.so", nil, 0o644),
		tc.Dir("opt/", 0o755),
		tc.File("opt/tool", []byte("#!/bin/sh\necho tool\n"), 0o755),
	))
	defer layer4Tar.Close() //nolint:errcheck

	layer4FS, err := mktar.Open(layer4Tar)
	if err != nil {
		t.Fatal("open layer4 tar:", err)
	}
	defer layer4FS.Close() //nolint:errcheck

	// ── Layer 4 EROFS: layer 4 data + merged layers 1, 2, 3 metadata (3 blobs). ──
	//
	// Each lower layer is loaded from its metadata-only EROFS image with
	// Merge so whiteouts are applied in order. Layer 4 tar is merged on top.
	layer1EFS2 := openErofs(layer1MetaImg, openDev(layer1Path))
	layer2EFS := openErofs(layer2MetaImg, openDev(layer2Path))
	layer3EFS := openErofs(layer3MetaImg, openDev(layer3Path))

	var layer4Buf erofstest.TestBuffer
	w4 := erofs.Create(&layer4Buf)
	// Blob device 1: layer 1 EROFS (base — no whiteouts to apply)
	if err := w4.CopyFrom(layer1EFS2, erofs.MetadataOnly()); err != nil {
		t.Fatal("layer4 CopyFrom layer1:", err)
	}
	// Blob device 2: layer 2 EROFS (merge overwrites layer 1 entries)
	if err := w4.CopyFrom(layer2EFS, erofs.MetadataOnly(), erofs.Merge()); err != nil {
		t.Fatal("layer4 CopyFrom layer2:", err)
	}
	// Blob device 3: layer 3 EROFS (merge applies whiteouts from layer 3)
	if err := w4.CopyFrom(layer3EFS, erofs.MetadataOnly(), erofs.Merge()); err != nil {
		t.Fatal("layer4 CopyFrom layer3:", err)
	}
	// Layer 4 tar merged on top; data stored inline.
	if err := w4.CopyFrom(layer4FS, erofs.Merge()); err != nil {
		t.Fatal("layer4 CopyFrom layer4:", err)
	}
	if err := w4.Close(); err != nil {
		t.Fatal("layer4 Close:", err)
	}

	// Open with all three blob devices.
	merged, err := erofs.Open(bytes.NewReader(layer4Buf.Bytes()),
		erofs.WithExtraDevices(openDev(layer1Path), openDev(layer2Path), openDev(layer3Path)))
	if err != nil {
		t.Fatal("open layer4 erofs:", err)
	}

	// ── Verify the final merged layer 4 image. ──

	// Updated by layer 4 (stored inline).
	erofstest.CheckFile(t, merged, "etc/config.json", `{"version":4}`)

	// From layer 1 device: preserved through all layers.
	erofstest.CheckFile(t, merged, "etc/passwd", "root:x:0:0:::/bin/sh\n")

	// From layer 2 device: app was updated in layer 2.
	erofstest.CheckFile(t, merged, "bin/app", "#!/bin/sh\necho v2\n")
	erofstest.CheckFile(t, merged, "lib/core.so", "\x7fELF-core")

	// Whiteout from layer 3 (applied via MetadataOnly+Merge): /bin/helper gone.
	erofstest.CheckNotExists(t, merged, "bin/helper")

	// Opaque from layer 3: /var/cache old entries gone, fresh.db from layer 3 device.
	erofstest.CheckNotExists(t, merged, "var/cache/data.db")
	erofstest.CheckFile(t, merged, "var/cache/fresh.db", "fresh-cache")

	// From layer 3 device: new.so was added in layer 3, then removed by layer 4.
	erofstest.CheckNotExists(t, merged, "usr/lib/new.so")

	// New file from layer 4 (stored inline).
	erofstest.CheckFile(t, merged, "opt/tool", "#!/bin/sh\necho tool\n")

	// Directory structure reflects all four layers.
	erofstest.CheckDirEntries(t, merged, ".", []string{"bin", "etc", "lib", "opt", "usr", "var"})
	erofstest.CheckDirEntries(t, merged, "bin", []string{"app"})
	erofstest.CheckDirEntries(t, merged, "usr/lib", nil)
}

// TestMergeErofsToErofs verifies that two full EROFS images (with data, not
// metadata-only) can be merged. The result stores all data inline. This
// exercises the fs.WalkDir path for EROFS sources (not copyFromImage) since
// file data must be read from the source images.
func TestMergeErofsToErofs(t *testing.T) {
	tc := erofstest.TarContext{UID: 0, GID: 0}

	// Build base EROFS from tar.
	baseTar := erofstest.TarFromWriterTo(erofstest.TarAll(
		tc.Dir("dir/", 0o755),
		tc.File("dir/base.txt", []byte("base-data"), 0o644),
		tc.File("dir/shared.txt", []byte("old-shared"), 0o644),
		tc.File("remove.txt", []byte("gone"), 0o644),
	))
	defer baseTar.Close() //nolint:errcheck
	baseTarFS, err := mktar.Open(baseTar)
	if err != nil {
		t.Fatal(err)
	}
	defer baseTarFS.Close() //nolint:errcheck
	var baseBuf erofstest.TestBuffer
	bw := erofs.Create(&baseBuf)
	if err := bw.CopyFrom(baseTarFS); err != nil {
		t.Fatal("build base:", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal("close base:", err)
	}

	// Build overlay EROFS from tar (contains whiteout + overwrite + new file).
	overlayTar := erofstest.TarFromWriterTo(erofstest.TarAll(
		tc.Dir("dir/", 0o755),
		tc.File("dir/shared.txt", []byte("new-shared"), 0o644),
		tc.File(".wh.remove.txt", nil, 0o644),
		tc.File("added.txt", []byte("added-data"), 0o644),
	))
	defer overlayTar.Close() //nolint:errcheck
	overlayTarFS, err := mktar.Open(overlayTar)
	if err != nil {
		t.Fatal(err)
	}
	defer overlayTarFS.Close() //nolint:errcheck
	var overlayBuf erofstest.TestBuffer
	ow := erofs.Create(&overlayBuf)
	if err := ow.CopyFrom(overlayTarFS); err != nil {
		t.Fatal("build overlay:", err)
	}
	if err := ow.Close(); err != nil {
		t.Fatal("close overlay:", err)
	}

	// Open both EROFS images from their serialized bytes (simulates
	// opening from disk — no in-process memory sharing).
	baseBytes := make([]byte, len(baseBuf.Bytes()))
	copy(baseBytes, baseBuf.Bytes())
	overlayBytes := make([]byte, len(overlayBuf.Bytes()))
	copy(overlayBytes, overlayBuf.Bytes())

	baseFS, err := erofs.Open(bytes.NewReader(baseBytes))
	if err != nil {
		t.Fatal("open base:", err)
	}
	overlayFS, err := erofs.Open(bytes.NewReader(overlayBytes))
	if err != nil {
		t.Fatal("open overlay:", err)
	}

	// Merge: base (full copy) + overlay (merge with whiteouts).
	var mergedBuf erofstest.TestBuffer
	w := erofs.Create(&mergedBuf)
	if err := w.CopyFrom(baseFS); err != nil {
		t.Fatal("CopyFrom base:", err)
	}
	if err := w.CopyFrom(overlayFS, erofs.Merge()); err != nil {
		t.Fatal("CopyFrom overlay:", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	// Read back and verify — all data should be stored inline.
	erofstest.FsckErofsBytes(t, mergedBuf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(mergedBuf.Bytes()))
	if err != nil {
		t.Fatal("open merged:", err)
	}

	erofstest.CheckFile(t, efs, "dir/base.txt", "base-data")
	erofstest.CheckFile(t, efs, "dir/shared.txt", "new-shared") // overlay wins
	erofstest.CheckFile(t, efs, "added.txt", "added-data")
	erofstest.CheckNotExists(t, efs, "remove.txt") // whiteout applied
}
