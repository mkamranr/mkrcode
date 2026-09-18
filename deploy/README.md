# Deploying mkr into the air-gapped environment

Two things are transferred on removable media: the **client** (one Windows
executable) and the **server** (a container image plus model weights).

## 1. Client — Windows workstations

The client is a single statically-linked executable with no runtime
dependencies. There is no installer, no Python, no Node, and no DLLs to
register.

**On the connected side:**

```
make bundle-windows        # produces dist/mkr-windows-amd64-<version>.zip
```

The zip contains `mkr.exe`, a default `config.json`, the documentation, and
`SHA256SUMS`.

**On the air-gapped side:**

1. Copy the zip to the workstation and extract it, for example to
   `C:\Tools\mkr\`.
2. Verify the checksum:
   ```powershell
   Get-FileHash .\mkr.exe -Algorithm SHA256
   ```
   and compare against `SHA256SUMS`.
3. Add the directory to `PATH`, or call `mkr.exe` by full path.
4. Point it at the inference host:
   ```powershell
   setx MKR_ENDPOINT "http://<inference-host>:8000"
   ```
5. Confirm connectivity:
   ```powershell
   mkr probe
   ```

**Application allowlisting.** If the fleet enforces AppLocker or WDAC, `mkr.exe`
must be signed with an internal code-signing certificate before distribution.
Start that process early; it usually has a longer lead time than the build.

## 2. Server — Ubuntu inference host

### Why a container image rather than a wheelhouse

vLLM, PyTorch and the CUDA runtime are roughly ten gigabytes of tightly coupled
wheels. Resolving that dependency graph offline with `pip download` is the single
most likely step to fail on a media transfer, and the failure typically appears
only at the end, after the weights have already been copied.

Transferring a container image reduces the whole problem to one file with one
checksum.

### On the connected side

```bash
# 1. Pull and export the vLLM image. Pin the tag; do not use :latest.
docker pull vllm/vllm-openai:v0.11.0
docker save vllm/vllm-openai:v0.11.0 | gzip > vllm-v0.11.0.tar.gz

# 2. Download the model weights.
pip install -U "huggingface_hub[cli]"
hf download Qwen/Qwen3-Coder-30B-A3B-Instruct \
    --local-dir ./models/Qwen/Qwen3-Coder-30B-A3B-Instruct

# 3. Record checksums for everything being transferred.
sha256sum vllm-v0.11.0.tar.gz > SHA256SUMS
find ./models -type f -exec sha256sum {} \; >> SHA256SUMS
```

**Media sizing.** Plan the drive capacity before starting:

| Model | Approximate download size |
|---|---|
| Qwen3-Coder-30B-A3B (BF16) | ~60 GB |
| Qwen3-Coder-30B-A3B (FP8) | ~30 GB |
| Qwen3-Coder-480B-A35B (FP8) | ~500 GB |
| vLLM container image | ~10 GB |

Use an exFAT or ext4 volume. FAT32 cannot hold the individual safetensors
shards.

### On the air-gapped side

```bash
# 1. Verify everything arrived intact before doing anything else.
sha256sum -c SHA256SUMS

# 2. Load the image.
gunzip -c vllm-v0.11.0.tar.gz | docker load

# 3. Put the weights where the serve script expects them.
sudo mkdir -p /opt/models
sudo cp -r ./models/* /opt/models/

# 4. Start the server.
./profiles/serve.sh small
```

`HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1` are set in the serve script, so
the container will fail loudly rather than silently attempting to reach the
internet if a path is wrong.

### Verifying the server

From the inference host:

```bash
curl -s http://localhost:8000/v1/models | head
```

From a Windows client, once the firewall rule is in place:

```powershell
mkr probe -endpoint http://<inference-host>:8000
```

A successful probe reports `native tools: true` and `adapter: native`. If it
reports `adapter: xml`, the client is still fully functional but vLLM's tool
parser is not matching the model: check `--tool-call-parser` against the value
your vLLM version expects for this model.

## 3. Network

See `NETWORK-REQUIREMENT.md`. One TCP port, one direction. This has the longest
lead time of anything in the deployment; submit it first.
