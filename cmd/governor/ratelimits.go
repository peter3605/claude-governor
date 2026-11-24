package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The usage-window export (GOV-B57).
//
// Claude Code hands the status line the account's rate-limit windows (the 5-hour and weekly
// usage percentages) and hands them to nothing else: no hook payload carries them. On a
// machine where the governor owns the statusLine slot, the governor is therefore the only
// process that ever sees them, and it writes them down for whoever else wants them. headroom
// is the reader this was built for, but nothing here knows that: the file is a contract, and
// headroom's docs/design.md §5.1 is its source of truth. Change the format there first.
//
// The governor does not render these figures, and does not read the file back.

// rateLimitsFile is the export, in the state directory beside the socket. It is listed in
// design.md section 13.3, with its staging pattern in stateGlobs.
const rateLimitsFile = "rate_limits.json"

// rateLimitsVersion is the format version headroom checks before trusting the file.
const rateLimitsVersion = 1

// rateLimitsFresh is how long an unchanged export is left alone (headroom §5.1's write rule).
//
// Install sets refreshInterval: 2, so every open session runs the status line every two
// seconds whether anything changed or not. Twenty sessions would rewrite an identical file
// ten times a second. Thirty seconds keeps captured_at recent enough for headroom's 120 s
// freshness band while making the steady state close to no writes at all.
const rateLimitsFresh = 30 * time.Second

// rateWindow is one usage window as exported. Both numbers are float64 because the payload
// sends integers today and nothing promises it always will; encoding/json writes a whole
// float64 without a decimal point, so 27 goes out as 27.
type rateWindow struct {
	UsedPct  float64 `json:"used_pct"`
	ResetsAt float64 `json:"resets_at"`
}

// rateLimits is the export's content, without its stamp.
//
// Windows is an open map, as headroom §5.1 specifies: a window the harness adds later is
// carried through by name. A nil entry is a window the payload sent as null, written as null.
// A window the payload did not send is not in the map at all, and so is not in the file: an
// absent window must never be read as 0% used.
type rateLimits struct {
	CCVersion string                 `json:"cc_version"`
	Windows   map[string]*rateWindow `json:"windows"`
}

// rateLimitsExport is the file on disk.
type rateLimitsExport struct {
	V          int     `json:"v"`
	Writer     string  `json:"writer"`
	CapturedAt float64 `json:"captured_at"`
	rateLimits
}

// rawWindow is a window as the payload sends it, measured in docs/hook-surface.md §14.3.
// Pointers, so that a missing field is told apart from a zero.
type rawWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *float64 `json:"resets_at"`
}

// parseRateLimits reads the usage windows out of a decoded payload, or returns nil when there
// is nothing to export.
//
// It is called on the abandonable path, so it is pure: no file, no clock. Loose throughout:
// a window it cannot read is dropped, never zeroed, and only a payload with no rate_limits
// object at all produces nil. An object with no readable window still produces an export
// with no windows, because "Claude Code sent usage and none of it applies" is itself current
// information (a plan whose windows have all reset).
func parseRateLimits(p statusLinePayload) *rateLimits {
	raw := bytes.TrimSpace(p.RateLimits)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	out := &rateLimits{Windows: map[string]*rateWindow{}}
	var version string
	if json.Unmarshal(p.Version, &version) == nil {
		out.CCVersion = version
	}
	for name, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			out.Windows[name] = nil
			continue
		}
		var w rawWindow
		if json.Unmarshal(v, &w) != nil || w.UsedPercentage == nil || w.ResetsAt == nil {
			continue
		}
		if !finite(*w.UsedPercentage) || !finite(*w.ResetsAt) {
			continue
		}
		out.Windows[name] = &rateWindow{UsedPct: *w.UsedPercentage, ResetsAt: *w.ResetsAt}
	}
	return out
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// rateLimitsPath is where the export lives, or false if there is no state directory.
func rateLimitsPath() (string, bool) {
	dir, err := transport.StateDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, rateLimitsFile), true
}

// writeRateLimits writes the export, atomically, unless an identical one is under
// rateLimitsFresh old.
//
// It runs after the line is printed, beside writeCache, and for writeCache's reasons
// (statuslineCommand says them): a write on the abandonable path can be cut off holding a
// temp file, and a temp file left there is what kept ~/.claude/governor/ from emptying
// (GOV-33). The state directory is never created, for writeCache's other reason: a status
// line that conjured it would put a file back after uninstall.
//
// The staging name is random, unlike surface.json.tmp, because every session on the machine
// writes this file and a fixed staging name would let two of them write into one temp file.
// The random name is covered by stateGlobs. Nothing here is on the abandonable goroutine, and
// the process is guarded against Claude Code's SIGTERM while this runs (GOV-B58,
// statusline_signals.go). A SIGKILL can still strand the staging file; the next cache write
// sweeps it once it is a minute old.
func writeRateLimits(diag io.Writer, r rateLimits, now time.Time) {
	path, ok := rateLimitsPath()
	if !ok {
		return
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintf(diag, "governor statusline: no state directory at %s, not exporting rate limits\n", dir)
		return
	}
	if unchangedAndFresh(path, r, now) {
		return
	}
	export := rateLimitsExport{
		V:          rateLimitsVersion,
		Writer:     "governor",
		CapturedAt: math.Round(float64(now.UnixNano())/1e6) / 1e3,
		rateLimits: r,
	}
	raw, err := json.Marshal(export)
	if err != nil {
		fmt.Fprintf(diag, "governor statusline: encode rate limits: %v\n", err)
		return
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, rateLimitsFile+".*")
	if err != nil {
		fmt.Fprintf(diag, "governor statusline: create rate limits: %v\n", err)
		return
	}
	defer os.Remove(tmp.Name())
	stallStatuslineWriteForTest()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		fmt.Fprintf(diag, "governor statusline: write rate limits: %v\n", err)
		return
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintf(diag, "governor statusline: close rate limits: %v\n", err)
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		fmt.Fprintf(diag, "governor statusline: install rate limits: %v\n", err)
	}
}

// unchangedAndFresh reports whether the file on disk already says exactly this and was
// written under rateLimitsFresh ago. Freshness is the file's mtime, not its captured_at, so a
// file another writer stamped with a skewed clock is judged by when it actually landed. Any
// failure to read or parse it is "write it", and so is an mtime in the future.
func unchangedAndFresh(path string, r rateLimits, now time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if age := now.Sub(info.ModTime()); age < 0 || age >= rateLimitsFresh {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var have rateLimitsExport
	if json.Unmarshal(raw, &have) != nil || have.V != rateLimitsVersion || have.Writer != "governor" {
		return false
	}
	if have.CCVersion != r.CCVersion || len(have.Windows) != len(r.Windows) {
		return false
	}
	for name, w := range r.Windows {
		h, ok := have.Windows[name]
		if !ok || (h == nil) != (w == nil) || (w != nil && *h != *w) {
			return false
		}
	}
	return true
}
