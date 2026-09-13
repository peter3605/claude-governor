package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// THE SPAWN SURFACE LEDGER (GOV-30).
//
// What this answers: does the PreToolUse matcher actually fire on the Claude Code that is
// running here, right now?
//
// Why a ledger rather than a comparison. Before GOV-30 the spawn check compared a constant —
// the version the tool names were last MEASURED against — to the version running, and when
// they differed it printed `ok` with a caveat. That is an unfalsifiable claim dressed as a
// check: it can notice that a measurement is old, but it cannot tell an old-but-still-correct
// matcher from one that stopped matching anything at all. Those two look identical from the
// outside, and the second one is this project's founding failure — "a wrong tool name means
// the hook silently never fires and everything downstream looks like it works".
//
// THE OBSERVATION IS FREE AND IT IS PROOF. The gate runs only BECAUSE the matcher fired. So
// every acquire the daemon receives is a real PreToolUse payload that the matcher caught, and
// the tool_name on it is the name the harness actually emitted — not a name read out of this
// repo's own source. Recording (Claude Code version, tool name) turns the check into one that
// can fail: if a future Claude Code renames the spawn tool, the matcher stops firing, the
// daemon sees no acquire on that version, the ledger stays empty for it, and doctor says so.
// Silence is the signal.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not widen the matcher to catch plausible names,
// and nothing here feeds spawnToolNames. A matcher that matches names the harness never sends
// is not safer, it is untested in a new way, and it would mask exactly the drift this exists
// to detect. The ledger only ever records what was seen.
//
// It is written by the daemon, off the acquire path, and read by doctor through the status
// reply. Losing it is harmless: an empty ledger reports "not confirmed", which is the honest
// answer, and the next spawn refills it.

// surfaceFile is the ledger's name inside the state directory, and surfaceTempFile is the
// fixed name it is staged under before the rename.
//
// Both are listed in design.md section 13.3 and removed by uninstall. A file the governor
// creates and uninstall leaves behind is the whole reason that section is normative — and a
// RANDOM temp name is precisely the thing uninstall cannot name, so this one is fixed. The
// writer is a goroutine nothing waits for, so a SIGTERM between create and rename is a real
// way to leave one behind; with a fixed name the directory still empties.
//
// A fixed name is only safe because writeMu serialises writers. Do not remove one without the
// other.
const (
	surfaceFile     = "surface.json"
	surfaceTempFile = "surface.json.tmp"
)

// surfaceSchema is the ledger's on-disk schema. A file at an unrecognised schema is ignored
// and overwritten rather than migrated — it holds observations that cost one spawn to
// reacquire, so there is nothing here worth a migration path.
const surfaceSchema = 1

// spawnSighting is every spawn tool name seen on one Claude Code version.
type spawnSighting struct {
	// Version is the Claude Code version of the session that spawned, or "" when this host
	// has no session registry to read it from. Empty is kept rather than dropped: it still
	// proves the matcher fired, which is most of what the check is for.
	Version string `json:"version"`
	// Tools are the distinct tool_name values seen on that version, sorted.
	Tools []string `json:"tools"`
	// First and Last bracket the sightings, RFC3339. Last is what tells a reader whether the
	// evidence is from this week or from an install three months ago.
	First string `json:"first"`
	Last  string `json:"last"`
	// Count is how many spawns backed this entry.
	Count int `json:"count"`
}

// surfaceLedger is the in-memory ledger and its file.
type surfaceLedger struct {
	mu   sync.Mutex
	path string
	// by is version -> tool name -> seen, the shape the hot path appends into.
	by map[string]map[string]bool
	// meta is version -> the timestamps and count that do not fit the set above.
	meta map[string]*spawnSighting
	// writeMu serialises save() from marshal through rename.
	//
	// It is separate from mu, and held across I/O where mu deliberately is not. Without it two
	// goroutines that each recorded a new pair can snapshot, marshal and rename in any order,
	// and the OLDER snapshot can land last — a confirmed lost update, reproduced at 8
	// concurrent writers. In the field that is a burst of first spawns across two Claude Code
	// versions: the ledger in memory is right, the file is missing a pair, and after the next
	// daemon restart doctor tells the user their spawns are not being governed.
	writeMu sync.Mutex
	// now is overridable for tests.
	now func() time.Time
}

type surfaceDoc struct {
	Schema   int             `json:"schema"`
	Observed []spawnSighting `json:"observed"`
}

func newSurfaceLedger(dir string) *surfaceLedger {
	l := &surfaceLedger{
		by:   map[string]map[string]bool{},
		meta: map[string]*spawnSighting{},
		now:  time.Now,
	}
	if dir != "" {
		l.path = filepath.Join(dir, surfaceFile)
	}
	l.load()
	return l
}

// load reads the ledger from disk. Every failure is silent and leaves an empty ledger: a
// missing, truncated or hand-mangled file means "nothing confirmed yet", which doctor already
// has a state for, and refusing to start a daemon over it would trade a diagnostic for an
// outage.
func (l *surfaceLedger) load() {
	if l.path == "" {
		return
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var doc surfaceDoc
	if err := json.Unmarshal(b, &doc); err != nil || doc.Schema != surfaceSchema {
		return
	}
	for _, s := range doc.Observed {
		set := map[string]bool{}
		for _, t := range s.Tools {
			if t != "" {
				set[t] = true
			}
		}
		l.by[s.Version] = set
		rec := s
		l.meta[s.Version] = &rec
	}
}

// note records one observed spawn. It reports whether anything changed, so the caller can
// skip the write on the overwhelmingly common case of a repeat sighting.
func (l *surfaceLedger) note(version, tool string) bool {
	if tool == "" {
		// Nothing to record. An acquire with no tool name is an older gate talking to a newer
		// daemon, and inventing a name for it would put a claim in the ledger that no payload
		// backs — which is the defect this whole file exists to remove.
		return false
	}
	if !matchedSpawnTool(tool) {
		// Defence in depth. The gate only reaches the daemon for a name the matcher caught, so
		// this should be unreachable — but the ledger is evidence that doctor draws a
		// conclusion from, and evidence accepted from an unexpected source is how a diagnostic
		// starts confirming things nobody measured. A name outside the matcher is not part of
		// the surface being recorded, whatever sent it.
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	stamp := l.now().UTC().Format(time.RFC3339)
	set, ok := l.by[version]
	if !ok {
		set = map[string]bool{}
		l.by[version] = set
		l.meta[version] = &spawnSighting{Version: version, First: stamp}
	}
	rec := l.meta[version]
	rec.Count++
	rec.Last = stamp
	if set[tool] {
		// A repeat sighting still moves Last and Count, but those are for a human reading the
		// file, not for the check. Persisting on every spawn to freshen a timestamp would put
		// a file write on a path that has none today.
		return false
	}
	set[tool] = true
	return true
}

// sightings is the ledger as the status reply carries it, sorted for a stable wire form.
func (l *surfaceLedger) sightings() []spawnSighting {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotLocked()
}

func (l *surfaceLedger) snapshotLocked() []spawnSighting {
	out := make([]spawnSighting, 0, len(l.by))
	for v, set := range l.by {
		tools := make([]string, 0, len(set))
		for t := range set {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		rec := l.meta[v]
		out = append(out, spawnSighting{
			Version: v, Tools: tools,
			First: rec.First, Last: rec.Last, Count: rec.Count,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// save writes the ledger atomically. Errors are dropped for the reason load's are: this file
// is evidence, not state anything depends on, and a daemon that failed to start because it
// could not write a diagnostic would be a worse bug than the one being diagnosed.
func (l *surfaceLedger) save() {
	if l.path == "" {
		return
	}
	// Taken BEFORE the snapshot, so that the last writer to rename is also the one holding the
	// newest view. Snapshotting outside this lock is what let an older file win.
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	l.mu.Lock()
	doc := surfaceDoc{Schema: surfaceSchema, Observed: l.snapshotLocked()}
	l.mu.Unlock()

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	b = append(b, '\n')
	name := filepath.Join(filepath.Dir(l.path), surfaceTempFile)
	if err := os.WriteFile(name, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(name, l.path); err != nil {
		// Leaving the staged file behind would defeat uninstall's "removes everything it
		// added", and it is named in section 13.3 so that it can be removed — but removing it
		// here is cheaper than relying on that.
		_ = os.Remove(name)
	}
}
