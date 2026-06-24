#!/usr/bin/env python3
"""compare_curves.py — overlay real / simulated latency-throughput curves.

Takes any number of curves as `label=path.csv` arguments and emits a side-by-side
comparison plot and a markdown table. Rows flagged empty are dropped; the Phase A
(`knee`) rows of an empirical run are dropped so only the curve sweep is compared.

Usage:
  python3 compare_curves.py real=REAL.csv blis=BLIS.csv qa=QA.csv --out-dir results
"""
import argparse
import csv
from pathlib import Path

METRICS = ("rps", "throughput", "avgTTFT", "avgITL", "avgRespTime", "concurrency")


def load(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            if r.get("phase") == "knee":      # drop empirical knee-discovery rows
                continue
            if r.get("empty") == "True":
                continue
            def num(k):
                try:
                    return float(r.get(k, ""))
                except (TypeError, ValueError):
                    return None
            rows.append({k: num(k) for k in METRICS} | {"saturation": r.get("saturation", "")})
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("curves", nargs="+", help="label=path.csv (first is the reference)")
    ap.add_argument("--out-dir", default="results")
    ap.add_argument("--title", default="Qwen2.5-14B-Instruct / H100 (in=1024 out=512, maxConc=256)")
    args = ap.parse_args()

    series = []
    for spec in args.curves:
        label, _, path = spec.partition("=")
        series.append((label, load(path)))
    out = Path(args.out_dir)
    out.mkdir(parents=True, exist_ok=True)

    # Markdown table: throughput + ITL + TTFT per curve, aligned on the shared RPS grid.
    labels = [lab for lab, _ in series]
    head = ["RPS"] + [f"{m} {lab}" for m in ("Tput", "ITL", "TTFT") for lab in labels]
    md = [f"# Sim vs Real — {args.title}\n\n",
          "| " + " | ".join(head) + " |\n",
          "|" + "|".join(["---"] * len(head)) + "|\n"]
    n = min(len(s) for _, s in series)
    for i in range(n):
        ref = series[0][1][i]
        cells = [f"{ref['rps']:.2f}"]
        for m in ("throughput", "avgITL", "avgTTFT"):
            for _, s in series:
                v = s[i][m]
                cells.append("---" if v is None else (f"{v:.2f}" if m == "throughput" else f"{v:.0f}"))
        md.append("| " + " | ".join(cells) + " |\n")
    (out / "comparison.md").write_text("".join(md))
    print(f"Table  -> {out/'comparison.md'}")

    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except Exception as exc:  # noqa: BLE001
        print(f"(plot skipped: {exc})")
        return

    styles = ["o-", "s--", "^:", "d-."]
    # 2x2 grid: top row = capacity (throughput, concurrency), bottom row = latency (ITL, TTFT).
    fig, ax = plt.subplots(2, 2, figsize=(13, 9))
    ax = ax.ravel()
    for idx, (label, data) in enumerate(series):
        st = styles[idx % len(styles)]
        rps = [r["rps"] for r in data]
        tput = [r["throughput"] for r in data]
        ax[0].plot(rps, tput, st, label=label)
        # Little's law L = X·W: multiplier is the actual throughput X (served rate),
        # not the offered rate. benchmark_curve.py computes the `concurrency` column
        # as throughput × avgRespTime, so we just plot it here.
        ax[1].plot(tput, [r["concurrency"] for r in data], st, label=label)
        ax[2].plot(tput, [r["avgITL"] for r in data], st, label=label)
        ax[3].plot(tput, [r["avgTTFT"] for r in data], st, label=label)
    ref_rps = [r["rps"] for r in series[0][1]]
    ax[0].plot(ref_rps, ref_rps, ":", color="gray", label="offered=served")
    ax[0].set(xlabel="offered RPS", ylabel="throughput (req/s)", title="Throughput vs offered")
    ax[1].set(xlabel="throughput (req/s)", ylabel="avg concurrency (L = X·W)",
              title="Concurrency vs throughput")
    ax[2].set(xlabel="throughput (req/s)", ylabel="ITL (ms)", title="ITL vs throughput")
    # TTFT spans ~40 ms (low load) to ~10 s (saturation); log y keeps both ends readable.
    ax[3].set(xlabel="throughput (req/s)", ylabel="TTFT (ms, log scale)", title="TTFT vs throughput")
    ax[3].set_yscale("log")
    for a in ax:
        a.legend(); a.grid(True, alpha=0.3, which="both")
    fig.suptitle(args.title)
    fig.tight_layout(rect=(0, 0, 1, 0.95))
    fig.savefig(out / "comparison.png", dpi=120)
    print(f"Plot   -> {out/'comparison.png'}")


if __name__ == "__main__":
    main()
