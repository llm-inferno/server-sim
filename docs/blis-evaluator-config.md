# Configuring the BLIS evaluator for a new model / GPU

The `blis-evaluator` runs an `inference-sim/BLIS` discrete-event simulation behind
the standard `POST /solve` contract. To simulate a given **model on a given GPU**
it stitches together **three** pieces of configuration. This doc explains what each
one is, which fields matter, where the values come from, and walks through adding a
new (model, GPU) pair end to end.

## The three config files

| # | File | Keyed by | Scope | Holds |
|---|------|----------|-------|-------|
| 1 | `blis-config.json` | `accelerator` + `model` | **per (accelerator, model) pair** | BLIS simulation params: KV blocks, batch caps, TP, scheduler, latency coefficients. One array entry per pair. |
| 2 | `hf-configs/<org>/<model>/config.json` | referenced by an entry's `hfConfigPath` | **per model** | The stock HuggingFace `config.json` (model architecture: hidden size, layers, heads, vocab). |
| 3 | `hardware_config.json` | referenced by an entry's `gpu` field | **per GPU** | GPU calibration: peak TFLOPs, memory bandwidth, MFU, VRAM. |

A `blis-config.json` entry is the glue: it names a `model`, points at an HF config
via `hfConfigPath`, and names a `gpu` key into `hardware_config.json`. At request
time the handler looks the entry up by `"<accelerator>|<model>"`, loads the HF
config and the GPU calibration it references, and builds the simulation
(`blis-evaluator/handler.go`).

### `accelerator` vs `gpu` — they are different keys

- **`accelerator`** is the **request-facing label**. server-sim sends it in
  `ProblemData.Accelerator`; it forms half of the lookup key.
- **`gpu`** is the **hardware-table key** into `hardware_config.json`.

They are allowed to differ. For example the existing A100 entries use
`accelerator: "A100"` but `gpu: "A100-80"` (the alias entry in
`hardware_config.json`). Pick the `accelerator` value to match what the caller
sends; pick `gpu` to match a key that exists in the hardware table.

## `blis-config.json` field reference

Each object in the top-level `"models"` array is one (accelerator, model) entry.
Validation and defaults live in `blis-evaluator/config.go`.

### Required (load fails loud if missing/invalid)

| Field | Type | Notes |
|-------|------|-------|
| `accelerator` | string | Request-facing label; half of the lookup key. |
| `model` | string | Model id sent by the caller; other half of the lookup key. |
| `hfConfigPath` | string | Path to the HuggingFace `config.json` for this model (relative to the evaluator's working dir). |
| `gpu` | string | Key into `hardware_config.json` (e.g. `H100`, `A100-80`, `L40S`). |
| `totalKVBlocks` | int | Total GPU KV-cache blocks. Must be `> 0`. **Model + GPU specific — see below.** |
| `maxRunningReqs` | int | Max concurrent requests in the running batch. Must be `> 0`. Mirrors vLLM `--max-num-seqs`. |
| `maxScheduledTokens` | int | Max total new tokens across the running batch. Must be `> 0`. Mirrors vLLM `--max-num-batched-tokens`. |

### Optional (defaulted)

| Field | Default | Notes |
|-------|---------|-------|
| `tp` | `1` | Tensor-parallel degree. Scales the bandwidth ceiling in the saturation check. |
| `blockSizeTokens` | `16` | Tokens per KV block. |
| `maxModelLen` | `0` (unlimited) | Max sequence length. |
| `scheduler` | `"fcfs"` | Also `"sjf"`, `"priority-fcfs"`. |
| `betaCoeffs` | none | Step-time regression coefficients. **Only consumed by the non-`roofline` latency backends** (`blackbox` ≥3, `crossmodel` ≥4, `trained-roofline`/`trained-physics` ≥7). `roofline` (the default backend) ignores them. |
| `alphaCoeffs` | `[0,0,0]` | Queueing-time coefficients `[α₀, α₁, α₂]` (µs). Zero means zero queueing overhead (underestimates TTFT). Supply calibrated values for accurate wait/TTFT. |
| `simulationHorizon` | `300000000` (300 s, µs) | Longer horizon reduces cold-start throughput bias but costs wall-clock time. |
| `numRequests` | `0` (horizon only) | Cap on requests to simulate. |
| `seed` | `42` | RNG seed for deterministic runs. |

### Where `totalKVBlocks` comes from

This is the one genuinely model+GPU-specific number, and it should not be eyeballed.
It depends on weights, dtype, `max_model_len`, KV-block size, and free VRAM —
exactly the calculation vLLM performs at startup.

**Recommended:** read it straight from the paired vLLM server's startup log, e.g.

```
INFO ... # GPU blocks: 6144, # CPU blocks: 4096
```

(older/newer builds may instead print `Maximum concurrency for N tokens per request`).
Use the `# GPU blocks` value directly.

If you have no running server, you can estimate it:

```
kvBytesPerToken = 2 (K and V) × num_key_value_heads × head_dim × num_hidden_layers × bytesPerElem
head_dim        = hidden_size / num_attention_heads
kvCacheBytes    ≈ (VRAM × gpu_memory_utilization) − weightBytes − activationOverhead
totalKVBlocks   ≈ kvCacheBytes / (kvBytesPerToken × blockSizeTokens)
```

Treat an estimate as a placeholder and replace it with the logged value when the
server is up — the saturation gate (`docs/blis-overload-detection.md`) keys off
`totalKVBlocks`, so an inaccurate value skews KV-capacity saturation.

## Worked example: Qwen/Qwen2.5-14B-Instruct on H100

H100 is already in `hardware_config.json`, so only two of the three files change.

**1. Add the HuggingFace config** — download the upstream `config.json` to
`hf-configs/Qwen/Qwen2.5-14B-Instruct/config.json`:

```bash
mkdir -p blis-evaluator/hf-configs/Qwen/Qwen2.5-14B-Instruct
curl -L -o blis-evaluator/hf-configs/Qwen/Qwen2.5-14B-Instruct/config.json \
  https://huggingface.co/Qwen/Qwen2.5-14B-Instruct/raw/main/config.json
```

Qwen2.5-14B-Instruct is `Qwen2ForCausalLM`: hidden 5120, 48 layers, 40 attention
heads / 8 KV heads (grouped-query attention), intermediate 13824, vocab 152064,
bf16.

**2. (GPU table) — nothing to do.** `H100` already exists in
`hardware_config.json`. You'd only touch this file for a GPU not yet listed
(see below).

**3. Add an entry to `blis-config.json`:**

```json
{
  "accelerator": "H100",
  "model": "Qwen/Qwen2.5-14B-Instruct",
  "hfConfigPath": "hf-configs/Qwen/Qwen2.5-14B-Instruct/config.json",
  "gpu": "H100",
  "tp": 1,
  "totalKVBlocks": 6144,
  "blockSizeTokens": 16,
  "maxRunningReqs": 256,
  "maxScheduledTokens": 8192,
  "maxModelLen": 0,
  "scheduler": "fcfs",
  "betaCoeffs": [0.15, 0.0, 1.4, 0.75, 32.0, 4.0, 126.0, 482.0, 0.0, 1.9],
  "alphaCoeffs": [15563.0, 777.0, 46.0],
  "simulationHorizon": 300000000,
  "numRequests": 0,
  "seed": 42
}
```

> **`totalKVBlocks: 6144` is an estimate** for 14B bf16 on a single 80 GB H100
> (≈40 GB left for KV after weights, at the default `gpu_memory_utilization`).
> Replace it with the `# GPU blocks` value from your vLLM server's startup log for
> accurate KV-capacity saturation.

`betaCoeffs`/`alphaCoeffs` reuse the shared defaults every other entry uses. With
the default `roofline` backend the `betaCoeffs` are ignored; supply calibrated
`alphaCoeffs` if you need accurate queueing/TTFT.

**4. Verify it loads:**

```bash
go build ./...
cd blis-evaluator && BLIS_CONFIG_FILE=blis-config.json HW_CONFIG_FILE=hardware_config.json go run .
# log should report one more "loaded N accelerator/model configurations"
```

Send a `/solve` with `accelerator: "H100"`, `model: "Qwen/Qwen2.5-14B-Instruct"` to
confirm the lookup resolves.

## Adding a GPU that isn't in `hardware_config.json` yet

If your accelerator's `gpu` key is missing, add a calibration block keyed by that
name:

```json
"H200": {
  "TFlopsPeak": 989.5,
  "TFlopsFP8":  1979.0,
  "BwPeakTBs":  4.8,
  "mfuPrefill": 0.45,
  "mfuDecode":  0.30,
  "MemoryGiB":  141.0
}
```

| Field | Meaning |
|-------|---------|
| `TFlopsPeak` | Peak dense (bf16/fp16) TFLOPs. |
| `TFlopsFP8` | Peak FP8 TFLOPs (0 if unsupported). |
| `BwPeakTBs` | Peak HBM bandwidth (TB/s). Drives the decode-bandwidth saturation ceiling. |
| `mfuPrefill` / `mfuDecode` | Achievable model-FLOPs utilization for prefill / decode (calibrated — see Discussion #589 referenced in the file). |
| `MemoryGiB` | Per-GPU VRAM. |

Then reference it from a `blis-config.json` entry via the `gpu` field. The
`hwConfigPath` per-entry field lets a single entry point at an alternate hardware
file; otherwise `HW_CONFIG_FILE` (default `hardware_config.json`) is used for all
entries.

## Related docs

- `docs/blis-overload-detection.md` — how `totalKVBlocks` and GPU bandwidth feed the
  pre-simulation saturation check.
- `docs/saturation-detection.md` — saturation semantics across evaluators.
- `docs/maxconcurrency-defaults-design.md` — how `maxRunningReqs` interacts with the
  request's `maxConcurrency`.
