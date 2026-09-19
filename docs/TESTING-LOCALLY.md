# Testing on macOS or Linux against a hosted API

`mkr` targets a self-hosted vLLM server, but it speaks the plain
OpenAI-compatible protocol, so any compatible endpoint works for evaluation —
a hosted provider's free tier, or a model running locally on your own machine.

This is for trying the tool out. It is not how the air-gapped deployment works,
and some of it is the opposite of what you would do there.

- [Install](#install)
- [Option A: a local model](#option-a-a-local-model-recommended)
- [Option B: a hosted API](#option-b-a-hosted-api)
- [Two things that differ from vLLM](#two-things-that-differ-from-vllm)
- [Try it](#try-it)
- [Troubleshooting](#troubleshooting)

---

## Install

If you have Go installed, one command is enough:

```bash
go install github.com/mkamranr/mkrcode/cmd/mkr@latest
```

Otherwise download the macOS archive from the Releases page and extract it. The
binary is universal — the same file runs on Intel and Apple silicon.

```bash
tar xzf mkr-darwin-1.2.0.tar.gz
shasum -a 256 -c SHA256SUMS        # verify before running
```

**macOS will quarantine a downloaded binary.** Gatekeeper blocks unsigned
executables that arrive from the internet, with a message about an unidentified
developer. Clear the flag:

```bash
xattr -d com.apple.quarantine ./mkr
./mkr version
```

Then put it somewhere on `PATH`:

```bash
mkdir -p ~/bin && mv ./mkr ~/bin/ && export PATH="$HOME/bin:$PATH"
```

Check the installation before configuring anything:

```bash
mkr selftest --skip-endpoint
```

---

## Option A: a local model (recommended)

Running a model on your own machine is the closest analogue to the real
deployment: a self-hosted OpenAI-compatible server, no API key, no data leaving
the machine. It also exercises the same code path the enclave will use.

### Ollama

```bash
brew install ollama
ollama serve &                      # serves an OpenAI-compatible API on 11434

# A small coding model with tool-calling support.
ollama pull qwen2.5-coder:7b
```

Point `mkr` at it. Note the base URL excludes `/v1` — `mkr` appends that:

```bash
mkr config set endpoint http://localhost:11434
mkr config set model qwen2.5-coder:7b
mkr config set max_model_len 32768
mkr probe
```

### llama.cpp

`llama-server` exposes the same protocol:

```bash
llama-server -m <model.gguf> --port 8080 --jinja
mkr config set endpoint http://localhost:8080
```

`--jinja` matters: it enables the chat template, without which tool calling
will not work and `mkr` will fall back to the XML adapter.

---

## Option B: a hosted API

Any provider offering an OpenAI-compatible endpoint works. Several have free
tiers; availability and limits change, so check the provider's current terms.

The **base URL excludes `/v1`**, because `mkr` appends the path itself. Getting
this wrong produces a 404 that looks like an outage:

| If the full endpoint is | Set this as the base URL |
|---|---|
| `https://api.example.com/v1/chat/completions` | `https://api.example.com` |
| `https://api.example.com/openai/v1/chat/completions` | `https://api.example.com/openai` |

```bash
mkr config set endpoint https://api.example.com
mkr config set api_key sk-...
mkr config set model <exact-model-id>
mkr config set max_model_len 32768
mkr probe
```

> **Before you do this, consider what you are sending.** A hosted API receives
> the contents of files the agent reads. Use a throwaway or public repository to
> evaluate the tool, never anything sensitive. This is exactly the exposure the
> air-gapped deployment exists to prevent.

### Both schemes work

`http://` and `https://` are both supported, and the port defaults from the
scheme. A local model is usually `http://localhost:11434`; a hosted provider is
`https://...` on 443. If you get the scheme wrong, `mkr` says so and tells you
which to use.

### The egress restriction still applies

`mkr` will reach the endpoint you configured **and nothing else**. That is the
same protection as in the enclave, just pointed somewhere different.

If your machine requires an HTTP proxy to reach the internet, `mkr` will not
work against a hosted API: no proxy is consulted, deliberately, because honouring
proxy environment variables would be a second route out. A local model has no
such problem.

---

## Two things that differ from vLLM

**No reported context window.** `max_model_len` is a vLLM extension. Other
servers do not report it, so automatic context compaction has nothing to size
itself against and stays off. `mkr probe` says so. Set it yourself:

```bash
mkr config set max_model_len 32768
```

Without this a long session will grow until the provider rejects it.

**Tool calling may be weaker.** `mkr probe` reports which adapter it selected.
`native` means the server produced a structured tool call. `xml` means it did
not, and `mkr` fell back to parsing tool calls out of the text — everything
still works, slightly less reliably. Small local models are more likely to land
on `xml`, and are generally less consistent at tool use than the 30B-class model
the real deployment targets.

Judge the tool's behaviour, not the model's, when evaluating on a small model.

---

## Try it

```bash
mkdir -p /tmp/mkr-demo && cd /tmp/mkr-demo
cat > greet.py <<'EOF'
def greet(name):
    return "Hello " + name
EOF

mkr -mode plan "what does this project do?"     # read-only, safe first run
mkr "add validation so greet rejects an empty name"
mkr -mode auto "run python3 -c 'import greet' and fix anything broken"
```

Worth exercising, because these are the parts that matter in production:

```bash
mkr audit verify        # the hash chain
/skills                 # after copying examples/skills into .mkr/skills/
/cost                   # context use
/mode plan              # switch posture mid-session
```

Plant a fake credential in a file and ask the agent to read it — the value
should come back as `[REDACTED:...]`. Ask it to run `curl` — it should be
refused by policy in every mode.

---

## Troubleshooting

**`connection refused`** — the server is not running. Check with
`curl http://localhost:11434/v1/models`.

**`404` from a hosted provider** — the base URL probably includes `/v1`. Remove
it; `mkr` appends the path.

**`401`** — no API key, or the wrong one. `mkr config set api_key ...`.

**`adapter: xml`** — the model or server did not produce a structured tool call.
Sessions still work. For llama.cpp, add `--jinja`.

**Sessions fail after a while** — no context window is configured. Set
`max_model_len`.

**`"killed" or Gatekeeper warning`** — the quarantine flag.
`xattr -d com.apple.quarantine ./mkr`.
