---
name: commit-message
description: Use when writing a git commit message, so the message explains why the change was made rather than restating the diff.
---

# Writing a commit message

Read the staged diff first — `exec` with `git diff --cached` — then write the
message from what the change accomplishes, not from what lines moved.

## Format

```
<short summary, imperative mood, under 72 characters>

<why this change was needed, and anything a reviewer would otherwise have to
work out for themselves. Wrap at 72 columns.>
```

## Rules

- The summary says what the change does: "add retry to the upload client",
  not "changes to upload.go".
- Explain **why**, not what. The diff already shows what.
- Mention anything surprising: a workaround, a constraint you discovered, a
  deliberate omission.
- If the change fixes a defect, say what the defect was, in one line.
- No trailers, no attribution, no tooling references.

## Before finishing

Check the message against the diff once more. If a reviewer reading only the
message would be surprised by something in the diff, that thing belongs in the
message.
