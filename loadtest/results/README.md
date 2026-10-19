# Load test results

Commit one folder per run, for example `2026-10-25-hit/`, containing:

- `summary.json` from `k6 run --summary-export=summary.json ...`
- `metrics.txt`, a copy of `curl -s localhost:8080/metrics | grep llmc_` taken right after the run
- `notes.md` with: machine (CPU, cores, RAM, OS), Go version (`go version`), k6 version,
  fakeupstream latency, the exact command, and anything unusual

Only copy numbers into the main README's Results table from these files.
