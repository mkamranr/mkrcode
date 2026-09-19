# mkr

An agentic coding assistant for air-gapped environments. It runs against a
locally hosted vLLM server and contacts nothing else.

`mkr` is a single statically linked executable with **zero external
dependencies** — no installer, no runtime, no DLLs, no `go.sum`. That is the
whole point: it can be carried in on removable media and run.

```
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

## What it does

- Conversational coding against your repository, with file reading, editing,
  search and shell execution
- **Three permission modes** — `plan` (read-only), `approve` (asks every time),
  `auto` (unattended)
- **Workspace containment** — no path outside the working directory is
  reachable, including through symlinks, UNC paths, or Windows device names
- **Egress restriction enforced in code** — the binary's only HTTP client
  refuses to dial anything but the configured endpoint
- **Secret redaction** — credentials are scrubbed from tool output before they
  reach the model
- **Tamper-evident audit log** — hash-chained JSONL of every prompt, tool call,
  permission decision and file diff, stored owner-readable only
- **Automatic context compaction** — long sessions stay inside the model's
  window instead of failing mid-task

## Architecture

```
cmd/mkr/              entry point, REPL, slash commands
internal/
  config/             layered configuration
  provider/           vLLM client, SSE streaming, tool-call adapters, probe
    mock/             scriptable fake server — the whole test strategy
  agent/              the conversation loop, system prompt, context budgeting
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

Every tool call passes the same gate in the same order: **permission →
execution → redaction → audit**. That ordering is enforced once, in
`internal/agent`, rather than in each tool.

### Tool calling adapts to the server

The served model and vLLM's `--tool-call-parser` flag are not known in advance,
and at least one target model postdates available documentation. So `mkr` does
not assume: at startup it asks the endpoint to make one real tool call. If that
works it uses the native OpenAI `tools` protocol; if it does not, it falls back
to carrying schemas in the system prompt and parsing calls out of the text
stream. A misconfigured server degrades instead of failing.

The fallback parser accepts both the Hermes JSON form and Qwen3-Coder's native
`<function=…><parameter=…>` form, and reassembles tags split across any stream
chunk boundary.

### Context budgeting without a tokenizer

vLLM reports `prompt_tokens` for every request, which is the exact size of the
transcript just sent. `mkr` counts characters and continuously recalibrates its
chars-per-token ratio against that measurement, converging on a figure correct
for the specific model and codebase in use. That is why there is no tokenizer
dependency.

When the transcript approaches the window it is compacted: the bodies of older
tool results are elided first, then whole exchanges are dropped. An assistant
message carrying `tool_calls` and the `tool` messages answering it are always
kept or dropped together — a dangling `tool_call_id` is rejected by vLLM with a
400, which would turn a context problem into a hard failure.

## Building

```
make check          # vet, tests, and the egress invariant lint
make windows        # dist/mkr.exe for the air-gapped clients
make bundle-windows # the shippable zip with checksums
```

Requires Go 1.23+. `CGO_ENABLED=0` throughout, so the binary is static and the
build needs no C toolchain.

## Developing without a GPU

The entire system is testable with no GPU and no network:

```
make mockserver
./dist/mockserver -addr 127.0.0.1:8000 -scenario demo &
./dist/mkr -endpoint http://127.0.0.1:8000 -mode auto -p "improve greet.py"
```

`-scenario` accepts `demo` (a full tool-using session), `secret` (exercises
redaction) and `egress` (exercises the command deny list).

## Deployment

See `deploy/README.md` for the media transfer procedure, `deploy/SIZING.md` for
GPU arithmetic, and `deploy/NETWORK-REQUIREMENT.md` for the one firewall rule
this needs — submit that first, it has the longest lead time.

Operator documentation is in `docs/OPERATOR-GUIDE.md`.

## Security posture, stated plainly

What is enforced in code: workspace containment, the single permitted network
destination, deny rules in every mode, read-before-edit, the audit chain, and
owner-only access to the audit log and transcripts (explicit DACLs on Windows,
where file mode bits have no effect).

What is mitigated but not enforced: egress from commands the operator approves,
which run with the operator's own network access. The deny list refuses the
common egress utilities, and every command is recorded before it runs, but
genuine enforcement requires host firewall policy outside this program.

What is heuristic: secret redaction is pattern-based and will miss unusual
credential formats.

What the audit chain does and does not prove: altering or deleting a record is
detectable; truncating the end of the file is not, from the file alone. Pair it
with a write-only collector if that matters.
