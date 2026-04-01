package erofs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erofs/go-erofs/internal/tartest"
)

var benchFs fs.FS = nil

func initBenchmark(b *testing.B) fs.FS {
	if benchFs != nil {
		return benchFs
	}

	tc := tartest.TarContext{}.WithModTime(time.Now().UTC())

	smallFile := bytes.Repeat([]byte{1, 2, 3, 4, 5}, 200)
	mediumFile := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 12800)
	largeFile := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 102400)

	writerTo := tartest.TarAll(
		tc.File("/small.txt", smallFile, 0600),
		tc.File("/medium.bin", mediumFile, 0600),
		tc.File("/large.bin", largeFile, 0600),
		tc.File("/empty.txt", []byte{}, 0600),
	)

	var dirs []tartest.WriterToTar
	for i := 0; i < 100; i++ {
		dirs = append(dirs, tc.Dir(filepath.Join("/dir100", dirName(i)), 0755))
		for j := 0; j < 10; j++ {
			dirs = append(dirs, tc.File(filepath.Join("/dir100", dirName(i), fmt.Sprintf("file%d.txt", j)), []byte("content"), 0600))
		}
	}

	writerTo = tartest.TarAll(append(dirs, writerTo)...)

	path := filepath.Join(b.TempDir(), "bench.erofs")
	err := tartest.ConvertTarErofs(context.Background(), tartest.TarFromWriterTo(writerTo), path, "", nil)
	if err != nil {
		b.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}

	fsys, err := EroFS(f)
	if err != nil {
		b.Fatal(err)
	}

	benchFs = fsys
	return benchFs
}

func dirName(i int) string {
	if i < 26 {
		return string(rune('a' + i))
	}
	return string(rune('a'+i/26-1)) + string(rune('a'+i%26))
}

func BenchmarkOpenSmallFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/small.txt")
		if err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

func BenchmarkOpenMediumFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/medium.bin")
		if err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

func BenchmarkOpenLargeFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/large.bin")
		if err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

func BenchmarkOpenNestedPath(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/dir100/z/file9.txt")
		if err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

func BenchmarkReadFullSmallFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	b.SetBytes(1000)
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/small.txt")
		if err != nil {
			b.Fatal(err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != 1000 {
			b.Fatal("unexpected length")
		}
	}
}

func BenchmarkReadFullMediumFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	b.SetBytes(102400)
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/medium.bin")
		if err != nil {
			b.Fatal(err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != 102400 {
			b.Fatal("unexpected length")
		}
	}
}

func BenchmarkReadFullLargeFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	b.SetBytes(819200)
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/large.bin")
		if err != nil {
			b.Fatal(err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != 819200 {
			b.Fatal("unexpected length")
		}
	}
}

func BenchmarkReadEmptyFile(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/empty.txt")
		if err != nil {
			b.Fatal(err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != 0 {
			b.Fatal("unexpected length")
		}
	}
}

func BenchmarkSequentialReadSmallFile(b *testing.B) {
	fsys := initBenchmark(b)
	buf := make([]byte, 100)
	b.ResetTimer()
	b.SetBytes(100 * 100)
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/small.txt")
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 10; j++ {
			n, err := f.Read(buf)
			if err != nil && err != io.EOF {
				b.Fatal(err)
			}
			if n != 100 {
				b.Fatal("unexpected read count")
			}
		}
		f.Close()
	}
}

func BenchmarkSequentialReadMediumFile(b *testing.B) {
	fsys := initBenchmark(b)
	buf := make([]byte, 4096)
	b.ResetTimer()
	b.SetBytes(4096 * 25)
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/medium.bin")
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 25; j++ {
			n, err := f.Read(buf)
			if err != nil && err != io.EOF {
				b.Fatal(err)
			}
			if n != 4096 {
				b.Fatal("unexpected read count")
			}
		}
		f.Close()
	}
}

func BenchmarkStat(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fi, err := fs.Stat(fsys, "/small.txt")
		if err != nil {
			b.Fatal(err)
		}
		if fi.Size() != 1000 {
			b.Fatal("unexpected size")
		}
	}
}

func BenchmarkReadDirRoot(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/")
		if err != nil {
			b.Fatal(err)
		}
		entries, err := f.(fs.ReadDirFile).ReadDir(-1)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) < 4 {
			b.Fatalf("unexpected entry count: got %d", len(entries))
		}
	}
}

func BenchmarkReadDirLarge(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/dir100/a")
		if err != nil {
			b.Fatal(err)
		}
		entries, err := f.(fs.ReadDirFile).ReadDir(-1)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 10 {
			b.Fatal("unexpected entry count")
		}
	}
}

func BenchmarkReadDirAllDirs(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	b.SetBytes(1000 * 10)
	for i := 0; i < b.N; i++ {
		for d := 0; d < 100; d++ {
			f, err := fsys.Open("/dir100/" + dirName(d))
			if err != nil {
				b.Fatal(err)
			}
			entries, err := f.(fs.ReadDirFile).ReadDir(-1)
			f.Close()
			if err != nil {
				b.Fatal(err)
			}
			if len(entries) != 10 {
				b.Fatal("unexpected entry count")
			}
		}
	}
}

func BenchmarkReadDirSequential(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	count := 0
	for i := 0; i < b.N; i++ {
		f, err := fsys.Open("/")
		if err != nil {
			b.Fatal(err)
		}
		rdf := f.(fs.ReadDirFile)
		for {
			entries, err := rdf.ReadDir(1)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count += len(entries)
		}
		f.Close()
	}
	_ = count
}

func BenchmarkOpenAndStatMultipleFiles(b *testing.B) {
	fsys := initBenchmark(b)
	b.ResetTimer()
	b.SetBytes(1000 * 10)
	files := []string{"/small.txt", "/medium.bin", "/large.bin", "/empty.txt"}
	for i := 0; i < b.N; i++ {
		for _, path := range files {
			f, err := fsys.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			_, err = f.Stat()
			f.Close()
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
