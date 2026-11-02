package trace

import (
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// The bound (GOV-B26).
//
// The log was append-only with no bound at all, and on the machine that dogfoods this repo it
// reached 699,334,271 bytes in six weeks: a 1 Hz sampler writing ~190-byte records, about
// 16 MB a day, which is exactly what an unbounded append-only log does. It was found only by
// listing the state directory after an uninstall.
//
// THE RULE: when the live file reaches MaxBytes, it is renamed to samples.jsonl.1, replacing
// any previous .1, and the next record starts a fresh file. One generation is kept, so the log
// never occupies more than about 2 x MaxBytes on disk.
//
// WHY 16 MiB. The two writers differ by three orders of magnitude, and the number is chosen
// against both:
//
//   - The gate's build records, the only thing a stock install writes, arrive at roughly
//     50 KB a day on a heavily used machine (133 builds and 235 agent records in 143 KB over
//     19 hours, 2026-11-02). 16 MiB is about a year of them in the live file and another year
//     in .1, which covers any analysis window this project has ever scored (GOV-17's are
//     weeks) and the 20 decisions `doctor --report` prints with enormous room.
//   - The Phase 0 sampler, present only after `governor trace install`, writes ~16 MB a day,
//     so 16 MiB is about a day of it live and a day in .1. Phase 0 closed on 2026-09-12;
//     someone re-running it on purpose to collect more than two days should copy the file
//     out, and that is stated in the guide rather than defaulted for everyone.
//
// 32 MiB as a worst case is three times the binary itself and under a twentieth of what one
// machine actually accumulated. Less would start rotating build history away within months.
var MaxBytes int64 = 16 << 20

// preBoundFactor separates a file this bound produced from one written before it existed.
//
// A file rotated by this code is at most MaxBytes plus whatever concurrent appenders wrote in
// the instant between crossing the line and the rename — a few records. A file more than
// twice the bound therefore cannot have come from here: it was written by a binary with no
// bound, and it is the six weeks of measurement that docs/calibration.md and its siblings
// argue from. Rotating it to .1 would mean the NEXT rotation deletes it, so the bound would
// quietly make a decision about old data while appearing only to limit new data. Instead it
// is renamed aside under its own name and never touched again. Deleting it is the user's
// call, `governor doctor` names it, and `uninstall --purge` removes it with the rest.
const preBoundFactor = 2

// RotatedSuffix is appended to the log's name for the one kept generation.
const RotatedSuffix = ".1"

// PreBoundGlob matches a file set aside because it was written before the bound existed.
const PreBoundGlob = "samples.pre-bound-*.jsonl"

// RotatedPath is the full path of the previous generation, samples.jsonl.1.
func RotatedPath() (string, error) {
	p, err := LogPath()
	if err != nil {
		return "", err
	}
	return p + RotatedSuffix, nil
}

// LogFile is one file of the trace log on disk.
type LogFile struct {
	Path string
	Size int64
}

// Files lists every file of the trace log that exists: the live file, the rotated
// generation, and any file set aside from before the bound, in that order.
//
// It is what doctor reports and what the guide points at. A missing file is simply absent
// from the list; only a failure to resolve the state directory is an error.
func Files() ([]LogFile, error) {
	live, err := LogPath()
	if err != nil {
		return nil, err
	}
	paths := []string{live, live + RotatedSuffix}
	pre, _ := filepath.Glob(filepath.Join(filepath.Dir(live), PreBoundGlob))
	sort.Strings(pre)
	paths = append(paths, pre...)

	var out []LogFile
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, LogFile{Path: p, Size: fi.Size()})
	}
	return out, nil
}

// rotateIfFull renames the live log aside when f, just written to, has reached the bound.
//
// IT RUNS AFTER THE WRITE, and on the gate's path that is AFTER the decision is on stdout
// (buildlog.go's ordering argument). Nothing here can delay a spawn.
//
// THE COMMON CASE IS ONE fstat ON A DESCRIPTOR ALREADY OPEN, and that is the whole cost added
// to the hot path: no path lookup, no lock, no second open. Everything below the size test
// runs once per MaxBytes written, which for build records is about once a year.
//
// CONCURRENT ROTATORS. Several hook processes and the sampler append at once, so two of them
// can see the same full file. If both renamed, the second would move the fresh, nearly empty
// file onto .1 and discard the generation the first had just kept. Two things stop that:
//
//   - The rename happens only if the path still names the very file f is open on. A process
//     that lost the race finds the path pointing at a new file and leaves it.
//   - The check and the rename happen under a non-blocking flock on that file, so two
//     processes cannot both pass the check before either renames. LOCK_NB, never a blocking
//     lock: a process that cannot take it at once leaves rotation to the one that did, and
//     no appender ever waits on another.
//
// Every failure is silent and leaves the log as it was. A missed rotation is retried by the
// next write; a log a little over the bound is harmless; an error here surfacing as a failed
// write would be an instrument interfering with what it measures.
func rotateIfFull(f *os.File, path string) {
	fi, err := f.Stat()
	if err != nil || fi.Size() < MaxBytes {
		return
	}
	fd := int(f.Fd())
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)

	cur, err := os.Stat(path)
	if err != nil || !os.SameFile(cur, fi) {
		return
	}
	dst := path + RotatedSuffix
	if fi.Size() > preBoundFactor*MaxBytes {
		dst = filepath.Join(filepath.Dir(path),
			"samples.pre-bound-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
		if _, err := os.Lstat(dst); err == nil {
			// Never replace one set-aside file with another. Two in the same second would
			// need two pre-bound binaries racing, and keeping the first is the safe answer.
			return
		}
	}
	_ = os.Rename(path, dst)
}
