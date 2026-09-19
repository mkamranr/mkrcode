# Security model

This document states what `mkr` enforces, what it merely mitigates, and what it
does not address at all. The distinctions matter for accreditation, so they are
made explicitly rather than implied.

- [Threat model](#threat-model)
- [Workspace containment](#workspace-containment)
- [Network egress](#network-egress)
- [Permission model](#permission-model)
- [Secret redaction](#secret-redaction)
- [Audit log](#audit-log)
- [File confidentiality](#file-confidentiality)
- [Extension mechanisms](#extension-mechanisms)
- [Supply chain](#supply-chain)
- [Summary of limitations](#summary-of-limitations)

---

## Threat model

`mkr` sends source code to a language model and executes what that model asks
for. The controls below address three concerns, in order of how seriously they
are treated:

1. **The model behaves unexpectedly** — because it is wrong, or because content
   in the workspace manipulated it (prompt injection). Controls: workspace
   containment, network egress restriction, the permission model.
2. **Sensitive data leaves the enclave or reaches somewhere it should not** —
   controls: egress restriction, secret redaction, file confidentiality.
3. **What happened cannot be established afterwards** — control: the audit log.

It does **not** attempt to defend against a malicious operator. Someone with
interactive access to the workstation can read the same files, run the same
commands, and reach the same network destinations without using `mkr` at all.

---

## Workspace containment

**Enforced in code.** `internal/fsjail` is the security boundary; a defect there
is a containment breach rather than a bug, so the package is deliberately small
and heavily tested.

Every path the model supplies is resolved to absolute, symlink-evaluated, and
required to sit beneath the workspace root. Both the lexical form and the
post-resolution form are checked, so a symlink inside the workspace pointing out
of it is refused.

Windows path grammar is considerably more hostile than POSIX, and each of these
is rejected explicitly:

| Vector | Example |
|---|---|
| Traversal | `..\..\Windows\win.ini` |
| Absolute paths | `C:\Windows\System32\config\SAM` |
| Extended-length prefix | `\\?\C:\Windows\win.ini` |
| Device namespace | `\\.\PhysicalDrive0` |
| UNC network paths | `\\fileserver\share\secret.txt` |
| Drive-relative paths | `C:notes.txt` |
| Alternate data streams | `notes.txt:hidden` |
| Reserved device names | `CON`, `NUL`, `COM1`, `LPT9`, `CON.txt` |
| Trailing dots and spaces | `evil.txt.` |
| Case-varied prefixes | containment compares case-insensitively on Windows |
| Sibling prefix confusion | workspace `C:\ws` must not admit `C:\ws-secrets` |

These are verified by table tests that run on every platform, and again natively
on Windows in CI.

---

## Network egress

**Enforced in code, with one stated limitation.**

`internal/netguard` supplies the only `http.Client` in the binary. Its dialer
compares the resolved destination against the single configured endpoint and
refuses anything else. Redirects off the permitted host are refused explicitly,
and no proxy is consulted.

Both `http` and `https` endpoints are supported; the restriction applies
identically to each, and the destination is checked at dial time, before any
TLS handshake. Certificate verification is always enforced — there is no option
to disable it. An endpoint using an internal certificate authority is supported
by supplying that authority (`ca_cert`), which extends the system trust store
rather than replacing it.

No other package constructs a transport. That invariant is enforced by a CI lint
(`make lint-egress`) which fails the build if any package outside `netguard`
builds an HTTP client — so a future change cannot quietly reintroduce a second
network path.

The practical consequence: **a prompt injection that persuades the model to
fetch an external URL still cannot reach one.**

### The limitation

This constrains the **agent process**. It does not constrain a command the
operator approves through the `exec` tool, which runs with the operator's own
network access.

Mitigation is layered, not absolute:

- Common egress utilities (`curl`, `wget`, `Invoke-WebRequest`, `bitsadmin`,
  `Start-BitsTransfer`, `ssh`, `scp`, `New-Object System.Net.WebClient`) are in
  the default deny list and are refused **in every mode, including `auto`**.
- Commands run under `pwsh -NoProfile -NonInteractive`.
- Every command is recorded in the audit log *before* it runs.
- The child process environment has credential-shaped variables removed.

If egress must be genuinely enforced rather than discouraged, that requires host
firewall or Windows Filtering Platform policy on the workstation, which is
outside the control of this program. **This should be recorded in accreditation
documentation rather than assumed away.**

---

## Permission model

Three modes, plus a rule set that applies across all of them.

| Mode | Mutating tools |
|---|---|
| `plan` | Refused outright |
| `approve` | Operator confirmation required for each |
| `auto` | Permitted |

Independently of mode:

- **Deny rules always win.** There is no mode in which a denied command becomes
  permitted, and a denied request never reaches the operator prompt — it
  short-circuits, so it cannot be approved by reflex.
- **Protected paths cannot be written in any mode**, including mkr's own
  `config.json` and `rules.json`, `.git/hooks`, `.ssh`, and `*.pem` / `*.key` /
  `*.pfx`. Path matching is case-insensitive.
- **Read-before-edit is required.** Editing a file the agent has not read is
  refused.
- **A non-interactive session fails closed.** If confirmation is required and no
  prompt is possible, the action is denied rather than allowed.
- **Changing mode clears remembered approvals**, so an "always allow" granted
  under `approve` cannot survive into a different posture.

An invalid regular expression in the rules file is reported at startup rather
than silently ignored, so a malformed rule cannot disable a control.

---

## Secret redaction

**Heuristic.** This is a meaningful reduction in exposure, not a guarantee.

Redaction runs between tool execution and the message history, so a credential
read from disk never reaches the model, the session transcript, or the audit
log. Detected classes include PEM private key blocks, AWS/Azure/GCP key shapes,
GitHub and Slack tokens, JWTs, bearer tokens, connection-string passwords, and
credential-named variable assignments.

Matches become `[REDACTED:kind]`, and the redaction event — the kind and
location, never the value — is audited.

**It will miss unusual credential formats.** It is not a substitute for keeping
secrets out of the workspace. `-no-redact` disables it; that is warned about on
screen and recorded in the audit log.

---

## Audit log

Append-only JSONL. Each record carries the SHA-256 of the preceding record,
forming a chain from the first line of the file.

Recorded events: session start and end, user prompts, model requests (token
counts and a prompt hash; full bodies only if `audit_prompts` is enabled), every
tool call with its arguments, every permission decision with the rule that
produced it, context compaction, mode changes, redaction events, and a unified
diff of every file mutation.

Records are written **after** redaction, so the audit log never becomes the
place a secret is preserved.

```console
mkr audit verify
```

**What it proves:** altering or deleting any record breaks every hash after it,
and verification reports the first break by record number.

**What it does not prove:** truncating the end of the file is not detectable
from the file alone — a shorter valid prefix is still a valid chain. The record
count is the signal. If tamper *resistance* is required rather than tamper
*evidence*, ship the log to an append-only collector.

An administrator can always rewrite the whole file. The chain establishes
evidence, not immutability.

---

## File confidentiality

The audit log, session transcripts and the configuration file are created
owner-readable only.

On Windows this required explicit work: **Go's file mode bits have essentially
no effect there**, and a file created with `0600` reports mode `0666` and
inherits a permissive ACL. This was found by running the test suite natively on
Windows in CI, where the audit log — containing prompts, tool arguments and
diffs — was world-readable.

`internal/secureio` now applies an explicit protected DACL (`D:P`) granting
access to the file's owner, LocalSystem and Administrators, and to nobody else.
Protected means it does not inherit permissive entries from a parent directory.

---

## Extension mechanisms

Two are supported, chosen because neither weakens anything above.

**Skills** are markdown instructions, not code. A skill steers the model but
cannot act; everything it leads to still passes the jail, the permission engine,
the deny rules and the audit log. `.mkr/skills/**` is a protected path in every
mode, so the agent cannot author instructions for itself.

Skills are still worth reviewing before installing — the same scrutiny as a
script someone asks you to run — because they influence what the agent proposes.

**Sub-agents** share the parent's permission engine instance, so a delegated
write still prompts the operator in `approve` mode and deny rules still apply.
A sub-agent is not given the `task` tool, so recursion is stopped structurally
rather than by a counter. Delegation is recorded in the audit log with its own
event.

### MCP is deliberately excluded

An MCP server is third-party code running with the agent's privileges. It can
open its own network connections, which **bypasses the egress restriction
entirely** — the guarantee that this program reaches only the vLLM endpoint
would become a guarantee about one process among several.

Most servers are Node or Python packages, which would end the zero-dependency
property and introduce an npm or PyPI supply chain into an environment that
currently has none.

Record this as an architectural decision rather than an omission. If a specific
capability is needed, a native tool implementing `tools.Tool` inherits every
control above automatically.

## Supply chain

- **Zero external dependencies.** The module graph is empty; there is no
  `go.sum`. Every line of the binary is either first-party or the Go standard
  library. CI fails if a dependency is ever introduced.
- **`CGO_ENABLED=0`** throughout, so the binary is static and depends on no
  system libraries or glibc version.
- **`-trimpath`** on release builds, so binaries do not embed build paths.
- **Checksums** are published with every bundle; verify before running.
- **Code signing** is the deploying organisation's responsibility. If the fleet
  enforces AppLocker or WDAC, sign `mkr.exe` with an internal certificate.

The dev-only mock server is behind a `devtools` build tag and is never part of a
shipped binary.

---

## Summary of limitations

Stated plainly, for accreditation:

| Area | Status |
|---|---|
| Workspace containment | **Enforced in code** |
| Network destination restriction (agent) | **Enforced in code** |
| Network egress from approved commands | **Mitigated only** — requires host firewall policy |
| Deny rules across all modes | **Enforced in code** |
| Read-before-edit | **Enforced in code** |
| Secret redaction | **Heuristic** — will miss unusual formats |
| Audit record alteration or deletion | **Detectable** |
| Audit log truncation | **Not detectable from the file alone** |
| Protection against a malicious operator | **Not attempted** — out of scope |
| Code signing | **Deploying organisation's responsibility** |
| Skills influencing agent behaviour | **Review before installing** — instructions, not code; cannot escalate |
| Sub-agent acting unsupervised | **Not possible** — shares the parent's permission engine |
| MCP / third-party server code | **Not supported, by design** |

## Reporting a problem

Report suspected security defects through your organisation's internal process.
Include the `mkr version`, the relevant audit log excerpt with any sensitive
values removed, and the minimal steps to reproduce.
