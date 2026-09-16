package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/peter3605/claude-governor/internal/calibrate"
	"github.com/peter3605/claude-governor/internal/config"
)

// calibrateBudget bounds the whole command.
//
// The probe is bounded by construction and the ambient read is a second, so this is a backstop
// rather than the mechanism — it is here because `calibrate` runs at the end of `install`, and
// an install that hangs on a reading is an install that gets killed halfway through. Every
// stage below degrades to "not measured" on a cancelled context rather than failing, so the
// deadline expiring costs precision and never the command.
const calibrateBudget = 30 * time.Second

// runCalibrate measures this host and writes its constants into the config.
//
// WHY THE COMMAND EXISTS AT ALL, since every number it writes has a compiled-in default that
// works: because those defaults were measured on one 8-core fanless laptop, and the project
// rule is that nothing tuned on Peter's machine ships as anyone else's default. What this
// writes is not a better guess — it is the same derivation applied to the machine it is
// actually running on.
//
// IT WRITES TWO NUMBERS AND A NOTE. The run-queue cut threshold, which after GOV-32 is the
// only control input the loop has; B_max, capped to what the cores here actually deliver; and
// the provenance line that says when this host was measured and from what. It deliberately
// does NOT write a setpoint or an EWMA window — both would be numbers no code reads, and the
// reasoning for each is recorded in internal/calibrate's package comment.
func runCalibrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("governor calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "measure and print, but write nothing")
	noProbe := fs.Bool("no-probe", false, "skip the load probe; B_max falls back to the core count")
	quiet := fs.Bool("quiet", false, "print only what changed")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "governor calibrate: %v\n", err)
		return 1
	}
	store := &config.FileStore{Path: path, Warn: func(s string) { fmt.Fprintln(stderr, s) }}

	ctx, cancel := context.WithTimeout(context.Background(), calibrateBudget)
	defer cancel()

	// READ FIRST, AND FAIL ON AN UNREADABLE CONFIG RATHER THAN OVERWRITING IT. The store
	// refuses a schema from the future and a file with no schema key at all, and both of
	// those refusals exist so that this command cannot save a truncated version of someone
	// else's settings over the original (design.md §13.4). Honouring them means not
	// proceeding to Save.
	before, err := store.Load(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "governor calibrate: %v\n", err)
		fmt.Fprintln(stderr, "Refusing to write over it. Move that file aside and run this again.")
		return 1
	}

	opts := calibrate.Options{NoProbe: *noProbe}
	if !*quiet {
		opts.Announce = func(s string) { fmt.Fprintf(stdout, "%s\n", s) }
	}

	m, err := calibrate.Measure(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "governor calibrate: %v\n", err)
		return 1
	}
	d := calibrate.Derive(m)

	if !*quiet {
		fmt.Fprint(stdout, calibrate.Report(m, d, calibrate.Derived{
			RunQueueMax: before.RunQueueMax,
			BMax:        before.BMax,
		}))
	}

	if *dryRun {
		fmt.Fprintf(stdout, "\nnothing written (--dry-run). Would write %s\n", path)
		return 0
	}

	// Everything not calibrated is carried through unchanged, Unknown keys included. This is
	// a merge into the user's config, not a replacement of it: `calibrate` owns three fields
	// and has no business resetting a hand-edited endpoint or a wait timeout.
	after := before
	after.RunQueueMax = d.RunQueueMax
	after.BMax = d.BMax
	after.CalibratedAt = m.At.UTC().Format(time.RFC3339)
	after.CalibratedFrom = calibrate.Provenance(m)

	if err := store.Save(ctx, after); err != nil {
		fmt.Fprintf(stderr, "governor calibrate: %v\n", err)
		return 1
	}
	if *quiet {
		fmt.Fprintf(stdout, "calibrated: run-queue cut %.2f per core, b_max %d slots\n", d.RunQueueMax, d.BMax)
	} else {
		fmt.Fprintf(stdout, "\nwritten to %s\n", path)
	}
	return 0
}
