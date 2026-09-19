# Configuration

- [Where settings come from](#where-settings-come-from)
- [The config command](#the-config-command)
- [Settings reference](#settings-reference)
- [Environment variables](#environment-variables)
- [Command-line flags](#command-line-flags)
- [Per-project configuration](#per-project-configuration)
- [Permission rules](#permission-rules)
- [Project instructions: MKR.md](#project-instructions-mkrmd)
- [Deploying a fleet](#deploying-a-fleet)
- [File locations](#file-locations)

---

## Where settings come from

Four layers. Later ones win:

```
built-in defaults
  └─ user config        %APPDATA%\mkr\config.json
      └─ project config  <workspace>\.mkr\config.json
          └─ environment  MKR_*
              └─ flags     -endpoint, -mode, …
```

This lets a team commit a shared `.mkr/config.json` to a repository while an
individual overrides one setting for one session with a flag.

To see which layer supplied a value — the question that actually gets asked when
something is not what you expect:

```console
$ mkr config show
resolved configuration

  endpoint       http://gpu-01:8000   (from the user config)
  model          (from /v1/models)
  mode           auto   (from MKR_MODE)
  adapter        auto
  shell          pwsh
  max_model_len  262144
  max_turns      40
  redact         true
  api_key        (not set)

  workspace      C:\repos\my-project
  audit log      C:\Users\you\AppData\Local\mkr\audit\mkr-audit.jsonl
  sessions       C:\Users\you\AppData\Local\mkr\sessions
  rules          C:\Users\you\AppData\Local\mkr\rules.json
```

---

## The `config` command

```console
mkr config show                        # resolved settings and their sources
mkr config path                        # where the files live
mkr config set endpoint http://gpu-01:8000
mkr config set mode auto
mkr config unset mode
```

`set` writes to the **user** config file, preserving every other key. Values are
validated as they are typed, so a mistake is caught immediately rather than
surfacing later as a confusing connection error:

```console
$ mkr config set endpoint gpu-01:8000
mkr: endpoint scheme must be http or https, got "gpu-01"
```

Writes are atomic and the file is created owner-readable only, because it can
contain an API key.

---

## Settings reference

| Key | Type | Default | Meaning |
|---|---|---|---|
| `endpoint` | URL | `http://localhost:8000` | vLLM base URL, without `/v1`. **The only network destination the binary may reach.** |
| `api_key` | string | *(none)* | Bearer token, if vLLM was started with `--api-key`. |
| `model` | string | *(from endpoint)* | Served model ID. Empty means whatever `/v1/models` reports. |
| `adapter` | `auto` \| `native` \| `xml` | `auto` | Tool-calling strategy. `auto` probes the endpoint and chooses. |
| `mode` | `plan` \| `approve` \| `auto` | `approve` | Starting permission mode. See [Usage](USAGE.md#permission-modes). |
| `shell` | path | `pwsh` on Windows | Executable used by the `exec` tool. |
| `max_model_len` | integer | *(from endpoint)* | Context window to assume. Lower it to keep sessions fast. |
| `max_turns` | integer | `40` | Maximum tool-use turns for a single request. |
| `max_tokens` | integer | *(server decides)* | Cap on a single completion. |
| `temperature` | float | `0.2` | Sampling temperature. |
| `top_p` | float | `0.95` | Nucleus sampling. |
| `redact` | boolean | `true` | Scrub credentials from tool output. Disabling is audited. |
| `audit_prompts` | boolean | `false` | Record full prompt bodies, not just hashes. |
| `audit_path` | path | *(data dir)* | Audit log destination. |
| `session_dir` | path | *(data dir)* | Where transcripts are stored. |
| `rules_path` | path | *(data dir)* | Allow/deny rules file. |
| `request_timeout` | duration | `5m` | Per-request timeout, e.g. `"90s"`. |
| `exec_timeout` | duration | `2m` | Per-command timeout. |

### Tuning `max_model_len`

If your server offers a very large window (Qwen3-Coder serves 262,144 tokens),
that is more headroom than most coding work needs. Every request re-sends the
whole transcript, so a large context makes each turn slower and consumes more
server memory, reducing how many people can work at once.

```console
mkr config set max_model_len 65536
```

This changes only what the client sends. It does not restart or reconfigure the
server, and different developers can choose different values against the same
endpoint. Most coding work fits comfortably in 32k–64k.

---

## Environment variables

Useful for CI, containers, or a login script that configures a whole fleet.

| Variable | Equivalent setting |
|---|---|
| `MKR_ENDPOINT` | `endpoint` |
| `MKR_API_KEY` | `api_key` |
| `MKR_MODEL` | `model` |
| `MKR_MODE` | `mode` |
| `MKR_ADAPTER` | `adapter` |
| `MKR_SHELL` | `shell` |
| `MKR_MAX_MODEL_LEN` | `max_model_len` |
| `MKR_MAX_TURNS` | `max_turns` |
| `MKR_REDACT` | `redact` |
| `MKR_AUDIT_PATH` | `audit_path` |
| `NO_COLOR` | Disables coloured output. |

---

## Command-line flags

Flags win over everything and apply to one invocation.

```
-endpoint URL     vLLM base URL
-model ID         served model ID
-mode MODE        plan | approve | auto
-adapter NAME     auto | native | xml
-C DIR            workspace directory (default: current directory)
-p                print mode: run one prompt and exit
-resume ID        resume a session by ID, or "last"
-no-redact        disable secret redaction (audited)
-timeout DUR      per-request timeout
-skip-endpoint    selftest only: skip the endpoint check
-json             selftest only: emit the report as JSON
```

---

## Per-project configuration

Commit `.mkr/config.json` to a repository so everyone working on it shares the
same settings:

```json
{
  "model": "Qwen/Qwen3-Coder-30B-A3B-Instruct",
  "mode": "approve",
  "max_model_len": 65536,
  "exec_timeout": "5m"
}
```

Do **not** put `api_key` in a committed file. Use the user config or
`MKR_API_KEY`.

---

## Permission rules

Beyond the three modes, an allow/deny rule set constrains which paths may be
written and which commands may be run. **Deny always wins, in every mode
including `auto`.**

The file lives at `%LOCALAPPDATA%\mkr\rules.json`. Anything you add is appended
to the built-in defaults rather than replacing them, so adding one rule does not
silently lose the others.

```json
{
  "deny_commands": [
    "(?i)\\bterraform\\s+destroy\\b",
    "(?i)\\bkubectl\\s+delete\\b"
  ],
  "deny_paths": [
    "config/production/**",
    "**/*.prod.yaml"
  ],
  "allow_commands": [],
  "auto_allow_tools": ["read_file", "list_dir", "glob", "grep"]
}
```

| Key | Effect |
|---|---|
| `deny_commands` | Regular expressions matched against the command line. A match refuses it in every mode. |
| `deny_paths` | Globs matched against write paths, case-insensitively. |
| `allow_commands` | If non-empty, **only** matching commands may run. A strict allowlist. |
| `auto_allow_tools` | Tools that never prompt, even in `approve` mode. |

Built-in defaults already refuse network utilities (`curl`, `wget`,
`Invoke-WebRequest`, `bitsadmin`, `ssh`, `scp`), credential access, security
control tampering, and writes to `.ssh`, `.git/hooks`, `*.pem`, `*.key`, and
mkr's own configuration.

An invalid regular expression is reported at startup, not silently ignored.

---

## Project instructions: MKR.md

Put an `MKR.md` in a repository root and it is loaded into every session for
that project. Use it for what a new team member would need to be told:

```markdown
# Project notes

- Build with `dotnet build`, test with `dotnet test`.
- Do not edit anything under `Generated/` — it comes from the schema compiler.
- Follow the existing style: tabs, and no comments on self-evident code.
- The integration tests need the local database running; skip them if it is not.
```

A personal `MKR.md` in your user config directory applies to every project, for
preferences that are yours rather than the project's.

Both are capped at 32 KB so an oversized file cannot crowd out the conversation.

---

## Deploying a fleet

The simplest approach is a login script or configuration management step that
sets the environment variable:

```powershell
[Environment]::SetEnvironmentVariable(
    "MKR_ENDPOINT", "http://gpu-01:8000", "Machine")
```

Or ship a prepared user config file to each profile. Either way, verify with:

```powershell
mkr selftest --json
```

which exits non-zero on failure and emits a machine-readable report suitable for
attaching to a deployment ticket.

---

## File locations

| | Windows | Linux / macOS |
|---|---|---|
| User config | `%APPDATA%\mkr\config.json` | `~/.config/mkr/config.json` |
| Project config | `<workspace>\.mkr\config.json` | same |
| Sessions | `%LOCALAPPDATA%\mkr\sessions\` | `~/.mkr/sessions/` |
| Audit log | `%LOCALAPPDATA%\mkr\audit\mkr-audit.jsonl` | `~/.mkr/audit/…` |
| Rules | `%LOCALAPPDATA%\mkr\rules.json` | `~/.mkr/rules.json` |
| User instructions | `%APPDATA%\mkr\MKR.md` | `~/.config/mkr/MKR.md` |

`mkr config path` prints these for the machine you are on.

All of them are created owner-readable only. On Windows that is an explicit
ACL, because file mode bits have no effect there.
