# mkr operator guide

`mkr` is a coding assistant that runs entirely inside the enclave. It talks to a
locally hosted vLLM server and never contacts anything else.

## First run

```powershell
setx MKR_ENDPOINT "http://<inference-host>:8000"
mkr probe                  # confirm the endpoint is reachable and usable
cd C:\path\to\your\repo
mkr                        # start an interactive session in this directory
```

The directory you start in is the **workspace**. Every file the assistant can
read or write must be inside it. Paths outside are refused, including via
symbolic links, UNC paths and `..`.

## Permission modes

Mode is the main control. Choose it deliberately.

| Mode | Reads | Writes and commands | When to use |
|---|---|---|---|
| `plan` | yes | **refused outright** | Understanding unfamiliar code, or getting a change proposed before anyone touches anything |
| `approve` | yes | asks you every time | **The default.** Normal day-to-day work |
| `auto` | yes | runs without asking | A well-defined, repetitive task you have already scoped |

```powershell
mkr -mode plan "explain how authentication works in this service"
mkr "add validation to the login handler"          # approve mode
mkr -mode auto "run the tests and fix the failures"
```

Change mode mid-session with `/mode auto`. Changing mode clears any "always
allow" answers you had given, so permissions never carry silently across a
change of posture.

## Answering a permission prompt

```
permission required: write src/auth.go (412 bytes)
  writes:   src/auth.go
  allow? [y]es / [n]o / [a]lways / [q]uit:
```

- `y` — allow this one action
- `n` — refuse it. The assistant is told why and will propose something else
- `a` — allow this exact action for the rest of the session
- `q` — end the session now

Pressing Enter with nothing typed **refuses**. That is deliberate: a prompt
that is easy to accept by accident is not a control.

## Commands

| Command | Effect |
|---|---|
| `/mode [plan\|approve\|auto]` | Show or change the permission mode |
| `/tools` | List the available tools and which of them change things |
| `/cost` | Token usage and how full the context window is |
| `/compact` | Reduce the transcript now, to free context |
| `/audit` | Verify the audit log's integrity |
| `/clear` | Start a fresh conversation, keeping the session |
| `/help` | Command list |
| `/exit` | End the session |

`Ctrl+C` interrupts the current turn and returns you to the prompt. `Ctrl+D`
ends the session.

## Project instructions: MKR.md

Put an `MKR.md` in the root of a repository and it is loaded into every session
for that project. Use it for the things a newcomer would need to be told:

```markdown
# Project notes for mkr

- Build with `dotnet build`, test with `dotnet test`.
- Do not edit anything under `Generated/`; it comes from the schema compiler.
- Follow the existing repository style: tabs, and no comments on obvious code.
```

A personal `MKR.md` in your user config directory applies to every project.

## Long sessions and the context window

Models have a fixed context window. As a session goes on, the transcript grows
until it would no longer fit, and the assistant would otherwise fail mid-task
with a server error.

`mkr` handles this automatically. As the window fills it compacts the
conversation, and tells you when it does:

```
context compaction: elided 9 older tool result(s) (~24100 to ~14800 tokens)
```

It drops the least useful history first: the *contents* of files read many
turns ago, keeping the recent ones intact. If that is not enough it drops the
oldest exchanges. Your system instructions, your current request and the recent
conversation are always kept.

Check how full the window is at any time with `/cost`, and compact early with
`/compact` if you are about to give a long instruction.

If the assistant seems to have forgotten something from earlier in a long
session, that is compaction. Ask it to read the file again, or start a fresh
session with `/clear`.

## Checking the installation

```powershell
mkr selftest
```

This validates the machine rather than the code: that the shell runs commands,
that config and data directories resolve and are writable, that the workspace
jail refuses escape paths, that the audit log can be written and tampering
detected, that redaction fires, and that the endpoint is reachable.

Use `mkr selftest --skip-endpoint` before the firewall rule exists, and
`mkr selftest --json` if you need to attach the result to a ticket. It exits
non-zero if any required check fails.

## What is recorded

Everything. Each session appends to a tamper-evident audit log: every prompt,
every model request, every tool call with its arguments, every permission
decision, and a diff of every file change.

```powershell
mkr audit verify           # confirm nothing has been altered
```

The log is hash-chained, so editing or deleting a record is detectable. Note
that truncating the end of the file is not detectable from the file alone; if
that matters, ship the log to a write-only collector.

## Secret redaction

Tool output is scanned for credentials before it reaches the model. Private
keys, cloud access keys, tokens, JWTs and password assignments are replaced with
`[REDACTED:kind]`. You will see a note on the tool result when this happens.

This reduces exposure; it does not eliminate it. Detection is pattern-based and
will miss an unusual credential format. Keep secrets out of the workspace.

`-no-redact` disables it. Doing so is recorded in the audit log and warned about
on screen.

## What the assistant cannot do

- Read or write outside the workspace directory
- Reach any network destination other than the configured vLLM endpoint
- Run `curl`, `wget`, `Invoke-WebRequest`, `ssh`, `scp` and similar, in any mode
- Modify its own configuration or rules files
- Write to `.ssh`, `.git/hooks`, or files matching `*.pem`, `*.key`, `*.pfx`

The last three are enforced by rules in `rules.json` in your user data
directory. Deny rules apply in every mode, including `auto`.

**One honest limitation.** The network restriction constrains the assistant
itself, which cannot construct a connection to anywhere but the endpoint. It
does not constrain a command you approve: that runs with your own network
access. The deny list above narrows the gap, but if egress must be genuinely
enforced, that is a host firewall policy, not something this program can do.

## Troubleshooting

**`probe: cannot list models: connection refused`** — vLLM is not running, or
the firewall rule is not in place. Check from the inference host first with
`curl http://localhost:8000/v1/models`.

**`adapter: xml` in the probe output** — the client is working, but vLLM's
tool-call parser is not producing structured calls for this model. Check
`--tool-call-parser` on the server. The assistant still works; it is slightly
slower and slightly less reliable at tool calls.

**`cannot run pwsh`** — PowerShell 7 is not installed. Either install it, or set
`"shell": "powershell.exe"` in your `config.json` to use Windows PowerShell 5.1.

**The assistant keeps asking permission for the same thing** — answer `a`
instead of `y`, or switch to `-mode auto` for that task.
