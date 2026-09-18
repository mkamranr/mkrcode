#!/usr/bin/env bash
# Launch vLLM for mkr on the air-gapped inference host.
#
# Usage:  ./serve.sh <profile>
# Profiles: small | medium | large
#
# The container image and model weights are expected to have been loaded
# from removable media already; see ../README.md.

set -euo pipefail

PROFILE="${1:-small}"
IMAGE="${MKR_VLLM_IMAGE:-vllm/vllm-openai:v0.11.0}"
MODEL_ROOT="${MKR_MODEL_ROOT:-/opt/models}"
PORT="${MKR_PORT:-8000}"

# --tool-call-parser must match the served model's chat template. Getting it
# wrong is not fatal: the mkr client probes the endpoint at startup and falls
# back to prompt-level tool parsing. It is, however, slower and less reliable,
# so it is worth getting right. Confirm the parser name against the vLLM
# version you are running:  vllm serve --help | grep -A5 tool-call-parser
case "$PROFILE" in
  small)
    MODEL="${MKR_MODEL:-Qwen/Qwen3-Coder-30B-A3B-Instruct}"
    TP=1
    MAX_LEN="${MKR_MAX_LEN:-32768}"
    PARSER="${MKR_TOOL_PARSER:-qwen3_coder}"
    EXTRA="--quantization fp8"
    ;;
  medium)
    MODEL="${MKR_MODEL:-Qwen/Qwen3-Coder-30B-A3B-Instruct}"
    TP=2
    MAX_LEN="${MKR_MAX_LEN:-131072}"
    PARSER="${MKR_TOOL_PARSER:-qwen3_coder}"
    EXTRA=""
    ;;
  large)
    MODEL="${MKR_MODEL:-Qwen/Qwen3-Coder-480B-A35B-Instruct}"
    TP=8
    MAX_LEN="${MKR_MAX_LEN:-131072}"
    PARSER="${MKR_TOOL_PARSER:-qwen3_coder}"
    EXTRA="--quantization fp8"
    ;;
  *)
    echo "unknown profile: $PROFILE (want small, medium or large)" >&2
    exit 2
    ;;
esac

echo "profile:     $PROFILE"
echo "model:       $MODEL"
echo "tensor par.: $TP"
echo "max len:     $MAX_LEN"
echo "tool parser: $PARSER"
echo

exec docker run --rm --gpus all \
  --network host \
  --ipc host \
  -v "$MODEL_ROOT:/models:ro" \
  -e HF_HUB_OFFLINE=1 \
  -e TRANSFORMERS_OFFLINE=1 \
  "$IMAGE" \
  --model "/models/${MODEL}" \
  --served-model-name "$MODEL" \
  --port "$PORT" \
  --host 0.0.0.0 \
  --tensor-parallel-size "$TP" \
  --max-model-len "$MAX_LEN" \
  --gpu-memory-utilization "${MKR_GPU_UTIL:-0.90}" \
  --enable-auto-tool-choice \
  --tool-call-parser "$PARSER" \
  $EXTRA
