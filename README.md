# go-erofs

A Go library for opening erofs files as a Go stdlib [fs.FS](https://pkg.go.dev/io/fs#FS).

## Scope

This library is designed to allow erofs files to be usable in any Go operation that uses
the standard filesystem interface. This could be useful for accessing an erofs file just
as you would a plain directory without needing to unpack. In the future this library
could provide an interface to create erofs files as well.

## Current state

- [x] Read erofs files created with default `mkfs.erofs` options
- [ ] Xattr support (needs interface defined, currently not in Go stdlib)
- [ ] Read chunk-based erofs files
- [ ] Read erofs files with compression
- [ ] Extra devices for chunked data
- [ ] Creating erofs files
