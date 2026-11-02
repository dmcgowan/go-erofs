package erofs

import "sync/atomic"

// Progress reports how much work a [Writer] has done. It is returned by
// [Writer.Progress]. The counts show that work is being done; they are not
// an estimate of the final image size or of the time left.
type Progress struct {
	// Entries is the number of entries in the image, excluding the root
	// directory. It goes down when entries are removed.
	Entries int64

	// EntriesWritten is the number of entries Close has written. It equals
	// Entries once Close has finished. For an image with at least one entry
	// besides the root, it is below Entries until then. For an image with
	// only the root, both are zero before Close starts.
	EntriesWritten int64

	// DataBytes is the number of file data bytes Close will write: data
	// taken in through [File.Write], or by reference from a
	// [Writer.CopyFrom] source, for files still in the image. It goes down
	// when the last name of a file is removed or the file is replaced.
	// Files added by a [MetadataOnly] CopyFrom are not counted.
	DataBytes int64

	// DataBytesWritten is the number of file data bytes Close has written.
	// Data stored in the data file set with [WithDataFile] counts as
	// written when Close writes the file's chunk indexes. It equals
	// DataBytes once Close has finished. DataBytesWritten can reach
	// DataBytes before the metadata is written. For an image with at least
	// one entry besides the root, EntriesWritten reaching Entries shows that
	// Close has finished.
	DataBytesWritten int64
}

// progressCounters holds the counters behind [Writer.Progress]. The Writer
// updates them atomically so Progress may be called from any goroutine.
type progressCounters struct {
	entries          atomic.Int64
	entriesWritten   atomic.Int64
	dataBytes        atomic.Int64
	dataBytesWritten atomic.Int64
}

// addData records n bytes of file data taken in for ino. They count toward
// DataBytes while a name in the tree refers to ino. Data for an inode with no
// names left, such as a file removed while still open, is not counted
// because Close will not write it.
func (fsys *Writer) addData(ino *fsInode, n int64) {
	if n <= 0 {
		return
	}
	ino.dataBytes += n
	if ino.names > 0 {
		fsys.progress.dataBytes.Add(n)
	}
}

// addName records that a name referring to ino has joined the tree. When
// it is the only name, the inode's data is counted again: CopyFrom can add
// a hard link to an inode whose names were all removed earlier in the same
// call.
func (fsys *Writer) addName(ino *fsInode) {
	ino.names++
	if ino.names == 1 && ino.dataBytes > 0 {
		fsys.progress.dataBytes.Add(ino.dataBytes)
	}
}

// dropName records that one name referring to ino has left the tree. When
// the last name goes, the inode's data is no longer counted.
func (fsys *Writer) dropName(ino *fsInode) {
	ino.names--
	if ino.names == 0 && ino.dataBytes > 0 {
		fsys.progress.dataBytes.Add(-ino.dataBytes)
	}
}

// Progress returns the Writer's current progress. It may be called from any
// goroutine, including while entries are being added or Close runs.
func (fsys *Writer) Progress() Progress {
	p := &fsys.progress
	// Load the written counts before the totals. The totals do not change
	// once Close starts, so a sample taken across the start of Close cannot
	// pair an older total with a newer written count. entriesWritten is
	// loaded first because Close stores it last.
	entriesWritten := p.entriesWritten.Load()
	dataBytesWritten := p.dataBytesWritten.Load()
	return Progress{
		Entries:          p.entries.Load(),
		EntriesWritten:   entriesWritten,
		DataBytes:        p.dataBytes.Load(),
		DataBytesWritten: dataBytesWritten,
	}
}
