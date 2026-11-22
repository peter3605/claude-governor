package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/peter3605/claude-governor/internal/trace"
)

// Stale Phase 0 instrumentation (GOV-B42).
//
// THE INCIDENT. On 2026-11-20 this machine's ~/.claude/settings.json still registered the five
// Phase 0 trace hooks a September install had written, all naming that September binary,
// while the governor's own hooks ran the 0.1.6 plugin copy. The sampler the old `trace ensure`
// kept restarting predated GOV-B26's log rotation, so when the 0.1.6 gate rotated
// samples.jsonl the old sampler went on writing to the replaced inode: 1.17 GB that nothing
// could read, and the 1 Hz samples that would have explained three nights of load averages of
// 40 to 70. doctor passed 18 of 18 the whole time. Fixing the binary could not reach it,
// because the hooks named a different binary.
//
// This check answers two questions, and only reads to answer them:
//   - do the trace hooks in settings.json run the same build the governor hooks run?
//   - is every running sampler that build, and is the samples.jsonl it holds the live one?
//
// A DEAD DESCRIPTOR IS A FAILURE; A DIFFERENT BUILD IS A WARNING. A sampler writing to a file
// that is no longer on disk is losing data and growing an invisible file right now. A trace
// hook from another build is the condition that produces that at the next rotation, and is
// worth acting on, but nothing is broken yet and it must not fail `governor install`.

// scanForSamplers is the process-table half of the check. Like scanForStrays it reads the
// whole machine, which no test can stage, so TestMain replaces it and the tests that are about
// it put a fake in its place.
var scanForSamplers = findTraceSamplers

// traceSampler is one running `governor trace sample`.
type traceSampler struct {
	pid int
	// path is argv[0], resolved against the process's working directory when relative, for
	// strayDaemon.path's reason: it survives the binary being deleted.
	path string
	// files are its open regular files, and filesErr why they could not be read.
	files    []openFile
	filesErr error
}

// findTraceSamplers lists every running Phase 0 sampler.
func findTraceSamplers(ctx context.Context) []traceSampler {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var found []traceSampler
	for _, p := range procs {
		pid := int(p.Pid)
		if pid == self {
			continue
		}
		argv, err := p.CmdlineSliceWithContext(ctx)
		if err != nil || !isTraceSamplerArgv(argv) {
			continue
		}
		path := argv[0]
		if !filepath.IsAbs(path) {
			if cwd, err := p.CwdWithContext(ctx); err == nil && cwd != "" {
				path = filepath.Join(cwd, path)
			}
		}
		files, ferr := openFilesOf(ctx, pid)
		found = append(found, traceSampler{pid: pid, path: path, files: files, filesErr: ferr})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].pid < found[j].pid })
	return found
}

// isTraceSamplerArgv reports whether argv is a governor running `trace sample`. The binary's
// name is matched as isTraceCommand matches it, so a release binary named
// governor-darwin-arm64 still counts.
func isTraceSamplerArgv(argv []string) bool {
	if len(argv) < 3 || !strings.Contains(strings.ToLower(filepath.Base(argv[0])), "governor") {
		return false
	}
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "trace" && argv[i+1] == "sample" {
			return true
		}
		if !strings.HasPrefix(argv[i], "-") {
			return false
		}
	}
	return false
}

// traceHookBinaries reads settings.json and returns the binary each governor trace hook runs,
// mapped to the events it is registered on. It never writes.
func traceHookBinaries() (map[string][]string, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	bins := map[string][]string{}
	for event, entries := range settings.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				if !isTraceCommand(h.Command) {
					continue
				}
				bin := traceHookBinary(h.Command)
				bins[bin] = append(bins[bin], event)
			}
		}
	}
	for _, evs := range bins {
		sort.Strings(evs)
	}
	return bins, nil
}

// traceHookBinary is the binary half of a command isTraceCommand accepted.
func traceHookBinary(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, suffix := range traceHookSuffixes {
		if strings.HasSuffix(cmd, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(cmd, suffix))
		}
	}
	return cmd
}

// sameBuild reports whether two binaries are the same build: the same file, or files with
// identical contents. Content rather than `version` output, because every development build
// reports 0.0.0-dev, and two of those are exactly the case that went unseen. A binary that
// cannot be read is not the same build as anything.
func sameBuild(a, b string) bool {
	if sameFile(a, b) {
		return true
	}
	ha, ok := fileDigest(a)
	if !ok {
		return false
	}
	hb, ok := fileDigest(b)
	return ok && ha == hb
}

func fileDigest(path string) ([sha256.Size]byte, bool) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, false
	}
	copy(sum[:], h.Sum(nil))
	return sum, true
}

// logDescriptors returns the descriptors on which s holds a trace log file of THIS install:
// a samples.jsonl* in the live log's directory. The directory is compared as a file, not as a
// string, because /tmp and /private/tmp name one directory on darwin.
//
// SCOPED TO THIS STATE DIRECTORY ON PURPOSE. A sampler writing some other GOVERNOR_STATE_DIR's
// log is another install's business, and comparing its inode with this install's live log
// would call a perfectly healthy file "replaced". That was the first draft's bug, caught by
// the install tests, whose doctor runs as a subprocess against a temporary state directory on
// a machine whose real sampler writes ~/.claude/governor.
func logDescriptors(s traceSampler, live string) []openFile {
	dir, base := filepath.Dir(live), filepath.Base(live)
	var out []openFile
	for _, f := range s.files {
		if strings.HasPrefix(filepath.Base(f.name), base) && sameFile(filepath.Dir(f.name), dir) {
			out = append(out, f)
		}
	}
	return out
}

// deadLogDescriptors returns those of logs that are no longer the live samples.jsonl:
// unlinked (link count 0), or a file whose inode is not the live path's. Measured on darwin, a
// descriptor reports its file's CURRENT name, so a rotated-away log shows as samples.jsonl.1
// and an unlinked one keeps its last name; the inode is what decides.
func deadLogDescriptors(logs []openFile, live string) []openFile {
	var liveIno uint64
	if fi, err := os.Stat(live); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			liveIno = uint64(st.Ino)
		}
	}
	var dead []openFile
	for _, f := range logs {
		if f.nlink == 0 || liveIno == 0 || f.ino != liveIno {
			dead = append(dead, f)
		}
	}
	return dead
}

// checkStaleTrace reports Phase 0 trace hooks and samplers that are not the build the governor
// hooks run, and any sampler writing to a log that is no longer on disk.
func (d *diagnosis) checkStaleTrace() {
	const name = "trace"

	hookBins, herr := traceHookBinaries()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	samplers := scanForSamplers(ctx)

	if herr != nil && len(samplers) == 0 {
		d.add(name, warn, "could not read the trace hooks in settings.json: "+herr.Error(), "")
		return
	}
	hooked, haveHooked := d.hookedBinary()
	live, _ := trace.LogPath()

	var staleHooks []string
	if haveHooked {
		for bin, events := range hookBins {
			if !sameBuild(bin, hooked) {
				staleHooks = append(staleHooks, fmt.Sprintf("%s (%s)", short(bin), strings.Join(events, ", ")))
			}
		}
		sort.Strings(staleHooks)
	}

	var deadLines, staleSamplers, uninspected []string
	var killPIDs []string
	ours := 0
	for _, s := range samplers {
		logs := logDescriptors(s, live)
		if s.filesErr == nil && len(logs) == 0 {
			continue // writes no log of this install; see logDescriptors
		}
		ours++
		dead := deadLogDescriptors(logs, live)
		if len(dead) > 0 {
			for _, f := range dead {
				why := "replaced"
				if f.nlink == 0 {
					why = "deleted"
				}
				deadLines = append(deadLines, fmt.Sprintf("sampler pid %d writes to a %s %s on fd %s",
					s.pid, why, filepath.Base(f.name), f.fd))
			}
			killPIDs = append(killPIDs, fmt.Sprint(s.pid))
			continue
		}
		if s.filesErr != nil {
			uninspected = append(uninspected, fmt.Sprintf("pid %d (%v)", s.pid, s.filesErr))
		}
		if haveHooked && !sameBuild(s.path, hooked) {
			staleSamplers = append(staleSamplers, fmt.Sprintf("sampler pid %d runs %s", s.pid, short(s.path)))
			killPIDs = append(killPIDs, fmt.Sprint(s.pid))
		}
	}

	if len(hookBins) == 0 && ours == 0 {
		d.add(name, pass, "Phase 0 trace is not installed", "")
		return
	}

	// One fix line, in the order a person runs it: point the hooks at the current build (or
	// take them out), then stop the sampler the old build started. `trace install` re-points
	// rather than adds since GOV-B42, so it is a complete way out on its own.
	var fixes []string
	if len(staleHooks) > 0 {
		fixes = append(fixes, fmt.Sprintf("either %s or %s", governorCommand("trace install"),
			governorCommand("trace uninstall")))
	}
	if len(killPIDs) > 0 {
		fixes = append(fixes, "then kill "+strings.Join(killPIDs, " ")+
			"; the next session's trace ensure starts the current build's sampler")
	}
	fix := strings.Join(fixes, ", ")

	switch {
	case len(deadLines) > 0:
		detail := strings.Join(deadLines, "; ") + ": its samples are lost and the file still takes disk"
		if len(staleHooks) > 0 {
			detail += "; trace hooks run another build: " + strings.Join(staleHooks, ", ")
		}
		if fix == "" {
			fix = "kill " + strings.Join(killPIDs, " ")
		}
		d.add(name, fail, detail, fix)
	case len(staleHooks) > 0 || len(staleSamplers) > 0:
		var parts []string
		if len(staleHooks) > 0 {
			parts = append(parts, "trace hooks run a different build from the governor hooks: "+
				strings.Join(staleHooks, ", "))
		}
		parts = append(parts, staleSamplers...)
		d.add(name, warn, strings.Join(parts, "; ")+"; an older sampler stops logging when the log rotates", fix)
	case len(uninspected) > 0:
		d.add(name, warn, "could not inspect the open files of sampler "+strings.Join(uninspected, ", "), "")
	default:
		d.add(name, pass, fmt.Sprintf("trace hooks and %d sampler(s) run the hooked build and write the live log",
			ours), "")
	}
}
