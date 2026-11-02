package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/peter3605/claude-governor/internal/trace"
)

// checkTraceLog names the trace log and its size (GOV-B26, clause 4).
//
// WHY DOCTOR AND NOT ONLY THE GUIDE. samples.jsonl was the largest thing the governor put on a
// machine by two orders of magnitude, and the only way anyone found it was listing the state
// directory after an uninstall. A sentence in the guide is read once, at install; a check is
// read every time someone asks whether the machine is healthy, which is when a 700 MB file
// should come up. So doctor reports it on every run — the guide also names it, but doctor is
// the one that cannot be missed.
//
// THE LINE IS 2 x trace.MaxBytes, and it is not a tuning constant. The bound keeps the live
// file under MaxBytes and the one rotated generation about the same, so no file the current
// binary writes can exceed twice the bound. A file that does was written before the bound
// existed: either the live log of a machine that has not appended since upgrading, or a log
// set aside as samples.pre-bound-*.jsonl on the first rotation. Those are exactly the files a
// person should be told about, and a bounded log can never trip it, so the warn cannot become
// a standing yellow line on a healthy machine.
//
// IT IS A WARN, NOT A FAILURE. Nothing is broken: the governor works the same with a large old
// log beside it. What to do with six weeks of measurement is the user's decision (GOV-B26's
// first trap), so the fix names the file and the choices and does not make one.
func (d *diagnosis) checkTraceLog() {
	files, err := trace.Files()
	if err != nil {
		d.add("tracelog", pass, "trace log location unresolved: "+err.Error(), "")
		return
	}
	if len(files) == 0 {
		d.add("tracelog", pass,
			fmt.Sprintf("no trace log yet; it will rotate at %s", humanBytes(trace.MaxBytes)), "")
		return
	}

	var parts, oversize []string
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("%s %s", filepath.Base(f.Path), humanBytes(f.Size)))
		if f.Size > 2*trace.MaxBytes {
			oversize = append(oversize, f.Path)
		}
	}
	dir := short(filepath.Dir(files[0].Path))
	if len(oversize) == 0 {
		d.add("tracelog", pass,
			fmt.Sprintf("%s in %s, rotating at %s with one older file kept",
				strings.Join(parts, ", "), dir, humanBytes(trace.MaxBytes)), "")
		return
	}
	d.add("tracelog", warn,
		fmt.Sprintf("%s in %s; a file over %s was written before the log had a bound and "+
			"is kept, never rotated away", strings.Join(parts, ", "), dir, humanBytes(2*trace.MaxBytes)),
		fmt.Sprintf("it is yours to keep, compress or delete: %s (uninstall --purge also removes it)",
			shortList(oversize)))
}

// keptTraceLog lists the trace log's files with their sizes, for uninstall to name what it
// left. Empty when there is none.
func keptTraceLog() string {
	files, err := trace.Files()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("%s (%s)", short(f.Path), humanBytes(f.Size)))
	}
	return strings.Join(parts, ", ")
}

// humanBytes renders a size the way a person reads a directory listing.
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KiB", float64(n)/k)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
