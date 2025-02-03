package erofs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
)

func TestBasic(t *testing.T) {
	fs, err := EroFS(loadTestFile(t, "basic"))
	if err != nil {
		t.Fatal(err)
	}

	checkFileString(t, fs, "/in-root.txt", "root file content\n")
	checkFileString(t, fs, "/usr/lib/testdir/emptyfile", "")
	checkFileBytes(t, fs, "/usr/lib/testdir/13k-zeros.raw", bytes.Repeat([]byte{0}, 1024*13))
	checkFileBytes(t, fs, "/usr/lib/testdir/16k-zeros.raw", bytes.Repeat([]byte{0}, 1024*16))
	checkFileBytes(t, fs, "/usr/lib/testdir/5k-sequence.raw", bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 128*5))
	checkFileBytes(t, fs, "/usr/lib/testdir/16k-sequence.raw", bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 128*16))
	checkDirectorySize(t, fs, "/usr/lib/testdir/emptydir", 0)
	checkDirectorySize(t, fs, "/usr/lib/testdir/lotsoffiles", 5000)
	checkNotExists(t, fs, "/not-exists.txt")
	checkNotExists(t, fs, "/not-exists/somefile")
	checkNotExists(t, fs, "/usr/lib/testdir/emptydir/somefile")
	checkFileString(t, fs, "/usr/lib/testdir/case/file.txt", "lower case dir\n")
	checkFileString(t, fs, "/usr/lib/testdir/CASE/file.txt", "upper case dir\n")
	checkFileString(t, fs, "/usr/lib/testdir/case.txt", "lower case file\n")
	checkFileString(t, fs, "/usr/lib/testdir/CASE.txt", "upper case file\n")
}

func loadTestFile(t testing.TB, name string) io.ReaderAt {
	t.Helper()
	f, err := os.Open("testdata/" + name + ".erofs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.Close()
	})
	return f
}

func checkFileString(t testing.TB, fsys fs.FS, name, content string) {
	t.Helper()

	f, err := fsys.Open(name)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		t.Error(err)
		return
	}

	actual := string(b)
	if actual != content {
		t.Errorf("Unexpected content in %s\n\tActual:   %q\n\tExpected: %q", name, actual, content)
	}
}

func checkFileBytes(t testing.TB, fsys fs.FS, name string, content []byte) {
	t.Helper()

	f, err := fsys.Open(name)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		t.Error(err)
		return
	}

	if !bytes.Equal(b, content) {
		if len(b) != len(content) {
			t.Logf("Unexpected content in %s\n\tActual Len: %d\n\tExpected Len: %d", name, len(b), len(content))
		} else if len(b) < 8192 {
			t.Logf("Unexpected content in %s\n\tActual:   %x\n\tExpected: %x", name, b, content)
		} else {
			t.Logf("Unexpected content in %s\n\tActual:   %x...%x\n\tExpected: %x...%x", name, b[:4096], b[len(b)-4096:], content[:4096], content[len(content)-4096:])
		}
		t.Fail()
	}
}

func checkDirectorySize(t testing.TB, fsys fs.FS, name string, n int) {
	t.Helper()

	entries, err := fs.ReadDir(fsys, name)
	if err != nil {
		t.Error(err)
	}
	if len(entries) != n {
		t.Errorf("Unexpected directory entries in %s: Got %d, expected %d", name, len(entries), n)
	}
}

func checkNotExists(t testing.TB, fsys fs.FS, name string) {
	t.Helper()

	_, err := fsys.Open(name)
	if err == nil {
		t.Errorf("expected error opening %s", name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected not exist error opening %s, got %v", name, err)
	}
}
