package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// openFilesOf lists the regular files pid holds open, through lsof.
//
// lsof on the user's own process needs no privilege, and it is the route doctor already takes
// for a daemon's socket (unixSocketOf), for the reason recorded there: gopsutil's OpenFiles
// returns "not implemented yet" on darwin. +L adds the link count, so a file that has been
// unlinked under the process reads k0, and -F gives one field per line to parse rather than
// columns to guess at. Measured on 2026-11-21 against a process holding a renamed file and an
// unlinked one: the renamed file reports its NEW name, the unlinked one its last name with k0.
//
// No sudo and no powermetrics, per the project rule.
func openFilesOf(ctx context.Context, pid int) ([]openFile, error) {
	ctx, cancel := context.WithTimeout(ctx, strayLsofBudget)
	defer cancel()

	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", fmt.Sprint(pid), "+L", "-Fftikn").Output()
	if err != nil {
		return nil, err
	}
	return parseLsofFiles(string(out)), nil
}

// parseLsofFiles reads lsof's -F output: one field per line, each prefixed by its identifier.
// "f" starts a new descriptor and the fields after it until the next "f" belong to it.
func parseLsofFiles(out string) []openFile {
	var files []openFile
	var cur openFile
	var regular, have bool
	flush := func() {
		// Numbered descriptors only: cwd, txt and mem are the process's directory and
		// executable, not files it is writing to.
		if _, err := strconv.Atoi(cur.fd); have && regular && err == nil && strings.HasPrefix(cur.name, "/") {
			files = append(files, cur)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'f':
			flush()
			cur, regular, have = openFile{fd: v}, false, true
		case 't':
			regular = v == "REG"
		case 'i':
			cur.ino, _ = strconv.ParseUint(v, 10, 64)
		case 'k':
			cur.nlink, _ = strconv.ParseUint(v, 10, 64)
		case 'n':
			cur.name = v
		}
	}
	flush()
	return files
}
