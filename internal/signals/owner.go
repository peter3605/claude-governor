package signals

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// maxAncestorHops bounds the walk from a hook process up to its session.
//
// The hook is a direct child of the session, or a child of one shell the harness used to
// run the command; anything past a handful of hops means the assumption is wrong, and
// walking to pid 1 would then attribute the lease to launchd or systemd — a process that
// never dies, so the lease would never be reaped for liveness. Stopping short and reporting
// "unknown" is the safe failure, per the Liveness contract.
const maxAncestorHops = 8

// procInfo is what the owner walk needs to know about one process.
type procInfo struct {
	ppid    int32
	name    string // argv[0]'s base name, falling back to the executable name
	started time.Time
}

// OwningSession finds the Claude Code session that this hook process belongs to, and
// returns its PID and start time.
//
// This is how a lease learns its owner. design.md section 4.3 reaps a confirmed lease when
// its owning process is gone, and docs/hook-surface.md section 5 records why the PID cannot
// simply be read off the payload: neither SessionStart nor SessionEnd carries one, so the
// owner has to be found from the hook process's own ancestry.
//
// It walks up rather than taking os.Getppid() directly, and that is the whole point of the
// function. Claude Code may run a hook through a shell, in which case the immediate parent
// is a process that exits the instant the hook does. Binding a lease to it would have
// PIDLiveness report the owner dead on the very next reaper tick, two seconds later, and
// every confirmed lease on the machine would be collected while its agent was still
// running — which over-subscribes the budget silently and looks exactly like the governor
// doing nothing. The session process is the one whose lifetime actually matches the
// lease's, so the walk continues until it finds one.
//
// The match is argv[0], not the executable name, for the reason recorded on
// ClaudeProcessName: Claude Code runs from a version-numbered file and sets argv[0] to
// "claude", so keying on the executable finds nothing on a machine running six sessions.
//
// ok is false when no such ancestor is found. Callers MUST treat that as "unknown owner"
// and send no PID at all rather than substituting the parent: an unknown owner leaves the
// lease to its TTL, while a wrong owner gets it reaped out from under a live agent. Those
// two mistakes are not the same size.
func OwningSession(ctx context.Context) (pid int, started time.Time, ok bool) {
	return walkToOwner(int32(os.Getpid()), ClaudeProcessName, func(p int32) (procInfo, bool) {
		return liveProcInfo(ctx, p)
	})
}

// walkToOwner climbs the ancestry from self and returns the first process named want.
//
// It is separated from the process table it usually reads because the behaviour worth
// testing — skipping an intervening shell, refusing to walk into pid 1, giving up rather
// than guessing — cannot be produced on demand from the real one. GOV-02 learnt this the
// expensive way with CPU attribution: a test against a live process tree only ever tests
// the platform it happens to run on, and the bug it missed was on the other one.
func walkToOwner(self int32, want string, lookup func(int32) (procInfo, bool)) (int, time.Time, bool) {
	cur := self
	for hop := 0; hop < maxAncestorHops; hop++ {
		info, ok := lookup(cur)
		if !ok {
			return 0, time.Time{}, false
		}
		// pid 1 is the init system. It is an ancestor of everything and it never exits, so
		// a lease bound to it is a lease the reaper can never collect for liveness.
		if info.ppid <= 1 {
			return 0, time.Time{}, false
		}
		parent, ok := lookup(info.ppid)
		if !ok {
			return 0, time.Time{}, false
		}
		if parent.name == want {
			return int(info.ppid), parent.started, true
		}
		cur = info.ppid
	}
	return 0, time.Time{}, false
}

// liveProcInfo reads one process from the real process table.
func liveProcInfo(ctx context.Context, pid int32) (procInfo, bool) {
	p, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return procInfo{}, false
	}
	ppid, err := p.PpidWithContext(ctx)
	if err != nil {
		return procInfo{}, false
	}
	info := procInfo{ppid: ppid, name: processName(ctx, p)}
	if ms, err := p.CreateTimeWithContext(ctx); err == nil {
		info.started = time.UnixMilli(ms)
	}
	// A zero start time is left as it is rather than treated as a failure: the process is
	// there and named, and PIDLiveness reads a zero start time as "the PID exists and there
	// is nothing to check it against", which is exactly the right reading.
	return info, true
}

// processName is argv[0]'s base name, falling back to the executable's own name.
//
// The order matters and is the argv[0] rule from ClaudeProcessName, measured in GOV-02:
// Claude Code runs /Users/<u>/.local/share/claude/versions/2.1.263 with argv[0] set to
// "claude", so the executable name says "2.1.263" and only argv[0] says what this is.
func processName(ctx context.Context, p *process.Process) string {
	if argv, err := p.CmdlineSliceWithContext(ctx); err == nil && len(argv) > 0 && argv[0] != "" {
		return filepath.Base(argv[0])
	}
	if name, err := p.NameWithContext(ctx); err == nil {
		return name
	}
	return ""
}
