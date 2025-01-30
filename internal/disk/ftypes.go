package disk

import "io/fs"

const (
	FileTypeReg     = 1
	FileTypeDir     = 2
	FileTypeChrdev  = 3
	FileTypeBlkdev  = 4
	FileTypeFifo    = 5
	FileTypeSock    = 6
	FileTypeSymlink = 7
)

// Converts EroFS filetypes to Go FileMode
func EroFSFtypeToFileMode(ftype uint8) fs.FileMode {
	switch ftype {
	case FileTypeDir:
		return fs.ModeDir
	case FileTypeChrdev:
		return fs.ModeCharDevice
	case FileTypeBlkdev:
		return fs.ModeDevice
	case FileTypeFifo:
		return fs.ModeNamedPipe
	case FileTypeSock:
		return fs.ModeSocket
	case FileTypeSymlink:
		return fs.ModeSymlink
	default:
		return 0
	}
}
