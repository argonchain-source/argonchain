// Command mkzip writes a release archive: mkzip <out.zip> <dir>. Entries sit
// under the folder's own name with forward slashes, the programs (files
// without an extension, .exe and .sh files) are marked executable so they
// run after unzip on Linux and macOS, and every entry carries the same fixed
// time, so the same files always give the same archive.
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mkzip <out.zip> <dir>")
		os.Exit(2)
	}
	if err := write(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "mkzip:", err)
		os.Exit(1)
	}
}

func write(out, dir string) error {
	root := filepath.Base(filepath.Clean(dir))
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	stamp := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, p := range files {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := path.Join(root, filepath.ToSlash(rel))
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: stamp}
		base := path.Base(name)
		mode := fs.FileMode(0o644)
		if ext := path.Ext(base); ext == "" || ext == ".exe" || ext == ".sh" {
			mode = 0o755
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}
