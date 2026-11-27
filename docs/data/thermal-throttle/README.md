# Thermal throttle measurements (GOV-B54, GOV-B60, GOV-B61)

Analysed in `docs/design.md` §5.3. M1 MacBook Air (MacBookAir10,1), macOS 25E253, 2026-11-27.

| File | What |
|---|---|
| `fair-excursions.txt` | Every non-nominal `thermalState` excursion in the Sep 8 – Nov 1 governor trace |
| `run-A-saturate.*` | 8× FP spin, 06:10:22Z – 06:40:22Z |
| `run-B-fanout.*` | 4 workers looping cold `go build` + `go test`, 06:53:04Z – 07:24:19Z |
| `run-A2-saturate.*` | 8× FP spin with `tools/clockprobe` alongside, 07:51:08Z – 08:21:08Z |
| `probe-A2.jsonl` | clockprobe's output for run A2 (calibration record, then one reading a second) |
| `run-A3-saturate.*` | 8× FP spin with `tools/clusterwatch` alongside, 20:46:27Z – 21:16:27Z (GOV-B60) |
| `run-C-clockcut/` | GOV-B61's live check of the clock cut: this build's daemon (run-queue cut disabled) through 5 min idle, 12 min 8× FP, 3 min recovery — trace samples (schema v5, with the P-cluster clock), the daemon's budget lines, and `governor status` at each phase |
| `watch-A3.jsonl` | clusterwatch's output for run A3 (IOReport P-cluster clock and IOHID P-cluster temperature, one reading a second) |

`*.tsv` is one row per `powermetrics` second joined to the governor's nearest 1 Hz sample
(`throttlecheck -tsv`); `*.summary.txt` is `throttlecheck`'s verdict. The raw `powermetrics`
captures (~24 MB each) are not committed. `powermetrics` was run by hand under `sudo` for this
measurement only; the product never runs it.
