# Saturation Sweep — Design Spec
**Date:** 2026-04-16  
**Status:** Approved

---

## Overview

A Python script that sweeps RPM (requests per minute) from 2 upward in steps of 2,
calling the server-sim API for each rate, until the evaluator reports saturation.
The sweep is run for three evaluator cases:

| Case | Evaluator | Latency backend |
|------|-----------|-----------------|
| `queue-analysis` | queue-analysis-evaluator | Markovian analytical model |
| `blis-roofline` | blis-evaluator | roofline |
| `blis-trained-physics` | blis-evaluator | trained-physics |

**Workload (same for all cases):** `granite-3.1-8b-instruct` on `H100`, avgInputTokens=2048,
avgOutputTokens=1024, maxConcurrency=64.

The maxConcurrency=64 choice was deliberate: at 256 concurrency × 3072 tokens/req the BLIS
pre-sim KV check fires on every call (786K tokens >> 372K KV slots), making the sweep
trivial. At 64 concurrency (196K tokens < 365K threshold) the KV check passes and the
binding bottleneck becomes decode bandwidth, which fires near RPM≈12.

Queue-analysis perfParms from run5: α=8.0 ms, β=0.016 ms/tok, γ=0.0005 ms/tok².

---

## Repository layout

```
scripts/
  sweep_saturation.py          # main sweep script
  sweep_results/               # gitignored at runtime; documented results stored here
    sweep_YYYYMMDD_HHMMSS.csv  # raw sweep data (all cases)
    sweep_YYYYMMDD_HHMMSS.md   # human-readable summary report
queue-analysis-evaluator/
  model-data.json              # NEW: granite_8b / H100 with perfParms
blis-evaluator/
  hardware_config.json         # NEW: copy of inference-sim@v0.7.4 hardware_config.json
docs/
  sweep-results/               # committed run reports (named by date/run label)
```

---

## Config files

### `queue-analysis-evaluator/model-data.json`

```json
{
  "models": [
    {
      "name": "granite_8b",
      "acc": "H100",
      "maxBatchSize": 256,
      "atTokens": 512,
      "perfParms": {
        "alpha": 8.0,
        "beta": 0.016,
        "gamma": 0.0005
      }
    }
  ]
}
```

Key: `"H100|granite_8b"` (matches `acc + "|" + name` in config.go).

### `blis-evaluator/hardware_config.json`

Copied verbatim from `inference-sim@v0.7.4/hardware_config.json`. Contains entries for
`H100`, `A100-SXM`, `A100-80`, `L40S`. The blis handler defaults to this filename when
`HW_CONFIG_FILE` is unset.

---

## Script design (`scripts/sweep_saturation.py`)

### Build phase (once, before all cases)

```
go build -o /tmp/serversim-sweep/server-sim    ./cmd/server-sim
go build -o /tmp/serversim-sweep/qa-eval       .         (cwd: queue-analysis-evaluator/)
go build -o /tmp/serversim-sweep/blis-eval     .         (cwd: blis-evaluator/)
```

### Per-case lifecycle

```
start evaluator  (port 8081, evaluator subdir as cwd)
start server-sim (port 8080, EVALUATOR_URL=http://localhost:8081)
health-check loop (both /health, up to 30 s)
RPM sweep
SIGTERM both processes, wait for exit
```

### Environment variables per case

| Case | Evaluator binary | Extra env vars |
|------|-----------------|----------------|
| `queue-analysis` | `qa-eval` | `MODEL_DATA_FILE=model-data.json` |
| `blis-roofline` | `blis-eval` | `BLIS_CONFIG_FILE=blis-config.json`, `HW_CONFIG_FILE=hardware_config.json`, `LATENCY_BACKEND=roofline` |
| `blis-trained-physics` | `blis-eval` | same + `LATENCY_BACKEND=trained-physics` |

All evaluators: `EVALUATOR_PORT=8081`.  
server-sim: `EVALUATOR_URL=http://localhost:8081`, `SERVERSIM_PORT=8080`.

### ProblemData per call

| Field | Value |
|-------|-------|
| `RPS` | `rpm / 60.0` |
| `maxConcurrency` | `64` |
| `avgInputTokens` | `2048` |
| `avgOutputTokens` | `1024` |
| `accelerator` | `"H100"` |
| `model` | `"granite_8b"` (qa) / `"ibm-granite/granite-3.1-8b-instruct"` (blis) |

### Sweep loop

```python
for rpm in range(2, 202, 2):       # 2, 4, 6, ... up to 200 safety cap
    rps = rpm / 60.0
    job_id = post_simulate(problem_data)
    result  = poll_job(job_id,
                       poll_interval = 5   if case == "queue-analysis" else 30,  # seconds
                       timeout       = 600)                                       # 10 min
    print_table_row(rpm, rps, result)
    append_to_results(result)
    if result["saturation"] != "":
        break
```

### Stdout format (per case)

```
=== queue-analysis: granite-3.1-8b / H100  in=2048 out=1024 conc=64 ===
 RPM     RPS   Throughput  RespTime  WaitTime    TTFT     ITL   MaxRPS  Saturation
   2  0.0333      0.0333    1234.5      10.2    200.1    15.3     0.20
   4  0.0667      0.0667    1278.3      12.1    210.5    15.5     0.20
  ...
  12  0.2000         ---       ---       ---      ---     ---     0.20  bandwidth
```
`---` indicates that the evaluator returned zero/omitted metrics (pre-sim saturation path).

### Output files

Both written to `scripts/sweep_results/` with a shared timestamp prefix:

- **CSV** `sweep_YYYYMMDD_HHMMSS.csv`: one row per RPM step per case.  
  Columns: `case, rpm, rps, throughput, avg_resp_ms, avg_wait_ms, ttft_ms, itl_ms, max_rps, saturation`

- **Markdown report** `sweep_YYYYMMDD_HHMMSS.md`: one table per case plus a summary
  section noting the saturation RPM and reason for each case.

After a successful run, the operator copies/renames these files into `docs/sweep-results/`
for permanent record-keeping.

---

## Expected saturation points

| Case | Binding bottleneck | Approx. saturation RPM |
|------|--------------------|------------------------|
| queue-analysis | Markovian queue overload | TBD by run |
| blis-roofline | Decode bandwidth (pre-sim) | ≈12 RPM |
| blis-trained-physics | Decode bandwidth (pre-sim) | ≈12 RPM |

Note: both BLIS cases share the same pre-sim `checkSaturation` logic (backend-independent).
They will differ only in the throughput/latency metrics for the DES runs below saturation.

---

## Error handling

- Build failure → print error and exit.
- Evaluator/server-sim fails to become healthy in 30 s → kill processes, print error, skip case.
- `poll_job` timeout (>600 s) → record `status=timeout`, continue to next RPM.
- Job `status=failed` → record error string, continue to next RPM.
- KeyboardInterrupt → SIGTERM all live processes, flush CSV/report, exit.

---

## Files changed

| File | Action |
|------|--------|
| `scripts/sweep_saturation.py` | Create |
| `queue-analysis-evaluator/model-data.json` | Create |
| `blis-evaluator/hardware_config.json` | Create |
| `docs/sweep-results/` | Create directory |
| `scripts/sweep_results/` | Create directory (gitignore transient output) |
