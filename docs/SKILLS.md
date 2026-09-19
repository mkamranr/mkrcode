# Skills and sub-agents

Two ways to extend what `mkr` can do, neither of which weakens the security
model: **skills** are reusable instructions, **sub-agents** are delegated work.

- [Skills](#skills)
- [Writing a skill](#writing-a-skill)
- [Using third-party skills](#using-third-party-skills)
- [Sub-agents](#sub-agents)
- [Why there is no MCP support](#why-there-is-no-mcp-support)

---

## Skills

A skill is a folder containing `SKILL.md` — reusable instructions for a
particular kind of work, loaded only when relevant.

| Location | Scope |
|---|---|
| `<workspace>/.mkr/skills/<name>/SKILL.md` | This project. Commit it, and the team shares it. |
| `<user config>/mkr/skills/<name>/SKILL.md` | Personal, every project. |

A project skill shadows a personal one of the same name.

```console
$ mkr
[approve] › /skills
2 skill(s):
  commit-message           (project)
    Use when writing a git commit message, so the message explains why...
  test-first               (project)
    Use when adding a feature or fixing a bug, to write a failing test...
```

### How they stay cheap

Only each skill's **name and description** go into the system prompt. The body
is fetched on demand when the model decides the skill applies.

This matters more than it sounds. Twenty skills with 12 KB bodies would be
240 KB of context if loaded eagerly — most of a session's window, spent before
anyone types anything. Listing them costs a few hundred tokens instead, and the
body arrives only when it is going to be used.

### Where they sit in the security model

A skill is **instructions, not code**. It steers the model; it cannot do
anything itself. Everything a skill leads the agent to do still passes the
workspace jail, the permission prompt, the deny rules and the audit log.

The agent **cannot write skills**. `.mkr/skills/**` is a protected path in every
mode, including `auto`. A skill you have not reviewed cannot appear because the
agent decided to write one.

---

## Writing a skill

````markdown
---
name: commit-message
description: Use when writing a git commit message, so the message explains why the change was made rather than restating the diff.
---

# Writing a commit message

Read the staged diff first — `exec` with `git diff --cached` — then write the
message from what the change accomplishes.

## Rules

- The summary says what the change does, in imperative mood, under 72 characters.
- Explain **why**, not what. The diff already shows what.
````

### Requirements

- The folder name and the `name` in the frontmatter **must match**. A directory
  listing is then an accurate inventory of what is installed.
- `name` is lowercase letters, digits, dashes and underscores.
- `description` is required, and is the single most important line you write —
  it is all the model sees when deciding whether to load the skill. Say **when
  to use it**, not what it contains.
- Frontmatter supports simple `key: value` entries only. Nested structures and
  lists are rejected with an error naming the file and line, rather than being
  half-understood.
- Bodies are capped at 64 KB. A skill larger than that should be several skills.

### Supporting files

Other files in the skill's folder are listed when the skill loads, so
instructions can refer to a checklist or template and the agent knows it may
read them.

### What makes a good skill

**Describe when to use it precisely.** "Use when reviewing a diff for security
defects before merging" beats "security stuff".

**Write procedure, not prose.** Numbered steps the agent can follow and verify.

**Say how to check the work.** The step that most improves results is the one
that tells the agent how to know it succeeded.

**Keep it focused.** One kind of task per skill. Two loosely related skills beat
one that tries to cover both.

Two worked examples ship in [`examples/skills/`](../examples/skills/).

---

## Using third-party skills

Open-source skill collections use this same markdown format, so most drop in
with little editing. Copy the folder into `.mkr/skills/` and check that the
folder name matches the declared name.

### Tool names

Skills written for other agents refer to tools by different names. `mkr` does
**not** rewrite the skill — substituting names across arbitrary markdown would
corrupt code samples that legitimately contain words like `Read` or `Task`.
Instead it appends a note when it detects them:

```
[Note: this skill refers to tools by names used elsewhere. In this
 environment: Bash = exec, Read = read_file. Use the names on the right.]
```

| Elsewhere | Here |
|---|---|
| `Read` | `read_file` |
| `Write` | `write_file` |
| `Edit`, `MultiEdit` | `edit_file` |
| `Bash`, `Shell` | `exec` |
| `Grep` | `grep` |
| `Glob` | `glob` |
| `LS` | `list_dir` |
| `Task`, `Agent` | `task` |

### Review before installing

A skill is instructions the agent will follow. Read it before adding it to a
shared repository — the same scrutiny you would give a script someone asked you
to run, even though a skill cannot act on its own.

---

## Sub-agents

The `task` tool delegates a self-contained piece of work to a nested agent with
its own context, returning only its findings.

```
→ delegate read-only task: survey the project
  delegating: survey the project
    · list .
    · grep "func main"
  ✓ task complete: survey the project (4820 tokens)
```

**Use it for open-ended investigation** — tracing how something works, searching
across many files, reviewing a large diff. Work that takes twenty tool calls and
produces one paragraph of conclusion is exactly what it is for: the reading stays
out of your main context.

**Do not use it for small, known tasks.** Delegating a single file read costs an
extra round trip and gains nothing.

`read_only: true` removes mutating tools from the sub-agent entirely, which is
the right setting for investigation.

### Constraints, and why

| Constraint | Reason |
|---|---|
| Shares the parent's permission engine | In `approve` mode a sub-agent's writes still prompt **you**. Delegation must never become a way to act unsupervised. |
| Cannot delegate further | A sub-agent does not receive the `task` tool at all — a structural stop, not a counter that could be miscounted. |
| Runs one at a time | Two sub-agents prompting for permission simultaneously would garble the terminal and invite approval by reflex. |
| Own turn budget | One delegated task cannot exhaust the whole session. |
| Cannot see your conversation | Give it a complete instruction. Its reply is all that returns. |

Deny rules apply inside a sub-agent exactly as outside, and its start is
recorded in the audit log with its own event, so the log shows which agent did
what.

---

## Why there is no MCP support

MCP has the largest open-source ecosystem, and integrating it would be
straightforward. It is excluded deliberately.

An MCP server is **third-party code running with the agent's privileges**, and
it can open its own network connections — **bypassing the in-code egress
restriction entirely**. The guarantee that this program can only reach your vLLM
endpoint would become a guarantee about one process among several.

Most servers are also Node or Python packages, which would end the
single-binary, zero-dependency property that lets `mkr` be carried in on
removable media and allowlisted on a locked-down fleet, and would add an npm or
PyPI supply chain to an environment that currently has none.

Those two properties are what make this deployable here. Skills and sub-agents
deliver most of the practical benefit without touching either: a skill is
instructions rather than code, and a sub-agent is this same binary, under the
same permission engine.

If a specific capability is genuinely needed, the better answer is a native tool
implementing `tools.Tool` — one interface, six methods — which inherits the
permission gate, redaction and audit trail automatically.
