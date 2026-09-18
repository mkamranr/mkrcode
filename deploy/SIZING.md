# GPU sizing for the mkr inference host

GPU hardware was still being procured when this was written, so this document
gives the arithmetic rather than a single answer, and a recommendation for what
to buy or allocate first.

## How to read the numbers

Weight memory is the parameter count multiplied by bytes per parameter:

- **FP8 / INT8** — 1 byte per parameter
- **BF16 / FP16** — 2 bytes per parameter

On top of weights you need KV cache and activation memory. Budget **20–30%**
above the weight figure for a normal context length, and considerably more if
you intend to serve the full context window to several concurrent users. The KV
cache is what actually runs out first in practice.

## Candidate models

| Model | Params | FP8 weights | BF16 weights | Minimum practical deployment |
|---|---|---|---|---|
| Qwen3-Coder-480B-A35B | 480B total / 35B active | ~480 GB | ~960 GB | 8×H100 80GB at FP8 (tight); 8×H200 141GB at BF16 |
| Qwen3-Coder-30B-A3B | 30B total / 3B active | ~30 GB | ~60 GB | 1×48GB at FP8; 1×80GB at BF16 |
| Qwen3.8-27B | see note | ~27 GB | ~54 GB | 1×48GB at FP8; 1×80GB at BF16 |

**Note on Qwen3.8-27B.** This model postdates the documentation available when
this was written. The figures above assume a dense 27B model and are an
inference from the name, not a verified fact. Confirm the parameter count,
context length and chat template from the model card before committing to
hardware, and update `profiles/` accordingly. The client does not depend on
these numbers being right: it probes the endpoint at runtime.

## Recommendation

**Start with Qwen3-Coder-30B-A3B.**

It is a mixture-of-experts model that activates only 3B parameters per token,
which makes it substantially faster than a dense model of comparable quality,
and it fits on a single card. For agentic coding, where the loop makes many
sequential requests, that latency advantage matters more than raw benchmark
scores.

Standing this up first turns the 480B decision into an informed one. You will
have measured throughput, real context usage and actual developer satisfaction
on your own codebases before committing to an eight-GPU purchase. The 480B model
is roughly sixteen times the memory for a benefit you can then quantify rather
than assume.

## Concurrency

A rough planning figure for interactive coding use: each active user needs
enough KV cache for their working context. At 32k tokens of context, budget
roughly 2–4 GB of KV cache per concurrent user for a 30B-class model. Ten
concurrent developers on a single 80GB card serving a 30B model at FP8 is
comfortable; the same card serving BF16 is not.

Set `--max-model-len` deliberately rather than leaving it at the model maximum.
Serving a 256k context window to every request reserves KV cache you almost
never use and sharply reduces how many users fit on the card.
