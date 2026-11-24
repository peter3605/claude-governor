package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// GOV-B58. Claude Code kills a status line that is still running when the next refresh starts.
//
// Measured on 2.1.331 (docs/hook-surface.md §14.4): every refresh, whether from an event or from
// refreshInterval, begins by aborting the run still in flight, and an abort sends SIGTERM to the
// command's process group, then SIGKILL to the group 1.5 s later if it is still there. Output of
// an aborted run is discarded. So a status line is not killed for having printed; it is killed
// for still being alive when the next refresh begins, which on a loaded machine includes the
// few milliseconds statuslineCommand spends writing the cache after the print.
//
// Go's default for SIGTERM is to exit at once, running no deferred function, so a TERM that lands
// between os.CreateTemp and os.Rename leaves statusline.json.<random> (or rate_limits.json.<random>)
// behind. That is the 0-byte stray of 2026-11-22: GOV-33 moved the write off the abandonable
// goroutine, but the process itself was still abandonable.
//
// Two layers, because SIGTERM can be caught and SIGKILL cannot:
//
//  1. guardStatuslineSignals: a terminating signal waits for a write already in flight, then
//     exits. One that arrives with no write in flight exits immediately, as before; this never
//     keeps a status line alive that was not writing. The longest it can delay an exit is one
//     write, and SIGKILL follows 1.5 s after the TERM regardless.
//  2. sweepStaleStatuslineTemps: any statusline.json.* or rate_limits.json.* older than a
//     minute is removed by the next run that writes the cache. That is for SIGKILL, a power cut,
//     and strays left before this fix. A live writer holds its temp file for milliseconds, and
//     SIGKILL ends any writer 1.5 s after it was asked to stop, so a minute-old temp has no owner.

// statuslineWriting is held across the post-print writes. A terminating signal takes it before
// exiting, so it waits for a write in flight and for nothing else.
var statuslineWriting sync.Mutex

// statuslineStopSignals are the ones that end a status line.
//
// NOT SIGPIPE. Go ignores a SIGPIPE from any descriptor but stdout and stderr, so a broken
// daemon socket is an EPIPE from the write. Notify for SIGPIPE delivers every one of them here,
// and the status line would then exit before printing because its daemon went away. Claude Code
// keeps stdout open until the process exits (hook-surface.md §14.4), and the print comes before
// the writes, so a SIGPIPE on stdout cannot strand a staging file anyway.
var statuslineStopSignals = []os.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT}

// guardStatuslineSignals installs the handler. It is called from main, only for the statusline
// subcommand, and never from runStatusline or statuslineCommand: tests call both in-process,
// and a test binary must not have its signals redirected. Doing so is what broke CI on the first
// version of this change: TestStatuslineExitsZero installed it, and a later test's SIGPIPE on
// Linux exited the whole test binary with status 141.
func guardStatuslineSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, statuslineStopSignals...)
	go func() {
		sig := <-ch
		statuslineWriting.Lock()
		code := 1
		if s, ok := sig.(syscall.Signal); ok {
			code = 128 + int(s)
		}
		os.Exit(code)
	}()
}

// statuslineTempMaxAge is how old a staging file must be before a later run treats it as
// abandoned. See layer 2 above for why a minute is far past any live writer.
const statuslineTempMaxAge = time.Minute

// sweepStaleStatuslineTemps removes abandoned staging files for the two files the status line
// writes. It runs after the line is printed and only when the cache was written, so it costs
// one directory read about once a second machine-wide, never on the render path. It never
// creates the directory and never touches any other name.
func sweepStaleStatuslineTemps(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, statuslineCacheFile+".") && !strings.HasPrefix(name, rateLimitsFile+".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) < statuslineTempMaxAge {
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
}

// statuslineWriteStall is empty in every real build. A test links a duration into it with
// -ldflags "-X main.statuslineWriteStall=300ms" so it can deliver a signal while a staging file
// exists, which is otherwise a window of microseconds no test can hit on purpose.
var statuslineWriteStall string

func stallStatuslineWriteForTest() {
	if statuslineWriteStall == "" {
		return
	}
	if d, err := time.ParseDuration(statuslineWriteStall); err == nil {
		time.Sleep(d)
	}
}
