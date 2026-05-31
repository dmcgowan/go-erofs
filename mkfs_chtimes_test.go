package erofs_test

import (
	"bytes"
	"testing"
	"time"

	erofs "github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/erofstest"
)

// TestFileChtimes verifies that *File.Chtimes sets the per-entry
// timestamps that are then visible after the image is finalised.
func TestFileChtimes(t *testing.T) {
	var buf testBuffer
	w := erofs.Create(&buf)

	f, err := w.Create("/timed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("when\n")); err != nil {
		t.Fatal(err)
	}
	wantMtime := time.Unix(1_600_000_000, 123_456_789)
	wantAtime := time.Unix(1_700_000_000, 987_654_321)
	if err := f.Chtimes(wantAtime, wantMtime); err != nil {
		t.Fatal("Chtimes:", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())
	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	st := erofstest.Stat(t, efs, "timed.txt")
	if uint64(wantMtime.Unix()) != st.Mtime {
		t.Errorf("Mtime: got %d, want %d", st.Mtime, wantMtime.Unix())
	}
	if uint32(wantMtime.Nanosecond()) != st.MtimeNs {
		t.Errorf("MtimeNs: got %d, want %d", st.MtimeNs, wantMtime.Nanosecond())
	}
}
