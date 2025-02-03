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

## Example use

Print out all the files in an erofs file

```
package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/dmcgowan/go-erofs"
)

func main() {
	f, err := os.Open("testdata/basic-default.erofs")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	img, err := erofs.EroFS(f)
	if err != nil {
		log.Fatal(err)
	}

	fs.WalkDir(img, "/", func(path string, entry fs.DirEntry, err error) error {
		fmt.Println(path)
		return nil
	})
}
```
