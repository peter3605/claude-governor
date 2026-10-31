# docs/

Every file in this directory is exactly one of three kinds:

- **spec**: the contract. Code is written from it, and where anything else disagrees with it,
  it wins.
- **guide**: explains the spec to someone using the governor. It never restates a number the
  spec holds; it links to it.
- **measurement record**: the evidence behind a decision, written down when the measurement was
  made. Read it for why a constant is what it is. Do not edit its conclusions after the fact.

New here? Start with [guide.md](guide.md), then [how-it-works.md](how-it-works.md).

`TestEveryDocsFileIsIndexed` fails when a file is added here without a line below.

- [README.md](README.md) — guide: this index
- [guide.md](guide.md) — guide: using the governor day to day — the status line, refusals, overrides, troubleshooting, uninstall
- [how-it-works.md](how-it-works.md) — guide: a ten-minute tour of the design, linking each claim to design.md
- [design.md](design.md) — spec: the design, its invariants, wire protocol, and install surface
- [hook-surface.md](hook-surface.md) — measurement record: the Claude Code hook payloads and behaviours, verbatim, pinned to the versions they were measured on
- [phase-0-findings.md](phase-0-findings.md) — measurement record: why the budget counts builds rather than agents
- [bash-corpus.md](bash-corpus.md) — measurement record: which `Bash` commands are build-shaped, and the classifier's measured ceiling
- [dogfooding.md](dogfooding.md) — measurement record: whether a fixed budget of four helped, method fixed before the result
- [control-proof.md](control-proof.md) — measurement record: whether the control loop controls a real build fan-out
- [run-queue-sweep.md](run-queue-sweep.md) — measurement record: sweeping the run-queue cut threshold (GOV-31)
- [climb-condition.md](climb-condition.md) — measurement record: why the budget could not climb, and the move from utilisation to run queue (GOV-32)
- [calibration.md](calibration.md) — measurement record: the re-sweep behind `governor calibrate`'s headroom (GOV-25)
- [advice.md](advice.md) — measurement record: whether the standing advice to ask before building measurably helps (GOV-17)
- [data/](data/) — measurement record: raw traces and logs the run-queue, climb-condition and calibration records were drawn from
