// Command probetrace runs the calibration load probe repeatedly and records every repetition.
//
// GOV-47: seven `governor calibrate` runs on one machine reported effective cores from 4.75 to
// 5.78, every one of them "5 of 5 repetitions usable", and the only numbers that survived were
// the summaries each printed. A bound on the probe's own disagreement has to be set from the
// repetitions themselves, not from a max and a range, so this keeps them.
//
// Each trial is exactly what calibrate does — the ambient read, then Probe with the shipped
// options — followed by a pause so successive trials are not measuring each other's heat. One
// JSON line per trial goes to stdout; nothing is written anywhere else.
//
//	go run ./tools/probetrace -trials 20 -gap 20s > trials.jsonl
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/peter3605/claude-governor/internal/calibrate"
)

type trial struct {
	At         time.Time             `json:"at"`
	LoadAvg1   float64               `json:"loadavg1"`
	AmbientRQ  float64               `json:"ambient_rq"`
	OK         bool                  `json:"ok"`
	Reason     string                `json:"reason,omitempty"`
	Effective  float64               `json:"effective"`
	Spread     float64               `json:"spread"`
	UsableReps int                   `json:"usable_reps"`
	Disagree   float64               `json:"disagreement"`
	Attempts   int                   `json:"attempts"`
	Samples    []calibrate.RepSample `json:"samples"`
}

func main() {
	trials := flag.Int("trials", 10, "how many calibrate-shaped probes to run")
	gap := flag.Duration("gap", 20*time.Second, "pause between trials")
	flag.Parse()

	cores := runtime.NumCPU()
	enc := json.NewEncoder(os.Stdout)
	for i := 0; i < *trials; i++ {
		if i > 0 {
			time.Sleep(*gap)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		la, rq, _ := calibrate.Ambient(ctx, float64(cores), calibrate.Options{})
		p := calibrate.Probe(ctx, cores, rq, calibrate.Options{})
		cancel()
		if err := enc.Encode(trial{
			At: time.Now(), LoadAvg1: la, AmbientRQ: rq, OK: p.OK, Reason: p.Reason,
			Effective: p.EffectiveCores, Spread: p.Spread, UsableReps: p.UsableReps, Disagree: p.Disagreement, Attempts: p.Attempts,
			Samples: p.Samples,
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
