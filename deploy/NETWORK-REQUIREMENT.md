# Network requirement: mkr coding assistant

**Purpose of this document.** It states the single network change required for
the mkr coding assistant to function, in the form a network team needs to
action it. Nothing else in the project is blocked on anything else; this is the
critical path item.

## The request

One inbound TCP allow rule:

| Field | Value |
|---|---|
| Source | Developer workstation segment (Windows clients) |
| Destination | vLLM inference host(s), Ubuntu segment |
| Protocol | TCP |
| Port | **8000** (default; adjust to the port vLLM is bound to) |
| Direction | **Client segment → server segment only** |
| Return traffic | Established/related, as normal for stateful firewalls |

## What is explicitly NOT required

These are stated so the rule can be scoped as narrowly as possible:

- **No outbound internet access from either segment.** Model weights and
  container images are transferred on removable media. Neither the client nor
  the server contacts any external host at any point.
- **No inbound access from the server segment to the client segment.** The
  connection is initiated by the client and is request/response only.
- **No access to any other port on the inference host.** SSH and other
  management access, if needed, is a separate administrative decision and is
  not part of this request.
- **No DNS dependency**, if the endpoint is configured by IP address.

## Why the rule is safe to grant

- The client binary enforces the restriction in code, not only in policy. Its
  HTTP transport refuses to dial any destination other than the single
  configured endpoint, and refuses to follow redirects off it. A single
  compromised or manipulated model response cannot cause the client to contact
  another host.
- The traffic is a standard OpenAI-compatible HTTP API: JSON request bodies and
  a `text/event-stream` response.
- Every request the client makes is recorded in a tamper-evident local audit
  log.

## Stated limitation

The in-code restriction constrains **the agent process**. It does not constrain
commands the operator approves through the agent's shell tool, which run with
the operator's own network access. The agent refuses common egress utilities
(`curl`, `wget`, `Invoke-WebRequest`, `bitsadmin`, `ssh`, `scp` and similar) by
policy in every permission mode, and every command is recorded before it runs.

If egress from the workstation must be genuinely enforced rather than
discouraged, that requires host firewall or Windows Filtering Platform policy on
the workstation itself, which is outside the control of this application. This
limitation should be recorded in the accreditation documentation rather than
assumed away.

## Verifying the rule once granted

From a Windows client, with the binary in place:

```
mkr probe -endpoint http://<inference-host>:8000
```

A successful probe prints the served model, its context length, and the
tool-calling mode in use. Any failure prints the specific reason, which
distinguishes a firewall problem from a vLLM configuration problem.
