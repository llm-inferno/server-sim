# Saturation Sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a Python sweep script that varies RPM from 2→… in steps of 2 for granite-3.1-8b on H100 (maxConcurrency=64, 2048/1024 tokens), testing all three evaluator backends (queue-analysis, blis-roofline, blis-trained-physics) until saturation, and saves table output + CSV + Markdown report.

**Architecture:** The script builds three Go binaries once, then for each of the three cases starts the evaluator + server-sim, runs the RPM sweep via the async `/simulate` API (POST → poll GET), and tears processes down between cases. Config files for queue-analysis and blis are committed alongside the script.

**Tech Stack:** Python 3 stdlib only (subprocess, urllib, json, csv, socket, signal). Go build toolchain. Gin-based server-sim and evaluator binaries.

---

## File map

| File | Action | Responsibility |
|------|--------|---------------|
| `queue-analysis-evaluator/model-data.json` | Create | granite_8b / H100 perfParms for queue-analysis evaluator |
| `blis-evaluator/hardware_config.json` | Create | H100/A100 hardware calibration data (from inference-sim@v0.7.4) |
| `scripts/sweep_saturation.py` | Create | Full sweep script: build, process management, HTTP, sweep loop, output |
| `scripts/sweep_results/.gitkeep` | Create | Placeholder so the output directory is tracked but results are gitignored |
| `docs/sweep-results/` | Create | Committed run reports after actual sweep runs |
| `.gitignore` | Modify | Ignore `scripts/sweep_results/*.csv` and `scripts/sweep_results/*.md` |

---

### Task 1: Create evaluator config files and directory structure

**Files:**
- Create: `queue-analysis-evaluator/model-data.json`
- Create: `blis-evaluator/hardware_config.json`
- Create: `scripts/sweep_results/.gitkeep`
- Create: `docs/sweep-results/.gitkeep`
- Modify: `.gitignore`

- [ ] **Step 1.1: Create `queue-analysis-evaluator/model-data.json`**

  This file maps `acc|name` keys to perfParms. The queue-analysis evaluator reads it
  via `MODEL_DATA_FILE` env var (default `model-data.json`, resolved from CWD).
  The key format is `acc + "|" + name`, so `H100|granite_8b`.

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

- [ ] **Step 1.2: Create `blis-evaluator/hardware_config.json`**

  Copy verbatim from the inference-sim@v0.7.4 module. The blis handler calls
  `latency.GetHWConfig(hwConfigFile, entry.GPU)` which looks up `"H100"` in this file.

  ```json
  {
    "H100": {
      "_comment":   "MFU values calibrated per Discussion #589: https://github.com/inference-sim/inference-sim/discussions/589",
      "TFlopsPeak":  989.5,
      "TFlopsFP8":   1979.0,
      "BwPeakTBs":   3.35,
      "mfuPrefill":  0.45,
      "mfuDecode":   0.30,
      "MemoryGiB":   80.0
    },
    "A100-SXM": {
      "_comment":   "MFU values calibrated per Discussion #589: https://github.com/inference-sim/inference-sim/discussions/589",
      "TFlopsPeak":  312,
      "TFlopsFP8":   0,
      "BwPeakTBs":   2.039,
      "mfuPrefill":  0.38,
      "mfuDecode":   0.18,
      "MemoryGiB":   80.0
    },
    "A100-80": {
      "_comment":   "Alias for A100-SXM — defaults.yaml uses A100-80 for all A100 model entries (BC-14). MFU values calibrated per Discussion #589: https://github.com/inference-sim/inference-sim/discussions/589",
      "TFlopsPeak":  312,
      "TFlopsFP8":   0,
      "BwPeakTBs":   2.039,
      "mfuPrefill":  0.38,
      "mfuDecode":   0.18,
      "MemoryGiB":   80.0
    },
    "L40S": {
      "_comment":   "MFU values calibrated per Discussion #589: https://github.com/inference-sim/inference-sim/discussions/589",
      "TFlopsPeak":  362.05,
      "TFlopsFP8":   1466.0,
      "BwPeakTBs":   0.864,
      "mfuPrefill":  0.32,
      "mfuDecode":   0.08,
      "MemoryGiB":   48.0
    }
  }
  ```

- [ ] **Step 1.3: Create output directories and `.gitkeep` files**

  ```bash
  mkdir -p scripts/sweep_results docs/sweep-results
  touch scripts/sweep_results/.gitkeep docs/sweep-results/.gitkeep
  ```

- [ ] **Step 1.4: Add gitignore rules for transient sweep output**

  Append to `.gitignore` (create it if absent):

  ```
  # Saturation sweep: transient per-run output (commit selectively to docs/sweep-results/)
  scripts/sweep_results/*.csv
  scripts/sweep_results/*.md
  ```

- [ ] **Step 1.5: Verify the queue-analysis evaluator loads the new config**

  ```bash
  cd queue-analysis-evaluator
  MODEL_DATA_FILE=model-data.json go run . &
  sleep 3
  curl -s -X POST http://localhost:8081/solve \
    -H "Content-Type: application/json" \
    -d '{"RPS":0.1,"maxConcurrency":64,"avgInputTokens":2048,"avgOutputTokens":1024,"accelerator":"H100","model":"granite_8b"}'
  kill %1
  ```

  Expected: JSON response with `throughput`, `avgITL`, etc. — no `"error"` key.
  If you see `"unknown accelerator/model combination"`, re-check the `acc` and `name`
  fields in model-data.json.

- [ ] **Step 1.6: Commit config files**

  ```bash
  git add queue-analysis-evaluator/model-data.json \
          blis-evaluator/hardware_config.json \
          scripts/sweep_results/.gitkeep \
          docs/sweep-results/.gitkeep \
          .gitignore
  git commit -m "feat(sweep): add evaluator configs and output directories for saturation sweep"
  ```

---

### Task 2: Write the sweep script

**Files:**
- Create: `scripts/sweep_saturation.py`

- [ ] **Step 2.1: Create `scripts/sweep_saturation.py` with the complete implementation**

  ```python
  #!/usr/bin/env python3
  """sweep_saturation.py — RPM sweep for saturation detection across evaluator backends.

  Workload: granite-3.1-8b-instruct / H100
            avgInputTokens=2048, avgOutputTokens=1024, maxConcurrency=64

  Cases (run sequentially):
    queue-analysis     — Markovian analytical model
    blis-roofline      — BLIS DES, roofline latency backend
    blis-trained-physics — BLIS DES, trained-physics latency backend

  Usage:
    python3 scripts/sweep_saturation.py [case ...]

  Arguments:
    case  One or more of: queue-analysis, blis-roofline, blis-trained-physics.
          Defaults to all three if omitted.

  Output:
    scripts/sweep_results/sweep_YYYYMMDD_HHMMSS.csv
    scripts/sweep_results/sweep_YYYYMMDD_HHMMSS.md
  """

  import csv
  import json
  import os
  import signal
  import socket
  import subprocess
  import sys
  import time
  import urllib.error
  import urllib.request
  from datetime import datetime
  from pathlib import Path

  # ---------------------------------------------------------------------------
  # Configuration
  # ---------------------------------------------------------------------------

  REPO_ROOT = Path(__file__).parent.parent.resolve()
  BUILD_DIR = Path("/tmp/serversim-sweep")

  SIM_PORT  = 8080
  EVAL_PORT = 8081
  SIM_URL   = f"http://localhost:{SIM_PORT}"
  EVAL_URL  = f"http://localhost:{EVAL_PORT}"

  MAX_RPM   = 200
  RPM_STEP  = 2

  CASES = [
      {
          "name":          "queue-analysis",
          "bin":           "qa-eval",
          "dir":           "queue-analysis-evaluator",
          "model":         "granite_8b",
          "env":           {"MODEL_DATA_FILE": "model-data.json"},
          "poll_interval": 5,
      },
      {
          "name":          "blis-roofline",
          "bin":           "blis-eval",
          "dir":           "blis-evaluator",
          "model":         "ibm-granite/granite-3.1-8b-instruct",
          "env":           {
              "BLIS_CONFIG_FILE": "blis-config.json",
              "HW_CONFIG_FILE":   "hardware_config.json",
              "LATENCY_BACKEND":  "roofline",
          },
          "poll_interval": 30,
      },
      {
          "name":          "blis-trained-physics",
          "bin":           "blis-eval",
          "dir":           "blis-evaluator",
          "model":         "ibm-granite/granite-3.1-8b-instruct",
          "env":           {
              "BLIS_CONFIG_FILE": "blis-config.json",
              "HW_CONFIG_FILE":   "hardware_config.json",
              "LATENCY_BACKEND":  "trained-physics",
          },
          "poll_interval": 30,
      },
  ]

  # ---------------------------------------------------------------------------
  # Build
  # ---------------------------------------------------------------------------

  def build_binaries():
      """Build all three binaries into BUILD_DIR. Exits on failure."""
      BUILD_DIR.mkdir(parents=True, exist_ok=True)
      targets = [
          (REPO_ROOT,                             ["go", "build", "-o", str(BUILD_DIR / "server-sim"), "./cmd/server-sim"]),
          (REPO_ROOT / "queue-analysis-evaluator", ["go", "build", "-o", str(BUILD_DIR / "qa-eval"),    "."]),
          (REPO_ROOT / "blis-evaluator",           ["go", "build", "-o", str(BUILD_DIR / "blis-eval"),  "."]),
      ]
      for cwd, cmd in targets:
          label = cmd[-1] if not cmd[-1].startswith("./") else cmd[-2]
          print(f"  building {label} …", flush=True)
          r = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
          if r.returncode != 0:
              print(f"BUILD FAILED ({' '.join(cmd)}):\n{r.stderr}", file=sys.stderr)
              sys.exit(1)

  # ---------------------------------------------------------------------------
  # HTTP helpers
  # ---------------------------------------------------------------------------

  def http_get(url, timeout=5):
      """GET url; return parsed JSON or None on any error."""
      try:
          with urllib.request.urlopen(url, timeout=timeout) as r:
              return json.loads(r.read())
      except Exception:
          return None

  def http_post(url, payload, timeout=15):
      """POST JSON payload to url; return parsed JSON. Raises on HTTP error."""
      data = json.dumps(payload).encode()
      req  = urllib.request.Request(
          url, data=data, headers={"Content-Type": "application/json"}
      )
      with urllib.request.urlopen(req, timeout=timeout) as r:
          return json.loads(r.read())

  def tcp_probe(host, port, timeout=1):
      """Return True if a TCP connection to host:port can be established."""
      try:
          with socket.create_connection((host, port), timeout=timeout):
              return True
      except OSError:
          return False

  def wait_ready(timeout=30):
      """Block until both server-sim /health and evaluator TCP port are up."""
      deadline = time.time() + timeout
      while time.time() < deadline:
          sim_ok  = http_get(SIM_URL  + "/health") is not None
          eval_ok = tcp_probe("localhost", EVAL_PORT)
          if sim_ok and eval_ok:
              return True
          time.sleep(1)
      return False

  # ---------------------------------------------------------------------------
  # Simulate API
  # ---------------------------------------------------------------------------

  def post_simulate(problem_data):
      """Submit a simulation job; return jobID string."""
      resp = http_post(SIM_URL + "/simulate", problem_data)
      return resp["jobID"]

  def poll_job(job_id, poll_interval, timeout=600):
      """Poll GET /simulate/{id} until completed/failed/timeout.

      Returns the result dict. On error or timeout returns {"error": "..."}.
      """
      deadline = time.time() + timeout
      while time.time() < deadline:
          resp = http_get(f"{SIM_URL}/simulate/{job_id}", timeout=15)
          if resp is None:
              time.sleep(poll_interval)
              continue
          status = resp.get("status")
          if status == "completed":
              return resp["result"]
          if status == "failed":
              return {"error": resp.get("error", "unknown")}
          time.sleep(poll_interval)
      return {"error": "timeout"}

  # ---------------------------------------------------------------------------
  # Table output
  # ---------------------------------------------------------------------------

  _HEADERS = ["RPM", "RPS", "Throughput", "RespTime(ms)", "WaitTime(ms)",
              "TTFT(ms)", "ITL(ms)", "MaxRPS", "Saturation"]
  _WIDTHS  = [5, 8, 11, 13, 13, 10, 10, 8, 12]

  def _fmt(v, width, decimals=4):
      if not v:
          return "---".rjust(width)
      return f"{float(v):.{decimals}f}".rjust(width)

  def print_header():
      row = "  ".join(h.rjust(w) for h, w in zip(_HEADERS, _WIDTHS))
      sep = "-" * len(row)
      print(row)
      print(sep)

  def print_row(rpm, rps, r):
      sat = r.get("saturation", "")
      cells = [
          str(rpm).rjust(_WIDTHS[0]),
          f"{rps:.4f}".rjust(_WIDTHS[1]),
          _fmt(r.get("throughput"),  _WIDTHS[2]),
          _fmt(r.get("avgRespTime"), _WIDTHS[3]),
          _fmt(r.get("avgWaitTime"), _WIDTHS[4]),
          _fmt(r.get("avgTTFT"),     _WIDTHS[5]),
          _fmt(r.get("avgITL"),      _WIDTHS[6]),
          _fmt(r.get("maxRPS"),      _WIDTHS[7]),
          sat.rjust(_WIDTHS[8]) if sat else "".rjust(_WIDTHS[8]),
      ]
      print("  ".join(cells), flush=True)

  # ---------------------------------------------------------------------------
  # Sweep loop
  # ---------------------------------------------------------------------------

  def run_sweep(case):
      """Run the RPM sweep for one case. Returns list of result dicts."""
      results = []
      print_header()
      for rpm in range(RPM_STEP, MAX_RPM + RPM_STEP, RPM_STEP):
          rps = rpm / 60.0
          pd  = {
              "RPS":             rps,
              "maxConcurrency":  64,
              "avgInputTokens":  2048,
              "avgOutputTokens": 1024,
              "accelerator":     "H100",
              "model":           case["model"],
          }
          try:
              job_id = post_simulate(pd)
              r      = poll_job(job_id, case["poll_interval"])
          except Exception as exc:
              r = {"error": str(exc)}

          print_row(rpm, rps, r)
          results.append({
              "case":        case["name"],
              "rpm":         rpm,
              "rps":         round(rps, 6),
              "throughput":  r.get("throughput",  0) or 0,
              "avg_resp_ms": r.get("avgRespTime", 0) or 0,
              "avg_wait_ms": r.get("avgWaitTime", 0) or 0,
              "ttft_ms":     r.get("avgTTFT",     0) or 0,
              "itl_ms":      r.get("avgITL",      0) or 0,
              "max_rps":     r.get("maxRPS",      0) or 0,
              "saturation":  r.get("saturation",  ""),
              "error":       r.get("error",       ""),
          })
          sat = r.get("saturation", "")
          if sat:
              max_rps = r.get("maxRPS", "N/A")
              print(f"\n  → Saturation detected: {sat}  maxRPS={max_rps}")
              break
      return results

  # ---------------------------------------------------------------------------
  # Output: CSV + Markdown
  # ---------------------------------------------------------------------------

  _CSV_FIELDS = ["case", "rpm", "rps", "throughput", "avg_resp_ms", "avg_wait_ms",
                 "ttft_ms", "itl_ms", "max_rps", "saturation", "error"]

  def save_outputs(all_results, timestamp):
      out_dir = REPO_ROOT / "scripts" / "sweep_results"
      out_dir.mkdir(parents=True, exist_ok=True)

      # --- CSV ---
      csv_path = out_dir / f"sweep_{timestamp}.csv"
      with open(csv_path, "w", newline="") as f:
          w = csv.DictWriter(f, fieldnames=_CSV_FIELDS)
          w.writeheader()
          w.writerows(all_results)
      print(f"\nCSV  → {csv_path}")

      # --- Markdown ---
      md_path = out_dir / f"sweep_{timestamp}.md"
      date_str = f"{timestamp[:4]}-{timestamp[4:6]}-{timestamp[6:8]}"
      lines = [
          f"# Saturation Sweep Results\n",
          f"**Date:** {date_str}  \n",
          f"**Workload:** granite-3.1-8b-instruct / H100  avgIn=2048  avgOut=1024  maxConc=64\n\n",
      ]

      by_case = {}
      for r in all_results:
          by_case.setdefault(r["case"], []).append(r)

      for case_name, rows in by_case.items():
          lines.append(f"## {case_name}\n\n")
          lines.append(
              "| RPM | RPS | Throughput | RespTime(ms) | WaitTime(ms) | "
              "TTFT(ms) | ITL(ms) | MaxRPS | Saturation |\n"
          )
          lines.append("|-----|-----|-----------|-------------|-------------|"
                       "---------|--------|--------|------------|\n")

          def fv(v):
              return f"{float(v):.4f}" if v else "---"

          for r in rows:
              lines.append(
                  f"| {r['rpm']} | {r['rps']:.4f} | {fv(r['throughput'])} | "
                  f"{fv(r['avg_resp_ms'])} | {fv(r['avg_wait_ms'])} | "
                  f"{fv(r['ttft_ms'])} | {fv(r['itl_ms'])} | "
                  f"{fv(r['max_rps'])} | {r['saturation']} |\n"
              )

          sat_row = next((r for r in rows if r["saturation"]), None)
          if sat_row:
              suffix = f"  maxRPS={sat_row['max_rps']:.4f}" if sat_row["max_rps"] else ""
              lines.append(f"\n**Saturated at RPM={sat_row['rpm']} ({sat_row['saturation']}){suffix}**\n\n")
          else:
              lines.append(f"\n**No saturation detected within {rows[-1]['rpm']} RPM**\n\n")

      md_path.write_text("".join(lines))
      print(f"Report → {md_path}")

  # ---------------------------------------------------------------------------
  # Process management
  # ---------------------------------------------------------------------------

  def start_processes(case):
      """Start evaluator + server-sim. Returns (eval_proc, sim_proc)."""
      eval_env = {**os.environ, "EVALUATOR_PORT": str(EVAL_PORT), **case["env"]}
      eval_proc = subprocess.Popen(
          [str(BUILD_DIR / case["bin"])],
          cwd=REPO_ROOT / case["dir"],
          env=eval_env,
          stdout=subprocess.DEVNULL,
          stderr=subprocess.DEVNULL,
      )

      sim_env = {
          **os.environ,
          "SERVERSIM_PORT":  str(SIM_PORT),
          "EVALUATOR_URL":   EVAL_URL,
      }
      sim_proc = subprocess.Popen(
          [str(BUILD_DIR / "server-sim")],
          cwd=REPO_ROOT,
          env=sim_env,
          stdout=subprocess.DEVNULL,
          stderr=subprocess.DEVNULL,
      )
      return eval_proc, sim_proc

  def stop_processes(*procs):
      """SIGTERM each process and wait up to 5 s; SIGKILL if needed."""
      for p in procs:
          if p and p.poll() is None:
              p.terminate()
              try:
                  p.wait(timeout=5)
              except subprocess.TimeoutExpired:
                  p.kill()

  # ---------------------------------------------------------------------------
  # Main
  # ---------------------------------------------------------------------------

  def main():
      # Determine which cases to run
      valid_names = {c["name"] for c in CASES}
      if len(sys.argv) > 1:
          selected = sys.argv[1:]
          unknown  = [s for s in selected if s not in valid_names]
          if unknown:
              print(f"Unknown case(s): {unknown}\nValid: {sorted(valid_names)}", file=sys.stderr)
              sys.exit(1)
          cases = [c for c in CASES if c["name"] in selected]
      else:
          cases = CASES

      timestamp  = datetime.now().strftime("%Y%m%d_%H%M%S")
      all_results = []
      live_procs  = []

      def cleanup(sig=None, frame=None):
          print("\nInterrupted — stopping processes …", file=sys.stderr)
          stop_processes(*live_procs)
          if all_results:
              save_outputs(all_results, timestamp)
          sys.exit(0)

      signal.signal(signal.SIGINT,  cleanup)
      signal.signal(signal.SIGTERM, cleanup)

      print("Building binaries …")
      build_binaries()
      print("Build complete.\n")

      for case in cases:
          bar = "=" * 70
          print(f"\n{bar}")
          print(f"  {case['name']}: granite-3.1-8b / H100  in=2048 out=1024 conc=64")
          print(f"{bar}")

          eval_proc, sim_proc = start_processes(case)
          live_procs[:] = [eval_proc, sim_proc]

          if not wait_ready(timeout=30):
              print(
                  f"  ERROR: processes not healthy within 30 s — skipping {case['name']}",
                  file=sys.stderr,
              )
              stop_processes(eval_proc, sim_proc)
              live_procs.clear()
              continue

          try:
              results = run_sweep(case)
              all_results.extend(results)
          finally:
              stop_processes(eval_proc, sim_proc)
              live_procs.clear()
              time.sleep(1)   # let ports drain before next case

      if all_results:
          save_outputs(all_results, timestamp)

      print("\nDone.")

  if __name__ == "__main__":
      main()
  ```

- [ ] **Step 2.2: Make the script executable**

  ```bash
  chmod +x scripts/sweep_saturation.py
  ```

- [ ] **Step 2.3: Commit the script**

  ```bash
  git add scripts/sweep_saturation.py
  git commit -m "feat(sweep): add sweep_saturation.py RPM sweep script"
  ```

---

### Task 3: Smoke-test with queue-analysis (fast)

The queue-analysis evaluator is analytical — each call completes in milliseconds.
Run just that case to verify the full pipeline works before launching the multi-hour
BLIS sweep.

**Files:** none (read-only test)

- [ ] **Step 3.1: Run the queue-analysis case only**

  From the repo root:
  ```bash
  python3 scripts/sweep_saturation.py queue-analysis
  ```

  Expected stdout (approximate — exact numbers depend on the analytical model):
  ```
  Building binaries …
    building ./cmd/server-sim …
    building . …
    building . …
  Build complete.

  ======================================================================
    queue-analysis: granite-3.1-8b / H100  in=2048 out=1024 conc=64
  ======================================================================
    RPM      RPS   Throughput  RespTime(ms)  WaitTime(ms)    TTFT(ms)   ITL(ms)    MaxRPS   Saturation
  -----  --------  -----------  -------------  -------------  ----------  ----------  --------  ------------
      2    0.0333      0.0333     ...
      4    0.0667      0.0667     ...
    ...
     XX    X.XXXX         ---           ---            ---         ---       ---      X.XXXX   overloaded

    → Saturation detected: overloaded  maxRPS=X.XXXX

  CSV  → scripts/sweep_results/sweep_YYYYMMDD_HHMMSS.csv
  Report → scripts/sweep_results/sweep_YYYYMMDD_HHMMSS.md

  Done.
  ```

  Verify:
  - Each unsaturated row shows non-zero `Throughput`, `RespTime(ms)`, `ITL(ms)`.
  - The final row shows `---` metrics and a `Saturation` reason.
  - Both CSV and Markdown files are written to `scripts/sweep_results/`.

  If the evaluator row shows `error`, inspect `scripts/sweep_results/sweep_*.csv` for
  the `error` column and check that `model-data.json` was found (CWD = repo root /
  `queue-analysis-evaluator`).

- [ ] **Step 3.2: Spot-check the CSV**

  ```bash
  cat scripts/sweep_results/sweep_*.csv
  ```

  Expected: header row + one row per RPM step + the saturation row at the end, all
  fields populated (no blank columns except `error` which should be empty on success).

---

### Task 4: Run the full sweep (all three cases)

The BLIS cases run a 300-second DES per unsaturated step. Based on the decode-bandwidth
formula (`decodeCapacityTPS ≈ 200 tok/s`, saturation at `RPS × 1024 > 200 × 0.98`),
expect ~5 unsaturated DES runs per BLIS case before the pre-sim bandwidth check fires
at RPM≈12. Each DES run may take 1–5 minutes wall-clock → ~25–50 min per BLIS case.
Total wall-clock estimate: queue-analysis (<5 min) + blis-roofline (≤1 h) +
blis-trained-physics (≤1 h).

**Files:** none during run; output committed after.

- [ ] **Step 4.1: Launch the full sweep (can be left running)**

  ```bash
  python3 scripts/sweep_saturation.py 2>&1 | tee /tmp/sweep_run.log
  ```

  Monitor progress in the terminal. Expected pattern per BLIS case:
  - RPM 2–10: full DES runs, each taking minutes; you'll see a new table row appear
    after each poll cycle.
  - RPM 12: pre-sim check fires immediately, `Saturation=bandwidth` row appears, sweep
    stops for this case.

- [ ] **Step 4.2: Copy results to `docs/sweep-results/` and commit**

  After the script exits, copy the generated files and commit them:

  ```bash
  # Substitute the actual timestamp from the run
  STAMP=$(ls scripts/sweep_results/*.csv | head -1 | grep -oE '[0-9]{8}_[0-9]{6}')
  cp scripts/sweep_results/sweep_${STAMP}.csv  docs/sweep-results/
  cp scripts/sweep_results/sweep_${STAMP}.md   docs/sweep-results/
  git add docs/sweep-results/
  git commit -m "docs(sweep): add granite-8b/H100 saturation sweep results (${STAMP})"
  ```

---

## Self-review

**Spec coverage check:**

| Spec requirement | Covered by |
|-----------------|-----------|
| queue-analysis config with α=8.0, β=0.016, γ=0.0005 | Task 1, Step 1.1 |
| blis-evaluator hardware_config.json (H100) | Task 1, Step 1.2 |
| RPM sweep from 2, step 2, until saturation | Task 2, `run_sweep()` |
| maxConcurrency=64, in=2048, out=1024, accelerator=H100 | Task 2, `pd` dict in `run_sweep()` |
| queue-analysis: model=`granite_8b` | Task 2, CASES[0] |
| blis-roofline: LATENCY_BACKEND=roofline | Task 2, CASES[1] |
| blis-trained-physics: LATENCY_BACKEND=trained-physics | Task 2, CASES[2] |
| Script starts/stops evaluator + server-sim | Task 2, `start_processes` / `stop_processes` |
| Table to stdout as results arrive | Task 2, `print_row()` in `run_sweep()` |
| CSV output | Task 2, `save_outputs()` |
| Markdown summary report | Task 2, `save_outputs()` |
| Results stored in repo under docs/sweep-results/ | Task 4, Step 4.2 |
| KV check passes (64 × 3072 = 196K < 365K threshold) | Design constraint — no code needed |
| Bandwidth saturation fires ~RPM 12 for BLIS | Emergent from blis-evaluator logic |
| Script can run subset of cases via argv | Task 2, `main()` arg parsing |
| Graceful Ctrl-C with partial results saved | Task 2, `cleanup()` signal handler |

**Placeholder scan:** No TBDs, no "implement later", no vague steps. All code is
complete.

**Type/name consistency:** `post_simulate` → `poll_job` → `print_row` →
`save_outputs` — chain is consistent throughout. Field names `avgRespTime`,
`avgWaitTime`, `avgTTFT`, `avgITL`, `maxRPS`, `saturation` match `AnalysisData`
JSON tags in `pkg/evaluator/types.go`.
