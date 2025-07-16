package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/erofs/go-erofs"
)

func main() {
	var (
		path string
	)

	flag.StringVar(&path, "img", "", "Path to erofs image")
	flag.Parse()

	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	img, err := erofs.EroFS(f)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Found valid image...\n")

	err = fs.WalkDir(img, "/", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error visting %s: %w", path, err)
		}
		fmt.Printf("visited: %q\n", path)
		fmt.Printf("\tName: %q\n", entry.Name())
		fmt.Printf("\tType: %o\n", entry.Type())
		if entry.IsDir() {
			fmt.Printf("\tIs a directory: yes\n")
		} else {
			fmt.Printf("\tIs a directory: no\n")
		}
		fi, err := entry.Info()
		if err != nil {
			return fmt.Errorf("error getting info for %s: %w", path, err)
		}
		fmt.Printf("\tMode: %o\n", fi.Mode())
		fmt.Printf("\tModTime: %s\n", fi.ModTime())
		st := fi.Sys().(*erofs.Stat)
		if len(st.Xattrs) > 0 {
			fmt.Printf("\tXattrs:\n")
			for k, v := range st.Xattrs {
				fmt.Printf("\t\t%s: %q\n", k, v)
			}
		}
		if entry.Name() == "." || entry.Name() == ".." {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}
