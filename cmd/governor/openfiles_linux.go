package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// openFilesOf lists the regular files pid holds open, from /proc/<pid>/fd.
//
// Reading another process's fd directory needs no privilege when the process is the user's
// own, which every sampler doctor looks at is. stat on /proc/<pid>/fd/N follows the magic
// link to the open file itself, so it reports the inode and link count of what the process
// is actually writing to, even after that file has been unlinked.
func openFilesOf(_ context.Context, pid int) ([]openFile, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []openFile
	for _, e := range entries {
		link := filepath.Join(dir, e.Name())
		target, err := os.Readlink(link)
		if err != nil || !strings.HasPrefix(target, "/") {
			continue // a socket, a pipe, or gone already
		}
		fi, err := os.Stat(link)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		out = append(out, openFile{
			fd:    e.Name(),
			name:  strings.TrimSuffix(target, " (deleted)"),
			ino:   uint64(st.Ino),
			nlink: uint64(st.Nlink),
		})
	}
	return out, nil
}
