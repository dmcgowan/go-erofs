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

	fs.WalkDir(img, "/", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			fmt.Printf("error visiting %s: %v", path, err)
			return err
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
			return err
		}
		fmt.Printf("\tMode: %o\n", fi.Mode())
		fmt.Printf("\tModTime: %s\n", fi.ModTime())
		if entry.Name() == "." || entry.Name() == ".." {
			return fs.SkipDir
		}
		return nil
	})
}
