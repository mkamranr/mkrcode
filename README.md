<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
  <img src="docs/assets/logo.svg" alt="MKR Code" width="372">
</picture>

**An agentic coding assistant for air-gapped environments.**

Runs against a self-hosted vLLM server. Contacts nothing else — by design, and enforced in code.

<br>

[Installation](docs/INSTALLATION.md) · [Configuration](docs/CONFIGURATION.md) · [Usage](docs/USAGE.md) · [Skills](docs/SKILLS.md) · [Security](docs/SECURITY.md) · [Deployment](deploy/README.md)

*Evaluating it first? → [Testing locally on macOS or Linux](docs/TESTING-LOCALLY.md)*

</div>

---

```console
$ mkr config set endpoint http://gpu-01:8000
$ mkr probe
endpoint:     http://gpu-01:8000
egress:       restricted to gpu-01:8000
model:        Qwen/Qwen3-Coder-30B-A3B
context:      262144 tokens
native tools: true
adapter:      native

$ mkr "add input validation to the login handler"
→ read src/auth.go
  ✓ read src/auth.go (142 lines)

permission required: edit src/auth.go
  allow? [y]es / [n]o / [a]lways / [q]uit: y
→ edit src/auth.go
  ✓ edited src/auth.go
→ go test ./...
  ✓ exec (exit 0, 2.1s)

Added a length check and a nil guard. Tests pass.
```

## Why this exists

Commercial coding assistants require outbound internet to a vendor API. Inside a
secure enclave that is not permissible, so developers there get no agentic
assistance at all.

`mkr` is the same working loop — conversation, file reading and editing, shell
execution, approval prompts — pointed at a model you host yourself, with every
action audited and no route for data to leave.

## What you get

| | |
|---|---|
| **Single static binary** | One `mkr.exe`. No installer, no runtime, no DLLs, **zero external dependencies**. |
| **Three permission modes** | `plan` is read-only, `approve` asks before every change, `auto` runs unattended. |
| **Workspace containment** | No path outside the working directory is reachable — including via symlinks, UNC paths, and Windows device names. |
| **Egress locked in code** | The binary's only HTTP client refuses to dial anything but the configured endpoint. |
| **Secret redaction** | Credentials are scrubbed from tool output *before* it reaches the model. |
| **Tamper-evident audit** | Hash-chained log of every prompt, tool call, decision and diff. Owner-readable only. |
| **Context compaction** | Long sessions stay inside the model's window instead of failing mid-task. |
| **Skills and sub-agents** | Reusable markdown instruction packs, and delegated work with isolated context. |

## Quick start

On a connected machine, download the release, then carry it in:

```powershell
# 1. Verify the binary
Get-FileHash .\mkr.exe -Algorithm SHA256     # compare against SHA256SUMS

# 2. Check the machine can run it — works before the server exists
mkr selftest --skip-endpoint

# 3. Point it at your vLLM host
mkr config set endpoint http://<inference-host>:8000
mkr probe

# 4. Use it
cd C:\repos\my-project
mkr
```

Full instructions: **[docs/INSTALLATION.md](docs/INSTALLATION.md)**

## Everyday use

```console
mkr                                   # interactive session in this directory
mkr "fix the null check in login.cs"  # one request, then stay interactive
mkr -p "summarise this module"        # one request, print and exit
mkr -mode plan "how does auth work?"  # read-only investigation
mkr -mode auto "run tests and fix"    # unattended
mkr -resume last                      # continue where you left off
```

In a session: `/mode`, `/tools`, `/cost`, `/compact`, `/audit`, `/clear`, `/help`.
`Ctrl+C` interrupts a turn, `Ctrl+D` exits.

See **[docs/USAGE.md](docs/USAGE.md)**.

## Architecture

```
cmd/mkr/              entry point, REPL, subcommands
internal/
  config/             layered configuration
  provider/           vLLM client, SSE streaming, tool-call adapters, probe
    mock/             scriptable fake server — the whole test strategy
  agent/              conversation loop, system prompt, context budgeting, sub-agents
  skills/             discovery of markdown instruction packs
  tools/              read, write, edit, list, glob, grep, exec
  fsjail/             workspace path containment (the security boundary)
  permission/         three modes, allow/deny rules, approval prompts
  netguard/           the only http.Client in the binary
  redact/             secret scrubbing
  audit/              hash-chained append-only log
  secureio/           owner-only file creation (ACLs on Windows)
  session/            transcript persistence and resume
  ui/                 renderer interface and terminal implementation
deploy/               vLLM serve profiles, GPU sizing, network requirement
devtools/mockserver/  dev-only fake vLLM (build-tagged, never shipped)
```

Every tool call passes the same gate in the same order:
**permission → execution → redaction → audit.** That ordering is enforced once,
in `internal/agent`, rather than in each tool.

### Tool calling adapts to the server

The served model and vLLM's `--tool-call-parser` flag are not known in advance.
So `mkr` does not assume: at startup it asks the endpoint to make one real tool
call. If that works it uses the native OpenAI `tools` protocol; if not, it falls
back to carrying schemas in the system prompt and parsing calls out of the text
stream. **A misconfigured server degrades instead of failing.**

The fallback parser accepts both the Hermes JSON form and Qwen3-Coder's native
`<function=…>` form, and reassembles tags split across any stream chunk boundary.

### Context budgeting without a tokenizer

vLLM reports `prompt_tokens` on every response — the exact size of the transcript
just sent. `mkr` counts characters and continuously recalibrates its
chars-per-token ratio against that measurement, converging on a figure correct
for the model and codebase in use. That is why there is no tokenizer dependency.

## Installing

Download a release archive for [Windows or macOS](https://github.com/mkamranr/mkrcode/releases),
or install with Go:

```bash
go install github.com/mkamranr/mkrcode/cmd/mkr@latest
```

Full instructions, including the air-gapped transfer, are in
[docs/INSTALLATION.md](docs/INSTALLATION.md).

## Building from source

Requires Go 1.23+. `CGO_ENABLED=0` throughout, so the binary is static and the
build needs no C toolchain and no network — the module graph is empty, so
`GOPROXY=off` builds work.

```bash
make check           # vet, tests, race, and the egress invariant lint
make windows         # dist/mkr.exe
make bundle-windows  # the shippable zip with checksums
```

### Developing without a GPU

The entire system is testable with no GPU and no network:

```bash
make mockserver
./dist/mockserver -addr 127.0.0.1:8000 -scenario demo &
./dist/mkr -endpoint http://127.0.0.1:8000 -mode auto -p "improve greet.py"
```

`-scenario` accepts `demo` (a full tool-using session), `secret` (exercises
redaction) and `egress` (exercises the command deny list).

## Security posture, stated plainly

**Enforced in code:** workspace containment · the single permitted network
destination · deny rules in every mode · read-before-edit · the audit hash chain
· owner-only access to logs and transcripts.

**Mitigated, not enforced:** egress from commands the operator approves, which
run with the operator's own network access. The deny list refuses the common
egress utilities and every command is recorded before it runs, but genuine
enforcement requires host firewall policy outside this program.

**Heuristic:** secret redaction is pattern-based and will miss unusual formats.

**Deliberately absent:** MCP. An MCP server is third-party code running with the
agent's privileges that can open its own network connections, bypassing the
egress restriction, and is typically a Node or Python package that would end the
zero-dependency property. See [docs/SKILLS.md](docs/SKILLS.md#why-there-is-no-mcp-support).

**What the audit chain proves:** altering or deleting a record is detectable.
Truncating the end of the file is not, from the file alone — pair it with a
write-only collector if that matters.

Full detail in **[docs/SECURITY.md](docs/SECURITY.md)**.

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Apache 2.0 includes an express grant of patent rights from contributors, and
requires that the `NOTICE` file be carried forward in redistributions.

## Documentation

| Document | Covers |
|---|---|
| [Installation](docs/INSTALLATION.md) | Getting the binary onto air-gapped machines, and the server up |
| [Configuration](docs/CONFIGURATION.md) | Every setting, where it can be set, and precedence |
| [Usage](docs/USAGE.md) | Daily use, permission modes, commands, project memory |
| [Skills](docs/SKILLS.md) | Writing and installing skills, delegating to sub-agents |
| [Security](docs/SECURITY.md) | The security model and its stated limits |
| [Testing locally](docs/TESTING-LOCALLY.md) | Trying it on macOS or Linux against a local model or hosted API |
| [Deployment](deploy/README.md) | Media transfer, vLLM serve profiles, GPU sizing |
| [Network requirement](deploy/NETWORK-REQUIREMENT.md) | The one firewall rule, written for a network team |
