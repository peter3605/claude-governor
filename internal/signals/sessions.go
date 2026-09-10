package signals

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// LiveSession is one Claude Code session running on this machine right now.
//
// The fields come from the harness's own session registry, ~/.claude/sessions/<pid>.json,
// one file per live session. That registry is what makes GOV-29's defect visible: hooks are
// bound at session start, so a session that predates the install never loaded the gate and
// is ungoverned for the rest of its life. Nothing in the payloads of docs/hook-surface.md
// sections 1-5 can see that, because the hooks of an ungoverned session never run — the
// evidence has to come from outside the hook surface, and this is where it lives.
type LiveSession struct {
	// ID is the session_id, and it is the join key. The same value arrives on every hook
	// payload (section 5), on the statusline payload (section 14.1), and is what the
	// daemon keys its session table by, so "live on this machine" and "registered with the
	// daemon" can be compared directly.
	ID string
	// PID is the claude process.
	PID int
	// Started is when the session started, from the registry's startedAt.
	Started time.Time
	// ProcStarted is the OS process start time, used only to detect a recycled PID.
	ProcStarted time.Time
	// CWD is the directory the session was opened in. It is carried so a diagnostic can
	// name which session a person has to restart — a bare UUID is not something anyone can
	// act on, and "restart the session in ~/Repos/nightcrew" is.
	CWD string
	// Version is the Claude Code version running that session.
	Version string
	// Status is the harness's own busy/idle marker. It is recorded rather than used: a
	// session is equally ungoverned whether or not it happens to be working right now.
	Status string
	// Kind is the harness's own session kind — "interactive" for a terminal session.
	//
	// It matters because the only fix for an ungoverned session is restarting it, and that is
	// not an action that exists for a headless `claude -p` run which is about to exit anyway.
	// A diagnostic that told someone to restart a batch job would be naming a fix they cannot
	// perform for a session that will be gone before they read it.
	Kind string
}

// ErrNoRegistry is returned when the session registry directory is not there at all.
//
// This is NOT a failure, and callers must not report it as one. The registry is an
// undocumented harness detail measured on 2.1.267 (docs/hook-surface.md section 17); a
// Claude Code that stops writing it, or an older one that never did, leaves the governor
// unable to answer how many sessions are live. The honest answer is then "unknown", on the
// same rule that makes an unmeasurable thermal state nominal rather than critical: an
// instrument that cannot read must say so, never guess a reading.
var ErrNoRegistry = errors.New("no Claude Code session registry on this machine")

// SessionsDir is where the harness keeps the registry.
func SessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// registryEntry is the subset of <pid>.json this needs. Every other key in that file is
// deliberately not read: the file is the harness's, not ours, and a governor that parsed
// fields it has no use for would break on a version that changed one of them.
type registryEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // epoch milliseconds
	ProcStart string `json:"procStart"` // "Mon Jan _2 15:04:05 2006", UTC
	Version   string `json:"version"`
	Status    string `json:"status"`
	Kind      string `json:"kind"`
}

// procStartLayout is the format of the registry's procStart field, which is written in UTC
// with a space-padded day of month.
const procStartLayout = "Mon Jan _2 15:04:05 2006"

// parseProcStart reads the registry's procStart, or the zero time.
//
// The zone is the entire point of this function existing separately. procStart carries no
// offset and is written in UTC, while the same text printed by `ps -o lstart` is local — so
// the natural reading is the wrong one. Parsing it as local time shifts every session by the
// machine's offset, four hours on the machine this was written on, which is enough to place a
// session that started after the install before it and report a governed session as ungoverned
// for life.
//
// It is a named function rather than three inline lines so that a test can drive THIS code
// with a fixed string. A test that called time.ParseInLocation itself would asserting the
// standard library's behaviour, pass against any mutation of the line below, and prove
// nothing — which is what the first version of that test did.
func parseProcStart(s string) time.Time {
	t, err := time.ParseInLocation(procStartLayout, s, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// LiveSessions reads the registry in dir and returns the sessions whose process is still
// running. An empty dir uses SessionsDir.
//
// A registry file whose process is gone is skipped rather than reported: the harness does
// not always clean up after an unclean exit, and counting a dead session as live would tell
// someone to restart a session that is not there. A file whose PID has been recycled by an
// unrelated process is skipped the same way, which is what ProcStarted is for.
func LiveSessions(dir string) ([]LiveSession, error) {
	if dir == "" {
		dir = SessionsDir()
	}
	if dir == "" {
		return nil, ErrNoRegistry
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoRegistry
		}
		return nil, err
	}

	var live []LiveSession
	unreadable := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// The name is "<pid>.json". Anything else in this directory is not ours to read;
		// the sibling "<pid>.<hash>.key" files are auth tokens and are never opened.
		if _, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json")); err != nil {
			continue
		}
		s, why := readRegistryEntry(filepath.Join(dir, e.Name()))
		switch why {
		case entryLive:
			live = append(live, s)
		case entryUnreadable:
			unreadable++
		}
	}
	// A registry whose files exist but cannot be understood must NOT come back as an empty
	// machine. That reading — "no Claude Code sessions running" — is the same false all-clear
	// this package was written to remove, arriving through a different door: a harness that
	// renamed a field or changed procStart's format would silently turn six live sessions into
	// zero, and every surface downstream would report a governed machine.
	//
	// Entries skipped because their PROCESS is gone are different and are not counted here: a
	// directory of stale files really does mean nothing is running, and that answer is correct.
	if len(live) == 0 && unreadable > 0 {
		return nil, fmt.Errorf("%w: %d session file(s) present, none of them readable",
			ErrNoRegistry, unreadable)
	}
	return live, nil
}

// entryVerdict is why one registry file did or did not yield a live session.
type entryVerdict int

const (
	// entryLive: parsed, and its process is running.
	entryLive entryVerdict = iota
	// entryDead: parsed fine, but the process is gone or the PID was recycled. Not a
	// measurement failure — this is what a stale file looks like, and skipping it is correct.
	entryDead
	// entryUnreadable: could not be parsed or carried no session id. Something about the
	// format is not what this code expects, and that is worth reporting rather than swallowing.
	entryUnreadable
)

// readRegistryEntry parses one registry file and reports whether it describes a session that
// is still running, and if not, which kind of not.
//
// No single failure is fatal: one unreadable file in a directory the governor does not own
// must not blind it to the other five. The verdict is returned rather than a bool so that the
// caller can tell "this machine has nothing running" from "this machine's registry is not in
// a format I understand", which are the same value and very different facts.
func readRegistryEntry(path string) (LiveSession, entryVerdict) {
	raw, err := readBounded(path)
	if err != nil {
		return LiveSession{}, entryUnreadable
	}
	var e registryEntry
	if json.Unmarshal(raw, &e) != nil {
		return LiveSession{}, entryUnreadable
	}
	if e.PID <= 0 || e.SessionID == "" {
		return LiveSession{}, entryUnreadable
	}

	s := LiveSession{
		ID:      e.SessionID,
		PID:     e.PID,
		CWD:     e.CWD,
		Version: e.Version,
		Status:  e.Status,
		Kind:    e.Kind,
	}
	if e.StartedAt > 0 {
		s.Started = time.UnixMilli(e.StartedAt)
	}
	s.ProcStarted = parseProcStart(e.ProcStart)
	if s.Started.IsZero() {
		// startedAt is the reliable field; procStart is the fallback, because a session
		// whose start time cannot be read at all cannot be compared against the install
		// time, and an entry that reports no start time would silently be treated as
		// having started at the epoch — that is, as ungoverned, always.
		s.Started = s.ProcStarted
	}
	if !alive(s.PID, s.ProcStarted) {
		return LiveSession{}, entryDead
	}
	return s, entryLive
}

// alive reports whether pid is running and is still the process the registry recorded.
//
// The start-time comparison is the recycled-PID guard. A registry file left behind by an
// unclean exit names a PID the kernel is free to hand to something else, and on a machine
// that has been up for weeks that is not hypothetical. Without the guard a stale file would
// present an unrelated process as a live Claude Code session, and the diagnostic built on
// top of it would tell someone to restart a session that ended days ago.
func alive(pid int, procStarted time.Time) bool {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	if procStarted.IsZero() {
		// Nothing to compare against. Being running is the best that can be said, and it
		// is the same answer PIDLiveness gives when it cannot do better.
		return true
	}
	ms, err := p.CreateTime()
	if err != nil {
		return true
	}
	// A tolerance, not an equality. The registry's procStart has no sub-second precision, and
	// the two sides do not derive the number the same way: on Linux gopsutil computes a start
	// time from the jiffy counter plus /proc/stat's btime, which can disagree with whatever
	// the harness wrote by a second in either direction. An exact comparison would then reject
	// EVERY entry on that platform and report a busy machine as empty. The window only has to
	// be tight enough to catch a recycled PID, and a PID recycled inside two seconds onto
	// another process the harness also recorded is not a case worth trading correctness for.
	delta := time.UnixMilli(ms).Sub(procStarted)
	if delta < 0 {
		delta = -delta
	}
	return delta <= procStartTolerance
}

// procStartTolerance is how far the OS's idea of a process start time may sit from the
// registry's before the entry is treated as describing a different process.
const procStartTolerance = 2 * time.Second

// maxRegistryFile bounds one registry file.
//
// The directory belongs to the harness, not to the governor, and os.ReadFile on a path that
// turned out to be a symlink to /dev/zero never reaches EOF. This runs inside the daemon,
// which is long-lived and holds the budget for the whole machine: a daemon that dies to an
// allocation takes every session ungoverned with it. A real entry is under a kilobyte.
const maxRegistryFile = 64 << 10

// readBounded reads at most maxRegistryFile bytes from a regular file.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		// A device, a fifo or a directory. Opening one is not something this has any business
		// doing, and a fifo would block the daemon forever.
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, maxRegistryFile))
}
