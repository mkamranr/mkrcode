# Usage

- [Starting a session](#starting-a-session)
- [Permission modes](#permission-modes)
- [Answering a permission prompt](#answering-a-permission-prompt)
- [Commands](#commands)
- [Tools](#tools)
- [Long sessions and the context window](#long-sessions-and-the-context-window)
- [Resuming work](#resuming-work)
- [Project instructions](#project-instructions)
- [Non-interactive use](#non-interactive-use)
- [What is recorded](#what-is-recorded)
- [Secret redaction](#secret-redaction)
- [Getting good results](#getting-good-results)
- [Troubleshooting](#troubleshooting)

---

## Starting a session

```powershell
cd C:\repos\my-project
mkr
```

The directory you start in is the **workspace**. Every file the assistant can
read or write must be inside it — paths outside are refused, including through
symbolic links, UNC paths and `..` traversal.

```powershell
mkr                                    # interactive
mkr "fix the null check in login.cs"   # start with a request, stay interactive
mkr -p "summarise this module"         # one request, print and exit
mkr -C C:\other\repo                   # work in a different directory
```

---

## Permission modes

Mode is the main control. Choose it deliberately.

| Mode | Reads | Writes and commands | Use for |
|---|---|---|---|
| **`plan`** | yes | **refused outright** | Understanding unfamiliar code, or getting a change proposed before anyone touches anything |
| **`approve`** | yes | asks you every time | **The default.** Normal day-to-day work |
| **`auto`** | yes | runs without asking | A well-scoped, repetitive task you have already thought through |

```powershell
mkr -mode plan "explain how authentication works in this service"
mkr "add validation to the login handler"          # approve mode
mkr -mode auto "run the tests and fix the failures"
```

Change mode mid-session with `/mode auto`. **Changing mode clears any "always
allow" answers you had given**, so permissions never carry silently across a
change of posture.

In `plan` mode the refusal is returned *to the model*, so it adapts and produces
a plan rather than stalling on a call it cannot make.

Regardless of mode, a deny rule always wins — see
[Configuration](CONFIGURATION.md#permission-rules).

---

## Answering a permission prompt

```
permission required: edit src/auth.go
  writes:   src/auth.go
  allow? [y]es / [n]o / [a]lways / [q]uit:
```

| Answer | Effect |
|---|---|
| `y` | Allow this one action |
| `n` | Refuse it. The assistant is told why and proposes something else |
| `a` | Allow **this exact action** for the rest of the session |
| `q` | End the session now |

**Pressing Enter with nothing typed refuses.** That is deliberate: a prompt
that is easy to accept by accident is not a control. Anything unrecognised also
refuses.

`a` is scoped to the action, not the tool — approving `go test ./...` once does
not approve every future command.

---

## Commands

| Command | Effect |
|---|---|
| `/mode [plan\|approve\|auto]` | Show or change the permission mode |
| `/tools` | List available tools and which of them change things |
| `/skills` | List the installed skills |
| `/cost` | Token usage and how full the context window is |
| `/compact` | Reduce the transcript now, to free context |
| `/audit` | Verify the audit log's integrity |
| `/clear` | Start a fresh conversation, keeping the session |
| `/help` | Command list |
| `/exit` | End the session |

`Ctrl+C` interrupts the current turn and returns you to the prompt — useful when
the assistant starts down the wrong path. `Ctrl+D` ends the session.

---

## Tools

| Tool | Changes things | What it does |
|---|---|---|
| `read_file` | no | Read a file, with line numbers and paging |
| `list_dir` | no | List a directory |
| `glob` | no | Find files by pattern, e.g. `**/*.cs` |
| `grep` | no | Search file contents by regular expression |
| `write_file` | **yes** | Create or replace a file |
| `edit_file` | **yes** | Replace an exact string in a file |
| `exec` | **yes** | Run a command in PowerShell |
| `skill` | no | Load a reusable instruction pack — see [Skills](SKILLS.md) |
| `task` | **yes** | Delegate self-contained work to a sub-agent |

The assistant must read a file before editing it — editing unseen content is
how work gets destroyed, so it is refused rather than discouraged.

`edit_file` requires the target string to appear exactly once unless
`replace_all` is set, so an ambiguous edit fails loudly instead of changing the
wrong line.

---

## Long sessions and the context window

Models have a fixed context window. As a session goes on the transcript grows,
and without intervention it would eventually exceed the window and fail
mid-task.

`mkr` handles this automatically, and tells you when it does:

```
context compaction: elided 9 older tool result(s) (~24100 to ~14800 tokens)
```

It drops the least useful history first — the *contents* of files read many
turns ago, keeping recent ones intact. If that is not enough it drops the oldest
exchanges. Your project instructions, your current request and the recent
conversation are always kept.

Check how full the window is with `/cost`. Compact early with `/compact` if you
are about to give a long instruction.

**If the assistant seems to have forgotten something from earlier in a long
session, that is compaction.** Ask it to read the file again, or start fresh
with `/clear`.

---

## Resuming work

Every session is written to a transcript as it happens, so an interrupted
session is not lost.

```powershell
mkr -resume last              # continue the most recent session
mkr -resume 20260919-143022-a3f9k2
```

`/clear` starts a fresh conversation without ending the session, which is the
right move when the current thread has gone off track.

---

## Project instructions

An `MKR.md` in the repository root is loaded into every session for that
project — the equivalent of a README written for the assistant:

```markdown
# Project notes

- Build with `dotnet build`, test with `dotnet test`.
- Do not edit anything under `Generated/` — it comes from the schema compiler.
- Follow the existing style: tabs, and no comments on self-evident code.
```

This is the highest-leverage thing you can do to improve results. See
[Configuration](CONFIGURATION.md#project-instructions-mkrmd).

---

## Skills and delegation

Skills are reusable instructions kept in `.mkr/skills/`, loaded only when
relevant. Sub-agents handle open-ended investigation without filling your
context with the reading.

Both are covered in **[Skills and sub-agents](SKILLS.md)**.

## Non-interactive use

```powershell
mkr -p "list every TODO in this repository and group them by file"
```

Print mode runs one request and exits, writing to stdout. Combined with
`-mode plan` it is safe in scripts, because nothing can be modified:

```powershell
mkr -mode plan -p "review the changes on this branch for obvious bugs" > review.txt
```

Output is unstyled when redirected, so captured text stays clean.

---

## What is recorded

Everything. Each session appends to a tamper-evident audit log: every prompt,
every model request, every tool call with its arguments, every permission
decision, and a unified diff of every file change.

```powershell
mkr audit verify              # confirm nothing has been altered
```

The log is hash-chained, so editing or deleting a record is detectable:

```
records read: 12
chain:        BROKEN at record 13
problem:      record 13 has been modified since it was written
```

Truncating the end of the file is not detectable from the file alone. If that
matters, ship the log to a write-only collector.

See [Security](SECURITY.md#audit-log) for the full record format.

---

## Secret redaction

Tool output is scanned for credentials before it reaches the model. Private
keys, cloud access keys, tokens, JWTs and password assignments are replaced with
`[REDACTED:kind]`, and you see a note on the tool result:

```
→ read .env
  ✓ read .env (2 lines) (redacted assigned_credential x1, aws_access_key x1)
```

This reduces exposure; it does not eliminate it. Detection is pattern-based and
will miss an unusual credential format. **Keep secrets out of the workspace.**

`-no-redact` disables it. Doing so is warned about on screen and recorded in the
audit log.

---

## Getting good results

**Say what you want changed, not how to change it.** "Add retry with backoff to
the HTTP client" works better than a description of the code to write.

**Work in small steps.** One coherent change per request. Long compound requests
produce long compound diffs that are hard to review at a permission prompt.

**Use `plan` mode to explore.** When you do not yet know what needs changing,
`plan` mode is faster and safer than letting it start editing.

**Write an `MKR.md`.** Build commands, directories to avoid, and house style are
the things it cannot infer and most often gets wrong.

**Let it run the tests.** It recovers from a failing test far better than from a
description of one.

**Refuse freely.** `n` at a prompt is normal, and the assistant adapts. It is
not a failure state.

---

## Troubleshooting

**`probe: cannot list models: connection refused`**
vLLM is not running, or the firewall rule is not in place. Check from the
inference host first with `curl http://localhost:8000/v1/models`.

**`adapter: xml` in the probe output**
The client works, but vLLM's tool-call parser is not producing structured calls
for this model. Check `--tool-call-parser` on the server. Sessions still work;
they are slightly slower and slightly less reliable at tool calls.

**`cannot run pwsh`**
PowerShell 7 is not installed. Either install it, or
`mkr config set shell powershell.exe` to use Windows PowerShell 5.1.

**It keeps asking permission for the same thing**
Answer `a` instead of `y`, or switch to `-mode auto` for that task.

**It forgot something from earlier**
The session was compacted. Ask it to re-read the file, or `/clear` and restate
the task concisely.

**It tried a command that was refused by policy**
Expected — the deny list applies in every mode. If the command is legitimate for
your environment, add it to `allow_commands` or narrow the deny rule in
`rules.json`.

**Something else**
`mkr selftest` checks the whole local installation and prints what is wrong.
