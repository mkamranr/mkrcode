# Installation

Two things are installed: the **client** (one Windows executable, on each
developer's workstation) and the **server** (vLLM, on a GPU host). They are
independent — install the client and validate it before the server exists.

- [Prerequisites](#prerequisites)
- [Client](#client-windows-workstations)
- [Server](#server-ubuntu-gpu-host)
- [Network](#network)
- [Verifying the whole path](#verifying-the-whole-path)
- [Upgrading](#upgrading)
- [Uninstalling](#uninstalling)

---

> **Just evaluating?** To try `mkr` on a Mac or Linux box against a local model
> or a hosted API, see **[Testing locally](TESTING-LOCALLY.md)** instead. This
> document covers the air-gapped deployment.

## Prerequisites

| | Requirement |
|---|---|
| **Client OS** | Windows 10 or 11, x64. Also builds for Linux and macOS. |
| **Client shell** | PowerShell 7 (`pwsh`) preferred; Windows PowerShell 5.1 and `cmd` work. |
| **Client runtime** | None. The binary is static and has no dependencies. |
| **Server OS** | Ubuntu 22.04 or 24.04 with NVIDIA drivers and a container runtime. |
| **Server GPU** | See [deploy/SIZING.md](../deploy/SIZING.md). One 80 GB card runs the recommended model. |
| **Network** | One TCP port from the client segment to the server. See [below](#network). |

---

## Client (Windows workstations)

### 1. Obtain the binary

On a **connected** machine, either download the release archive from the
repository's Releases page, or build it:

```bash
git clone <repo-url> && cd mkrcode
make bundle-windows VERSION=1.1.0
# produces dist/mkr-windows-amd64-1.1.0.zip
```

The archive contains `mkr.exe`, a starter `config.json`, the documentation, and
`SHA256SUMS`.

> **Application allowlisting.** If your fleet enforces AppLocker or WDAC, sign
> `mkr.exe` with an internal code-signing certificate before distributing it.
> An unsigned binary will not launch, and the certificate process usually takes
> longer than everything else here. Start it early.

### 2. Transfer and verify

Copy the archive to the workstation on removable media and extract it, for
example to `C:\Tools\mkr\`. Then verify it arrived intact:

```powershell
cd C:\Tools\mkr
Get-FileHash .\mkr.exe -Algorithm SHA256
Get-Content .\SHA256SUMS | Select-String mkr.exe
```

The two hashes must match. If they do not, the transfer is corrupt — do not run
the binary.

### 3. Put it on PATH

```powershell
# Current user, persists across sessions:
[Environment]::SetEnvironmentVariable(
    "PATH", "$env:PATH;C:\Tools\mkr", "User")
```

Open a new terminal, then confirm:

```powershell
mkr version
```

### 4. Validate the machine

Before configuring anything, check that this workstation can actually run it:

```powershell
mkr selftest --skip-endpoint
```

```
mkr 1.1.0 self-test

  [PASS] platform         windows/amd64, go go1.23.12
  [PASS] config paths     config C:\Users\you\AppData\Roaming\mkr, data C:\Users\you\AppData\Local\mkr
  [PASS] shell            pwsh runs commands
  [PASS] terminal         interactive terminal
  [PASS] workspace jail   10 escape attempts refused
  [PASS] audit log        chain written, verified, and tampering detected
  [PASS] redaction        synthetic credential scrubbed

7 of 7 checks passed
```

This works **before** the server or firewall rule exist, which makes it the
right first step. It exits non-zero if a required check fails, so it can gate an
automated rollout.

If `shell` fails, PowerShell 7 is missing. Either install it, or
`mkr config set shell powershell.exe` to use Windows PowerShell 5.1.

### 5. Point it at the server

```powershell
mkr config set endpoint http://<inference-host>:8000
mkr probe
```

A successful probe reports the served model, its context length, and the
tool-calling mode in use. See [Configuration](CONFIGURATION.md) for every
setting and how to deploy a shared configuration across a team.

---

## Server (Ubuntu GPU host)

The full runbook with commands is in **[deploy/README.md](../deploy/README.md)**.
The summary:

### Why a container image, not a wheelhouse

vLLM, PyTorch and the CUDA runtime are roughly ten gigabytes of tightly coupled
wheels. Resolving that graph offline with `pip download` is the single most
likely step to fail on a media transfer, and it typically fails at the end,
after the weights are already copied.

Transferring a container image reduces the whole problem to one file with one
checksum.

### On the connected side

```bash
docker pull vllm/vllm-openai:v0.11.0
docker save vllm/vllm-openai:v0.11.0 | gzip > vllm.tar.gz

hf download Qwen/Qwen3-Coder-30B-A3B-Instruct \
    --local-dir ./models/Qwen/Qwen3-Coder-30B-A3B-Instruct

sha256sum vllm.tar.gz > SHA256SUMS
find ./models -type f -exec sha256sum {} \; >> SHA256SUMS
```

**Media sizing.** Use exFAT or ext4 — FAT32 cannot hold the individual
safetensors shards.

| Item | Approximate size |
|---|---|
| vLLM container image | ~10 GB |
| Qwen3-Coder-30B-A3B (FP8) | ~30 GB |
| Qwen3-Coder-30B-A3B (BF16) | ~60 GB |
| Qwen3-Coder-480B-A35B (FP8) | ~500 GB |

### On the air-gapped side

```bash
sha256sum -c SHA256SUMS            # verify before anything else
gunzip -c vllm.tar.gz | docker load
sudo mkdir -p /opt/models && sudo cp -r ./models/* /opt/models/
./deploy/profiles/serve.sh small
```

Then confirm locally before involving the network:

```bash
curl -s http://localhost:8000/v1/models
```

`HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1` are set in the serve script, so
the container fails loudly rather than silently attempting to reach the internet
if a path is wrong.

---

## Network

One inbound TCP allow rule, client segment to server, one direction. Nothing
else — no outbound internet from either side, no reverse path, no other ports.

**[deploy/NETWORK-REQUIREMENT.md](../deploy/NETWORK-REQUIREMENT.md)** is written
to be handed straight to a network team, including the "what is explicitly NOT
required" section that gets narrow requests approved quickly.

This usually has the longest lead time of anything in the deployment. Submit it
first.

---

## Verifying the whole path

Once client, server and network are all in place:

```powershell
mkr selftest          # every check, including the endpoint
```

Then a real session:

```powershell
cd C:\repos\some-project
mkr -mode plan "explain how this project is structured"
```

Plan mode is read-only, so this is a safe first exercise: it proves the model
can read your code and answer, without any possibility of changing anything.

---

## Upgrading

The client is one file. Replace `mkr.exe` with the new one and verify its
checksum. Configuration, sessions and audit logs are untouched, and the audit
chain continues across versions.

```powershell
mkr version
mkr selftest --skip-endpoint
```

---

## Uninstalling

Delete the directory containing `mkr.exe` and remove it from `PATH`. To remove
user data as well:

```powershell
Remove-Item -Recurse $env:APPDATA\mkr        # configuration
Remove-Item -Recurse $env:LOCALAPPDATA\mkr   # sessions, audit logs, rules
```

Consider retaining the audit log if your retention policy requires it.
