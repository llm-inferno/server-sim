#!/usr/bin/env python3
"""benchmark_curve.py — latency/throughput curve sweep against a live vLLM server.

Drives the continuous-vllm-server-evaluator's POST /solve directly to build, for a
fixed workload (model, accelerator, token means, maxConcurrency):

  * delay  vs throughput  — avgTTFT, avgITL, avgRespTime vs throughput
  * concurrency vs throughput — avg in-flight derived via Little's law
                                (N = throughput x avgRespTime), no evaluator change

The evaluator is a LOSS system: its limiter is drop-if-full, so excess offered load
beyond maxConcurrency is dropped (counted in offeredRPS, not throughput). Saturation
therefore shows as a throughput plateau with a rising drop fraction
(dropFraction = 1 - throughput/offeredRPS), not unbounded latency.

Range finding (--seed-from):
  empirical  Phase A geometrically ramps RPS until a knee trips (drop fraction over
             --drop-threshold, a saturation flag, or TTFT past --ttft-knee-mult x the
             low-load baseline), then Phase B sweeps --points rates up to alpha*R_knee
             so the top points sit just PAST the knee.
  analytic   Skip Phase A; take R_knee from --rps-knee (e.g. the BLIS KV/bandwidth
             ceiling, see docs/blis-overload-detection.md). Phase B as above.
  manual     Skip Phase A; sweep --points rates linearly in [--rps-min, --rps-max].

The driver must be the SOLE /solve caller during a run: deploy the paired stack with
SERVERSIM_CONTINUOUS=false and no load-emulator/controller. See
docs/benchmarking-latency-throughput.md.

Usage:
  python3 scripts/benchmark_curve.py --model qwen_0_5b --accelerator cpu \
      --in-tokens 512 --out-tokens 256 --max-concurrency 64 --window-sec 30

Output (under --out-dir, default scripts/benchmark_results):
  curve_YYYYMMDD_HHMMSS.csv
  curve_YYYYMMDD_HHMMSS.md
  curve_YYYYMMDD_HHMMSS.png   (skipped if matplotlib is unavailable or --no-plot)
"""

import argparse
import csv
import json
import sys
import time
import urllib.request
from datetime import datetime
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.resolve()


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

def solve(eval_url, pd, timeout=120):
    """POST ProblemData to {eval_url}/solve; return AnalysisData dict. Raises on error."""
    data = json.dumps(pd).encode()
    req = urllib.request.Request(
        eval_url.rstrip("/") + "/solve",
        data=data,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read())


def problem_data(rps, args):
    return {
        "RPS": float(rps),
        "maxConcurrency": int(args.max_concurrency),
        "avgInputTokens": float(args.in_tokens),
        "avgOutputTokens": float(args.out_tokens),
        "accelerator": args.accelerator,
        "model": args.model,
    }


# ---------------------------------------------------------------------------
# One steady-state measurement
# ---------------------------------------------------------------------------

def _num(ad, key):
    """AnalysisData uses omitempty, so absent fields read as 0."""
    return float(ad.get(key, 0) or 0)


def measure_point(eval_url, rps, args, first=False):
    """Set the live load, let the trailing window fill, then average --reads windows.

    Returns a record dict with measured and derived metrics.
    """
    pd = problem_data(rps, args)

    # Set the setpoint (resizes limiter, repoints the Poisson rate).
    solve(eval_url, pd)

    # Settle: the trailing window must flush old-rate samples, and must hold at least
    # --min-samples completions so the evaluator returns populated metrics rather than
    # an empty window. The first point also pays the one-time warmup.
    settle = max(args.window_sec + args.margin_sec,
                 args.min_samples / max(rps, 1e-9) + args.margin_sec)
    if first:
        settle += args.warmup_sec
    time.sleep(settle)

    # Average several reads to damp window noise. Re-POSTing the same setpoint keeps
    # the rate steady; each call returns a fresh trailing-window aggregate.
    acc = {"throughput": 0.0, "offeredRPS": 0.0, "avgTTFT": 0.0,
           "avgITL": 0.0, "avgRespTime": 0.0, "avgWaitTime": 0.0}
    n = 0
    saturation = ""
    for k in range(max(1, args.reads)):
        if k > 0:
            time.sleep(args.read_gap)
        ad = solve(eval_url, pd)
        if _num(ad, "throughput") == 0 and _num(ad, "offeredRPS") == 0:
            continue  # empty window (no completions yet) — skip from the average
        for key in acc:
            acc[key] += _num(ad, key)
        saturation = ad.get("saturation", "") or saturation
        n += 1

    rec = {"rps": round(float(rps), 6), "reads": n, "saturation": saturation}
    if n == 0:
        # No populated window in any read — record zeros and flag.
        rec.update({k: 0.0 for k in acc})
        rec["dropFraction"] = None
        rec["concurrency"] = 0.0
        rec["empty"] = True
        return rec

    for key in acc:
        rec[key] = acc[key] / n
    offered = rec["offeredRPS"]
    tput = rec["throughput"]
    rec["dropFraction"] = (1.0 - tput / offered) if offered > 0 else None
    # Little's law: avg admitted in-flight = throughput (req/s) x avgRespTime (s).
    rec["concurrency"] = tput * rec["avgRespTime"] / 1000.0
    rec["empty"] = False
    return rec


# ---------------------------------------------------------------------------
# Live table
# ---------------------------------------------------------------------------

_COLS = [("rps", "RPS", 8), ("offeredRPS", "Offered", 9), ("throughput", "Tput", 9),
         ("dropFraction", "Drop", 7), ("avgTTFT", "TTFT(ms)", 10),
         ("avgITL", "ITL(ms)", 9), ("avgRespTime", "Resp(ms)", 10),
         ("concurrency", "Conc", 8), ("saturation", "Sat", 11)]


def print_header():
    print("  ".join(h.rjust(w) for _, h, w in _COLS))
    print("-" * (sum(w for _, _, w in _COLS) + 2 * (len(_COLS) - 1)))


def print_row(rec):
    cells = []
    for key, _, w in _COLS:
        v = rec.get(key)
        if key == "saturation":
            cells.append((v or "").rjust(w))
        elif v is None:
            cells.append("---".rjust(w))
        else:
            cells.append(f"{float(v):.4f}".rjust(w))
    print("  ".join(cells), flush=True)


# ---------------------------------------------------------------------------
# Phase A: empirical knee discovery
# ---------------------------------------------------------------------------

def find_knee(eval_url, args):
    """Geometrically ramp RPS until a knee criterion trips. Returns (R_knee, records)."""
    print("\nPhase A — knee discovery (ramp x%.2f from %.4f rps)" %
          (args.ramp_factor, args.rps_seed))
    print_header()
    records = []
    rps = args.rps_seed
    baseline_ttft = None
    first = True
    knee = None
    while rps <= args.rps_ceiling:
        rec = measure_point(eval_url, rps, args, first=first)
        rec["phase"] = "knee"
        records.append(rec)
        print_row(rec)
        first = False

        if not rec["empty"]:
            if baseline_ttft is None and rec["avgTTFT"] > 0:
                baseline_ttft = rec["avgTTFT"]
            tripped = (
                (rec["dropFraction"] is not None and rec["dropFraction"] > args.drop_threshold)
                or bool(rec["saturation"])
                or (baseline_ttft and rec["avgTTFT"] > args.ttft_knee_mult * baseline_ttft)
            )
            if tripped:
                knee = rps
                print(f"  -> knee at RPS={rps:.4f} "
                      f"(drop={rec['dropFraction']}, sat='{rec['saturation']}', "
                      f"ttft={rec['avgTTFT']:.1f}ms)")
                break
        rps *= args.ramp_factor

    if knee is None:
        knee = rps / args.ramp_factor
        print(f"  -> no knee within ceiling {args.rps_ceiling}; using last RPS={knee:.4f}")
    return knee, records


# ---------------------------------------------------------------------------
# Phase B: curve sweep
# ---------------------------------------------------------------------------

def linspace(lo, hi, n):
    if n <= 1:
        return [hi]
    step = (hi - lo) / (n - 1)
    return [lo + i * step for i in range(n)]


def sweep_curve(eval_url, args, rps_min, rps_max):
    print("\nPhase B — curve sweep (%d points, %.4f .. %.4f rps)" %
          (args.points, rps_min, rps_max))
    print_header()
    records = []
    first = True
    for rps in linspace(rps_min, rps_max, args.points):
        rec = measure_point(eval_url, rps, args, first=first)
        rec["phase"] = "curve"
        records.append(rec)
        print_row(rec)
        first = False
    return records


# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

_CSV_FIELDS = ["phase", "rps", "offeredRPS", "throughput", "dropFraction",
               "avgTTFT", "avgITL", "avgRespTime", "avgWaitTime", "concurrency",
               "saturation", "reads", "empty"]


def save_outputs(records, args, timestamp):
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    stem = f"curve_{timestamp}"

    workload = (f"{args.model} / {args.accelerator}  in={args.in_tokens} "
                f"out={args.out_tokens} maxConc={args.max_concurrency}")

    csv_path = out_dir / f"{stem}.csv"
    with open(csv_path, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=_CSV_FIELDS)
        w.writeheader()
        for r in records:
            w.writerow({k: r.get(k, "") for k in _CSV_FIELDS})
    print(f"\nCSV    -> {csv_path}")

    md_path = out_dir / f"{stem}.md"
    date_str = f"{timestamp[:4]}-{timestamp[4:6]}-{timestamp[6:8]}"
    lines = [
        "# Latency / Throughput Curve\n\n",
        f"**Date:** {date_str}  \n",
        f"**Workload:** {workload}  \n",
        f"**Window:** {args.window_sec}s trailing, {args.reads} reads/point  \n\n",
        "| Phase | RPS | Offered | Tput | Drop | TTFT(ms) | ITL(ms) | "
        "Resp(ms) | Conc | Sat |\n",
        "|-------|-----|---------|------|------|----------|---------|"
        "----------|------|-----|\n",
    ]

    def fv(v):
        return "---" if v is None else f"{float(v):.4f}"

    for r in records:
        lines.append(
            f"| {r.get('phase','')} | {fv(r.get('rps'))} | {fv(r.get('offeredRPS'))} | "
            f"{fv(r.get('throughput'))} | {fv(r.get('dropFraction'))} | "
            f"{fv(r.get('avgTTFT'))} | {fv(r.get('avgITL'))} | "
            f"{fv(r.get('avgRespTime'))} | {fv(r.get('concurrency'))} | "
            f"{r.get('saturation','')} |\n"
        )
    md_path.write_text("".join(lines))
    print(f"Report -> {md_path}")

    if not args.no_plot:
        _save_plots(records, workload, out_dir / f"{stem}.png")


def _save_plots(records, workload, png_path):
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except Exception as exc:  # noqa: BLE001 - optional dependency
        print(f"(plot skipped: matplotlib unavailable: {exc})")
        return

    pts = [r for r in records if not r.get("empty")]
    tput = [r["throughput"] for r in pts]
    ttft = [r["avgTTFT"] for r in pts]
    itl = [r["avgITL"] for r in pts]
    conc = [r["concurrency"] for r in pts]
    offered = [r["offeredRPS"] for r in pts]
    drop = [(r["dropFraction"] or 0.0) for r in pts]

    fig, ax = plt.subplots(1, 3, figsize=(16, 4.5))
    ax[0].plot(tput, ttft, "o-", label="TTFT")
    ax[0].plot(tput, itl, "s-", label="ITL")
    ax[0].set_xlabel("throughput (req/s)")
    ax[0].set_ylabel("latency (ms)")
    ax[0].set_title("Delay vs throughput")
    ax[0].legend()
    ax[0].grid(True, alpha=0.3)

    ax[1].plot(tput, conc, "o-", color="tab:green")
    ax[1].set_xlabel("throughput (req/s)")
    ax[1].set_ylabel("avg concurrency (Little's law)")
    ax[1].set_title("Concurrency vs throughput")
    ax[1].grid(True, alpha=0.3)

    ax[2].plot(offered, drop, "o-", color="tab:red")
    ax[2].set_xlabel("offered (req/s)")
    ax[2].set_ylabel("drop fraction")
    ax[2].set_title("Drop fraction vs offered")
    ax[2].grid(True, alpha=0.3)

    fig.suptitle(workload)
    fig.tight_layout(rect=(0, 0, 1, 0.95))
    fig.savefig(png_path, dpi=120)
    print(f"Plots  -> {png_path}")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--eval-url", default="http://localhost:8081",
                   help="continuous evaluator base URL (default: %(default)s)")
    p.add_argument("--model", required=True, help="model key (matches evaluator config)")
    p.add_argument("--accelerator", required=True, help="accelerator key (matches config)")
    p.add_argument("--in-tokens", type=float, default=512)
    p.add_argument("--out-tokens", type=float, default=256)
    p.add_argument("--max-concurrency", type=int, default=0,
                   help="0 = evaluator default; set a known cap for a clean ceiling")

    p.add_argument("--seed-from", choices=["empirical", "analytic", "manual"],
                   default="empirical")
    p.add_argument("--points", type=int, default=10, help="curve points (Phase B)")
    p.add_argument("--alpha", type=float, default=1.3,
                   help="Phase B top = alpha * R_knee (>1 goes past the knee)")

    # Phase A (empirical) knobs.
    p.add_argument("--rps-seed", type=float, default=0.5)
    p.add_argument("--ramp-factor", type=float, default=1.5)
    p.add_argument("--rps-ceiling", type=float, default=1e6,
                   help="hard upper bound for the ramp safety stop")
    p.add_argument("--drop-threshold", type=float, default=0.10)
    p.add_argument("--ttft-knee-mult", type=float, default=3.0)

    # analytic / manual seeds.
    p.add_argument("--rps-knee", type=float, default=None,
                   help="analytic mode: R_knee (e.g. BLIS KV/bandwidth ceiling)")
    p.add_argument("--rps-min", type=float, default=None, help="manual mode lower bound")
    p.add_argument("--rps-max", type=float, default=None, help="manual mode upper bound")

    # Measurement timing (window-sec MUST match the evaluator's trailingWindowSec).
    p.add_argument("--window-sec", type=float, default=30)
    p.add_argument("--warmup-sec", type=float, default=0)
    p.add_argument("--min-samples", type=int, default=3)
    p.add_argument("--margin-sec", type=float, default=5)
    p.add_argument("--reads", type=int, default=3)
    p.add_argument("--read-gap", type=float, default=2.0)

    p.add_argument("--out-dir", default=str(REPO_ROOT / "scripts" / "benchmark_results"))
    p.add_argument("--no-plot", action="store_true")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv if argv is not None else sys.argv[1:])
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")

    if args.max_concurrency <= 0:
        print("WARNING: --max-concurrency is 0 (evaluator default); set a known cap "
              "for a reproducible throughput ceiling.", file=sys.stderr)

    records = []
    try:
        if args.seed_from == "manual":
            if args.rps_min is None or args.rps_max is None:
                print("manual mode requires --rps-min and --rps-max", file=sys.stderr)
                return 2
            records += sweep_curve(args.eval_url, args, args.rps_min, args.rps_max)
        else:
            if args.seed_from == "analytic":
                if args.rps_knee is None:
                    print("analytic mode requires --rps-knee", file=sys.stderr)
                    return 2
                knee = args.rps_knee
                print(f"\nPhase A skipped — analytic R_knee={knee:.4f}")
            else:
                knee, knee_recs = find_knee(args.eval_url, args)
                records += knee_recs
            rps_min = args.rps_min if args.rps_min is not None else knee * 0.1
            rps_max = args.rps_max if args.rps_max is not None else knee * args.alpha
            records += sweep_curve(args.eval_url, args, rps_min, rps_max)
    except KeyboardInterrupt:
        print("\nInterrupted — saving partial results …", file=sys.stderr)
    except Exception as exc:  # noqa: BLE001 - surface and still save what we have
        print(f"\nERROR during sweep: {exc}", file=sys.stderr)

    if records:
        save_outputs(records, args, timestamp)
    else:
        print("No records collected.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
